package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCatalogDetailsFetchesAndPersistsRealSourceDescription(t *testing.T) {
	app := testApp(t)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `<html><head><meta name="description" content="A real custom content description."></head><body><a href="/files/sample.zip">Download</a></body></html>`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	adapter := newHTMLAdapter("customcontent.net", "https://thesims4.customcontent.net/", client)
	adapter.minInterval = 0
	app.scrapers = []SourceAdapter{adapter}
	mods := []CatalogMod{{Name: "Detail Sample", Source: "customcontent.net", URL: "https://thesims4.customcontent.net/detail-sample/"}}
	if err := app.persistCatalog(mods); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/mods/details", nil)
	request.SetPathValue("modID", strconv.FormatInt(mods[0].CatalogID, 10))
	response := httptest.NewRecorder()
	app.catalogDetails(response, request)
	if response.Code != 200 {
		t.Fatalf("details returned %d: %s", response.Code, response.Body.String())
	}
	var description, download string
	if err := app.db.QueryRow(`SELECT description,source_download_url FROM mods WHERE id=?`, mods[0].CatalogID).Scan(&description, &download); err != nil {
		t.Fatal(err)
	}
	if description != "A real custom content description." || download != "https://thesims4.customcontent.net/files/sample.zip" {
		t.Fatalf("description=%q download=%q", description, download)
	}
}

func TestPublicSourceDownloadStoresArchiveAndTracksLocally(t *testing.T) {
	app := testApp(t)
	app.archiveDir = t.TempDir()
	if _, err := app.db.Exec(`INSERT INTO repos(name,type,url) VALUES('Studio Feed','rss','https://feed.example.org/rss')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source,url,source_download_url) VALUES(2,'source-item','Direct Mod','Studio Feed','https://mods.example.org/direct','https://cdn.example.org/direct.zip')`); err != nil {
		t.Fatal(err)
	}
	content := []byte("PK\x03\x04sample archive")
	app.sourceDownloadHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/zip"}, "Content-Disposition": []string{`attachment; filename="direct.zip"`}}, Body: io.NopCloser(strings.NewReader(string(content))), Request: r}, nil
	})}
	request := httptest.NewRequest("POST", "/api/mods/2/download", nil)
	request.SetPathValue("modID", "2")
	response := httptest.NewRecorder()
	app.downloadSourceArchive(response, request)
	if response.Code != 201 {
		t.Fatalf("download returned %d: %s", response.Code, response.Body.String())
	}
	var hash, tracking string
	if err := app.db.QueryRow(`SELECT sha256,tracking_source FROM mod_versions WHERE mod_id=2`).Scan(&hash, &tracking); err != nil {
		t.Fatal(err)
	}
	if tracking != "source_download" || len(hash) != 64 {
		t.Fatalf("tracking=%q hash=%q", tracking, hash)
	}
	stored, err := os.ReadFile(filepath.Join(app.archiveDir, hash[:2], hash+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(content) {
		t.Fatalf("stored bytes mismatch: %q", stored)
	}
}

func TestWickedCCPublicDownloadFollowsAllowedCDNWithSourceReferer(t *testing.T) {
	app := testApp(t)
	app.archiveDir = t.TempDir()
	page := "https://wicked.cc/clothing/grayvixen/storm-lingerie-set"
	target := page + "/download/Xq7KRU3IU8"
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source,url,source_download_url) VALUES(2,'wickedcc-item','Storm Lingerie Set','WickedCC',?,?)`, page, target); err != nil {
		t.Fatal(err)
	}
	content := []byte("DBPF package bytes")
	app.sourceDownloadHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/download/") {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://files.wicked.cc/file/wickedcc/Storm.package"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		if r.URL.Hostname() != "files.wicked.cc" || r.Referer() != page {
			t.Errorf("unexpected CDN request URL=%s Referer=%q", r.URL, r.Referer())
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader(string(content))), Request: r}, nil
	})}
	request := httptest.NewRequest("POST", "/api/mods/2/download", nil)
	request.SetPathValue("modID", "2")
	response := httptest.NewRecorder()
	app.downloadSourceArchive(response, request)
	if response.Code != 201 {
		t.Fatalf("WickedCC download returned %d: %s", response.Code, response.Body.String())
	}
	var hash string
	if err := app.db.QueryRow(`SELECT sha256 FROM mod_versions WHERE mod_id=2`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(app.archiveDir, hash[:2], hash+".package"))
	if err != nil || string(stored) != string(content) {
		t.Fatalf("stored WickedCC package=%q err=%v", stored, err)
	}
}
