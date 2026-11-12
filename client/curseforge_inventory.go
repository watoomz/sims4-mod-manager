package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const sims4CurseForgeGameTypeID = 78062
const maxCurseForgeGameInstanceBytes = 20 << 20

type curseForgeAddonReport struct {
	AddonID          int64  `json:"addonID"`
	Name             string `json:"name"`
	FileID           int64  `json:"fileId"`
	FileName         string `json:"fileName"`
	DownloadURL      string `json:"downloadUrl"`
	Fingerprint      uint32 `json:"fingerprint"`
	ModFolder        string `json:"modFolder"`
	ModuleFolderName string `json:"moduleFolderName,omitempty"`
}

type curseForgeGameInstance struct {
	GameTypeID      int                        `json:"gameTypeID"`
	InstalledAddons []curseForgeInstalledAddon `json:"installedAddons"`
}

type curseForgeInstalledAddon struct {
	AddonID       int64  `json:"addonID"`
	Name          string `json:"name"`
	ModFolderPath string `json:"modFolderPath"`
	InstalledFile struct {
		ID          int64  `json:"id"`
		FileName    string `json:"fileName"`
		DownloadURL string `json:"downloadUrl"`
		Modules     []struct {
			FolderName  string `json:"foldername"`
			Fingerprint uint32 `json:"fingerprint"`
		} `json:"modules"`
	} `json:"installedFile"`
}

func curseForgeGameInstancePath() string {
	if path := strings.TrimSpace(os.Getenv("CURSEFORGE_ADDON_GAME_INSTANCE_PATH")); path != "" {
		return path
	}
	root := strings.TrimSpace(os.Getenv("APPDATA"))
	if root == "" {
		root, _ = os.UserConfigDir()
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, "CurseForge", "agent", "GameInstances", "AddonGameInstance.json")
}

func (c *SyncClient) curseForgeAddons() []curseForgeAddonReport {
	path := c.CurseForgeGameInstancePath
	if path == "" {
		path = curseForgeGameInstancePath()
	}
	return readCurseForgeInstalledAddons(path, c.ModsDir)
}

func readCurseForgeInstalledAddons(path, modsDir string) []curseForgeAddonReport {
	if path == "" || modsDir == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxCurseForgeGameInstanceBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var games []curseForgeGameInstance
	if json.Unmarshal(data, &games) != nil {
		return nil
	}
	root, err := filepath.Abs(modsDir)
	if err != nil {
		return nil
	}
	root = filepath.Clean(root)
	result := []curseForgeAddonReport{}
	seen := map[string]bool{}
	for _, game := range games {
		if game.GameTypeID != sims4CurseForgeGameTypeID {
			continue
		}
		for _, addon := range game.InstalledAddons {
			folder, ok := relativeCurseForgeModFolder(root, addon.ModFolderPath)
			if !ok || addon.AddonID < 1 || addon.InstalledFile.ID < 1 || len(addon.InstalledFile.Modules) == 0 {
				continue
			}
			for _, module := range addon.InstalledFile.Modules {
				if module.Fingerprint == 0 {
					continue
				}
				entry := curseForgeAddonReport{
					AddonID: addon.AddonID, Name: strings.TrimSpace(addon.Name),
					FileID: addon.InstalledFile.ID, FileName: filepath.Base(addon.InstalledFile.FileName),
					DownloadURL: strings.TrimSpace(addon.InstalledFile.DownloadURL), Fingerprint: module.Fingerprint,
					ModFolder: folder, ModuleFolderName: filepath.Base(strings.TrimSpace(module.FolderName)),
				}
				key := strings.Join([]string{filepath.ToSlash(entry.ModFolder), stringInt(entry.AddonID), stringInt(entry.FileID), stringInt(int64(entry.Fingerprint)), strings.ToLower(entry.ModuleFolderName)}, "|")
				if seen[key] {
					continue
				}
				seen[key] = true
				result = append(result, entry)
			}
		}
	}
	return result
}

func relativeCurseForgeModFolder(root, candidate string) (string, bool) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || !filepath.IsAbs(candidate) {
		return "", false
	}
	absolute, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", false
	}
	relative := ""
	if runtime.GOOS == "windows" {
		prefix := strings.TrimRight(root, `\\/`)
		if strings.EqualFold(prefix, absolute) {
			return "", false
		}
		prefix += string(filepath.Separator)
		if len(absolute) <= len(prefix) || !strings.EqualFold(absolute[:len(prefix)], prefix) {
			return "", false
		}
		relative = absolute[len(prefix):]
	} else {
		rel, relErr := filepath.Rel(root, absolute)
		if relErr != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false
		}
		relative = rel
	}
	if relative == "." {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func stringInt(value int64) string {
	return strconv.FormatInt(value, 10)
}
