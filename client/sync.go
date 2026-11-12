package main

import (
	"archive/zip"
	"bytes"
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
	"strconv"
	"strings"
	"time"
)

const maxArchiveBytes = 512 << 20
const maxExtractedBytes = 2 << 30
const defaultDownloadRetries = 3

// TransferProgress is emitted while a client download is in progress. Total is
// zero when the server did not provide a Content-Length.
type TransferProgress struct {
	Kind      string
	Completed int64
	Total     int64
}

type DesiredMod struct {
	ModID       int64  `json:"mod_id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Enabled     bool   `json:"enabled"`
	Desired     bool   `json:"desired"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
	FileName    string `json:"file_name"`
}
type inventoryReport struct {
	Files            []inventoryEntry        `json:"files"`
	CurseForgeAddons []curseForgeAddonReport `json:"curseforge_addons,omitempty"`
}
type inventoryEntry struct {
	Path   string `json:"path"`
	ModID  int64  `json:"mod_id"`
	SHA256 string `json:"sha256,omitempty"`
}
type auditEntry struct {
	Path    string `json:"path"`
	ModID   int64  `json:"mod_id,omitempty"`
	SHA256  string `json:"sha256"`
	Enabled bool   `json:"enabled"`
}
type localState struct {
	Mods map[string]installedState `json:"mods"`
}
type installedState struct {
	Version string   `json:"version"`
	SHA256  string   `json:"sha256"`
	Enabled bool     `json:"enabled"`
	Files   []string `json:"files"`
}

type SyncClient struct {
	ServerURL  string
	ClientID   string
	ModsDir    string
	ForceAudit bool
	// CurseForgeGameInstancePath is normally discovered beneath %APPDATA%; it
	// can be set by tests or an operator who uses a nonstandard app location.
	CurseForgeGameInstancePath string
	HTTPClient                 *http.Client
	// DownloadLimitBytesPerSecond is shared by archive and client update downloads.
	// Zero leaves transfers unlimited.
	DownloadLimitBytesPerSecond int64
	OnTransferProgress          func(TransferProgress)
}

func DetectModsDir() string {
	candidates := []string{}
	if explicit := os.Getenv("SIMS4_MODS_DIR"); explicit != "" {
		candidates = append(candidates, explicit)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "Documents", "Electronic Arts", "The Sims 4", "Mods"))
	}
	for _, envName := range []string{"OneDrive", "OneDriveConsumer", "OneDriveCommercial"} {
		if root := os.Getenv(envName); root != "" {
			candidates = append(candidates, filepath.Join(root, "Documents", "Electronic Arts", "The Sims 4", "Mods"))
		}
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func (c *SyncClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 0}
}
func (c *SyncClient) SyncOnce(ctx context.Context) error {
	if c.ServerURL == "" || c.ClientID == "" || c.ModsDir == "" {
		return errors.New("server URL, client ID, and Mods directory are required")
	}
	if err := os.MkdirAll(c.ModsDir, 0755); err != nil {
		return err
	}
	if err := ensureResourceCfg(c.ModsDir); err != nil {
		return err
	}
	state, err := readState(c.ModsDir)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.ServerURL, "/") + "/api/clients/" + url.PathEscape(c.ClientID) + "/desired"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return fmt.Errorf("desired state API returned HTTP %d", resp.StatusCode)
	}
	var desired []DesiredMod
	err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&desired)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var failures []string
	for _, mod := range desired {
		key := strconv.FormatInt(mod.ModID, 10)
		current, ok := state.Mods[key]
		if ok && !validManagedFiles(current.Files, mod.Name, mod.ModID) {
			failures = append(failures, mod.Name+": local state contains an unsafe managed path")
			continue
		}
		if !mod.Desired {
			if ok {
				removeInstalled(c.ModsDir, current)
				delete(state.Mods, key)
			}
			continue
		}
		if !ok || current.SHA256 != mod.SHA256 || current.Version != mod.Version {
			if !ok && mod.DownloadURL == "" {
				failures = append(failures, mod.Name+": no archive is available")
				continue
			}
			if mod.DownloadURL == "" || mod.SHA256 == "" {
				failures = append(failures, mod.Name+": archive URL or hash is missing")
				continue
			}
			archive, err := c.download(ctx, mod.DownloadURL, mod.SHA256)
			if err != nil {
				failures = append(failures, mod.Name+": "+err.Error())
				continue
			}
			files, err := installArchive(c.ModsDir, mod, archive)
			if err != nil {
				failures = append(failures, mod.Name+": "+err.Error())
				continue
			}
			if ok {
				prunePreviousFiles(c.ModsDir, current.Files, files)
			}
			current = installedState{Version: mod.Version, SHA256: mod.SHA256, Enabled: true, Files: files}
			state.Mods[key] = current
		}
		if current.Enabled != mod.Enabled {
			if err := setFilesEnabled(c.ModsDir, current.Files, current.Enabled, mod.Enabled); err != nil {
				failures = append(failures, mod.Name+": "+err.Error())
				continue
			}
			current.Enabled = mod.Enabled
			state.Mods[key] = current
		}
	}
	if err := writeState(c.ModsDir, state); err != nil {
		return err
	}
	if err := c.reportInventory(ctx, state); err != nil {
		failures = append(failures, "inventory report: "+err.Error())
	}
	if c.ForceAudit || needsInventoryAudit(c.ModsDir) {
		if err := c.auditInventory(ctx, state); err != nil {
			failures = append(failures, "inventory audit: "+err.Error())
		} else {
			c.ForceAudit = false
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return c.acknowledgeSync(ctx)
}

func needsInventoryAudit(modsDir string) bool {
	_, err := os.Stat(auditMarkerPath(modsDir))
	return os.IsNotExist(err)
}

func auditMarkerPath(modsDir string) string {
	return filepath.Join(modsDir, ".sims4-mod-manager-audit.json")
}

func (c *SyncClient) auditInventory(ctx context.Context, state localState) error {
	files, err := scanModsDirectory(c.ModsDir, state)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Files            []auditEntry            `json:"files"`
		CurseForgeAddons []curseForgeAddonReport `json:"curseforge_addons,omitempty"`
	}{Files: files, CurseForgeAddons: c.curseForgeAddons()})
	if err != nil {
		return err
	}
	target := strings.TrimRight(c.ServerURL, "/") + "/api/clients/" + url.PathEscape(c.ClientID) + "/audit"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("inventory audit API returned HTTP %d", resp.StatusCode)
	}
	marker, err := os.CreateTemp(c.ModsDir, ".s4mm-audit-*.tmp")
	if err != nil {
		return err
	}
	markerName := marker.Name()
	defer os.Remove(markerName)
	if _, err := marker.WriteString("{\"audited\":true}\n"); err != nil {
		marker.Close()
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}
	return os.Rename(markerName, auditMarkerPath(c.ModsDir))
}

