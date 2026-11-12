package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidCustomFeedURLBlocksLocalTargets(t *testing.T) {
	for _, value := range []string{"file:///etc/passwd", "http://localhost/feed", "http://127.0.0.1/feed", "http://example.local/feed", "http://example.com:8080/feed", "https://user:pass@example.com/feed"} {
		if err := validCustomFeedURL(value); err == nil {
			t.Errorf("expected URL to be rejected: %s", value)
		}
	}
	if err := validCustomFeedURL("https://mods.example.org/feed.xml"); err != nil {
		t.Fatalf("public feed URL rejected: %v", err)
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "::1", "fd00::1", "2001:db8::1"} {
		if publicFeedIP(mustAddr(address)) {
			t.Errorf("expected non-public address %s to be blocked", address)
		}
	}
}

func mustAddr(value string) netip.Addr { return netip.MustParseAddr(value) }

func TestRSSAndAtomFeedParsingAndCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Summer Hair Set</title><link href="https://mods.example.org/hair"/><summary>New hairstyles</summary><author><name>Creator A</name></author></entry><entry><title>Winter Outfit</title><link href="https://mods.example.org/outfit"/><summary>Clothing</summary></entry></feed>`))
	}))
	defer server.Close()
	adapter := &rssAdapter{name: "Community Feed", feed: server.URL + "/atom", client: server.Client()}
	first, err := adapter.Search(context.Background(), "summer", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Name != "Summer Hair Set" || first[0].Creator != "Creator A" || first[0].Source != "Community Feed" {
		t.Fatalf("unexpected Atom item: %#v", first)
	}
	second, err := adapter.Search(context.Background(), "outfit", 10)
	if err != nil || len(second) != 1 || second[0].Name != "Winter Outfit" || calls.Load() != 1 {
		t.Fatalf("cached query result=%#v calls=%d err=%v", second, calls.Load(), err)
	}

	feed := `<rss version="2.0"><channel><item><title>Package Mod</title><link>https://mods.example.org/package</link><description>Details</description><creator>Creator B</creator></item><item><title>Direct Mod</title><link>https://cdn.example.org/direct-mod.zip</link><description>Public archive</description></item></channel></rss>`
	parsed := rssAdapter{name: "RSS", feed: server.URL, client: roundTripClient(func() string { return feed })}
	items, err := parsed.Search(context.Background(), "package", 1)
	if err != nil || len(items) != 1 || items[0].URL != "https://mods.example.org/package" || items[0].Creator != "Creator B" {
		t.Fatalf("unexpected RSS item: %#v err=%v", items, err)
	}
	direct, err := parsed.Search(context.Background(), "direct", 1)
	if err != nil || len(direct) != 1 || direct[0].SourceDownloadURL != "https://cdn.example.org/direct-mod.zip" {
		t.Fatalf("RSS direct archive was not identified: %#v err=%v", direct, err)
	}
}

func roundTripClient(body func() string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body())), Request: r}, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAddCustomRepositoryValidatesAndPersists(t *testing.T) {
	app := testApp(t)
	request := httptest.NewRequest("POST", "/api/repos", strings.NewReader(`{"name":"Studio Feed","url":"https://mods.example.org/rss.xml"}`))
	recorder := httptest.NewRecorder()
	app.addRepo(recorder, request)
	if recorder.Code != 201 {
		t.Fatalf("add repo returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var count int
	if err := app.db.QueryRow(`SELECT count(*) FROM repos WHERE name='Studio Feed' AND type='rss'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repo not persisted: count=%d err=%v", count, err)
	}
	bad := httptest.NewRequest("POST", "/api/repos", strings.NewReader(`{"name":"Local","url":"http://127.0.0.1:8080/"}`))
	badRecorder := httptest.NewRecorder()
	app.addRepo(badRecorder, bad)
	if badRecorder.Code != 400 {
		t.Fatalf("expected SSRF URL to be rejected, got %d", badRecorder.Code)
	}
}
