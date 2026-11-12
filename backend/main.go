package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed web/*
var web embed.FS

type App struct {
	db                 *sql.DB
	api                *CurseForgeAdapter
	scrapers           []SourceAdapter
	archiveDir         string
	thumbnailClient    *http.Client
	thumbnailMu        sync.Mutex
	thumbnailCache     map[string]cachedThumbnail
	sourceDownloadHTTP *http.Client
}

type cachedThumbnail struct {
	contentType string
	body        []byte
	expires     time.Time
}

func main() {
	dbPath := env("DB_PATH", "/data/catalog.db")
	if err := os.MkdirAll(dirOf(dbPath), 0755); err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		log.Fatal(err)
	}
	archiveDir := env("ARCHIVE_PATH", "/archive")
	if err := os.MkdirAll(archiveDir, 0750); err != nil {
		log.Fatal(err)
	}
	app := &App{db: db, api: NewCurseForgeAdapter(os.Getenv("CURSEFORGE_API_KEY"), nil), scrapers: NewHTMLSources(nil), archiveDir: archiveDir}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/status", app.status)
	mux.HandleFunc("GET /api/wickedwhims/status", app.wickedWhimsStatus)
	mux.HandleFunc("GET /api/client/version", app.clientVersion)
	mux.HandleFunc("GET /api/client/download", app.downloadClient)
	mux.HandleFunc("GET /api/sources", app.sources)
	mux.HandleFunc("GET /api/repos", app.listRepos)
	mux.HandleFunc("POST /api/repos", app.addRepo)
	mux.HandleFunc("GET /api/mods", app.listMods)
	mux.HandleFunc("GET /api/library", app.library)
	mux.HandleFunc("GET /api/thumbnail", app.thumbnail)
	mux.HandleFunc("GET /api/trending", app.trending)
	mux.HandleFunc("GET /api/browse", app.browse)
	mux.HandleFunc("GET /api/search", app.search)
	mux.HandleFunc("GET /api/curseforge/mods/{modID}", app.curseForgeMod)
	mux.HandleFunc("GET /api/curseforge/mods/{modID}/files", app.curseForgeFiles)
	mux.HandleFunc("GET /api/curseforge/mods/{modID}/files/{fileID}/download-url", app.curseForgeDownloadURL)
	mux.HandleFunc("POST /api/curseforge/mods/{modID}/files/{fileID}/import", app.importCurseForgeFile)
	mux.HandleFunc("POST /api/curseforge/fingerprints", app.curseForgeFingerprints)
	mux.HandleFunc("POST /api/reconcile/{source}", app.reconcile)
	mux.HandleFunc("GET /api/desired", app.getDesired)
	mux.HandleFunc("GET /api/updates", app.updates)
	mux.HandleFunc("POST /api/desired", app.setDesired)
	mux.HandleFunc("DELETE /api/desired/{modID}", app.uninstallDesired)
	mux.HandleFunc("GET /api/clients", app.listClients)
	mux.HandleFunc("GET /api/profiles", app.listProfiles)
	mux.HandleFunc("POST /api/profiles", app.saveProfile)
	mux.HandleFunc("POST /api/profiles/{profileID}/mods", app.saveProfileMod)
	mux.HandleFunc("DELETE /api/profiles/{profileID}", app.deleteProfile)
	mux.HandleFunc("POST /api/clients/{clientID}/profile", app.assignClientProfile)
	mux.HandleFunc("POST /api/clients", app.registerClient)
	mux.HandleFunc("GET /api/clients/{clientID}/desired", app.clientDesired)
	mux.HandleFunc("POST /api/clients/{clientID}/audit", app.auditInventory)
	mux.HandleFunc("GET /api/inventory/compare", app.compareInventory)
	mux.HandleFunc("GET /api/annotations", app.listAnnotations)
	mux.HandleFunc("PUT /api/mods/{modID}/annotations/{kind}", app.setAnnotation)
	mux.HandleFunc("GET /api/conflicts", app.listConflicts)
	mux.HandleFunc("POST /api/clients/{clientID}/inventory", app.reportInventory)
	mux.HandleFunc("POST /api/clients/{clientID}/sync/ack", app.acknowledgeSync)
	mux.HandleFunc("POST /api/mods/{modID}/archives", app.uploadArchive)
	mux.HandleFunc("GET /api/mods/{modID}/details", app.catalogDetails)
	mux.HandleFunc("POST /api/mods/{modID}/download", app.downloadSourceArchive)
	mux.HandleFunc("GET /api/archives/{hash}", app.downloadArchive)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := web.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	addr := env("LISTEN_ADDR", ":8080")
	log.Printf("sims4-mods API listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logging(mux)))
}

const maxThumbnailBytes = 8 << 20

func (a *App) thumbnail(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() != "static.loverslab.com" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		http.Error(w, "thumbnail host is not allowed", http.StatusBadRequest)
		return
	}
	if u.Path == "" {
		http.Error(w, "invalid thumbnail URL", http.StatusBadRequest)
		return
	}

	a.thumbnailMu.Lock()
	if hit, ok := a.thumbnailCache[rawURL]; ok && time.Now().Before(hit.expires) {
		a.thumbnailMu.Unlock()
		w.Header().Set("Content-Type", hit.contentType)
		w.Header().Set("Cache-Control", "public, max-age=600")
		_, _ = w.Write(hit.body)
		return
	}
	a.thumbnailMu.Unlock()

	client := a.thumbnailClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, "invalid thumbnail URL", http.StatusBadRequest)
		return
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Sims4ModManager/1.0)")
	request.Header.Set("Referer", "https://www.loverslab.com/")
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "thumbnail fetch failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		http.Error(w, "thumbnail fetch failed", http.StatusBadGateway)
		return
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		http.Error(w, "upstream did not return an image", http.StatusBadGateway)
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxThumbnailBytes+1))
	if err != nil || len(body) > maxThumbnailBytes {
		http.Error(w, "thumbnail too large or unreadable", http.StatusBadGateway)
		return
	}
	a.thumbnailMu.Lock()
	if a.thumbnailCache == nil {
		a.thumbnailCache = make(map[string]cachedThumbnail)
	}
	if len(a.thumbnailCache) >= 128 {
		for key, entry := range a.thumbnailCache {
			if time.Now().After(entry.expires) {
				delete(a.thumbnailCache, key)
			}
		}
		if len(a.thumbnailCache) >= 128 {
			for key := range a.thumbnailCache {
				delete(a.thumbnailCache, key)
				break
			}
		}
	}
	a.thumbnailCache[rawURL] = cachedThumbnail{contentType: contentType, body: body, expires: time.Now().Add(10 * time.Minute)}
	a.thumbnailMu.Unlock()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=600")
	_, _ = w.Write(body)
}

const clientExecutableName = "sims4-mod-manager.exe"

func (a *App) clientExecutable() (*os.File, string, string, error) {
	path := filepath.Join(a.archiveDir, clientExecutableName)
	f, err := os.Open(path)
	if err != nil {
		return nil, "", "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, "", "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, "", "", err
	}
	version := strings.TrimSpace(os.Getenv("CLIENT_VERSION"))
	if version == "" {
		version = "dev"
	}
	return f, version, hex.EncodeToString(h.Sum(nil)), nil
}

func (a *App) clientVersion(w http.ResponseWriter, r *http.Request) {
	f, version, digest, err := a.clientExecutable()
	if err != nil {
		http.Error(w, "Windows client build is not published", http.StatusServiceUnavailable)
		return
	}
	_ = f.Close()
	writeJSON(w, http.StatusOK, map[string]string{"version": version, "download_url": "/api/client/download", "sha256": digest})
}

func (a *App) downloadClient(w http.ResponseWriter, r *http.Request) {
	f, _, digest, err := a.clientExecutable()
	if err != nil {
		http.Error(w, "Windows client build is not published", http.StatusServiceUnavailable)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "could not read Windows client build", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
	w.Header().Set("Content-Disposition", `attachment; filename="sims4-mod-manager.exe"`)
	w.Header().Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, clientExecutableName, info.ModTime(), f)
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"curseforge": map[string]any{"configured": a.api.Configured()}})
}

func (a *App) wickedWhimsStatus(w http.ResponseWriter, r *http.Request) {
	var current, previous, pageURL string
	err := a.db.QueryRow(`SELECT current_version,previous_version,url FROM mods WHERE source='WickedWhims' ORDER BY id LIMIT 1`).Scan(&current, &previous, &pageURL)
	if err == sql.ErrNoRows {
		http.Error(w, "WickedWhims has not been reconciled yet", 404)
		return
	}
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	result := map[string]any{"name": "WickedWhims", "current_version": current, "previous_version": previous, "update_available": current != "" && previous != "" && current != previous, "url": pageURL}
	for _, adapter := range a.scrapers {
		if html, ok := adapter.(*HTMLAdapter); ok && html.Source() == "WickedWhims" {
			if info, infoErr := html.WickedWhimsOfficialInfo(r.Context()); infoErr == nil {
				result["official_version"] = info.Version
				result["supported_game_version"] = info.SupportedVersion
				result["conflicting_mods"] = info.Conflicts
			}
			break
		}
	}
	writeJSON(w, 200, result)
}
func (a *App) sources(w http.ResponseWriter, r *http.Request) {
	sources := []map[string]any{{"name": "CurseForge", "configured": a.api.Configured(), "type": "api"}}
	for _, s := range a.scrapers {
		sources = append(sources, map[string]any{"name": s.Source(), "configured": true, "type": "html"})
	}
	custom, err := a.customRepoAdapters()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	for _, s := range custom {
		sources = append(sources, map[string]any{"name": s.Source(), "configured": true, "type": "rss"})
	}
	writeJSON(w, 200, sources)
}
func (a *App) listMods(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,name,creator,description,source,url,last_seen,CASE WHEN EXISTS(SELECT 1 FROM mod_versions v WHERE v.mod_id=mods.id) THEN 1 ELSE 0 END,thumbnail_url,source_download_url FROM mods WHERE catalog_active=1 AND url NOT LIKE '%/blog/%' ORDER BY name LIMIT 500`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	mods := []map[string]any{}
	for rows.Next() {
		var id int64
		var hasArchive int
		var name, creator, desc, source, url, updated, thumbnail, sourceDownload string
		if err := rows.Scan(&id, &name, &creator, &desc, &source, &url, &updated, &hasArchive, &thumbnail, &sourceDownload); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		mods = append(mods, map[string]any{"id": id, "name": name, "creator": creator, "description": desc, "source": source, "url": url, "updated_at": updated, "has_archive": hasArchive == 1, "thumbnail_url": thumbnail, "source_download_url": sourceDownload})
	}
	writeJSON(w, 200, mods)
}

func (a *App) library(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT m.id,m.name,m.creator,m.description,m.source,m.url,m.thumbnail_url,
		COALESCE((SELECT value FROM annotations WHERE mod_id=m.id AND kind='note'),''),
		COALESCE((SELECT value FROM annotations WHERE mod_id=m.id AND kind='flag'),''),
		COALESCE((SELECT value FROM annotations WHERE mod_id=m.id AND kind='blacklist'),''),
		EXISTS(SELECT 1 FROM mod_versions v WHERE v.mod_id=m.id),
		EXISTS(SELECT 1 FROM mod_versions v WHERE v.mod_id=m.id AND v.tracking_source='curseforge_api_key'),
		EXISTS(SELECT 1 FROM client_inventory i WHERE i.mod_id=m.id)
		FROM mods m WHERE EXISTS(SELECT 1 FROM mod_versions v WHERE v.mod_id=m.id) OR EXISTS(SELECT 1 FROM client_inventory i WHERE i.mod_id=m.id) ORDER BY m.name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, creator, desc, source, pageURL, thumbnail, note, flag, blacklist string
		var archived, curseForgeTracked, installed bool
		if err := rows.Scan(&id, &name, &creator, &desc, &source, &pageURL, &thumbnail, &note, &flag, &blacklist, &archived, &curseForgeTracked, &installed); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		items = append(items, map[string]any{"id": id, "name": name, "creator": creator, "description": desc, "source": source, "url": pageURL, "thumbnail_url": thumbnail, "note": note, "flag": flag, "blacklist": blacklist, "has_archive": archived, "installed": installed, "curseforge_api_tracked_locally": curseForgeTracked})
	}
	writeJSON(w, 200, items)
}

func (a *App) trending(w http.ResponseWriter, r *http.Request) {
	result := map[string]any{}
	if a.api.Configured() {
		mods, err := a.api.Trending(r.Context(), 12)
		if err == nil && len(mods) == 0 {
			mods, err = a.api.Search(r.Context(), "", 0, 12)
		}
		if err == nil && a.persistCatalog(mods) == nil {
			result["CurseForge"] = mods
		} else {
			result["CurseForge"] = []CatalogMod{}
		}
	} else {
		result["CurseForge"] = []CatalogMod{}
	}
	for _, adapter := range a.scrapers {
		mods, err := adapter.Search(r.Context(), "", 12)
		if err == nil && len(mods) == 0 {
			mods, err = adapter.Search(r.Context(), "", 100)
		}
		if err != nil {
			result[adapter.Source()] = []CatalogMod{}
			continue
		}
		if a.persistCatalog(mods) != nil {
			result[adapter.Source()] = []CatalogMod{}
			continue
		}
		result[adapter.Source()] = mods
	}
	custom, err := a.customRepoAdapters()
	if err == nil {
		for _, adapter := range custom {
			mods, searchErr := adapter.Search(r.Context(), "", 12)
			if searchErr == nil && len(mods) == 0 {
				mods, searchErr = adapter.Search(r.Context(), "", 100)
			}
			if searchErr == nil && a.persistCatalog(mods) == nil {
				result[adapter.Source()] = mods
			} else {
				result[adapter.Source()] = []CatalogMod{}
			}
		}
	}
	writeJSON(w, 200, result)
}

// browse returns a page from a source's current catalogue. The HTML and feed
// adapters expose their fetched front-page/feed listing; those results are
// cached by the adapters and paginated here. CurseForge supports upstream
// offset pagination, so it is paged directly.
func (a *App) browse(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if offset < 0 {
		offset = 0
	}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	if source == "CurseForge" {
		if !a.api.Configured() {
			http.Error(w, "CurseForge is not configured", http.StatusServiceUnavailable)
			return
		}
		mods, err := a.api.Search(r.Context(), "", offset, limit)
		if err != nil {
			http.Error(w, "CurseForge browse failed", http.StatusBadGateway)
			return
		}
		if err := a.persistCatalog(mods); err != nil {
			http.Error(w, "catalog write failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"source": source, "items": mods, "offset": offset, "limit": limit, "has_more": len(mods) == limit})
		return
	}
	var adapter SourceAdapter
	for _, candidate := range a.scrapers {
		if candidate.Source() == source {
			adapter = candidate
			break
		}
	}
	if adapter == nil {
		custom, err := a.customRepoAdapters()
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		for _, candidate := range custom {
			if candidate.Source() == source {
				adapter = candidate
				break
			}
		}
	}
	if adapter == nil {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	if paged, ok := adapter.(paginatedSourceAdapter); ok {
		mods, hasMore, err := paged.Browse(r.Context(), r.URL.Query().Get("category"), offset, limit)
		if err != nil {
			http.Error(w, "source browse failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := a.persistCatalog(mods); err != nil {
			http.Error(w, "catalog write failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"source": source, "items": mods, "offset": offset, "limit": limit, "has_more": hasMore})
		return
	}
	mods, err := adapter.Search(r.Context(), "", 100)
	if err != nil {
		http.Error(w, "source browse failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := a.persistCatalog(mods); err != nil {
		http.Error(w, "catalog write failed", 500)
		return
	}
	total := len(mods)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source, "items": mods[offset:end], "offset": offset, "limit": limit, "total": total, "has_more": end < total})
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, []any{})
		return
	}
	source := r.URL.Query().Get("source")
	if source == "CurseForge" && a.api.Configured() {
		result, err := a.api.Search(r.Context(), q, 0, 20)
		if err != nil {
			http.Error(w, "CurseForge search failed", 502)
			return
		}
		if err := a.persistCatalog(result); err != nil {
			http.Error(w, "catalog write failed", 500)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	for _, adapter := range a.scrapers {
		if source == adapter.Source() {
			result, err := adapter.Search(r.Context(), q, 30)
			if err != nil {
				http.Error(w, "source search failed: "+err.Error(), 502)
				return
			}
			if err := a.persistCatalog(result); err != nil {
				http.Error(w, "catalog write failed", 500)
				return
			}
			writeJSON(w, 200, result)
			return
		}
	}
	if source != "" {
		custom, err := a.customRepoAdapters()
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		for _, adapter := range custom {
			if source != adapter.Source() {
				continue
			}
			result, err := adapter.Search(r.Context(), q, 100)
			if err != nil {
				http.Error(w, "custom repository search failed: "+err.Error(), 502)
				return
			}
			if err := a.persistCatalog(result); err != nil {
				http.Error(w, "catalog write failed", 500)
				return
			}
			writeJSON(w, 200, result)
			return
		}
	}
	rows, err := a.db.Query(`SELECT id,name,creator,description,source,url,last_seen,CASE WHEN EXISTS(SELECT 1 FROM mod_versions v WHERE v.mod_id=mods.id) THEN 1 ELSE 0 END,thumbnail_url,source_download_url FROM mods WHERE catalog_active=1 AND url NOT LIKE '%/blog/%' AND (name LIKE ? OR creator LIKE ?) ORDER BY name LIMIT 100`, "%"+q+"%", "%"+q+"%")
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	mods := []map[string]any{}
	for rows.Next() {
		var id int64
		var hasArchive int
		var name, creator, desc, source, url, updated, thumbnail, sourceDownload string
		if rows.Scan(&id, &name, &creator, &desc, &source, &url, &updated, &hasArchive, &thumbnail, &sourceDownload) == nil {
			mods = append(mods, map[string]any{"id": id, "name": name, "creator": creator, "description": desc, "source": source, "url": url, "updated_at": updated, "has_archive": hasArchive == 1, "thumbnail_url": thumbnail, "source_download_url": sourceDownload})
		}
	}
	writeJSON(w, 200, mods)
}

func parseCurseForgeID(r *http.Request, key string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid CurseForge id")
	}
	return id, nil
}

func (a *App) curseForgeMod(w http.ResponseWriter, r *http.Request) {
	id, err := parseCurseForgeID(r, "modID")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	data, err := a.api.ModDetails(r.Context(), id)
	if err != nil {
		http.Error(w, "CurseForge mod lookup failed: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (a *App) curseForgeFiles(w http.ResponseWriter, r *http.Request) {
	id, err := parseCurseForgeID(r, "modID")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	data, err := a.api.ModFiles(r.Context(), id)
	if err != nil {
		http.Error(w, "CurseForge file lookup failed: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (a *App) curseForgeDownloadURL(w http.ResponseWriter, r *http.Request) {
	modID, err := parseCurseForgeID(r, "modID")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fileID, err := parseCurseForgeID(r, "fileID")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	value, err := a.api.DownloadURL(r.Context(), modID, fileID)
	if err != nil {
		http.Error(w, "CurseForge download lookup failed: "+err.Error(), 502)
		return
	}
	writeJSON(w, 200, map[string]string{"download_url": value})
}

func (a *App) curseForgeFingerprints(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Fingerprints []uint32 `json:"fingerprints"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || len(input.Fingerprints) == 0 || len(input.Fingerprints) > 10000 {
		http.Error(w, "fingerprints must contain between 1 and 10000 values", 400)
		return
	}
	data, err := a.api.FingerprintMatches(r.Context(), input.Fingerprints)
	if err != nil {
		http.Error(w, "CurseForge fingerprint lookup failed: "+err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (a *App) reconcile(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	var adapter SourceAdapter
	for _, candidate := range a.scrapers {
		if candidate.Source() == source {
			adapter = candidate
			break
		}
	}
	if adapter == nil {
		custom, err := a.customRepoAdapters()
		if err != nil {
			http.Error(w, "database error", 500)
			return
		}
		for _, candidate := range custom {
			if candidate.Source() == source {
				adapter = candidate
				break
			}
		}
	}
	if adapter == nil {
		http.Error(w, "unknown source", 404)
		return
	}
	mods, err := adapter.Search(r.Context(), r.URL.Query().Get("q"), 100)
	if err != nil {
		log.Printf("source reconciliation failed for %q: %v", source, err)
		http.Error(w, "source reconciliation failed: "+err.Error(), 502)
		return
	}
	if err := a.persistCatalog(mods); err != nil {
		http.Error(w, "catalog write failed", 500)
		return
	}
	if source == "LoversLab" {
		if err := a.markSourceCatalogCurrent(source, mods); err != nil {
			http.Error(w, "catalog reconciliation state failed", 500)
			return
		}
	}
	if source == "customcontent.net" {
		if _, err := a.db.Exec(`UPDATE mods SET source_details_checked=0 WHERE source=?`, source); err != nil {
			http.Error(w, "catalog metadata state failed", 500)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"source": source, "stored": len(mods)})
}

func (a *App) markSourceCatalogCurrent(source string, mods []CatalogMod) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE mods SET catalog_active=0 WHERE source=?`, source); err != nil {
		return err
	}
	for _, mod := range mods {
		externalID := mod.URL
		if mod.Source == "CurseForge" && mod.ID != 0 {
			externalID = strconv.Itoa(mod.ID)
		} else if externalID == "" && mod.ID != 0 {
			externalID = fmt.Sprintf("%d", mod.ID)
		}
		if externalID == "" {
			continue
		}
		if _, err = tx.Exec(`UPDATE mods SET catalog_active=1 WHERE source=? AND external_id=?`, source, externalID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) persistCatalog(mods []CatalogMod) error {
	for i := range mods {
		externalID := mods[i].URL
		if mods[i].Source == "CurseForge" && mods[i].ID != 0 {
			externalID = strconv.Itoa(mods[i].ID)
		}
		if externalID == "" && mods[i].ID != 0 {
			externalID = fmt.Sprintf("%d", mods[i].ID)
		}
		if externalID == "" {
			continue
		}
		_, err := a.db.Exec(`INSERT INTO mods(external_id,name,creator,description,source,url,last_seen,thumbnail_url,source_download_url,current_version,previous_version) VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP,?,?,?,'') ON CONFLICT(source,external_id) DO UPDATE SET name=excluded.name,creator=excluded.creator,description=CASE WHEN excluded.description<>'' THEN excluded.description ELSE mods.description END,url=excluded.url,last_seen=CURRENT_TIMESTAMP,thumbnail_url=CASE WHEN excluded.thumbnail_url<>'' THEN excluded.thumbnail_url ELSE mods.thumbnail_url END,source_download_url=CASE WHEN excluded.source_download_url<>'' THEN excluded.source_download_url ELSE mods.source_download_url END,current_version=CASE WHEN excluded.current_version<>'' THEN excluded.current_version ELSE mods.current_version END,previous_version=CASE WHEN excluded.current_version<>'' AND mods.current_version<>'' AND excluded.current_version<>mods.current_version THEN mods.current_version ELSE mods.previous_version END,catalog_active=1`, externalID, mods[i].Name, mods[i].Creator, mods[i].Description, mods[i].Source, mods[i].URL, mods[i].ThumbnailURL, mods[i].SourceDownloadURL, mods[i].Version)
		if err != nil {
			return err
		}
		if err = a.db.QueryRow(`SELECT id FROM mods WHERE source=? AND external_id=?`, mods[i].Source, externalID).Scan(&mods[i].CatalogID); err != nil {
			return err
		}
		var versionCount int
		if err = a.db.QueryRow(`SELECT count(*) FROM mod_versions WHERE mod_id=?`, mods[i].CatalogID).Scan(&versionCount); err != nil {
			return err
		}
		mods[i].HasArchive = versionCount > 0
		if err = a.db.QueryRow(`SELECT description,source_download_url,current_version FROM mods WHERE id=?`, mods[i].CatalogID).Scan(&mods[i].Description, &mods[i].SourceDownloadURL, &mods[i].Version); err != nil {
			return err
		}
	}
	return nil
}

type desiredInput struct {
	ModID    int64   `json:"mod_id"`
	ClientID *string `json:"client_id"`
	Version  string  `json:"version"`
	Enabled  *bool   `json:"enabled"`
}

func (a *App) setDesired(w http.ResponseWriter, r *http.Request) {
	var input desiredInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		http.Error(w, "invalid request body", 400)
		return
	}
	if input.ModID < 1 {
		http.Error(w, "mod_id is required", 400)
		return
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.Version == "" {
		input.Version = "latest"
	}
	if input.ClientID != nil {
		var exists int
		if a.db.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, *input.ClientID).Scan(&exists) != nil || exists == 0 {
			http.Error(w, "unknown client_id", 404)
			return
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	var exists int
	if tx.QueryRow(`SELECT count(*) FROM mods WHERE id=?`, input.ModID).Scan(&exists) != nil || exists == 0 {
		http.Error(w, "unknown mod_id", 404)
		return
	}
	_, err = tx.Exec(`DELETE FROM installed_mods WHERE mod_id=? AND client_id IS ?`, input.ModID, input.ClientID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	_, err = tx.Exec(`INSERT INTO installed_mods(mod_id,client_id,version,enabled,desired,updated_at) VALUES(?,?,?,?,1,CURRENT_TIMESTAMP)`, input.ModID, input.ClientID, input.Version, boolInt(enabled))
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if err = queueSyncAction(tx, input.ModID, input.ClientID, desiredAction(true, enabled)); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"mod_id": input.ModID, "client_id": input.ClientID, "version": input.Version, "enabled": enabled, "desired": true})
}
func (a *App) uninstallDesired(w http.ResponseWriter, r *http.Request) {
	modID, err := strconv.ParseInt(r.PathValue("modID"), 10, 64)
	if err != nil || modID < 1 {
		http.Error(w, "invalid mod id", 400)
		return
	}
	var clientID *string
	if v := r.URL.Query().Get("client_id"); v != "" {
		clientID = &v
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE installed_mods SET desired=0,updated_at=CURRENT_TIMESTAMP WHERE mod_id=? AND client_id IS ? AND desired=1`, modID, clientID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		http.Error(w, "desired mod not found", 404)
		return
	}
	if err = queueSyncAction(tx, modID, clientID, "uninstall"); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"mod_id": modID, "client_id": clientID, "desired": false})
}
func (a *App) getDesired(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	var rows *sql.Rows
	var err error
	if clientID == "" {
		rows, err = a.db.Query(`SELECT im.mod_id,m.name,m.source,im.client_id,im.version,im.enabled,im.desired,im.notes,COALESCE(v.download_url,''),COALESCE(v.sha256,''),COALESCE(v.file_name,'') FROM installed_mods im JOIN mods m ON m.id=im.mod_id LEFT JOIN mod_versions v ON v.id=(SELECT mv.id FROM mod_versions mv WHERE mv.mod_id=m.id AND (mv.version=im.version OR im.version='latest') ORDER BY mv.created_at DESC LIMIT 1) ORDER BY m.name`)
	} else {
		var exists int
		if a.db.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, clientID).Scan(&exists) != nil || exists == 0 {
			http.Error(w, "unknown client", 404)
			return
		}
		rows, err = a.db.Query(effectiveDesiredSQL, clientID, clientID)
	}
	if err != nil {
		log.Printf("desired state query failed: %v", err)
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	writeDesiredRows(w, rows)
}

func (a *App) updates(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT im.mod_id,m.name,im.client_id,im.version,latest.version
		FROM installed_mods im JOIN mods m ON m.id=im.mod_id
		JOIN mod_versions latest ON latest.id=(SELECT mv.id FROM mod_versions mv WHERE mv.mod_id=im.mod_id ORDER BY mv.created_at DESC,mv.id DESC LIMIT 1)
		WHERE im.desired=1 AND im.version<>'latest' AND im.version<>latest.version ORDER BY m.name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, version, latest string
		var clientID sql.NullString
		if rows.Scan(&id, &name, &clientID, &version, &latest) != nil {
			continue
		}
		out = append(out, map[string]any{"mod_id": id, "name": name, "client_id": clientID.String, "version": version, "latest_version": latest})
	}
	writeJSON(w, 200, out)
}
func (a *App) clientDesired(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	var exists int
	if a.db.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, clientID).Scan(&exists) != nil || exists == 0 {
		http.Error(w, "unknown client", 404)
		return
	}
	_, _ = a.db.Exec(`UPDATE clients SET last_seen=CURRENT_TIMESTAMP WHERE id=?`, clientID)
	rows, err := a.db.Query(effectiveDesiredSQL, clientID, clientID)
	if err != nil {
		log.Printf("client desired query failed: %v", err)
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	writeDesiredRows(w, rows)
}

const effectiveDesiredSQL = `SELECT m.id,m.name,m.source,COALESCE(own.client_id,global.client_id),COALESCE(own.version,pm.version,global.version),COALESCE(own.enabled,pm.enabled,global.enabled),COALESCE(own.desired,pm.desired,global.desired),COALESCE(own.notes,pm.notes,global.notes),COALESCE(v.download_url,''),COALESCE(v.sha256,''),COALESCE(v.file_name,'') FROM mods m JOIN clients c ON c.id=? LEFT JOIN installed_mods global ON global.mod_id=m.id AND global.client_id IS NULL LEFT JOIN profile_mods pm ON pm.mod_id=m.id AND pm.profile_id=c.profile_id LEFT JOIN installed_mods own ON own.mod_id=m.id AND own.client_id=c.id LEFT JOIN mod_versions v ON v.id=(SELECT mv.id FROM mod_versions mv WHERE mv.mod_id=m.id AND (mv.version=COALESCE(own.version,pm.version,global.version) OR COALESCE(own.version,pm.version,global.version)='latest') ORDER BY mv.created_at DESC LIMIT 1) WHERE own.id IS NOT NULL OR pm.mod_id IS NOT NULL OR global.id IS NOT NULL ORDER BY m.name`

func queueSyncAction(tx *sql.Tx, modID int64, clientID *string, action string) error {
	if clientID != nil {
		if _, err := tx.Exec(`DELETE FROM sync_jobs WHERE client_id=? AND mod_id=? AND status='pending'`, *clientID, modID); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sync_jobs(client_id,mod_id,action,payload) VALUES(?,?,?,'{}')`, *clientID, modID, action)
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sync_jobs WHERE mod_id=? AND status='pending'`, modID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO sync_jobs(client_id,mod_id,action,payload) SELECT id,?,?, '{}' FROM clients`, modID, action)
	return err
}

func (a *App) listClients(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT c.id,c.name,c.last_seen,c.local_path,c.profile_id,COALESCE(p.name,'') FROM clients c LEFT JOIN profiles p ON p.id=c.profile_id ORDER BY c.name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, path, profileName string
		var lastSeen sql.NullString
		var profileID sql.NullInt64
		if rows.Scan(&id, &name, &lastSeen, &path, &profileID, &profileName) != nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "name": name, "last_seen": lastSeen.String, "local_path": path, "profile_id": nullableInt(profileID), "profile_name": profileName})
	}
	writeJSON(w, 200, out)
}

func nullableInt(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func (a *App) listProfiles(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT p.id,p.name,p.created_at,pm.mod_id,m.name,pm.version,pm.enabled,pm.desired,pm.notes FROM profiles p LEFT JOIN profile_mods pm ON pm.profile_id=p.id LEFT JOIN mods m ON m.id=pm.mod_id ORDER BY p.name,m.name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	byID := map[int64]map[string]any{}
	ordered := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, created string
		var modID sql.NullInt64
		var modName, version, notes sql.NullString
		var enabled, desired sql.NullInt64
		if rows.Scan(&id, &name, &created, &modID, &modName, &version, &enabled, &desired, &notes) != nil {
			continue
		}
		p := byID[id]
		if p == nil {
			p = map[string]any{"id": id, "name": name, "created_at": created, "mods": []any{}}
			byID[id] = p
			ordered = append(ordered, p)
		}
		if modID.Valid {
			list := p["mods"].([]any)
			list = append(list, map[string]any{"mod_id": modID.Int64, "name": modName.String, "version": version.String, "enabled": enabled.Int64 == 1, "desired": desired.Int64 == 1, "notes": notes.String})
			p["mods"] = list
		}
	}
	writeJSON(w, 200, ordered)
}

func parseProfileID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid profile id")
	}
	return id, nil
}

func (a *App) saveProfile(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input) != nil || strings.TrimSpace(input.Name) == "" {
		http.Error(w, "name is required", 400)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.ID > 0 {
		if _, err := a.db.Exec(`UPDATE profiles SET name=? WHERE id=?`, input.Name, input.ID); err != nil {
			http.Error(w, "profile not found or name already used", 409)
			return
		}
		writeJSON(w, 200, map[string]any{"id": input.ID, "name": input.Name})
		return
	}
	res, err := a.db.Exec(`INSERT INTO profiles(name) VALUES(?)`, input.Name)
	if err != nil {
		http.Error(w, "profile name already exists", 409)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name})
}

func (a *App) queueClientReconciliation(tx *sql.Tx, clientID string) error {
	if _, err := tx.Exec(`DELETE FROM sync_jobs WHERE client_id=? AND status='pending'`, clientID); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT m.id,CASE WHEN COALESCE(own.desired,pm.desired,global.desired,0)=0 THEN 'uninstall' WHEN COALESCE(own.enabled,pm.enabled,global.enabled,1)=0 THEN 'disable' ELSE 'install' END FROM mods m JOIN clients c ON c.id=? LEFT JOIN installed_mods own ON own.mod_id=m.id AND own.client_id=c.id LEFT JOIN profile_mods pm ON pm.mod_id=m.id AND pm.profile_id=c.profile_id LEFT JOIN installed_mods global ON global.mod_id=m.id AND global.client_id IS NULL WHERE own.id IS NOT NULL OR pm.mod_id IS NOT NULL OR global.id IS NOT NULL OR EXISTS(SELECT 1 FROM profile_mods anypm WHERE anypm.mod_id=m.id)`, clientID)
	if err != nil {
		return err
	}
	type actionRow struct {
		modID  int64
		action string
	}
	actions := []actionRow{}
	for rows.Next() {
		var item actionRow
		if err := rows.Scan(&item.modID, &item.action); err != nil {
			rows.Close()
			return err
		}
		actions = append(actions, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range actions {
		cid := clientID
		if err := queueSyncAction(tx, item.modID, &cid, item.action); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) queueProfileClients(tx *sql.Tx, profileID int64) error {
	rows, err := tx.Query(`SELECT id FROM clients WHERE profile_id=?`, profileID)
	if err != nil {
		return err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if err := a.queueClientReconciliation(tx, id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (a *App) saveProfileMod(w http.ResponseWriter, r *http.Request) {
	profileID, err := parseProfileID(r.PathValue("profileID"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var input struct {
		ModID   int64  `json:"mod_id"`
		Version string `json:"version"`
		Enabled *bool  `json:"enabled"`
		Desired *bool  `json:"desired"`
		Notes   string `json:"notes"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input) != nil || input.ModID < 1 {
		http.Error(w, "mod_id is required", 400)
		return
	}
	if input.Version == "" {
		input.Version = "latest"
	}
	enabled, desired := true, true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.Desired != nil {
		desired = *input.Desired
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO profile_mods(profile_id,mod_id,version,enabled,desired,notes,updated_at) VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(profile_id,mod_id) DO UPDATE SET version=excluded.version,enabled=excluded.enabled,desired=excluded.desired,notes=excluded.notes,updated_at=CURRENT_TIMESTAMP`, profileID, input.ModID, input.Version, boolInt(enabled), boolInt(desired), input.Notes); err != nil {
		http.Error(w, "unknown profile or mod", 404)
		return
	}
	if err = a.queueProfileClients(tx, profileID); err != nil {
		http.Error(w, "sync queue failed", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"profile_id": profileID, "mod_id": input.ModID, "version": input.Version, "enabled": enabled, "desired": desired})
}

func (a *App) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id, err := parseProfileID(r.PathValue("profileID"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	clients, err := tx.Query(`SELECT id FROM clients WHERE profile_id=?`, id)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	ids := []string{}
	for clients.Next() {
		var cid string
		if clients.Scan(&cid) != nil {
			clients.Close()
			http.Error(w, "database error", 500)
			return
		}
		ids = append(ids, cid)
	}
	clients.Close()
	if _, err = tx.Exec(`UPDATE clients SET profile_id=NULL WHERE profile_id=?`, id); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	for _, cid := range ids {
		if err = a.queueClientReconciliation(tx, cid); err != nil {
			http.Error(w, "sync queue failed", 500)
			return
		}
	}
	res, err := tx.Exec(`DELETE FROM profiles WHERE id=?`, id)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "profile not found", 404)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "deleted": true})
}

func (a *App) assignClientProfile(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	var input struct {
		ProfileID *int64 `json:"profile_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input) != nil {
		http.Error(w, "invalid request body", 400)
		return
	}
	if input.ProfileID != nil && *input.ProfileID < 1 {
		http.Error(w, "invalid profile_id", 400)
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	var exists int
	if tx.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, clientID).Scan(&exists) != nil || exists == 0 {
		http.Error(w, "unknown client", 404)
		return
	}
	if input.ProfileID != nil {
		if tx.QueryRow(`SELECT count(*) FROM profiles WHERE id=?`, *input.ProfileID).Scan(&exists) != nil || exists == 0 {
			http.Error(w, "unknown profile", 404)
			return
		}
	}
	if _, err = tx.Exec(`UPDATE clients SET profile_id=? WHERE id=?`, input.ProfileID, clientID); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if err = a.queueClientReconciliation(tx, clientID); err != nil {
		http.Error(w, "sync queue failed", 500)
		return
	}
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"client_id": clientID, "profile_id": input.ProfileID})
}
func (a *App) registerClient(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      string `json:"name"`
		LocalPath string `json:"local_path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || strings.TrimSpace(input.Name) == "" {
		http.Error(w, "name is required", 400)
		return
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		http.Error(w, "could not create client id", 500)
		return
	}
	id := hex.EncodeToString(raw[:])
	if _, err := a.db.Exec(`INSERT INTO clients(id,name,local_path,last_seen) VALUES(?,?,?,CURRENT_TIMESTAMP)`, id, strings.TrimSpace(input.Name), input.LocalPath); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": input.Name})
}

func (a *App) acknowledgeSync(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	var exists int
	if a.db.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, clientID).Scan(&exists) != nil || exists == 0 {
		http.Error(w, "unknown client", 404)
		return
	}
	result, err := a.db.Exec(`UPDATE sync_jobs SET status='complete',updated_at=CURRENT_TIMESTAMP WHERE client_id=? AND status='pending'`, clientID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	count, _ := result.RowsAffected()
	_, _ = a.db.Exec(`UPDATE clients SET last_seen=CURRENT_TIMESTAMP WHERE id=?`, clientID)
	writeJSON(w, 200, map[string]any{"client_id": clientID, "acknowledged": count})
}

func (a *App) listAnnotations(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT a.mod_id,m.name,m.source,a.kind,a.value FROM annotations a JOIN mods m ON m.id=a.mod_id WHERE a.value<>'' ORDER BY m.name,a.kind`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var modID int64
		var name, source, kind, value string
		if rows.Scan(&modID, &name, &source, &kind, &value) != nil {
			continue
		}
		out = append(out, map[string]any{"mod_id": modID, "name": name, "source": source, "kind": kind, "value": value})
	}
	writeJSON(w, 200, out)
}

func (a *App) setAnnotation(w http.ResponseWriter, r *http.Request) {
	modID, err := strconv.ParseInt(r.PathValue("modID"), 10, 64)
	if err != nil || modID < 1 {
		http.Error(w, "invalid mod id", 400)
		return
	}
	kind := r.PathValue("kind")
	if kind != "blacklist" && kind != "flag" && kind != "note" {
		http.Error(w, "invalid annotation kind", 400)
		return
	}
	var input struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil {
		http.Error(w, "invalid request body", 400)
		return
	}
	var exists int
	if a.db.QueryRow(`SELECT count(*) FROM mods WHERE id=?`, modID).Scan(&exists) != nil || exists == 0 {
		http.Error(w, "unknown mod", 404)
		return
	}
	value := strings.TrimSpace(input.Value)
	if _, err = a.db.Exec(`INSERT INTO annotations(mod_id,kind,value) VALUES(?,?,?) ON CONFLICT(mod_id,kind) DO UPDATE SET value=excluded.value`, modID, kind, value); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"mod_id": modID, "kind": kind, "value": value})
}

func writeDesiredRows(w http.ResponseWriter, rows *sql.Rows) {
	out := []map[string]any{}
	for rows.Next() {
		var modID int64
		var name, source, version string
		var client sql.NullString
		var enabled, desired int
		var notes string
		var downloadURL, sha256, fileName sql.NullString
		if rows.Scan(&modID, &name, &source, &client, &version, &enabled, &desired, &notes, &downloadURL, &sha256, &fileName) != nil {
			continue
		}
		var clientID any
		if client.Valid {
			clientID = client.String
		}
		out = append(out, map[string]any{"mod_id": modID, "name": name, "source": source, "client_id": clientID, "version": version, "enabled": enabled == 1, "desired": desired == 1, "notes": notes, "download_url": downloadURL.String, "sha256": sha256.String, "file_name": fileName.String})
	}
	writeJSON(w, 200, out)
}
func desiredAction(desired, enabled bool) string {
	if !desired {
		return "uninstall"
	}
	if enabled {
		return "install"
	}
	return "disable"
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func dirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return "."
	}
	return p[:i]
}
