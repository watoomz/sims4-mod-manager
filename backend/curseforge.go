package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const curseForgeGameID = 432

type CurseForgeAdapter struct {
	key     string
	client  *http.Client
	baseURL string
}
type CFMod struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Summary       string `json:"summary"`
	WebsiteURL    string `json:"links.websiteUrl"`
	DownloadCount int64  `json:"downloadCount"`
	ClassID       int    `json:"classId"`
	Authors       []struct {
		Name string `json:"name"`
	} `json:"authors"`
}
type cfSearchResponse struct {
	Data []struct {
		ID            int    `json:"id"`
		Name          string `json:"name"`
		Summary       string `json:"summary"`
		DownloadCount int64  `json:"downloadCount"`
		Authors       []struct {
			Name string `json:"name"`
		} `json:"authors"`
		Links struct {
			WebsiteURL string `json:"websiteUrl"`
		} `json:"links"`
		Logo struct {
			URL string `json:"url"`
		} `json:"logo"`
	} `json:"data"`
}
type CatalogMod struct {
	ID                int    `json:"id"`
	CatalogID         int64  `json:"catalog_id,omitempty"`
	Name              string `json:"name"`
	Creator           string `json:"creator"`
	Description       string `json:"description"`
	Source            string `json:"source"`
	URL               string `json:"url"`
	Downloads         int64  `json:"downloads"`
	HasArchive        bool   `json:"has_archive"`
	ThumbnailURL      string `json:"thumbnail_url,omitempty"`
	SourceDownloadURL string `json:"source_download_url,omitempty"`
	Version           string `json:"version,omitempty"`
}

func NewCurseForgeAdapter(key string, client *http.Client) *CurseForgeAdapter {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &CurseForgeAdapter{key: key, client: client, baseURL: "https://api.curseforge.com"}
}
func (a *CurseForgeAdapter) Configured() bool { return a.key != "" }
func (a *CurseForgeAdapter) Search(ctx context.Context, query string, index, pageSize int) ([]CatalogMod, error) {
	return a.search(ctx, query, index, pageSize, false)
}

func (a *CurseForgeAdapter) search(ctx context.Context, query string, index, pageSize int, popular bool) ([]CatalogMod, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("CurseForge API key is not configured")
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 20
	}
	// Sims 4's catalogue should be scoped by gameId. A hard-coded classId
	// can exclude the game's listings when CurseForge uses a different class map.
	u := a.baseURL + "/v1/mods/search?gameId=432&searchFilter=" + url.QueryEscape(query) + fmt.Sprintf("&index=%d&pageSize=%d", index, pageSize)
	if popular {
		u += "&sortField=6&sortOrder=desc"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("User-Agent", "Sims4ModManager/0.1 (+personal mod catalog)")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CurseForge returned HTTP %d", resp.StatusCode)
	}
	var decoded cfSearchResponse
	if err = json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	out := make([]CatalogMod, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		creator := ""
		if len(m.Authors) > 0 {
			creator = m.Authors[0].Name
		}
		out = append(out, CatalogMod{ID: m.ID, Name: m.Name, Creator: creator, Description: m.Summary, Source: "CurseForge", URL: m.Links.WebsiteURL, Downloads: m.DownloadCount, ThumbnailURL: m.Logo.URL})
	}
	return out, nil
}

func (a *CurseForgeAdapter) Trending(ctx context.Context, limit int) ([]CatalogMod, error) {
	return a.search(ctx, "", 0, limit, true)
}

func (a *CurseForgeAdapter) ModDetails(ctx context.Context, modID int64) (json.RawMessage, error) {
	return a.getJSON(ctx, "/v1/mods/"+strconv.FormatInt(modID, 10))
}

func (a *CurseForgeAdapter) ModFiles(ctx context.Context, modID int64) (json.RawMessage, error) {
	return a.getJSON(ctx, "/v1/mods/"+strconv.FormatInt(modID, 10)+"/files?pageSize=50")
}

func (a *CurseForgeAdapter) ModFile(ctx context.Context, modID, fileID int64) (json.RawMessage, error) {
	return a.getJSON(ctx, "/v1/mods/"+strconv.FormatInt(modID, 10)+"/files/"+strconv.FormatInt(fileID, 10))
}

func (a *CurseForgeAdapter) DownloadURL(ctx context.Context, modID, fileID int64) (string, error) {
	data, err := a.getJSON(ctx, "/v1/mods/"+strconv.FormatInt(modID, 10)+"/files/"+strconv.FormatInt(fileID, 10)+"/download-url")
	if err != nil {
		return "", err
	}
	var response struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", err
	}
	if response.Data == "" {
		return "", fmt.Errorf("CurseForge returned an empty download URL")
	}
	return response.Data, nil
}

func (a *CurseForgeAdapter) FingerprintMatches(ctx context.Context, fingerprints []uint32) (json.RawMessage, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("CurseForge API key is not configured")
	}
	body, err := json.Marshal(map[string][]uint32{"fingerprints": fingerprints})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/fingerprints", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.doJSON(req)
}

func (a *CurseForgeAdapter) getJSON(ctx context.Context, path string) (json.RawMessage, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("CurseForge API key is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return a.doJSON(req)
}

func (a *CurseForgeAdapter) doJSON(req *http.Request) (json.RawMessage, error) {
	req.Header.Set("x-api-key", a.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Sims4ModManager/0.1 (+personal mod catalog)")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CurseForge returned HTTP %d", resp.StatusCode)
	}
	var decoded json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}
