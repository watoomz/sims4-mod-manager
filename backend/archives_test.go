package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestArchiveUploadDownloadAndDesiredMetadata(t *testing.T) {
	app := testApp(t)
	app.archiveDir = t.TempDir()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("version", "1.2"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "Example.package")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("sample package data")
	if _, err = part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/mods/1/archives", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.SetPathValue("modID", "1")
	w := httptest.NewRecorder()
	app.uploadArchive(w, r)
	if w.Code != 201 {
		t.Fatalf("upload returned %d: %s", w.Code, w.Body.String())
	}
	var uploaded struct {
		SHA string `json:"sha256"`
		URL string `json:"download_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if len(uploaded.SHA) != 64 || uploaded.URL != "/api/archives/"+uploaded.SHA {
		t.Fatalf("unexpected upload response: %#v", uploaded)
	}
	if _, err := os.Stat(app.archiveDir + "/" + uploaded.SHA[:2] + "/" + uploaded.SHA + ".package"); err != nil {
		t.Fatal(err)
	}
	install := httptest.NewRequest("POST", "/api/desired", bytes.NewBufferString(`{"mod_id":1,"version":"1.2"}`))
	installRecorder := httptest.NewRecorder()
	app.setDesired(installRecorder, install)
	if installRecorder.Code != 200 {
		t.Fatalf("desired state returned %d: %s", installRecorder.Code, installRecorder.Body.String())
	}
	desired := httptest.NewRecorder()
	app.getDesired(desired, httptest.NewRequest("GET", "/api/desired", nil))
	var rows []map[string]any
	if err := json.Unmarshal(desired.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["download_url"] != uploaded.URL || rows[0]["file_name"] != "Example.package" {
		t.Fatalf("desired state missing archive metadata: %#v", rows)
	}
	download := httptest.NewRequest("GET", "/api/archives/"+uploaded.SHA, nil)
	download.SetPathValue("hash", uploaded.SHA)
	downloadRecorder := httptest.NewRecorder()
	app.downloadArchive(downloadRecorder, download)
	if downloadRecorder.Code != 200 || !bytes.Equal(downloadRecorder.Body.Bytes(), content) {
		t.Fatalf("download returned %d: %q", downloadRecorder.Code, downloadRecorder.Body.Bytes())
	}
}

func TestArchiveRejectsUnsupportedFile(t *testing.T) {
	app := testApp(t)
	app.archiveDir = t.TempDir()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "program.exe")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("no"))
	_ = form.Close()
	r := httptest.NewRequest("POST", "/api/mods/1/archives", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.SetPathValue("modID", "1")
	w := httptest.NewRecorder()
	app.uploadArchive(w, r)
	if w.Code != 415 {
		t.Fatalf("expected unsupported file rejection, got %d", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) == "" {
		t.Fatal("expected error message")
	}
}

func TestCatalogReportsArchiveReadiness(t *testing.T) {
	app := testApp(t)
	mods := []CatalogMod{{Name: "Ready Check", Source: "sample", URL: "https://example.test/ready"}}
	if err := app.persistCatalog(mods); err != nil {
		t.Fatal(err)
	}
	if mods[0].HasArchive {
		t.Fatal("new catalog listing unexpectedly has an archive")
	}
	if _, err := app.db.Exec(`INSERT INTO mod_versions(mod_id,version,download_url) VALUES(?, '1.0', '/api/archives/test')`, mods[0].CatalogID); err != nil {
		t.Fatal(err)
	}
	updated := []CatalogMod{{Name: "Ready Check", Source: "sample", URL: "https://example.test/ready"}}
	if err := app.persistCatalog(updated); err != nil {
		t.Fatal(err)
	}
	if !updated[0].HasArchive {
		t.Fatal("catalog listing with stored version is not marked installable")
	}
}

func TestLibraryIncludesArchivedAndObservedModsWithAnnotations(t *testing.T) {
	app := testApp(t)
	mods := []CatalogMod{
		{Name: "Archived", Creator: "Maker", Description: "Archive description", Source: "sample", URL: "https://example.test/archived", ThumbnailURL: "https://example.test/archived.jpg"},
		{Name: "Observed", Source: "sample", URL: "https://example.test/observed"},
	}
	if err := app.persistCatalog(mods); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO mod_versions(mod_id,version) VALUES(?, '1.0')`, mods[0].CatalogID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('pc','PC')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO client_inventory(client_id,path,file_name,mod_id) VALUES('pc','Mods/observed.package','observed.package',?)`, mods[1].CatalogID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO annotations(mod_id,kind,value) VALUES(?,'note','Keep this'),(?,'flag','Check update')`, mods[0].CatalogID, mods[0].CatalogID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.library(w, httptest.NewRequest("GET", "/api/library", nil))
	if w.Code != 200 {
		t.Fatalf("library returned %d: %s", w.Code, w.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected archive and inventory entries, got %#v", items)
	}
	if items[0]["name"] != "Archived" || items[0]["thumbnail_url"] != "https://example.test/archived.jpg" || items[0]["note"] != "Keep this" || items[0]["flag"] != "Check update" || items[0]["has_archive"] != true {
		t.Fatalf("archived library metadata missing: %#v", items[0])
	}
	if items[1]["name"] != "Observed" || items[1]["installed"] != true || items[1]["has_archive"] != false {
		t.Fatalf("inventory library metadata missing: %#v", items[1])
	}
}

func TestLibraryLabelsCurseForgeAPITrackingAsLocal(t *testing.T) {
	app := testApp(t)
	mod := CatalogMod{Name: "Tracked CurseForge", Source: "CurseForge", URL: "https://www.curseforge.com/sims4/mods/tracked"}
	mods := []CatalogMod{mod}
	if err := app.persistCatalog(mods); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO mod_versions(mod_id,version,tracking_source) VALUES(?,'file-42','curseforge_api_key')`, mods[0].CatalogID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.library(w, httptest.NewRequest("GET", "/api/library", nil))
	var items []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["curseforge_api_tracked_locally"] != true {
		t.Fatalf("CurseForge tracking was not exposed in local library: %#v", items)
	}
}

func TestCatalogPersistsThumbnailURL(t *testing.T) {
	app := testApp(t)
	mod := CatalogMod{Name: "Image Mod", Source: "sample", URL: "https://example.test/image-mod", ThumbnailURL: "https://example.test/image.jpg"}
	if err := app.persistCatalog([]CatalogMod{mod}); err != nil {
		t.Fatal(err)
	}
	var thumbnail string
	if err := app.db.QueryRow(`SELECT thumbnail_url FROM mods WHERE source=? AND url=?`, mod.Source, mod.URL).Scan(&thumbnail); err != nil {
		t.Fatal(err)
	}
	if thumbnail != mod.ThumbnailURL {
		t.Fatalf("thumbnail=%q", thumbnail)
	}
}
