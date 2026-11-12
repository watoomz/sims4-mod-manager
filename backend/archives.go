package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var archiveHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func isSupportedArchiveExtension(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".zip" || ext == ".package" || ext == ".ts4script"
}

func (a *App) uploadArchive(w http.ResponseWriter, r *http.Request) {
	modID, err := strconv.ParseInt(r.PathValue("modID"), 10, 64)
	if err != nil || modID < 1 {
		http.Error(w, "invalid mod id", 400)
		return
	}
	var exists int
	if a.db.QueryRow(`SELECT count(*) FROM mods WHERE id=?`, modID).Scan(&exists) != nil || exists == 0 {
		http.Error(w, "unknown mod", 404)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	if err = r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "invalid or oversized upload", 413)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file is required", 400)
		return
	}
	defer file.Close()
	name := filepath.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".zip" && ext != ".package" && ext != ".ts4script" {
		http.Error(w, "only .zip, .package, and .ts4script files are supported", 415)
		return
	}
	if err = os.MkdirAll(a.archiveDir, 0750); err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return
	}
	temp, err := os.CreateTemp(a.archiveDir, "upload-*.tmp")
	if err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, hash), file)
	if err != nil {
		temp.Close()
		http.Error(w, "upload failed", 500)
		return
	}
	if err = temp.Close(); err != nil {
		http.Error(w, "upload failed", 500)
		return
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	folder := filepath.Join(a.archiveDir, digest[:2])
	if err = os.MkdirAll(folder, 0750); err != nil {
		http.Error(w, "archive storage unavailable", 500)
		return
	}
	destination := filepath.Join(folder, digest+ext)
	if _, err = os.Stat(destination); os.IsNotExist(err) {
		if err = os.Rename(tempName, destination); err != nil {
			http.Error(w, "archive storage unavailable", 500)
			return
		}
	}
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		version = "imported-" + time.Now().UTC().Format("20060102T150405Z")
	}
	downloadURL := `/api/archives/` + digest
	_, err = a.db.Exec(`INSERT INTO mod_versions(mod_id,version,download_url,sha256,file_name,published_at,metadata) VALUES(?,?,?,?,?,?,?) ON CONFLICT(mod_id,version) DO UPDATE SET download_url=excluded.download_url,sha256=excluded.sha256,file_name=excluded.file_name,published_at=excluded.published_at,metadata=excluded.metadata,created_at=CURRENT_TIMESTAMP`, modID, version, downloadURL, digest, name, time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(`{"size":%d}`, size))
	if err != nil {
		http.Error(w, "catalog write failed", 500)
		return
	}
	writeJSON(w, 201, map[string]any{"mod_id": modID, "version": version, "file_name": name, "size": size, "sha256": digest, "download_url": downloadURL})
}

func (a *App) downloadArchive(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("hash")
	if !archiveHashPattern.MatchString(digest) {
		http.NotFound(w, r)
		return
	}
	var fileName string
	if err := a.db.QueryRow(`SELECT file_name FROM mod_versions WHERE sha256=? ORDER BY created_at DESC LIMIT 1`, digest).Scan(&fileName); err != nil {
		http.NotFound(w, r)
		return
	}
	pattern := filepath.Join(a.archiveDir, digest[:2], digest+".*")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(matches[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	safeName := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, fileName)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeName}))
	http.ServeContent(w, r, fileName, stat.ModTime(), f)
}
