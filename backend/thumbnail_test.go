package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type thumbnailRoundTripper func(*http.Request) (*http.Response, error)

func (f thumbnailRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestThumbnailProxyAllowListAndPassthrough(t *testing.T) {
	image := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	fetches := 0
	app := &App{thumbnailClient: &http.Client{Transport: thumbnailRoundTripper(func(req *http.Request) (*http.Response, error) {
		fetches++
		if req.URL.Host != "static.loverslab.com" || req.Header.Get("Referer") != "https://www.loverslab.com/" {
			t.Fatalf("unexpected upstream request: %s referer=%q", req.URL, req.Header.Get("Referer"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader(string(image))), Request: req}, nil
	})}}
	serve := func(raw string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/thumbnail?url="+raw, nil)
		w := httptest.NewRecorder()
		app.thumbnail(w, r)
		return w
	}

	bad := serve("https://static.loverslab.com.evil.test/image.png")
	if bad.Code != http.StatusBadRequest || fetches != 0 {
		t.Fatalf("disallowed host status=%d fetches=%d", bad.Code, fetches)
	}
	for _, raw := range []string{
		"http://static.loverslab.com/image.png",
		"https://static.loverslab.com:8443/image.png",
		"https://user@static.loverslab.com/image.png",
	} {
		if got := serve(raw); got.Code != http.StatusBadRequest {
			t.Errorf("URL %q status=%d, want 400", raw, got.Code)
		}
	}
	wantURL := "https://static.loverslab.com/screenshots/example.png"
	first := serve(wantURL)
	second := serve(wantURL)
	for i, got := range []*httptest.ResponseRecorder{first, second} {
		if got.Code != http.StatusOK || got.Header().Get("Content-Type") != "image/png" || string(got.Body.Bytes()) != string(image) {
			t.Errorf("response %d did not pass image through: status=%d type=%q body=%v", i, got.Code, got.Header().Get("Content-Type"), got.Body.Bytes())
		}
	}
	if fetches != 1 {
		t.Fatalf("expected cache to serve second request; upstream fetches=%d", fetches)
	}
}
