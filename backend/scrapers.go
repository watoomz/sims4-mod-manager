package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type SourceAdapter interface {
	Source() string
	Search(context.Context, string, int) ([]CatalogMod, error)
}

type paginatedSourceAdapter interface {
	Browse(context.Context, string, int, int) ([]CatalogMod, bool, error)
}

type HTMLAdapter struct {
	source, baseURL string
	client          *http.Client
	minInterval     time.Duration
	mu              sync.Mutex
	next            time.Time
	cache           map[string]cachedPage
	detailCache     map[string]cachedModDetails
	infoCache       wickedWhimsOfficialInfo
	infoExpires     time.Time
}

type wickedWhimsOfficialInfo struct {
	Version          string `json:"official_version,omitempty"`
	SupportedVersion string `json:"supported_game_version,omitempty"`
	Conflicts        string `json:"conflicting_mods,omitempty"`
}

type cachedPage struct {
	expires time.Time
	mods    []CatalogMod
	hasMore bool
}

type cachedModDetails struct {
	expires     time.Time
	description string
	downloadURL string
}

var backgroundImagePattern = regexp.MustCompile(`(?i)background-image\s*:\s*url\(\s*["']?([^)'"\s]+)`)
var wickedWhimsVersionPattern = regexp.MustCompile(`(?i)WickedWhims\s+(v[0-9][A-Za-z0-9.]*)`)

func thumbnailFromSelection(card *goquery.Selection, pageURL string) string {
	thumbnail := ""
	if img := card.Find("img").First(); img.Length() > 0 {
		thumbnail = img.AttrOr("data-src", img.AttrOr("data-lazy-src", img.AttrOr("src", "")))
		if strings.HasPrefix(strings.ToLower(thumbnail), "data:image/gif;base64,") {
			thumbnail = img.AttrOr("data-lazy-src", "")
		}
	}
	if thumbnail == "" {
		style := card.Find("[style*='background-image']").First().AttrOr("style", "")
		if match := backgroundImagePattern.FindStringSubmatch(style); len(match) == 2 {
			thumbnail = match[1]
		}
	}
	if thumbnail == "" {
		return ""
	}
	imageURL, err := url.Parse(thumbnail)
	if err != nil {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return ""
	}
	return base.ResolveReference(imageURL).String()
}

