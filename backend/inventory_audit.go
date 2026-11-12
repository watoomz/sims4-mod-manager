package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path"
	"strings"
)

type auditInventoryRequest struct {
	Files            []auditInventoryFile       `json:"files"`
	CurseForgeAddons []curseForgeAddonInventory `json:"curseforge_addons"`
}

type auditInventoryFile struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	ModID   int64  `json:"mod_id,omitempty"`
	Enabled bool   `json:"enabled"`
}

func (a *App) auditInventory(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	var clientCount int
	if a.db.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, clientID).Scan(&clientCount) != nil || clientCount == 0 {
		http.Error(w, "unknown client", 404)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	var input auditInventoryRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid inventory audit", 400)
		return
	}
	if len(input.Files) > 20000 {
		http.Error(w, "inventory exceeds 20000 files", 413)
		return
	}
	for i := range input.Files {
		file := &input.Files[i]
		file.Path = canonicalInventoryPath(file.Path)
		if file.Path == "." || file.Path == ".." || strings.HasPrefix(file.Path, "../") || strings.HasPrefix(file.Path, "/") || strings.Contains(file.Path, ":") {
			http.Error(w, "inventory paths must be relative to Mods", 400)
			return
		}
		ext := strings.ToLower(path.Ext(undisabledPath(file.Path)))
		if ext != ".package" && ext != ".ts4script" {
			http.Error(w, "inventory only accepts .package and .ts4script files", 400)
			return
		}
		if !validSHA256(file.SHA256) {
			http.Error(w, "inventory file is missing a SHA-256 hash", 400)
			return
		}
		if file.ModID > 0 {
			var exists int
			if a.db.QueryRow(`SELECT count(*) FROM mods WHERE id=?`, file.ModID).Scan(&exists) != nil || exists == 0 {
				http.Error(w, "inventory references unknown mod_id", 400)
				return
			}
		}
	}
	folders, err := a.crossReferenceCurseForgeAddons(r.Context(), clientID, input.CurseForgeAddons)
	if err != nil {
		http.Error(w, "CurseForge inventory cross-reference failed", 500)
		return
	}
	for i := range input.Files {
		file := &input.Files[i]
		if file.ModID == 0 {
			file.ModID = matchCurseForgeFolder(file.Path, folders)
		}
		if file.ModID == 0 {
			matched, err := a.matchInventoryFile(file)
			if err != nil {
				http.Error(w, "catalog match failed", 500)
				return
			}
			file.ModID = matched
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM client_inventory WHERE client_id=?`, clientID); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	for _, file := range input.Files {
		var modValue any
		if file.ModID > 0 {
			modValue = file.ModID
		}
		fileName := path.Base(undisabledPath(file.Path))
		if _, err := tx.Exec(`INSERT INTO client_inventory(client_id,path,file_name,sha256,mod_id,enabled,observed_at) VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP)`, clientID, file.Path, fileName, strings.ToLower(file.SHA256), modValue, boolInt(file.Enabled)); err != nil {
			http.Error(w, "inventory write failed", 500)
			return
		}
	}
	if _, err := tx.Exec(`INSERT INTO client_inventory_audits(client_id,audited_at,item_count) VALUES(?,CURRENT_TIMESTAMP,?) ON CONFLICT(client_id) DO UPDATE SET audited_at=CURRENT_TIMESTAMP,item_count=excluded.item_count`, clientID, len(input.Files)); err != nil {
		http.Error(w, "inventory audit write failed", 500)
		return
	}
	_, _ = tx.Exec(`UPDATE clients SET last_seen=CURRENT_TIMESTAMP WHERE id=?`, clientID)
	if err := tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	unmanaged := 0
	for _, file := range input.Files {
		if file.ModID == 0 {
			unmanaged++
		}
	}
	writeJSON(w, 200, map[string]any{"client_id": clientID, "files_received": len(input.Files), "recognized": len(input.Files) - unmanaged, "unmanaged": unmanaged})
}

func (a *App) matchInventoryFile(file *auditInventoryFile) (int64, error) {
	var modID int64
	err := a.db.QueryRow(`SELECT m.id FROM mod_versions v JOIN mods m ON m.id=v.mod_id WHERE lower(v.sha256)=lower(?) AND v.sha256<>'' ORDER BY v.created_at DESC LIMIT 1`, file.SHA256).Scan(&modID)
	if err == nil {
		return modID, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	fileName := strings.ToLower(path.Base(undisabledPath(file.Path)))
	err = a.db.QueryRow(`SELECT m.id FROM mod_versions v JOIN mods m ON m.id=v.mod_id WHERE lower(v.file_name)=? ORDER BY v.created_at DESC LIMIT 1`, fileName).Scan(&modID)
	if err == nil {
		return modID, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	stem := strings.TrimSuffix(fileName, path.Ext(fileName))
	err = a.db.QueryRow(`SELECT id FROM mods WHERE lower(name)=? ORDER BY last_seen DESC LIMIT 1`, stem).Scan(&modID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return modID, err
}

func (a *App) compareInventory(w http.ResponseWriter, r *http.Request) {
	clientRows, err := a.db.Query(`SELECT c.id,c.name,COALESCE(a.audited_at,''),c.profile_id,COALESCE(p.name,'') FROM clients c LEFT JOIN client_inventory_audits a ON a.client_id=c.id LEFT JOIN profiles p ON p.id=c.profile_id ORDER BY c.name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	type clientInfo struct {
		ID, Name, AuditedAt, ProfileName string
		ProfileID                        sql.NullInt64
	}
	clients := []clientInfo{}
	for clientRows.Next() {
		var client clientInfo
		if clientRows.Scan(&client.ID, &client.Name, &client.AuditedAt, &client.ProfileID, &client.ProfileName) == nil {
			clients = append(clients, client)
		}
	}
	clientRows.Close()
	modRows, err := a.db.Query(`SELECT DISTINCT m.id,m.name,m.source FROM mods m WHERE EXISTS(SELECT 1 FROM installed_mods im WHERE im.mod_id=m.id AND im.desired=1) OR EXISTS(SELECT 1 FROM profile_mods pm WHERE pm.mod_id=m.id AND pm.desired=1) OR EXISTS(SELECT 1 FROM client_inventory ci WHERE ci.mod_id=m.id) ORDER BY m.name`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	type modRow struct {
		ID           int64
		Name, Source string
	}
	mods := []modRow{}
	for modRows.Next() {
		var mod modRow
		if modRows.Scan(&mod.ID, &mod.Name, &mod.Source) == nil {
			mods = append(mods, mod)
		}
	}
	modRows.Close()
	type inventoryStatus struct {
		ClientID   string `json:"client_id"`
		ClientName string `json:"client_name"`
		State      string `json:"state"`
	}
	out := make([]map[string]any, 0, len(mods))
	for _, mod := range mods {
		statuses := make([]inventoryStatus, 0, len(clients))
		for _, client := range clients {
			var enabled, disabled int
			if err := a.db.QueryRow(`SELECT COALESCE(sum(enabled=1),0),COALESCE(sum(enabled=0),0) FROM client_inventory WHERE client_id=? AND mod_id=?`, client.ID, mod.ID).Scan(&enabled, &disabled); err != nil {
				http.Error(w, "database error", 500)
				return
			}
			state := "missing"
			if client.AuditedAt == "" {
				state = "unaudited"
			} else if enabled > 0 {
				state = "enabled"
			} else if disabled > 0 {
				state = "disabled"
			}
			statuses = append(statuses, inventoryStatus{ClientID: client.ID, ClientName: client.Name, State: state})
		}
		out = append(out, map[string]any{"mod_id": mod.ID, "name": mod.Name, "source": mod.Source, "clients": statuses})
	}
	unmanagedRows, err := a.db.Query(`SELECT ci.client_id,c.name,ci.path,ci.file_name,ci.sha256,ci.enabled FROM client_inventory ci JOIN clients c ON c.id=ci.client_id WHERE ci.mod_id IS NULL ORDER BY c.name,ci.path`)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	unmanaged := []map[string]any{}
	for unmanagedRows.Next() {
		var clientID, clientName, filePath, fileName, hash string
		var enabled int
		if unmanagedRows.Scan(&clientID, &clientName, &filePath, &fileName, &hash, &enabled) == nil {
			unmanaged = append(unmanaged, map[string]any{"client_id": clientID, "client_name": clientName, "path": filePath, "file_name": fileName, "sha256": hash, "enabled": enabled == 1})
		}
	}
	_ = unmanagedRows.Close()
	clientsOut := make([]map[string]any, 0, len(clients))
	for _, client := range clients {
		clientsOut = append(clientsOut, map[string]any{"id": client.ID, "name": client.Name, "audited_at": client.AuditedAt, "audited": client.AuditedAt != "", "profile_id": nullableInt(client.ProfileID), "profile_name": client.ProfileName})
	}
	writeJSON(w, 200, map[string]any{"clients": clientsOut, "mods": out, "unmanaged": unmanaged})
}

func canonicalInventoryPath(value string) string {
	return path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
}

func undisabledPath(value string) string {
	if strings.EqualFold(path.Ext(value), ".disabled") {
		return value[:len(value)-len(".disabled")]
	}
	return value
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
