package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestReadCurseForgeInstalledAddonsFiltersGameAndModsFolder(t *testing.T) {
	modsDir := t.TempDir()
	installedFolder := filepath.Join(modsDir, "WickedMods", "Example")
	if err := os.MkdirAll(installedFolder, 0755); err != nil {
		t.Fatal(err)
	}
	outsideFolder := t.TempDir()
	data := `[
{"gameTypeID":432,"installedAddons":[{"addonID":999,"name":"Wrong game","modFolderPath":"` + filepath.ToSlash(installedFolder) + `"}]},
{"gameTypeID":78062,"installedAddons":[
 {"addonID":700,"name":"Local CurseForge Mod","modFolderPath":"` + filepath.ToSlash(installedFolder) + `","installedFile":{"id":800,"fileName":"example.zip","downloadUrl":"https://edge.forgecdn.net/files/example.zip","modules":[{"foldername":"Example.package","fingerprint":123456}]}},
 {"addonID":701,"name":"Outside Mods","modFolderPath":"` + filepath.ToSlash(outsideFolder) + `","installedFile":{"id":801,"fileName":"outside.zip","modules":[{"foldername":"Outside.package","fingerprint":222}]}},
 {"addonID":702,"name":"No modules","modFolderPath":"` + filepath.ToSlash(installedFolder) + `","installedFile":{"id":802,"fileName":"empty.zip","modules":[]}}
]}]`
	file := filepath.Join(t.TempDir(), "AddonGameInstance.json")
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	addons := readCurseForgeInstalledAddons(file, modsDir)
	if len(addons) != 1 {
		t.Fatalf("unexpected reported addons: %#v", addons)
	}
	got := addons[0]
	if got.AddonID != 700 || got.Name != "Local CurseForge Mod" || got.FileID != 800 || got.FileName != "example.zip" || got.Fingerprint != 123456 || got.ModFolder != "WickedMods/Example" || got.ModuleFolderName != "Example.package" {
		t.Fatalf("unexpected addon metadata: %#v", got)
	}
}

func TestReadCurseForgeInstalledAddonsSkipsMissingMalformedAndOversizedFiles(t *testing.T) {
	modsDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing.json")
	if got := readCurseForgeInstalledAddons(missing, modsDir); len(got) != 0 {
		t.Fatalf("missing file should be ignored: %#v", got)
	}
	malformed := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(malformed, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readCurseForgeInstalledAddons(malformed, modsDir); len(got) != 0 {
		t.Fatalf("malformed file should be ignored: %#v", got)
	}
	tooLarge := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(tooLarge, make([]byte, maxCurseForgeGameInstanceBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readCurseForgeInstalledAddons(tooLarge, modsDir); len(got) != 0 {
		t.Fatalf("oversized file should be ignored: %#v", got)
	}
}

func TestCurseForgeGameInstancePathUsesAppData(t *testing.T) {
	appData := t.TempDir()
	t.Setenv("CURSEFORGE_ADDON_GAME_INSTANCE_PATH", "")
	t.Setenv("APPDATA", appData)
	got := curseForgeGameInstancePath()
	want := filepath.Join(appData, "CurseForge", "agent", "GameInstances", "AddonGameInstance.json")
	if got != want {
		t.Fatalf("game instance path=%q want %q", got, want)
	}
}

func TestInventoryReportIncludesReadOnlyCurseForgeAddonMetadata(t *testing.T) {
	modsDir := t.TempDir()
	modFolder := filepath.Join(modsDir, "Packages", "Installed By CurseForge")
	if err := os.MkdirAll(modFolder, 0755); err != nil {
		t.Fatal(err)
	}
	gameFile := filepath.Join(t.TempDir(), "AddonGameInstance.json")
	data := `[{"gameTypeID":78062,"installedAddons":[{"addonID":42,"name":"Inventory Mod","modFolderPath":"` + filepath.ToSlash(modFolder) + `","installedFile":{"id":43,"fileName":"inventory.zip","downloadUrl":"https://edge.forgecdn.net/files/inventory.zip","modules":[{"foldername":"inventory.package","fingerprint":44}]}}]}]`
	if err := os.WriteFile(gameFile, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var report inventoryReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/clients/pc/inventory" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Errorf("decode inventory report: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &SyncClient{ServerURL: server.URL, ClientID: "pc", ModsDir: modsDir, HTTPClient: server.Client(), CurseForgeGameInstancePath: gameFile}
	if err := client.reportInventory(context.Background(), localState{}); err != nil {
		t.Fatal(err)
	}
	if len(report.CurseForgeAddons) != 1 || report.CurseForgeAddons[0].AddonID != 42 || report.CurseForgeAddons[0].Fingerprint != 44 || report.CurseForgeAddons[0].ModFolder != "Packages/Installed By CurseForge" {
		t.Fatalf("inventory omitted local CurseForge metadata: %#v", report)
	}
}
