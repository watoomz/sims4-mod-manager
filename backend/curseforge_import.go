package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *App) importCurseForgeFile(w http.ResponseWriter, r *http.Request) {
	cfModID, err := parseCurseForgeID(r, "modID")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	fileID, err := parseCurseForgeID(r, "fileID")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var modID int64
	if err := a.db.QueryRow(`SELECT id FROM mods WHERE source='CurseForge' AND external_id=?`, strconv.FormatInt(cfModID, 10)).Scan(&modID); err != nil {
		http.Error(w, "search for this CurseForge mod before importing its file", 404)
		return
	}
	fileData, err := a.api.ModFile(r.Context(), cfModID, fileID)
	if err != nil {
		http.Error(w, "CurseForge file lookup failed: "+err.Error(), 502)
		return
	}
	var response struct {
		Data struct {
			FileName    string `json:"fileName"`
			DisplayName string `json:"displayName"`
			DownloadURL string `json:"downloadUrl"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fileData, &response); err != nil || !response.Data.IsAvailable {
		http.Error(w, "CurseForge file is unavailable", 404)
		return
	}
	name := filepath.Base(strings.ReplaceAll(response.Data.FileName, "\\", "/"))
	if name == "." || name == "" {
		http.Error(w, "CurseForge file has no valid filename", 502)
		return
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".zip" && ext != ".package" && ext != ".ts4script" {
		http.Error(w, "CurseForge file is not a supported Sims 4 archive", 415)
		return
	}
	downloadURL, err := a.api.DownloadURL(r.Context(), cfModID, fileID)
	if err != nil {
		http.Error(w, "CurseForge download lookup failed: "+err.Error(), 502)
		return
	}
	if err := a.storeCurseForgeArchive(r.Context(), modID, fileID, name, response.Data.DisplayName, downloadURL, w); err != nil {
		// The helper writes an HTTP error for a clean response path.
		return
	}
}

func (a *App) storeCurseForgeArchive(ctx context.Context, modID, fileID int64, fileName, displayName, downloadURL string, w http.ResponseWriter) error {
	target, err := url.Parse(downloadURL)
	if err != nil || !validCurseForgeDownloadURL(target, a.api.baseURL) {
		http.Error(w, "CurseForge returned an unexpected download host", 502)
		return fmt.Errorf("unexpected download host")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "invalid CurseForge download URL", 502)
		return err
	}
	client := &http.Client{Timeout: 10 * time.Minute, Transport: a.api.client.Transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validCurseForgeDownloadURL(req.URL, a.api.baseURL) {
			return fmt.Errorf("CurseForge download redirected to an unexpected host")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "CurseForge download failed", 502)
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("CurseForge download returned HTTP %d", response.StatusCode), 502)
		return fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	if err := os.MkdirAll(a.archiveDir, 0750); err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return err
	}
	temp, err := os.CreateTemp(a.archiveDir, "curseforge-*.tmp")
	if err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, digest), io.LimitReader(response.Body, (512<<20)+1))
	if err != nil {
		_ = temp.Close()
		http.Error(w, "CurseForge download failed", 502)
		return err
	}
	if size > 512<<20 {
		_ = temp.Close()
		http.Error(w, "CurseForge archive exceeds 512 MiB", 413)
		return fmt.Errorf("download too large")
	}
	if err := temp.Close(); err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	folder := filepath.Join(a.archiveDir, hash[:2])
	if err := os.MkdirAll(folder, 0750); err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return err
	}
	path := filepath.Join(folder, hash+strings.ToLower(filepath.Ext(fileName)))
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.Rename(tempName, path); err != nil {
			http.Error(w, "archive storage unavailable", 500)
			return err
		}
	}
	version := fmt.Sprintf("curseforge-%d", fileID)
	if displayName != "" {
		version = fmt.Sprintf("%s (file %d)", strings.TrimSpace(displayName), fileID)
	}
	_, err = a.db.Exec(`INSERT INTO mod_versions(mod_id,version,download_url,sha256,file_name,published_at,metadata,tracking_source) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(mod_id,version) DO UPDATE SET download_url=excluded.download_url,sha256=excluded.sha256,file_name=excluded.file_name,published_at=excluded.published_at,metadata=excluded.metadata,tracking_source=excluded.tracking_source,created_at=CURRENT_TIMESTAMP`, modID, version, "/api/archives/"+hash, hash, fileName, time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(`{"curseforge_file_id":%d,"size":%d,"fetched_via":"configured_curseforge_api_key","tracking_scope":"local"}`, fileID, size), "curseforge_api_key")
	if err != nil {
		http.Error(w, "catalog write failed", 500)
		return err
	}
	writeJSON(w, 201, map[string]any{"mod_id": modID, "file_id": fileID, "version": version, "file_name": fileName, "size": size, "sha256": hash, "download_url": "/api/archives/" + hash})
	return nil
}

func allowedCurseForgeHost(host, apiBaseURL string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	parsed, err := url.Parse(apiBaseURL)
	if err == nil && strings.EqualFold(host, parsed.Hostname()) {
		return true
	}
	return strings.HasSuffix(host, ".forgecdn.net") || host == "forgecdn.net" || strings.HasSuffix(host, ".curseforge.com") || host == "curseforge.com"
}

func validCurseForgeDownloadURL(target *url.URL, apiBaseURL string) bool {
	if target == nil || target.Scheme != "https" || target.User != nil || !allowedCurseForgeHost(target.Hostname(), apiBaseURL) {
		return false
	}
	if target.Port() == "" || target.Port() == "443" {
		return true
	}
	base, err := url.Parse(apiBaseURL)
	return err == nil && strings.EqualFold(target.Hostname(), base.Hostname()) && target.Port() == base.Port()
}
