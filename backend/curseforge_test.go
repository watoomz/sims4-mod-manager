package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCurseForgeSearchMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing API key")
		}
		if r.URL.Query().Get("gameId") != "432" {
			t.Errorf("wrong game id")
		}
		if r.URL.Query().Has("classId") {
			t.Errorf("Sims 4 search should not be restricted by a hard-coded class id: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("sortField") != "" || r.URL.Query().Get("sortOrder") != "" {
			t.Errorf("regular search should keep relevance ordering: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":42,"name":"Test Mod","summary":"A test","downloadCount":12,"authors":[{"name":"Creator"}],"links":{"websiteUrl":"https://example.test/mod"},"logo":{"url":"https://example.test/mod.jpg"}}]}`))
	}))
	defer server.Close()
	a := NewCurseForgeAdapter("test-key", server.Client())
	a.baseURL = server.URL
	got, err := a.Search(context.Background(), "test", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Test Mod" || got[0].Creator != "Creator" || got[0].URL != "https://example.test/mod" || got[0].ThumbnailURL != "https://example.test/mod.jpg" {
		t.Fatalf("unexpected result: %#v", got)
	}
}

func TestTrendingFallsBackToCurseForgeCatalogueWhenPopularIsEmpty(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("sortField") != "" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":42,"name":"Fallback Mod","links":{"websiteUrl":"https://example.test/mod"}}]}`))
	}))
	defer server.Close()
	app := testApp(t)
	app.api = NewCurseForgeAdapter("test-key", server.Client())
	app.api.baseURL = server.URL
	w := httptest.NewRecorder()
	app.trending(w, httptest.NewRequest("GET", "/api/trending", nil))
	var result map[string][]CatalogMod
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(result["CurseForge"]) != 1 || result["CurseForge"][0].Name != "Fallback Mod" {
		t.Fatalf("popular fallback failed after %d API calls: %#v", calls, result)
	}
}

func TestCurseForgeTrendingSortsByDownloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sortField") != "6" || r.URL.Query().Get("sortOrder") != "desc" {
			t.Errorf("trending should sort by downloads: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	a := NewCurseForgeAdapter("test-key", server.Client())
	a.baseURL = server.URL
	if _, err := a.Trending(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
}

func TestCurseForgeDetailFilesDownloadAndFingerprints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing API key for %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/mods/42":
			_, _ = w.Write([]byte(`{"data":{"id":42,"name":"Mock Mod"}}`))
		case "/v1/mods/42/files":
			if r.URL.Query().Get("pageSize") != "50" {
				t.Errorf("unexpected files paging: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data":[{"id":7,"fileName":"mock.zip"}]}`))
		case "/v1/mods/42/files/7/download-url":
			_, _ = w.Write([]byte(`{"data":"https://download.example/mock.zip"}`))
		case "/v1/fingerprints":
			var body struct {
				Fingerprints []uint32 `json:"fingerprints"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Fingerprints) != 2 || body.Fingerprints[0] != 11 {
				t.Errorf("unexpected fingerprint request: %+v err=%v", body, err)
			}
			_, _ = w.Write([]byte(`{"data":{"exactMatches":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a := NewCurseForgeAdapter("test-key", server.Client())
	a.baseURL = server.URL
	if _, err := a.ModDetails(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ModFiles(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if got, err := a.DownloadURL(context.Background(), 42, 7); err != nil || got != "https://download.example/mock.zip" {
		t.Fatalf("download URL=%q err=%v", got, err)
	}
	if _, err := a.FingerprintMatches(context.Background(), []uint32{11, 22}); err != nil {
		t.Fatal(err)
	}
}

func TestCurseForgeFingerprintRequiresConfiguredAdapter(t *testing.T) {
	a := NewCurseForgeAdapter("", nil)
	if _, err := a.FingerprintMatches(context.Background(), []uint32{1}); err == nil {
		t.Fatal("expected missing-key error")
	}
	if _, err := a.DownloadURL(context.Background(), 1, 1); err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestCurseForgeSearchCatalogUsesRemoteModID(t *testing.T) {
	app := testApp(t)
	if err := app.persistCatalog([]CatalogMod{{ID: 42, Name: "Curse Mod", Source: "CurseForge", URL: "https://www.curseforge.com/sims4/mods/42"}}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := app.db.QueryRow(`SELECT id FROM mods WHERE source='CurseForge' AND external_id='42'`).Scan(&id); err != nil || id < 1 {
		t.Fatalf("CurseForge remote ID not persisted: id=%d err=%v", id, err)
	}
}
func TestCurseForgeUnconfigured(t *testing.T) {
	a := NewCurseForgeAdapter("", nil)
	if a.Configured() {
		t.Fatal("empty key reported configured")
	}
	if _, err := a.Search(context.Background(), "x", 0, 20); err == nil {
		t.Fatal("expected missing-key error")
	}
}
