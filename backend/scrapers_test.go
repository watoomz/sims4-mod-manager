package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

func TestHTMLSourceExtraction(t *testing.T) {
	tests := []struct {
		source, html string
		count        int
		name         string
	}{
		{"customcontent.net", `<div id="slp"><a class="card" href="https://thesims4.customcontent.net/sample-mod/"><img src="/images/sample.jpg"><div class="ct2"><h4>Sample Mod</h4></div><div class="ct3"><p>Creator</p></div></a></div>`, 1, "Sample Mod"},
		{"LoversLab", `<ul><li class="ipsCarousel_item"><a class="ipsThumb ipsThumb_bg" style='background-image: url("https://example.test/sample.jpg")'></a><h2 class="ipsDataItem_title"><a href="/files/file/32129-sims-4-sample/">Sims 4 Sample</a></h2></li></ul>`, 1, "Sims 4 Sample"},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			a := newHTMLAdapter(tt.source, "https://example.test/", http.DefaultClient)
			doc, err := goquery.NewDocumentFromReader(strings.NewReader(tt.html))
			if err != nil {
				t.Fatal(err)
			}
			got := a.extract(doc, "https://example.test/", "")
			if len(got) != tt.count || got[0].Name != tt.name {
				t.Fatalf("got %#v", got)
			}
			if tt.source == "customcontent.net" && got[0].ThumbnailURL != "https://example.test/images/sample.jpg" {
				t.Fatalf("thumbnail URL not extracted: %#v", got[0])
			}
			if tt.source == "LoversLab" && got[0].ThumbnailURL != "https://example.test/sample.jpg" {
				t.Fatalf("background thumbnail URL not extracted: %#v", got[0])
			}
		})
	}
}

func TestWickedWhimsExtractsPublishedVersion(t *testing.T) {
	a := newHTMLAdapter("WickedWhims", "https://turbodriver.itch.io/wickedwhims", http.DefaultClient)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<h1 class="game_title">WickedWhims</h1><h1 class="text-center">WickedWhims v185k</h1>`))
	if err != nil {
		t.Fatal(err)
	}
	mods := a.extract(doc, a.baseURL, "")
	if len(mods) != 1 || mods[0].Version != "v185k" {
		t.Fatalf("unexpected WickedWhims version: %#v", mods)
	}
}

func TestLoversLabUsesSims4CategoryListing(t *testing.T) {
	for _, adapter := range NewHTMLSources(http.DefaultClient) {
		if html, ok := adapter.(*HTMLAdapter); ok && html.Source() == "LoversLab" {
			if html.baseURL != "https://www.loverslab.com/files/category/161-the-sims-4/" {
				t.Fatalf("unexpected LoversLab listing URL: %s", html.baseURL)
			}
			return
		}
	}
	t.Fatal("LoversLab adapter not registered")
}

func TestCustomContentSearchFiltersContentCards(t *testing.T) {
	a := newHTMLAdapter("customcontent.net", "https://thesims4.customcontent.net/", http.DefaultClient)
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<article><h2><a href="/blog/post/">Editorial Post</a></h2></article><div id="slp"><a class="card" href="/crop-top/"><img data-src="/images/crop.jpg"><div class="ct2"><h4>Snow Crop Top</h4></div><div class="ct3"><p>GrayVixen</p></div></a><a class="card" href="/dress/"><div class="ct2"><h4>Floral Dress</h4></div><div class="ct3"><p>Creator</p></div></a></div>`))
	if err != nil {
		t.Fatal(err)
	}
	got := a.extract(doc, a.baseURL, "snow")
	if len(got) != 1 || got[0].Name != "Snow Crop Top" || got[0].Creator != "GrayVixen" {
		t.Fatalf("unexpected filtered results: %#v", got)
	}
	if got[0].ThumbnailURL != "https://thesims4.customcontent.net/images/crop.jpg" {
		t.Fatalf("thumbnail URL not resolved: %#v", got[0])
	}
}

func TestHTMLAdapterLoadsCachedDescriptionAndPublicArchiveLink(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `<html><head><meta name="description" content="A real outfit description."></head><a href="/files/outfit.zip">Download</a></html>`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	a := newHTMLAdapter("customcontent.net", "https://thesims4.customcontent.net/", client)
	a.minInterval = 0
	desc, download, err := a.Details(context.Background(), "https://thesims4.customcontent.net/outfit/")
	if err != nil || desc != "A real outfit description." || download != "https://thesims4.customcontent.net/files/outfit.zip" {
		t.Fatalf("details=%q download=%q err=%v", desc, download, err)
	}
	if _, _, err := a.Details(context.Background(), "https://thesims4.customcontent.net/outfit/"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("details cache made %d requests", calls.Load())
	}
}

