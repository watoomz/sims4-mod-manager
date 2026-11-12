package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *App) catalogDetails(w http.ResponseWriter, r *http.Request) {
	modID, err := strconv.ParseInt(r.PathValue("modID"), 10, 64)
	if err != nil || modID < 1 {
		http.Error(w, "invalid mod id", 400)
		return
	}
	var name, source, pageURL, description, downloadURL string
	var detailsChecked bool
	if err := a.db.QueryRow(`SELECT name,source,url,description,source_download_url,source_details_checked FROM mods WHERE id=?`, modID).Scan(&name, &source, &pageURL, &description, &downloadURL, &detailsChecked); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
		} else {
			http.Error(w, "database error", 500)
		}
		return
	}
	needsFetch := description == "" || description == name || ((source == "customcontent.net" || source == "WickedCC") && !detailsChecked)
	if needsFetch {
		for _, adapter := range a.scrapers {
			html, ok := adapter.(*HTMLAdapter)
			if !ok || html.Source() != source {
				continue
			}
			desc, direct, err := html.Details(r.Context(), pageURL)
			if err != nil {
				http.Error(w, "source details unavailable", 502)
				return
			}
			if desc != "" {
				description = desc
			}
			if direct != "" {
				downloadURL = direct
			}
			if _, err := a.db.Exec(`UPDATE mods SET description=CASE WHEN ?<>'' THEN ? ELSE description END,source_download_url=CASE WHEN ?<>'' THEN ? ELSE source_download_url END,source_details_checked=1 WHERE id=?`, desc, desc, direct, direct, modID); err != nil {
				http.Error(w, "catalog write failed", 500)
				return
			}
			break
		}
	}
	writeJSON(w, 200, map[string]any{"id": modID, "name": name, "description": description, "source": source, "source_download_url": downloadURL})
}

