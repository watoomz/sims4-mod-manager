package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

type rssAdapter struct {
	name   string
	feed   string
	client *http.Client
}

type cachedFeed struct {
	items   []CatalogMod
	expires time.Time
}

var feedCache = struct {
	sync.Mutex
	items map[string]cachedFeed
}{items: make(map[string]cachedFeed)}

func (a *rssAdapter) Source() string { return a.name }

type rssDocument struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Items []rssItem `xml:"entry"`
}

type rssItem struct {
	Title          string  `xml:"title"`
	Link           rssLink `xml:"link"`
	Description    string  `xml:"description"`
	Summary        string  `xml:"summary"`
	Content        string  `xml:"content"`
	MediaThumbnail struct {
		URL string `xml:"url,attr"`
	} `xml:"http://search.yahoo.com/mrss/ thumbnail"`
	Creator string `xml:"creator"`
	Author  struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

type rssLink struct {
	Text string `xml:",chardata"`
	Href string `xml:"href,attr"`
}

func (a *rssAdapter) Search(ctx context.Context, query string, limit int) ([]CatalogMod, error) {
	feedCache.Lock()
	cached, ok := feedCache.items[a.feed]
	if !ok || time.Now().After(cached.expires) {
		items, err := a.fetch(ctx)
		if err != nil {
			feedCache.Unlock()
			return nil, err
		}
		cached = cachedFeed{items: items, expires: time.Now().Add(30 * time.Minute)}
		feedCache.items[a.feed] = cached
	}
	feedCache.Unlock()
	if limit < 1 || limit > 200 {
		limit = 100
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	result := []CatalogMod{}
	for _, item := range cached.items {
		if needle == "" || strings.Contains(strings.ToLower(item.Name), needle) || strings.Contains(strings.ToLower(item.Creator), needle) {
			result = append(result, item)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (a *rssAdapter) fetch(ctx context.Context) ([]CatalogMod, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.feed, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Sims4ModManager/0.1 (+personal mod catalog)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml")
	response, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("custom feed returned HTTP %d", response.StatusCode)
	}
	var document rssDocument
	if err := xml.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&document); err != nil {
		return nil, fmt.Errorf("parse custom feed: %w", err)
	}
	items := append(document.Channel.Items, document.Items...)
	result := []CatalogMod{}
	for _, item := range items {
		name := strings.TrimSpace(item.Title)
		link := strings.TrimSpace(item.Link.Text)
		if link == "" {
			link = strings.TrimSpace(item.Link.Href)
		}
		if name == "" || link == "" {
			continue
		}
		if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
			continue
		}
		creator := item.Creator
		if creator == "" {
			creator = item.Author.Name
		}
		description := item.Description
		if description == "" {
			description = item.Summary
		}
		if description == "" {
			description = item.Content
		}
		thumbnail := strings.TrimSpace(item.MediaThumbnail.URL)
		if thumbnail == "" && description != "" {
			if doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(description)); parseErr == nil {
				img := doc.Find("img").First()
				thumbnail = img.AttrOr("data-src", img.AttrOr("data-lazy-src", img.AttrOr("src", "")))
				if strings.HasPrefix(strings.ToLower(thumbnail), "data:image/gif;base64,") {
					thumbnail = img.AttrOr("data-lazy-src", "")
				}
			}
		}
		if thumbnail != "" {
			base, _ := url.Parse(link)
			imageURL, parseErr := url.Parse(thumbnail)
			if parseErr == nil {
				thumbnail = base.ResolveReference(imageURL).String()
			} else {
				thumbnail = ""
			}
		}
		mod := CatalogMod{Name: name, Creator: strings.TrimSpace(creator), Description: strings.TrimSpace(description), Source: a.name, URL: link, ThumbnailURL: thumbnail}
		if parsed, parseErr := url.Parse(link); parseErr == nil && parsed.Scheme == "https" && isSupportedArchiveExtension(parsed.Path) && validCustomFeedURL(link) == nil {
			mod.SourceDownloadURL = link
		}
		result = append(result, mod)
	}
	return result, nil
}

func validCustomFeedURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("feed URL must be an http(s) URL without embedded credentials")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("feed host must be a public DNS name")
	}
	port := parsed.Port()
	if port != "" && port != "80" && port != "443" {
		return fmt.Errorf("custom feed ports must be 80 or 443")
	}
	return nil
}

func safeFeedHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, candidate := range ips {
				ip, ok := netip.AddrFromSlice(candidate.IP)
				if !ok || !publicFeedIP(ip.Unmap()) {
					continue
				}
				conn, dialErr := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				lastErr = dialErr
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("feed host resolved only to non-public addresses")
		},
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many feed redirects")
		}
		return validCustomFeedURL(req.URL.String())
	}}
}

func publicFeedIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	blocked := []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func (a *App) listRepos(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT id,name,type,url,enabled FROM repos ORDER BY name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name, kind, feed string
		var enabled int
		if rows.Scan(&id, &name, &kind, &feed, &enabled) == nil {
			out = append(out, map[string]any{"id": id, "name": name, "type": kind, "url": feed, "enabled": enabled == 1})
		}
	}
	writeJSON(w, 200, out)
}

func (a *App) addRepo(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&input); err != nil || strings.TrimSpace(input.Name) == "" {
		http.Error(w, "name and feed URL are required", 400)
		return
	}
	if err := validCustomFeedURL(input.URL); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := strings.TrimSpace(input.Name)
	var exists int
	if err := a.db.QueryRow(`SELECT count(*) FROM repos WHERE lower(name)=lower(?)`, name).Scan(&exists); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	if exists > 0 {
		http.Error(w, "repository name already exists", 409)
		return
	}
	result, err := a.db.Exec(`INSERT INTO repos(name,type,url) VALUES(?,'rss',?)`, name, strings.TrimSpace(input.URL))
	if err != nil {
		http.Error(w, "could not save repository", 409)
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, 201, map[string]any{"id": id, "name": name, "type": "rss", "url": strings.TrimSpace(input.URL), "enabled": true})
}

func (a *App) customRepoAdapters() ([]SourceAdapter, error) {
	rows, err := a.db.Query(`SELECT name,url FROM repos WHERE enabled=1 AND type='rss' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceAdapter{}
	for rows.Next() {
		var name, feed string
		if err := rows.Scan(&name, &feed); err != nil {
			return nil, err
		}
		out = append(out, &rssAdapter{name: name, feed: feed, client: safeFeedHTTPClient()})
	}
	return out, nil
}