func TestHTMLAdapterCachesAndSendsUserAgent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.UserAgent() == "" {
			t.Error("missing User-Agent")
		}
		_, _ = w.Write([]byte(`<div id="slp"><a class="card" href="/post"><div class="ct2"><h4>Test Post</h4></div><div class="ct3"><p>Creator</p></div></a></div>`))
	}))
	defer server.Close()
	a := newHTMLAdapter("customcontent.net", server.URL, http.DefaultClient)
	a.minInterval = time.Millisecond
	for range 2 {
		got, err := a.Search(context.Background(), "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d results", len(got))
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected cached response, made %d requests", calls.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("12", now); got != 12*time.Second {
		t.Fatalf("seconds Retry-After = %s", got)
	}
	if got := parseRetryAfter(now.Add(7*time.Minute).Format(http.TimeFormat), now); got != 5*time.Minute {
		t.Fatalf("date Retry-After should be capped, got %s", got)
	}
	if got := parseRetryAfter("not a date", now); got != 0 {
		t.Fatalf("invalid Retry-After = %s", got)
	}
}

func TestLoversLabPublicPageFetcher(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("wget is required for the LoversLab-compatible transport")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "Sims4ModManager/0.1 (+personal catalog; contact: sims4-mod-manager)" {
			t.Errorf("unexpected User-Agent: %q", r.UserAgent())
		}
		_, _ = w.Write([]byte(`<a href="/files/file/32129-sims-4-sample/" class="ipsDataItem_title">Sims 4 Sample</a>`))
	}))
	defer server.Close()
	doc, err := fetchLoversLab(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	mods := newHTMLAdapter("LoversLab", server.URL, server.Client()).extract(doc, server.URL, "")
	if len(mods) != 1 || mods[0].Name != "Sims 4 Sample" {
		t.Fatalf("unexpected result: %#v", mods)
	}
}

func TestWickedCCExtractsRealListingFixture(t *testing.T) {
	html, err := os.ReadFile("testdata/wickedcc-card.html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(html)))
	if err != nil {
		t.Fatal(err)
	}
	a := newHTMLAdapter("WickedCC", "https://wicked.cc/mods", http.DefaultClient)
	mods := a.extract(doc, a.baseURL, "")
	if len(mods) != 1 || mods[0].Name != "WickedWhims" || mods[0].Creator != "TURBODRIVER" {
		t.Fatalf("unexpected WickedCC listing parse: %#v", mods)
	}
	if mods[0].ThumbnailURL != "https://wicked.cc/storage/174/Featured.jpg" {
		t.Fatalf("unexpected WickedCC thumbnail: %s", mods[0].ThumbnailURL)
	}
	if mods[0].URL != "https://wicked.cc/mods/TURBODRIVER/wickedwhims" {
		t.Fatalf("unexpected WickedCC item URL: %s", mods[0].URL)
	}
}

func TestWickedCCDetailsFindDescriptionAndPublicPackageRoute(t *testing.T) {
	html, err := os.ReadFile("testdata/wickedcc-detail.html")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(html))), Request: r}, nil
	})}
	a := newHTMLAdapter("WickedCC", "https://wicked.cc/mods", client)
	a.minInterval = 0
	description, download, err := a.Details(context.Background(), "https://wicked.cc/clothing/grayvixen/storm-lingerie-set")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, "Maxis Match") || download != "https://wicked.cc/clothing/grayvixen/storm-lingerie-set/download/Xq7KRU3IU8" {
		t.Fatalf("unexpected WickedCC details: %q %q", description, download)
	}
}

func TestWickedCCSearchAndPaginatedBrowse(t *testing.T) {
	a := newHTMLAdapter("WickedCC", "https://wicked.cc/mods", http.DefaultClient)
	search, err := a.searchURL("storm lingerie")
	if err != nil || search != "https://wicked.cc/search?s=storm+lingerie" {
		t.Fatalf("unexpected WickedCC search URL %q: %v", search, err)
	}
	var requested string
	a.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requested = r.URL.String()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`<a class="post-card" href="https://wicked.cc/clothing/grayvixen/storm-lingerie-set"><h3 class="post-card-title">Storm Lingerie Set</h3><div class="post-card-avatar"><img alt="GrayVixen"></div></a><nav class="pagination"><a rel="next" href="https://wicked.cc/clothing/all?sort=downloads&amp;page=3">Next</a></nav>`)), Request: r}, nil
	})}
	a.minInterval = 0
	mods, hasMore, err := a.Browse(context.Background(), "clothing", 20, 20)
	if err != nil || !hasMore || len(mods) != 1 || mods[0].Name != "Storm Lingerie Set" {
		t.Fatalf("unexpected paginated browse: %#v %v %v", mods, hasMore, err)
	}
	if requested != "https://wicked.cc/clothing/all?sort=downloads&page=2" {
		t.Fatalf("unexpected WickedCC page URL: %s", requested)
	}
	mods, hasMore, err = a.Browse(context.Background(), "clothing", 20, 20)
	if err != nil || !hasMore || len(mods) != 1 || requested != "https://wicked.cc/clothing/all?sort=downloads&page=2" {
		t.Fatalf("cached page lost pagination state: %#v %v %v requested=%s", mods, hasMore, err, requested)
	}
}

func TestWickedWhimsOfficialInfoExtractsLiveMarkupFixture(t *testing.T) {
	html, err := os.ReadFile("testdata/wickedwhims-official-info.html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(html)))
	if err != nil {
		t.Fatal(err)
	}
	info := extractWickedWhimsOfficialInfo(doc)
	if info.Version != "v185k" || info.SupportedVersion != "1.129.39/October 27" || info.Conflicts != "WonderfulWhims" {
		t.Fatalf("unexpected official WickedWhims info: %#v", info)
	}
}
