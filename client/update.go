package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxClientBinaryBytes = 256 << 20

// ClientRelease is the response contract for GET /api/client/version.
type ClientRelease struct {
	Version     string `json:"version"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

func (c *SyncClient) CheckForUpdate(ctx context.Context) error {
	endpoint := strings.TrimRight(c.ServerURL, "/") + "/api/client/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("client version API returned HTTP %d", resp.StatusCode)
	}
	var release ClientRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return err
	}
	if release.Version == "" || release.DownloadURL == "" || len(release.SHA256) != 64 {
		return errors.New("client version API returned an incomplete release")
	}
	newer, err := isNewerVersion(release.Version, ClientVersion)
	if err != nil {
		return err
	}
	if !newer {
		return nil
	}
	if err := c.stageSelfUpdate(ctx, release); err != nil {
		return err
	}
	return applySelfUpdate()
}

func isNewerVersion(candidate, current string) (bool, error) {
	parse := func(value string) ([]int, error) {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		parts := strings.Split(value, ".")
		if len(parts) < 2 || len(parts) > 4 {
			return nil, fmt.Errorf("invalid version %q", value)
		}
		result := make([]int, 4)
		for i, part := range parts {
			if part == "" {
				return nil, fmt.Errorf("invalid version %q", value)
			}
			for _, char := range part {
				if char < '0' || char > '9' {
					return nil, fmt.Errorf("invalid version %q", value)
				}
			}
			if _, err := fmt.Sscanf(part, "%d", &result[i]); err != nil {
				return nil, fmt.Errorf("invalid version %q", value)
			}
		}
		return result, nil
	}
	a, err := parse(candidate)
	if err != nil {
		return false, err
	}
	b, err := parse(current)
	if err != nil {
		return false, err
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i], nil
		}
	}
	return false, nil
}

func (c *SyncClient) stageSelfUpdate(ctx context.Context, release ClientRelease) error {
	target, err := url.Parse(release.DownloadURL)
	if err != nil {
		return err
	}
	base, err := url.Parse(strings.TrimRight(c.ServerURL, "/") + "/")
	if err != nil {
		return err
	}
	data, err := c.downloadBytes(ctx, base.ResolveReference(target).String(), "client update", maxClientBinaryBytes)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), release.SHA256) {
		return errors.New("client update SHA-256 does not match")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	stage := executable + ".s4mm-update"
	if err := os.WriteFile(stage, data, 0700); err != nil {
		return err
	}
	return nil
}