func NewHTMLSources(client *http.Client) []SourceAdapter {
	if client == nil {
		// A dedicated transport uses HTTP/1.1, which is broadly compatible with
		// the HTML hosts used here and avoids holding a shared global connection pool.
		client = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	return []SourceAdapter{
		newHTMLAdapter("WickedWhims", "https://turbodriver.itch.io/wickedwhims", client),
		newHTMLAdapter("LoversLab", "https://www.loverslab.com/files/category/161-the-sims-4/", client),
		newHTMLAdapter("customcontent.net", "https://thesims4.customcontent.net/", client),
		newHTMLAdapter("WickedCC", "https://wicked.cc/mods", client),
	}
}

func newHTMLAdapter(source, base string, client *http.Client) *HTMLAdapter {
	return &HTMLAdapter{source: source, baseURL: base, client: client, minInterval: 10 * time.Second, cache: map[string]cachedPage{}, detailCache: map[string]cachedModDetails{}}
}
func (a *HTMLAdapter) Source() string { return a.source }

func (a *HTMLAdapter) WickedWhimsOfficialInfo(ctx context.Context) (wickedWhimsOfficialInfo, error) {
	if a.source != "WickedWhims" {
		return wickedWhimsOfficialInfo{}, fmt.Errorf("official WickedWhims details requested from another source")
	}
	a.mu.Lock()
	if time.Now().Before(a.infoExpires) {
		info := a.infoCache
		a.mu.Unlock()
		return info, nil
	}
	a.mu.Unlock()
	doc, err := a.fetch(ctx, "https://wickedwhimsmod.com/download/")
	if err != nil {
		return wickedWhimsOfficialInfo{}, err
	}
	info := extractWickedWhimsOfficialInfo(doc)
	a.mu.Lock()
	a.infoCache = info
	a.infoExpires = time.Now().Add(30 * time.Minute)
	a.mu.Unlock()
	return info, nil
}

func extractWickedWhimsOfficialInfo(doc *goquery.Document) wickedWhimsOfficialInfo {
	info := wickedWhimsOfficialInfo{}
	doc.Find("h2").EachWithBreak(func(_ int, heading *goquery.Selection) bool {
		if match := wickedWhimsVersionPattern.FindStringSubmatch(strings.Join(strings.Fields(heading.Text()), " ")); len(match) == 2 {
			info.Version = match[1]
			return false
		}
		return true
	})
	doc.Find("strong").EachWithBreak(func(_ int, strong *goquery.Selection) bool {
		if strings.TrimSpace(strong.Text()) != "Supported Game Versions:" {
			return true
		}
		if sibling := strong.Parent().Next(); sibling.Length() > 0 {
			info.SupportedVersion = strings.Join(strings.Fields(sibling.Text()), " ")
		}
		return false
	})
	doc.Find("p").EachWithBreak(func(_ int, paragraph *goquery.Selection) bool {
		text := strings.Join(strings.Fields(paragraph.Text()), " ")
		marker := "Conflicting Mods:"
		if index := strings.Index(text, marker); index >= 0 {
			info.Conflicts = strings.Trim(strings.TrimSpace(text[index+len(marker):]), "• -\t")
			return false
		}
		return true
	})
	return info
}

func (a *HTMLAdapter) Details(ctx context.Context, pageURL string) (string, string, error) {
	parsed, err := url.Parse(pageURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return "", "", fmt.Errorf("invalid source page URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if a.source == "LoversLab" && host != "www.loverslab.com" && host != "loverslab.com" {
		return "", "", fmt.Errorf("unexpected LoversLab host")
	}
	if a.source == "customcontent.net" && host != "thesims4.customcontent.net" {
		return "", "", fmt.Errorf("unexpected customcontent.net host")
	}
	if a.source == "WickedCC" && host != "wicked.cc" && host != "www.wicked.cc" {
		return "", "", fmt.Errorf("unexpected WickedCC host")
	}
	a.mu.Lock()
	if cached, ok := a.detailCache[pageURL]; ok && time.Now().Before(cached.expires) {
		a.mu.Unlock()
		return cached.description, cached.downloadURL, nil
	}
	a.mu.Unlock()
	doc, err := a.fetch(ctx, pageURL)
	if err != nil {
		return "", "", err
	}
	description := strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
	if description == "" {
		description = strings.TrimSpace(doc.Find("meta[property='og:description']").AttrOr("content", ""))
	}
	if description == "" {
		for _, selector := range []string{".entry-content p", ".ipsType_richText p", ".ipsType_normal p", "[itemprop='description']"} {
			description = strings.Join(strings.Fields(doc.Find(selector).First().Text()), " ")
			if description != "" {
				break
			}
		}
	}
	downloadURL := ""
	if a.source == "customcontent.net" || a.source == "WickedCC" {
		doc.Find("a[href]").EachWithBreak(func(_ int, anchor *goquery.Selection) bool {
			href := strings.TrimSpace(anchor.AttrOr("href", ""))
			ref, parseErr := url.Parse(href)
			if parseErr != nil {
				return true
			}
			absolute := parsed.ResolveReference(ref)
			allowed := absolute.Scheme == "https" && absolute.User == nil && isSupportedArchiveExtension(absolute.Path)
			if a.source == "WickedCC" {
				allowed = absolute.Scheme == "https" && absolute.Hostname() == "wicked.cc" && strings.Contains(absolute.Path, "/download/")
			}
			if allowed {
				downloadURL = absolute.String()
				return false
			}
			return true
		})
	}
	a.mu.Lock()
	if a.detailCache == nil {
		a.detailCache = map[string]cachedModDetails{}
	}
	a.detailCache[pageURL] = cachedModDetails{expires: time.Now().Add(30 * time.Minute), description: description, downloadURL: downloadURL}
	a.mu.Unlock()
	return description, downloadURL, nil
}

func (a *HTMLAdapter) Search(ctx context.Context, query string, limit int) ([]CatalogMod, error) {
	if limit < 1 || limit > 100 {
		limit = 30
	}
	pageURL, err := a.searchURL(query)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if hit, ok := a.cache[pageURL]; ok && time.Now().Before(hit.expires) {
		out := append([]CatalogMod(nil), hit.mods...)
		a.mu.Unlock()
		if len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	}
	a.mu.Unlock()
	doc, err := a.fetch(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	mods := a.extract(doc, pageURL, query)
	a.mu.Lock()
	a.cache[pageURL] = cachedPage{expires: time.Now().Add(30 * time.Minute), mods: append([]CatalogMod(nil), mods...)}
	a.mu.Unlock()
	if len(mods) > limit {
		mods = mods[:limit]
	}
	return mods, nil
}

// waitTurn spaces every upstream attempt, including retries, across concurrent
// searches made through this adapter.
func (a *HTMLAdapter) waitTurn(ctx context.Context) error {
	a.mu.Lock()
	now := time.Now()
	wait := time.Until(a.next)
	if wait < 0 {
		wait = 0
	}
	base := now
	if a.next.After(base) {
		base = a.next
	}
	a.next = base.Add(a.minInterval)
	a.mu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (a *HTMLAdapter) searchURL(query string) (string, error) {
	base, err := url.Parse(a.baseURL)
	if err != nil {
		return "", err
	}
	query = strings.TrimSpace(query)
	switch a.source {
	case "customcontent.net":
		// This site uses a custom front-page card feed for uploaded CC; its
		// WordPress `?s=` route searches editorial posts instead.
	case "LoversLab":
		if query != "" {
			values := base.Query()
			values.Set("q", query)
			base.RawQuery = values.Encode()
		}
	case "WickedCC":
		base, _ = url.Parse("https://wicked.cc/search")
		if query == "" {
			base, _ = url.Parse("https://wicked.cc/mods")
		} else {
			values := base.Query()
			values.Set("s", query)
			base.RawQuery = values.Encode()
		}
	}
	return base.String(), nil
}

func (a *HTMLAdapter) Browse(ctx context.Context, category string, offset, limit int) ([]CatalogMod, bool, error) {
	if a.source != "WickedCC" {
		return nil, false, fmt.Errorf("source does not support paginated browsing")
	}
	if limit < 1 || limit > 50 {
		limit = 20
	}
	allowedCategories := map[string]bool{"mods": true, "clothing": true, "animations": true, "body": true, "accessories": true, "objects": true}
	if !allowedCategories[category] {
		category = "mods"
	}
	page := offset/limit + 1
	target := fmt.Sprintf("https://wicked.cc/%s/all?sort=downloads&page=%d", category, page)
	a.mu.Lock()
	cached, ok := a.cache[target]
	if ok && time.Now().Before(cached.expires) {
		mods := append([]CatalogMod(nil), cached.mods...)
		hasMore := cached.hasMore
		a.mu.Unlock()
		if len(mods) > limit {
			mods = mods[:limit]
		}
		return mods, hasMore, nil
	}
	a.mu.Unlock()
	doc, err := a.fetch(ctx, target)
	if err != nil {
		return nil, false, err
	}
	mods := a.extract(doc, target, "")
	hasMore := doc.Find("nav.pagination a[rel='next']").Length() > 0
	a.mu.Lock()
	a.cache[target] = cachedPage{expires: time.Now().Add(30 * time.Minute), mods: append([]CatalogMod(nil), mods...), hasMore: hasMore}
	a.mu.Unlock()
	if len(mods) > limit {
		mods = mods[:limit]
	}
	return mods, hasMore, nil
}
func (a *HTMLAdapter) fetch(ctx context.Context, target string) (*goquery.Document, error) {
	if a.source == "LoversLab" {
		return fetchLoversLab(ctx, target, a.waitTurn)
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := a.waitTurn(ctx); err != nil {
			return nil, err
		}
		retryDelay := time.Duration(0)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "Sims4ModManager/0.1 (+personal catalog; contact: sims4-mod-manager)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, err := a.client.Do(req)
		if err != nil {
			last = err
		} else {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				doc, parseErr := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, 5<<20))
				_ = resp.Body.Close()
				if parseErr != nil {
					return nil, parseErr
				}
				return doc, nil
			}
			last = fmt.Errorf("%s returned HTTP %d", a.source, resp.StatusCode)
			retry := resp.StatusCode == 429 || resp.StatusCode >= 500
			retryDelay = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			_ = resp.Body.Close()
			if !retry {
				return nil, last
			}
		}
		if attempt < 2 {
			if retryDelay == 0 {
				retryDelay = time.Duration(1<<attempt) * time.Second
			}
			if err := waitContext(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}
	return nil, last
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, 5*time.Minute)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return min(date.Sub(now), 5*time.Minute)
	}
	return 0
}

// LoversLab returns 403 to Go's TLS client, while its public listing accepts a
// normal HTTP/1.1 request. Use the system wget as a narrow transport fallback;
// do not solve challenges or access authenticated downloads.
func fetchLoversLab(ctx context.Context, target string, waiters ...func(context.Context) error) (*goquery.Document, error) {
	waitTurn := func(context.Context) error { return nil }
	if len(waiters) > 0 && waiters[0] != nil {
		waitTurn = waiters[0]
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if err := waitTurn(ctx); err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "wget", "-qO-", "--timeout=20", "--tries=1", "--user-agent=Sims4ModManager/0.1 (+personal catalog; contact: sims4-mod-manager)", "--header=Accept: text/html,application/xhtml+xml", target)
		body, err := cmd.Output()
		if err == nil {
			if len(body) > 5<<20 {
				return nil, fmt.Errorf("LoversLab response exceeded 5 MiB")
			}
			doc, parseErr := goquery.NewDocumentFromReader(bytes.NewReader(body))
			if parseErr != nil {
				return nil, parseErr
			}
			if doc.Find("script[src*='challenge-platform'], #challenge-form").Length() > 0 {
				return nil, fmt.Errorf("LoversLab returned a browser challenge")
			}
			return doc, nil
		}
		last = fmt.Errorf("LoversLab wget failed: %w", err)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt < 2 {
			if err := waitContext(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
				return nil, err
			}
		}
	}
	return nil, last
}
func (a *HTMLAdapter) extract(doc *goquery.Document, pageURL, query string) []CatalogMod {
	seen := map[string]bool{}
	out := []CatalogMod{}
	if a.source == "WickedWhims" {
		title := strings.TrimSpace(doc.Find("h1").First().Text())
		if title == "" {
			title = "WickedWhims"
		}
		creator := "TURBODRIVER"
		desc := strings.TrimSpace(doc.Find("meta[name='description']").AttrOr("content", ""))
		version := ""
		doc.Find("h1.text-center").EachWithBreak(func(_ int, heading *goquery.Selection) bool {
			if match := wickedWhimsVersionPattern.FindStringSubmatch(strings.Join(strings.Fields(heading.Text()), " ")); len(match) == 2 {
				version = match[1]
				return false
			}
			return true
		})
		out = append(out, CatalogMod{ID: 0, Name: title, Creator: creator, Description: desc, Source: a.source, URL: a.baseURL, ThumbnailURL: doc.Find("meta[property='og:image']").AttrOr("content", ""), Version: version})
		return out
	}
	if a.source == "customcontent.net" {
		doc.Find("#slp a.card").Each(func(_ int, card *goquery.Selection) {
			href, ok := card.Attr("href")
			if !ok {
				return
			}
			name := strings.Join(strings.Fields(card.Find("h4").First().Text()), " ")
			creator := strings.Join(strings.Fields(card.Find(".ct3 p").First().Text()), " ")
			if name == "" || (query != "" && !strings.Contains(strings.ToLower(name+" "+creator), strings.ToLower(query))) {
				return
			}
			base, _ := url.Parse(pageURL)
			ref, err := url.Parse(href)
			if err != nil {
				return
			}
			absolute := base.ResolveReference(ref).String()
			thumbnail := thumbnailFromSelection(card, pageURL)
			if seen[absolute] {
				return
			}
			seen[absolute] = true
			out = append(out, CatalogMod{Name: name, Creator: creator, Source: a.source, URL: absolute, ThumbnailURL: thumbnail})
		})
		return out
	}
	if a.source == "WickedCC" {
		doc.Find("a.post-card").Each(func(_ int, card *goquery.Selection) {
			if len(out) >= 100 {
				return
			}
			href := strings.TrimSpace(card.AttrOr("href", ""))
			ref, err := url.Parse(href)
			if err != nil {
				return
			}
			base, _ := url.Parse(pageURL)
			absolute := base.ResolveReference(ref)
			if absolute.Scheme != "https" || (absolute.Hostname() != "wicked.cc" && absolute.Hostname() != "www.wicked.cc") {
				return
			}
			name := strings.Join(strings.Fields(card.Find("h3.post-card-title").First().Text()), " ")
			creator := strings.TrimSpace(card.Find(".post-card-avatar img").First().AttrOr("alt", ""))
			if name == "" || (query != "" && !strings.Contains(strings.ToLower(name+" "+creator), strings.ToLower(query))) || seen[absolute.String()] {
				return
			}
			seen[absolute.String()] = true
			out = append(out, CatalogMod{Name: name, Creator: creator, Source: a.source, URL: absolute.String(), ThumbnailURL: thumbnailFromSelection(card, pageURL)})
		})
		return out
	}
	selector := "a[href*='/files/file/'], .ipsDataItem_title a"
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		if len(out) >= 100 {
			return
		}
		href, ok := s.Attr("href")
		if !ok {
			return
		}
		ref, err := url.Parse(strings.TrimSpace(href))
		if err != nil {
			return
		}
		base, _ := url.Parse(pageURL)
		absolute := base.ResolveReference(ref).String()
		if a.source == "LoversLab" && !strings.Contains(ref.Path, "/files/file/") {
			return
		}
		if a.source == "customcontent.net" && !strings.Contains(ref.Hostname(), "customcontent.net") {
			return
		}
		if seen[absolute] || strings.TrimSpace(s.Text()) == "" {
			return
		}
		seen[absolute] = true
		name := strings.Join(strings.Fields(s.Text()), " ")
		if len(name) < 3 {
			return
		}
		creator := ""
		article := s.Closest("article, .ipsDataItem, .ipsStreamItem")
		if article.Length() == 0 {
			article = s.Closest("li")
		}
		if article.Length() > 0 {
			creator = strings.TrimSpace(article.Find(".author, .ipsType_blendLinks a, .entry-meta a[rel='author']").First().Text())
		}
		if query != "" && !strings.Contains(strings.ToLower(name+" "+creator), strings.ToLower(query)) {
			return
		}
		thumbnail := thumbnailFromSelection(article, pageURL)
		out = append(out, CatalogMod{Name: name, Creator: creator, Source: a.source, URL: absolute, ThumbnailURL: thumbnail})
	})
	return out
}