func scanModsDirectory(modsDir string, state localState) ([]auditEntry, error) {
	managed := map[string]int64{}
	for key, mod := range state.Mods {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			continue
		}
		for _, name := range mod.Files {
			canonical := strings.TrimSuffix(strings.ToLower(filepath.ToSlash(name)), ".disabled")
			managed[canonical] = id
		}
	}
	files := []auditEntry{}
	err := filepath.WalkDir(modsDir, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(modsDir, filePath)
		if err != nil {
			return err
		}
		portable := filepath.ToSlash(relative)
		actualName := portable
		enabled := true
		if strings.EqualFold(filepath.Ext(actualName), ".disabled") {
			actualName = actualName[:len(actualName)-len(".disabled")]
			enabled = false
		}
		ext := strings.ToLower(filepath.Ext(actualName))
		if ext != ".package" && ext != ".ts4script" {
			return nil
		}
		if len(files) >= 20000 {
			return fmt.Errorf("Mods folder contains more than 20000 package/script files")
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(hasher, io.LimitReader(file, maxExtractedBytes+1))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written > maxExtractedBytes {
			return fmt.Errorf("inventory file exceeds 2 GiB: %s", portable)
		}
		canonical := strings.ToLower(actualName)
		files = append(files, auditEntry{Path: portable, ModID: managed[canonical], SHA256: hex.EncodeToString(hasher.Sum(nil)), Enabled: enabled})
		return nil
	})
	return files, err
}

func (c *SyncClient) acknowledgeSync(ctx context.Context) error {
	target := strings.TrimRight(c.ServerURL, "/") + "/api/clients/" + url.PathEscape(c.ClientID) + "/sync/ack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sync acknowledgement API returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func validManagedFiles(files []string, name string, id int64) bool {
	prefix := modFolderName(name, id) + string(filepath.Separator)
	for _, file := range files {
		portable := strings.ReplaceAll(file, "\\", "/")
		clean := filepath.Clean(filepath.FromSlash(portable))
		if filepath.IsAbs(clean) || strings.Contains(clean, ":") || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || !strings.HasPrefix(clean, prefix) {
			return false
		}
	}
	return len(files) > 0
}

