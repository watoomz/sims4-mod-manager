package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClientReleaseVersionAndDownload(t *testing.T) {
	dir := t.TempDir()
	body := []byte("test windows executable")
	if err := os.WriteFile(filepath.Join(dir, clientExecutableName), body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIENT_VERSION", "1.2.3")
	app := &App{archiveDir: dir}

	versionResponse := httptest.NewRecorder()
	app.clientVersion(versionResponse, httptest.NewRequest("GET", "/api/client/version", nil))
	if versionResponse.Code != 200 {
		t.Fatalf("version returned %d: %s", versionResponse.Code, versionResponse.Body.String())
	}
	var release struct {
		Version  string `json:"version"`
		Download string `json:"download_url"`
		SHA256   string `json:"sha256"`
	}
	if err := json.Unmarshal(versionResponse.Body.Bytes(), &release); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if release.Version != "1.2.3" || release.Download != "/api/client/download" || release.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("unexpected release metadata: %+v", release)
	}

	downloadResponse := httptest.NewRecorder()
	app.downloadClient(downloadResponse, httptest.NewRequest("GET", release.Download, nil))
	if downloadResponse.Code != 200 || downloadResponse.Body.String() != string(body) {
		t.Fatalf("download returned %d with body %q", downloadResponse.Code, downloadResponse.Body.String())
	}
	if got := downloadResponse.Header().Get("Content-Disposition"); got != `attachment; filename="sims4-mod-manager.exe"` {
		t.Fatalf("unexpected Content-Disposition: %q", got)
	}
}

func TestClientReleaseUnavailableWithoutPublishedBinary(t *testing.T) {
	app := &App{archiveDir: t.TempDir()}
	response := httptest.NewRecorder()
	app.clientVersion(response, httptest.NewRequest("GET", "/api/client/version", nil))
	if response.Code != 503 {
		t.Fatalf("expected 503, got %d", response.Code)
	}
}
