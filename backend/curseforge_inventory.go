package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const maxReportedCurseForgeAddons = 20000

type curseForgeAddonInventory struct {
	AddonID          int64  `json:"addonID"`
	Name             string `json:"name"`
	FileID           int64  `json:"fileId"`
	FileName         string `json:"fileName"`
	DownloadURL      string `json:"downloadUrl"`
	Fingerprint      uint32 `json:"fingerprint"`
	ModFolder        string `json:"modFolder"`
	ModuleFolderName string `json:"moduleFolderName"`
}

type matchedCurseForgeFolder struct {
	Folder string
	ModID  int64
}

type curseForgeFingerprintResponse struct {
	Data struct {
		ExactMatches []struct {
			ID          int64  `json:"id"`
			Fingerprint uint32 `json:"fingerprint"`
			File        struct {
				ID              int64  `json:"id"`
				FileFingerprint uint32 `json:"fileFingerprint"`
			} `json:"file"`
		} `json:"exactMatches"`
	} `json:"data"`
}

func (a *App) crossReferenceCurseForgeAddons(ctx context.Context, clientID string, input []curseForgeAddonInventory) ([]matchedCurseForgeFolder, error) {
	if len(input) == 0 {
		return nil, nil
	}
	if len(input) > maxReportedCurseForgeAddons {
		return nil, nil
	}
	addons := make([]curseForgeAddonInventory, 0, len(input))
	seen := map[string]bool{}
	for _, item := range input {
		if item.AddonID < 1 || item.FileID < 1 || item.Fingerprint == 0 {
			continue
		}
		item.Name = strings.TrimSpace(item.Name)
		if item.Name == "" {
			continue
		}
		if len(item.Name) > 500 {
			item.Name = item.Name[:500]
		}
		item.FileName = path.Base(strings.ReplaceAll(strings.TrimSpace(item.FileName), "\\", "/"))
		if item.FileName == "." || item.FileName == "/" || len(item.FileName) > 255 {
			continue
		}
		item.ModFolder = canonicalInventoryPath(item.ModFolder)
		if item.ModFolder == "." || item.ModFolder == ".." || strings.HasPrefix(item.ModFolder, "../") || strings.HasPrefix(item.ModFolder, "/") || strings.Contains(item.ModFolder, ":") {
			continue
		}
		item.ModuleFolderName = path.Base(strings.TrimSpace(item.ModuleFolderName))
		if item.ModuleFolderName == "." || item.ModuleFolderName == "/" {
			item.ModuleFolderName = ""
		}
		parsed, err := url.Parse(strings.TrimSpace(item.DownloadURL))
		if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() != "edge.forgecdn.net" {
			item.DownloadURL = ""
		}
		key := strings.Join([]string{strconv.FormatInt(item.AddonID, 10), strconv.FormatInt(item.FileID, 10), strconv.FormatUint(uint64(item.Fingerprint), 10), item.ModuleFolderName}, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		addons = append(addons, item)
	}
	if len(addons) == 0 {
		return nil, nil
	}

	fingerprintsToCheck := []uint32{}
	if a.api != nil && a.api.Configured() {
		unique := map[uint32]bool{}
		for _, item := range addons {
			if unique[item.Fingerprint] {
				continue
			}
			unique[item.Fingerprint] = true
			var checked sql.NullString
			err := a.db.QueryRowContext(ctx, `SELECT max(fingerprint_checked_at) FROM client_curseforge_addons WHERE fingerprint=?`, int64(item.Fingerprint)).Scan(&checked)
			if err != nil {
				return nil, err
			}
			if !checked.Valid || checked.String == "" {
				fingerprintsToCheck = append(fingerprintsToCheck, item.Fingerprint)
				continue
			}
			checkedAt, parseErr := time.Parse("2006-01-02 15:04:05", checked.String)
			if parseErr != nil || time.Since(checkedAt) >= 24*time.Hour {
				fingerprintsToCheck = append(fingerprintsToCheck, item.Fingerprint)
			}
		}
	}

	matchedIDs := map[uint32]int64{}
	matchedFingerprints := map[uint32]bool{}
	checkedAt := ""
	if len(fingerprintsToCheck) > 0 {
		checkedAt = time.Now().UTC().Format("2006-01-02 15:04:05")
		raw, err := a.api.FingerprintMatches(ctx, fingerprintsToCheck)
		if err == nil {
			var matches curseForgeFingerprintResponse
			if json.Unmarshal(raw, &matches) == nil {
				for _, match := range matches.Data.ExactMatches {
					fingerprint := match.Fingerprint
					if fingerprint == 0 {
						fingerprint = match.File.FileFingerprint
					}
					if fingerprint != 0 && match.ID > 0 {
						matchedIDs[fingerprint] = match.ID
						matchedFingerprints[fingerprint] = true
					}
				}
			}
		}
	}

	foldersByKey := map[string]int64{}
	folders := []matchedCurseForgeFolder{}
	for _, item := range addons {
		modID := item.AddonID
		if exactID := matchedIDs[item.Fingerprint]; exactID > 0 {
			// The local CurseForge app already identifies the project. Use the
			// fingerprint response only when it corroborates that same project.
			if exactID == item.AddonID {
				matchedFingerprints[item.Fingerprint] = true
			}
		}
		if _, err := a.db.ExecContext(ctx, `INSERT INTO mods(external_id,name,creator,description,source,url,last_seen,thumbnail_url,source_download_url,current_version,previous_version) VALUES(?,?, '', '', 'CurseForge', '',CURRENT_TIMESTAMP,'','','','') ON CONFLICT(source,external_id) DO UPDATE SET name=CASE WHEN mods.name='' THEN excluded.name ELSE mods.name END,last_seen=CURRENT_TIMESTAMP,catalog_active=1`, strconv.FormatInt(modID, 10), item.Name); err != nil {
			return nil, err
		}
		var catalogID int64
		if err := a.db.QueryRowContext(ctx, `SELECT id FROM mods WHERE source='CurseForge' AND external_id=?`, strconv.FormatInt(modID, 10)).Scan(&catalogID); err != nil {
			return nil, err
		}
		var checked any
		if checkedAt != "" {
			checked = checkedAt
		}
		matched := 0
		if matchedFingerprints[item.Fingerprint] && matchedIDs[item.Fingerprint] == item.AddonID {
			matched = 1
		}
		if _, err := a.db.ExecContext(ctx, `INSERT INTO client_curseforge_addons(client_id,addon_id,name,file_id,file_name,download_url,fingerprint,mod_folder,module_folder_name,mod_id,fingerprint_matched,fingerprint_checked_at,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(client_id,addon_id,file_id,fingerprint,module_folder_name) DO UPDATE SET name=excluded.name,file_name=excluded.file_name,download_url=excluded.download_url,mod_folder=excluded.mod_folder,mod_id=excluded.mod_id,fingerprint_matched=max(client_curseforge_addons.fingerprint_matched,excluded.fingerprint_matched),fingerprint_checked_at=COALESCE(excluded.fingerprint_checked_at,client_curseforge_addons.fingerprint_checked_at),observed_at=CURRENT_TIMESTAMP`, clientID, item.AddonID, item.Name, item.FileID, item.FileName, item.DownloadURL, int64(item.Fingerprint), item.ModFolder, item.ModuleFolderName, catalogID, matched, checked); err != nil {
			return nil, err
		}
		key := strings.ToLower(item.ModFolder)
		if _, exists := foldersByKey[key]; !exists {
			foldersByKey[key] = catalogID
			folders = append(folders, matchedCurseForgeFolder{Folder: item.ModFolder, ModID: catalogID})
		}
		_, _ = a.db.ExecContext(ctx, `UPDATE client_inventory SET mod_id=? WHERE client_id=? AND mod_id IS NULL AND (lower(path)=lower(?) OR lower(path) LIKE lower(?))`, catalogID, clientID, item.ModFolder, item.ModFolder+"/%")
	}
	return folders, nil
}

func matchCurseForgeFolder(filePath string, folders []matchedCurseForgeFolder) int64 {
	filePath = strings.ToLower(canonicalInventoryPath(filePath))
	for _, folder := range folders {
		prefix := strings.ToLower(strings.TrimSuffix(folder.Folder, "/")) + "/"
		if strings.HasPrefix(filePath, prefix) {
			return folder.ModID
		}
	}
	return 0
}
