package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDownloadRetriesTransientFailureAndReportsProgress(t *testing.T) {
	var requests atomic.Int32
	var lastProgress TransferProgress
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("hello world"))
	}))
	defer server.Close()
	client := &SyncClient{HTTPClient: server.Client(), OnTransferProgress: func(p TransferProgress) { lastProgress = p }}
	data, err := client.downloadBytes(context.Background(), server.URL, "archive", 100)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" || requests.Load() != 2 {
		t.Fatalf("data=%q requests=%d", data, requests.Load())
	}
	if lastProgress.Completed != 11 || lastProgress.Total != 11 || lastProgress.Kind != "archive" {
		t.Fatalf("unexpected progress: %#v", lastProgress)
	}
}

func TestDownloadLimitAndVersionComparison(t *testing.T) {
	payload := []byte("rate limited")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer server.Close()
	client := &SyncClient{HTTPClient: server.Client(), DownloadLimitBytesPerSecond: 100}
	started := time.Now()
	if _, err := client.downloadBytes(context.Background(), server.URL, "archive", 100); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("transfer was not paced: %v", elapsed)
	}
	if newer, err := isNewerVersion("1.10.0", "1.9.9"); err != nil || !newer {
		t.Fatalf("version comparison: newer=%t err=%v", newer, err)
	}
	if newer, err := isNewerVersion("v1.2.0", "1.2.0"); err != nil || newer {
		t.Fatalf("equal version comparison: newer=%t err=%v", newer, err)
	}
}

func TestStageSelfUpdateVerifiesAndWritesBinary(t *testing.T) {
	binary := []byte("new windows executable")
	sum := sha256.Sum256(binary)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(binary) }))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stage := executable + ".s4mm-update"
	defer os.Remove(stage)
	client := &SyncClient{ServerURL: server.URL, HTTPClient: server.Client()}
	if err := client.stageSelfUpdate(context.Background(), ClientRelease{DownloadURL: "/client.exe", SHA256: digest}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(stage)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binary) {
		t.Fatalf("staged data=%q", got)
	}
	if err := client.stageSelfUpdate(context.Background(), ClientRelease{DownloadURL: "/client.exe", SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("expected update with a mismatched hash to be rejected")
	}
}

func TestSyncInstallDisableUninstallAndInventory(t *testing.T) {
	archive := makeZip(t, map[string]string{"Tuning.package": "package", "Scripts/Animations.ts4script": "script", "ignore.txt": "ignore"})
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	modsDir := t.TempDir()
	var state = DesiredMod{ModID: 7, Name: "Example Mod", Version: "1.0", Enabled: true, Desired: true, DownloadURL: "/api/archives/" + digest, SHA256: digest, FileName: "example.zip"}
	var downloads, inventoryCalls, auditCalls atomic.Int32
	lastInventory := inventoryReport{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/clients/client-1/desired":
			_ = json.NewEncoder(w).Encode([]DesiredMod{state})
		case r.Method == http.MethodGet && r.URL.Path == "/api/archives/"+digest:
			downloads.Add(1)
			_, _ = w.Write(archive)
		case r.Method == http.MethodPost && r.URL.Path == "/api/clients/client-1/inventory":
			inventoryCalls.Add(1)
			if err := json.NewDecoder(r.Body).Decode(&lastInventory); err != nil {
				t.Error(err)
			}
			w.WriteHeader(200)
		case r.Method == http.MethodPost && r.URL.Path == "/api/clients/client-1/audit":
			auditCalls.Add(1)
			w.WriteHeader(200)
		case r.Method == http.MethodPost && r.URL.Path == "/api/clients/client-1/sync/ack":
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &SyncClient{ServerURL: server.URL, ClientID: "client-1", ModsDir: modsDir, HTTPClient: server.Client()}
	if err := client.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(modsDir, modFolderName(state.Name, state.ModID))
	script := filepath.Join(root, "Animations.ts4script")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("script file not installed at one-folder depth: %v", err)
	}
	packageFile := filepath.Join(root, "Tuning.package")
	if _, err := os.Stat(packageFile); err != nil {
		t.Fatal(err)
	}
	if len(lastInventory.Files) != 2 {
		t.Fatalf("expected two managed files in inventory, got %#v", lastInventory.Files)
	}
	state.Enabled = false
	if err := client.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script + ".disabled"); err != nil {
		t.Fatalf("disabled script not renamed: %v", err)
	}
	if len(lastInventory.Files) != 0 {
		t.Fatalf("disabled files reported as active: %#v", lastInventory.Files)
	}
	state.Desired = false
	if err := client.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script + ".disabled"); !os.IsNotExist(err) {
		t.Fatalf("uninstalled script remains: %v", err)
	}
	if downloads.Load() != 1 || inventoryCalls.Load() != 3 || auditCalls.Load() != 1 {
		t.Fatalf("unexpected requests: downloads=%d inventory=%d audit=%d", downloads.Load(), inventoryCalls.Load(), auditCalls.Load())
	}
}