func (c *SyncClient) download(ctx context.Context, downloadURL, expectedHash string) ([]byte, error) {
	target, err := url.Parse(downloadURL)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(strings.TrimRight(c.ServerURL, "/") + "/")
	if err != nil {
		return nil, err
	}
	target = base.ResolveReference(target)
	data, err := c.downloadBytes(ctx, target.String(), "archive", maxArchiveBytes)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), expectedHash) {
		return nil, errors.New("archive SHA-256 does not match")
	}
	return data, nil
}

func (c *SyncClient) downloadBytes(ctx context.Context, target, kind string, limit int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < defaultDownloadRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 400 * time.Millisecond
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		data, retry, err := c.downloadAttempt(ctx, target, kind, limit)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("download failed after %d attempts: %w", defaultDownloadRetries, lastErr)
}

func (c *SyncClient) downloadAttempt(ctx context.Context, target, kind string, limit int64) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retry, fmt.Errorf("%s API returned HTTP %d", kind, resp.StatusCode)
	}
	reader := io.Reader(resp.Body)
	if c.DownloadLimitBytesPerSecond > 0 {
		reader = &limitedReader{source: resp.Body, bytesPerSecond: c.DownloadLimitBytesPerSecond, started: time.Now()}
	}
	reader = &progressReader{source: reader, kind: kind, total: resp.ContentLength, report: c.OnTransferProgress}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, ctx.Err() == nil, err
	}
	if int64(len(data)) > limit {
		return nil, false, fmt.Errorf("%s exceeds %d bytes", kind, limit)
	}
	return data, false, nil
}

type progressReader struct {
	source io.Reader
	kind   string
	total  int64
	read   int64
	report func(TransferProgress)
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	r.read += int64(n)
	if n > 0 && r.report != nil {
		r.report(TransferProgress{Kind: r.kind, Completed: r.read, Total: r.total})
	}
	return n, err
}

type limitedReader struct {
	source         io.Reader
	bytesPerSecond int64
	started        time.Time
	read           int64
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if r.bytesPerSecond <= 0 {
		return r.source.Read(p)
	}
	maxChunk := int(r.bytesPerSecond / 4)
	if maxChunk < 1 {
		maxChunk = 1
	}
	if len(p) > maxChunk {
		p = p[:maxChunk]
	}
	n, err := r.source.Read(p)
	r.read += int64(n)
	if n > 0 {
		wantElapsed := time.Duration(float64(r.read) / float64(r.bytesPerSecond) * float64(time.Second))
		if wait := time.Until(r.started.Add(wantElapsed)); wait > 0 {
			time.Sleep(wait)
		}
	}
	return n, err
}