func (a *App) downloadSourceArchive(w http.ResponseWriter, r *http.Request) {
	modID, err := strconv.ParseInt(r.PathValue("modID"), 10, 64)
	if err != nil || modID < 1 {
		http.Error(w, "invalid mod id", 400)
		return
	}
	var source, sourceURL, pageURL, name string
	if err := a.db.QueryRow(`SELECT source,source_download_url,url,name FROM mods WHERE id=?`, modID).Scan(&source, &sourceURL, &pageURL, &name); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
		} else {
			http.Error(w, "database error", 500)
		}
		return
	}
	if source == "LoversLab" {
		http.Error(w, "LoversLab downloads require a member session; import a file you can access", http.StatusForbidden)
		return
	}
	isWickedCC := source == "WickedCC"
	if sourceURL == "" || (source != "customcontent.net" && !isWickedCC && !isCustomRepository(r.Context(), a.db, source)) {
		http.Error(w, "no public direct archive is available for this listing", http.StatusConflict)
		return
	}
	target, err := url.Parse(sourceURL)
	wickedTargetAllowed := isWickedCC && target != nil && target.Scheme == "https" && target.User == nil && target.Hostname() == "wicked.cc" && strings.Contains(target.Path, "/download/")
	archiveTargetAllowed := !isWickedCC && target != nil && isSupportedArchiveExtension(target.Path) && validCustomFeedURL(sourceURL) == nil
	if err != nil || target == nil || target.Scheme != "https" || target.User != nil || (!wickedTargetAllowed && !archiveTargetAllowed) {
		http.Error(w, "source archive URL is not allowed", 400)
		return
	}
	client := a.sourceDownloadHTTP
	if client == nil {
		client = safeFeedHTTPClient()
	}
	clientCopy := *client
	clientCopy.Timeout = 10 * time.Minute
	previousRedirect := client.CheckRedirect
	clientCopy.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || request.URL.Scheme != "https" {
			return fmt.Errorf("source download redirect is not allowed")
		}
		if isWickedCC {
			host := strings.ToLower(request.URL.Hostname())
			if host != "wicked.cc" && host != "www.wicked.cc" && host != "files.wicked.cc" {
				return fmt.Errorf("WickedCC download redirected to an unexpected host")
			}
			if err := validCustomFeedURL(request.URL.String()); err != nil {
				return err
			}
		} else if err := validCustomFeedURL(request.URL.String()); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "invalid archive URL", 400)
		return
	}
	request.Header.Set("User-Agent", "Sims4ModManager/0.1 (+personal mod catalog)")
	if isWickedCC {
		page, pageErr := url.Parse(pageURL)
		if pageErr != nil || page.Scheme != "https" || (page.Hostname() != "wicked.cc" && page.Hostname() != "www.wicked.cc") {
			http.Error(w, "WickedCC item page URL is invalid", http.StatusBadRequest)
			return
		}
		request.Header.Set("Referer", pageURL)
	}
	response, err := clientCopy.Do(request)
	if err != nil {
		http.Error(w, "source archive download failed", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("source archive returned HTTP %d", response.StatusCode), 502)
		return
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		http.Error(w, "source link returned a web page, not an archive", 415)
		return
	}
	filePath := target.Path
	if response.Request != nil && response.Request.URL != nil {
		filePath = response.Request.URL.Path
	}
	fileName := filepath.Base(filePath)
	if disposition, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Disposition")); parseErr == nil && disposition != "" {
		if _, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
			fileName = filepath.Base(strings.ReplaceAll(params["filename"], "\\", "/"))
		}
	}
	if fileName == "." || fileName == "/" || !isSupportedArchiveExtension(fileName) {
		http.Error(w, "source did not provide a supported archive filename", 415)
		return
	}
	if err := a.storeSourceArchive(r.Context(), modID, fileName, response.Body); err != nil {
		http.Error(w, "archive storage failed", 500)
		return
	}
	var downloadURL string
	_ = a.db.QueryRow(`SELECT download_url FROM mod_versions WHERE mod_id=? ORDER BY created_at DESC LIMIT 1`, modID).Scan(&downloadURL)
	writeJSON(w, 201, map[string]any{"mod_id": modID, "name": name, "file_name": fileName, "download_url": downloadURL})
}

func isCustomRepository(ctx context.Context, db *sql.DB, source string) bool {
	var count int
	return db.QueryRowContext(ctx, `SELECT count(*) FROM repos WHERE name=? AND enabled=1 AND type='rss'`, source).Scan(&count) == nil && count > 0
}

func (a *App) storeSourceArchive(ctx context.Context, modID int64, fileName string, body io.Reader) error {
	if err := os.MkdirAll(a.archiveDir, 0750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(a.archiveDir, "source-download-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(body, (512<<20)+1))
	if err != nil {
		temp.Close()
		return err
	}
	if size > 512<<20 {
		temp.Close()
		return fmt.Errorf("source archive exceeds 512 MiB")
	}
	if err := temp.Close(); err != nil {
		return err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	folder := filepath.Join(a.archiveDir, hash[:2])
	if err := os.MkdirAll(folder, 0750); err != nil {
		return err
	}
	dest := filepath.Join(folder, hash+strings.ToLower(filepath.Ext(fileName)))
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		if err := os.Rename(tempName, dest); err != nil {
			return err
		}
	}
	version := "source-" + hash[:12]
	_, err = a.db.ExecContext(ctx, `INSERT INTO mod_versions(mod_id,version,download_url,sha256,file_name,published_at,metadata,tracking_source) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(mod_id,version) DO UPDATE SET download_url=excluded.download_url,sha256=excluded.sha256,file_name=excluded.file_name,metadata=excluded.metadata,tracking_source=excluded.tracking_source,created_at=CURRENT_TIMESTAMP`, modID, version, "/api/archives/"+hash, hash, fileName, time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(`{"size":%d,"fetched_via":"public_source_link","tracking_scope":"local"}`, size), "source_download")
	return err
}
