package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCurseForgeFileIntoManagedArchive(t *testing.T) {
	payload := []byte("mock curseforge archive bytes")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" && strings.HasPrefix(r.URL.Path, "/v1/") {
			t.Errorf("missing API key for %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/mods/42/files/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"fileName": "Example.zip", "displayName": "Version 1.2", "isAvailable": true}})
		case "/v1/mods/42/files/7/download-url":
			_ = json.NewEncoder(w).Encode(map[string]string{"data": serverURL(r, "/download")})
		case "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source,url) VALUES(2,'42','Curse Mod','CurseForge','https://www.curseforge.com/sims4/mods/42')`); err != nil {
		t.Fatal(err)
	}
	app.api = NewCurseForgeAdapter("test-key", server.Client())
	app.api.baseURL = server.URL
	app.archiveDir = t.TempDir()
	request := httptest.NewRequest(http.MethodPost, "/api/curseforge/mods/42/files/7/import", nil)
	request.SetPathValue("modID", "42")
	request.SetPathValue("fileID", "7")
	recorder := httptest.NewRecorder()
	app.importCurseForgeFile(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		ModID    int64  `json:"mod_id"`
		Version  string `json:"version"`
		SHA256   string `json:"sha256"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ModID != 2 || result.Version != "Version 1.2 (file 7)" || result.FileName != "Example.zip" || len(result.SHA256) != 64 {
		t.Fatalf("unexpected import result: %+v", result)
	}
	if data, err := os.ReadFile(filepath.Join(app.archiveDir, result.SHA256[:2], result.SHA256+".zip")); err != nil || string(data) != string(payload) {
		t.Fatalf("archive not saved: data=%q err=%v", data, err)
	}
	var versionCount int
	if err := app.db.QueryRow(`SELECT count(*) FROM mod_versions WHERE mod_id=2 AND version=?`, result.Version).Scan(&versionCount); err != nil || versionCount != 1 {
		t.Fatalf("version not linked: count=%d err=%v", versionCount, err)
	}
}

func serverURL(r *http.Request, path string) string { return "https://" + r.Host + path }