func installArchive(modsDir string, mod DesiredMod, data []byte) ([]string, error) {
	root := filepath.Join(modsDir, modFolderName(mod.Name, mod.ModID))
	stage, err := os.MkdirTemp(modsDir, ".s4mm-stage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	ext := strings.ToLower(filepath.Ext(mod.FileName))
	stagedFiles := []string{}
	if ext == ".package" || ext == ".ts4script" {
		name := filepath.Base(mod.FileName)
		if name == "." || name == string(filepath.Separator) || strings.Contains(name, ":") {
			return nil, errors.New("invalid archive filename")
		}
		destination := filepath.Join(stage, name)
		if err := os.WriteFile(destination, data, 0644); err != nil {
			return nil, err
		}
		stagedFiles = append(stagedFiles, name)
	} else {
		reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("only valid ZIP archives can be extracted: %w", err)
		}
		var extracted int64
		seen := map[string]bool{}
		for _, item := range reader.File {
			if item.FileInfo().IsDir() {
				continue
			}
			portableName := strings.ReplaceAll(item.Name, "\\", "/")
			clean := filepath.Clean(filepath.FromSlash(portableName))
			if filepath.IsAbs(clean) || strings.Contains(clean, ":") || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("unsafe ZIP path %q", item.Name)
			}
			fileExt := strings.ToLower(filepath.Ext(clean))
			if fileExt != ".package" && fileExt != ".ts4script" {
				continue
			}
			relative := clean
			if fileExt == ".ts4script" {
				relative = filepath.Base(clean)
			} else {
				dir := filepath.Dir(clean)
				depth := 0
				if dir != "." {
					depth = strings.Count(dir, string(filepath.Separator)) + 1
				}
				if depth+1 > 5 {
					relative = filepath.Base(clean)
				}
			}
			key := strings.ToLower(relative)
			if seen[key] {
				return nil, fmt.Errorf("archive contains duplicate target path %q", relative)
			}
			seen[key] = true
			destination := filepath.Join(stage, relative)
			if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
				return nil, err
			}
			source, err := item.Open()
			if err != nil {
				return nil, err
			}
			out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				source.Close()
				return nil, err
			}
			written, copyErr := io.Copy(out, io.LimitReader(source, maxArchiveBytes+1))
			closeSourceErr := source.Close()
			closeOutErr := out.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeSourceErr != nil {
				return nil, closeSourceErr
			}
			if closeOutErr != nil {
				return nil, closeOutErr
			}
			if written > maxArchiveBytes {
				return nil, errors.New("archive entry exceeds 512 MiB")
			}
			extracted += written
			if extracted > maxExtractedBytes {
				return nil, errors.New("archive expands beyond 2 GiB")
			}
			stagedFiles = append(stagedFiles, relative)
		}
		if len(stagedFiles) == 0 {
			return nil, errors.New("archive contains no .package or .ts4script files")
		}
	}
	backup := ""
	if _, statErr := os.Lstat(root); statErr == nil {
		var err error
		backup, err = os.MkdirTemp(modsDir, ".s4mm-backup-")
		if err != nil {
			return nil, err
		}
		if err := os.Remove(backup); err != nil {
			return nil, err
		}
		if err := os.Rename(root, backup); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	if err := os.Rename(stage, root); err != nil {
		if backup != "" {
			_ = os.Rename(backup, root)
		}
		return nil, err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	files := make([]string, 0, len(stagedFiles))
	for _, name := range stagedFiles {
		files = append(files, filepath.Join(modFolderName(mod.Name, mod.ModID), name))
	}
	return files, nil
}

func setFilesEnabled(modsDir string, files []string, wasEnabled, enabled bool) error {
	if wasEnabled == enabled {
		return nil
	}
	for _, name := range files {
		current := filepath.Join(modsDir, name)
		from, to := current, current+".disabled"
		if enabled {
			from, to = current+".disabled", current
		}
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func removeInstalled(modsDir string, state installedState) {
	for _, name := range state.Files {
		file := filepath.Join(modsDir, name)
		_ = os.Remove(file)
		if !strings.HasSuffix(strings.ToLower(file), ".disabled") {
			_ = os.Remove(file + ".disabled")
		}
	}
}
func prunePreviousFiles(modsDir string, oldFiles, newFiles []string) {
	keep := map[string]bool{}
	for _, name := range newFiles {
		keep[name] = true
	}
	for _, name := range oldFiles {
		if keep[name] {
			_ = os.Remove(filepath.Join(modsDir, name) + ".disabled")
			continue
		}
		_ = os.Remove(filepath.Join(modsDir, name))
		_ = os.Remove(filepath.Join(modsDir, name) + ".disabled")
	}
}
func modFolderName(name string, id int64) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune('-')
		}
	}
	clean := strings.Trim(b.String(), "-_")
	if clean == "" {
		clean = "Mod"
	}
	return fmt.Sprintf("%s-%d", clean, id)
}
func statePath(modsDir string) string { return filepath.Join(modsDir, ".sims4-mod-manager.json") }
func readState(modsDir string) (localState, error) {
	state := localState{Mods: map[string]installedState{}}
	data, err := os.ReadFile(statePath(modsDir))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Mods == nil {
		state.Mods = map[string]installedState{}
	}
	return state, nil
}
func writeState(modsDir string, state localState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(modsDir), data, 0600)
}
func ensureResourceCfg(modsDir string) error {
	file := filepath.Join(modsDir, "Resource.cfg")
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	cfg := "Priority 500\nPackedFile *.package\nPackedFile *\\*.package\nPackedFile *\\*\\*.package\nPackedFile *\\*\\*\\*.package\nPackedFile *\\*\\*\\*\\*.package\nPackedFile *\\*\\*\\*\\*\\*.package\nPackedFile *.ts4script\nPackedFile *\\*.ts4script\n"
	return os.WriteFile(file, []byte(cfg), 0644)
}

func (c *SyncClient) reportInventory(ctx context.Context, state localState) error {
	entries := []inventoryEntry{}
	for key, mod := range state.Mods {
		if !mod.Enabled {
			continue
		}
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			continue
		}
		for _, name := range mod.Files {
			if strings.HasSuffix(strings.ToLower(name), ".disabled") {
				continue
			}
			ext := strings.ToLower(filepath.Ext(name))
			if ext == ".package" || ext == ".ts4script" {
				entries = append(entries, inventoryEntry{Path: filepath.ToSlash(name), ModID: id, SHA256: mod.SHA256})
			}
		}
	}
	data, err := json.Marshal(inventoryReport{Files: entries, CurseForgeAddons: c.curseForgeAddons()})
	if err != nil {
		return err
	}
	target := strings.TrimRight(c.ServerURL, "/") + "/api/clients/" + url.PathEscape(c.ClientID) + "/inventory"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("inventory API returned HTTP %d", resp.StatusCode)
	}
	return nil
}