func TestArchiveHashAndTraversalProtection(t *testing.T) {
	modsDir := t.TempDir()
	root := filepath.Join(modsDir, modFolderName("Unsafe", 3))
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.package"), []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}
	archive := makeZip(t, map[string]string{"..\\..\\outside.package": "no"})
	mod := DesiredMod{ModID: 3, Name: "Unsafe", FileName: "unsafe.zip"}
	if _, err := installArchive(modsDir, mod, archive); err == nil {
		t.Fatal("expected traversal archive to be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(modsDir), "outside.package")); !os.IsNotExist(err) {
		t.Fatalf("archive wrote outside Mods: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "keep.package")); err != nil || string(data) != "existing" {
		t.Fatalf("failed extraction disturbed prior install: data=%q err=%v", data, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("wrong hash")) }))
	defer server.Close()
	client := &SyncClient{ServerURL: server.URL, HTTPClient: server.Client()}
	if _, err := client.download(context.Background(), "/archive", strings.Repeat("0", 64)); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

func TestArchiveRejectsDuplicateFlattenedTargets(t *testing.T) {
	modsDir := t.TempDir()
	archive := makeZip(t, map[string]string{"one/Script.ts4script": "one", "two/Script.ts4script": "two"})
	if _, err := installArchive(modsDir, DesiredMod{ModID: 9, Name: "Scripts", FileName: "scripts.zip"}, archive); err == nil {
		t.Fatal("expected duplicate flattened script paths to be rejected")
	}
	if _, err := os.Stat(filepath.Join(modsDir, modFolderName("Scripts", 9))); !os.IsNotExist(err) {
		t.Fatalf("failed extraction left partial install: %v", err)
	}
}

func TestManagedPathsStayInsideTheirModFolder(t *testing.T) {
	if !validManagedFiles([]string{"Example-Mod-4/Thing.package"}, "Example Mod", 4) {
		t.Fatal("expected generated mod path to be accepted")
	}
	for _, path := range []string{"../../outside.package", "Other-Mod-5/Thing.package", "C:\\Windows\\system.ini"} {
		if validManagedFiles([]string{path}, "Example Mod", 4) {
			t.Errorf("expected unsafe or foreign path %q to be rejected", path)
		}
	}
}

func TestScanModsDirectoryHashesManagedAndUnmanagedDisabledFiles(t *testing.T) {
	modsDir := t.TempDir()
	managedPath := filepath.Join(modsDir, "Manager", "Managed.package")
	unknownPath := filepath.Join(modsDir, "Old", "Unmanaged.ts4script.disabled")
	if err := os.MkdirAll(filepath.Dir(managedPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(unknownPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedPath, []byte("known package bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknownPath, []byte("unknown script bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	state := localState{Mods: map[string]installedState{"7": {Files: []string{"Manager/Managed.package"}}}}
	files, err := scanModsDirectory(modsDir, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected two package/script files, got %#v", files)
	}
	byPath := map[string]auditEntry{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	managed := byPath["Manager/Managed.package"]
	unknown := byPath["Old/Unmanaged.ts4script.disabled"]
	if managed.ModID != 7 || !managed.Enabled || len(managed.SHA256) != 64 {
		t.Fatalf("managed file not identified and hashed: %+v", managed)
	}
	if unknown.ModID != 0 || unknown.Enabled || len(unknown.SHA256) != 64 {
		t.Fatalf("unmanaged disabled file not retained and hashed: %+v", unknown)
	}
}
