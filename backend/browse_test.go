package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type browseAdapter struct{ mods []CatalogMod }

func (a browseAdapter) Source() string { return "sample" }
func (a browseAdapter) Search(_ context.Context, _ string, limit int) ([]CatalogMod, error) {
	if limit < len(a.mods) {
		return append([]CatalogMod(nil), a.mods[:limit]...), nil
	}
	return append([]CatalogMod(nil), a.mods...), nil
}

func TestBrowseCurseForgeUsesUpstreamOffset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("gameId") != "432" || r.URL.Query().Get("index") != "7" || r.URL.Query().Get("pageSize") != "2" {
			t.Errorf("unexpected browse query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":42,"name":"Curse Page","links":{"websiteUrl":"https://example.test/42"}}]}`))
	}))
	defer server.Close()
	app := testApp(t)
	app.api = NewCurseForgeAdapter("test-key", server.Client())
	app.api.baseURL = server.URL
	w := httptest.NewRecorder()
	app.browse(w, httptest.NewRequest("GET", "/api/browse?source=CurseForge&offset=7&limit=2", nil))
	if w.Code != 200 {
		t.Fatalf("browse returned %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		Items  []CatalogMod `json:"items"`
		Offset int          `json:"offset"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Offset != 7 || len(page.Items) != 1 || page.Items[0].Name != "Curse Page" || page.Items[0].CatalogID == 0 {
		t.Fatalf("unexpected CurseForge page: %#v", page)
	}
}

func TestBrowsePaginatesSourceCatalogue(t *testing.T) {
	app := testApp(t)
	mods := []CatalogMod{{Name: "One", Source: "sample", URL: "https://example.test/1"}, {Name: "Two", Source: "sample", URL: "https://example.test/2"}, {Name: "Three", Source: "sample", URL: "https://example.test/3"}}
	app.scrapers = []SourceAdapter{browseAdapter{mods}}
	req := httptest.NewRequest("GET", "/api/browse?source=sample&offset=1&limit=1", nil)
	w := httptest.NewRecorder()
	app.browse(w, req)
	if w.Code != 200 {
		t.Fatalf("browse returned %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Items   []CatalogMod `json:"items"`
		Total   int          `json:"total"`
		HasMore bool         `json:"has_more"`
		Offset  int          `json:"offset"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].Name != "Two" || result.Total != 3 || !result.HasMore || result.Offset != 1 || result.Items[0].CatalogID == 0 {
		t.Fatalf("unexpected browse page: %#v", result)
	}
}

func TestBrowseRequiresKnownSource(t *testing.T) {
	app := testApp(t)
	w := httptest.NewRecorder()
	app.browse(w, httptest.NewRequest("GET", "/api/browse?source=missing", nil))
	if w.Code != 404 {
		t.Fatalf("unknown source returned %d", w.Code)
	}
}

func TestWickedWhimsVersionChangeIsReportedAsUpdate(t *testing.T) {
	app := testApp(t)
	mods := []CatalogMod{{Name: "WickedWhims", Source: "WickedWhims", URL: "https://turbodriver.itch.io/wickedwhims", Version: "v185k"}}
	if err := app.persistCatalog(mods); err != nil {
		t.Fatal(err)
	}
	mods[0].Version = "v186a"
	if err := app.persistCatalog(mods); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.wickedWhimsStatus(w, httptest.NewRequest("GET", "/api/wickedwhims/status", nil))
	var status struct {
		Current  string `json:"current_version"`
		Previous string `json:"previous_version"`
		Update   bool   `json:"update_available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Current != "v186a" || status.Previous != "v185k" || !status.Update {
		t.Fatalf("version update not reported: %#v", status)
	}
}

func TestLoversLabCurrentCatalogueHidesOldSitewideRows(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source,url) VALUES(2,'skyrim','SexLab SE Framework','LoversLab','https://www.loverslab.com/files/file/20058-sexlab-se/')`); err != nil {
		t.Fatal(err)
	}
	current := []CatalogMod{{Name: "Sims 4 listing", Source: "LoversLab", URL: "https://www.loverslab.com/files/file/3169-wickedwhims/"}}
	if err := app.persistCatalog(current); err != nil {
		t.Fatal(err)
	}
	if err := app.markSourceCatalogCurrent("LoversLab", current); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := app.db.QueryRow(`SELECT catalog_active FROM mods WHERE id=2`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatal("site-wide Skyrim row remained in the active catalogue")
	}
	w := httptest.NewRecorder()
	app.search(w, httptest.NewRequest("GET", "/api/search?q=SexLab", nil))
	if strings.Contains(w.Body.String(), "SexLab SE Framework") {
		t.Fatalf("stale Skyrim item remained searchable: %s", w.Body.String())
	}
}
