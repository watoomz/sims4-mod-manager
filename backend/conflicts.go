package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path"
	"strings"
)

type inventoryRequest struct {
	Files            []inventoryFile            `json:"files"`
	CurseForgeAddons []curseForgeAddonInventory `json:"curseforge_addons"`
}
type inventoryFile struct {
	Path   string `json:"path"`
	ModID  int64  `json:"mod_id"`
	SHA256 string `json:"sha256,omitempty"`
}

func (a *App) reportInventory(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("clientID")
	var clientCount int
	if a.db.QueryRow(`SELECT count(*) FROM clients WHERE id=?`, clientID).Scan(&clientCount) != nil || clientCount == 0 {
		http.Error(w, "unknown client", 404)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	var input inventoryRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid inventory", 400)
		return
	}
	if len(input.Files) > 20000 {
		http.Error(w, "inventory exceeds 20000 files", 413)
		return
	}
	files := make([]inventoryFile, 0, len(input.Files))
	for _, file := range input.Files {
		clean := path.Clean(strings.ReplaceAll(strings.TrimSpace(file.Path), "\\", "/"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
			http.Error(w, "inventory paths must be relative to Mods", 400)
			return
		}
		if file.ModID > 0 {
			var count int
			if a.db.QueryRow(`SELECT count(*) FROM mods WHERE id=?`, file.ModID).Scan(&count) != nil || count == 0 {
				http.Error(w, "inventory references unknown mod_id", 400)
				return
			}
		}
		file.Path = clean
		files = append(files, file)
	}
	folders, err := a.crossReferenceCurseForgeAddons(r.Context(), clientID, input.CurseForgeAddons)
	if err != nil {
		http.Error(w, "CurseForge inventory cross-reference failed", 500)
		return
	}
	for i := range files {
		if files[i].ModID == 0 {
			files[i].ModID = matchCurseForgeFolder(files[i].Path, folders)
		}
	}
	conflicts := detectInventoryConflicts(a.db, files)
	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec(`UPDATE conflicts SET resolved=1 WHERE client_id=? AND resolved=0`, clientID)
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	for _, conflict := range conflicts {
		modJSON, _ := json.Marshal(conflict.ModIDs)
		if _, err = tx.Exec(`INSERT INTO conflicts(client_id,type,severity,message,mod_ids) VALUES(?,?,?,?,?)`, clientID, conflict.Kind, conflict.Severity, conflict.Message, string(modJSON)); err != nil {
			http.Error(w, "database error", 500)
			return
		}
	}
	_, _ = tx.Exec(`UPDATE clients SET last_seen=CURRENT_TIMESTAMP WHERE id=?`, clientID)
	if err = tx.Commit(); err != nil {
		http.Error(w, "database error", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"client_id": clientID, "files_received": len(files), "conflicts": conflicts})
}

type detectedConflict struct {
	Kind     string  `json:"type"`
	Severity string  `json:"severity"`
	Message  string  `json:"message"`
	ModIDs   []int64 `json:"mod_ids"`
}

func detectInventoryConflicts(db *sql.DB, files []inventoryFile) []detectedConflict {
	out := []detectedConflict{}
	paths := map[string]map[int64]bool{}
	modIDs := map[int64]bool{}
	for _, file := range files {
		if file.ModID > 0 {
			modIDs[file.ModID] = true
		}
		key := strings.ToLower(file.Path)
		if paths[key] == nil {
			paths[key] = map[int64]bool{}
		}
		if file.ModID > 0 {
			paths[key][file.ModID] = true
		}
		if strings.EqualFold(path.Ext(file.Path), ".ts4script") {
			parts := strings.Split(file.Path, "/")
			if len(parts) > 2 {
				out = append(out, detectedConflict{Kind: "script_depth", Severity: "high", Message: file.Path + " is more than one folder deep", ModIDs: nonzeroIDs(file.ModID)})
			}
		}
	}
	for filename, owners := range paths {
		if len(owners) > 1 {
			ids := mapKeys(owners)
			out = append(out, detectedConflict{Kind: "filename_collision", Severity: "warning", Message: "Multiple mods contain " + filename, ModIDs: ids})
		}
	}
	if hasModName(db, modIDs, "wickedwhims") && hasModName(db, modIDs, "wonderfulwhims") {
		ids := []int64{}
		for id := range modIDs {
			if containsModName(db, id, "wickedwhims") || containsModName(db, id, "wonderfulwhims") {
				ids = append(ids, id)
			}
		}
		out = append(out, detectedConflict{Kind: "known_incompatible", Severity: "high", Message: "WickedWhims and WonderfulWhims should not be installed together", ModIDs: ids})
	}
	for id := range modIDs {
		rows, err := db.Query(`SELECT kind,value FROM annotations WHERE mod_id=? AND value<>'' AND kind IN ('blacklist','flag')`, id)
		if err != nil {
			continue
		}
		for rows.Next() {
			var kind, value string
			if rows.Scan(&kind, &value) == nil {
				severity := "warning"
				message := "Flagged mod: " + value
				if kind == "blacklist" {
					severity = "high"
					message = "Blacklisted mod: " + value
				}
				out = append(out, detectedConflict{Kind: kind, Severity: severity, Message: message, ModIDs: []int64{id}})
			}
		}
		_ = rows.Close()
	}
	return out
}

func (a *App) listConflicts(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("client_id")
	query := `SELECT id,client_id,type,severity,message,mod_ids,resolved,created_at FROM conflicts`
	var rows *sql.Rows
	var err error
	if clientID == "" {
		rows, err = a.db.Query(query + ` ORDER BY resolved,severity DESC,created_at DESC`)
	} else {
		rows, err = a.db.Query(query+` WHERE client_id=? ORDER BY resolved,severity DESC,created_at DESC`, clientID)
	}
	if err != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var client, typeName, severity, message, modIDs, created string
		var resolved int
		if rows.Scan(&id, &client, &typeName, &severity, &message, &modIDs, &resolved, &created) != nil {
			continue
		}
		var ids []int64
		_ = json.Unmarshal([]byte(modIDs), &ids)
		out = append(out, map[string]any{"id": id, "client_id": client, "type": typeName, "severity": severity, "message": message, "mod_ids": ids, "resolved": resolved == 1, "created_at": created})
	}
	writeJSON(w, 200, out)
}

func mapKeys(values map[int64]bool) []int64 {
	out := make([]int64, 0, len(values))
	for id := range values {
		out = append(out, id)
	}
	return out
}
func nonzeroIDs(id int64) []int64 {
	if id == 0 {
		return []int64{}
	}
	return []int64{id}
}
func hasModName(db *sql.DB, ids map[int64]bool, needle string) bool {
	for id := range ids {
		if containsModName(db, id, needle) {
			return true
		}
	}
	return false
}
func containsModName(db *sql.DB, id int64, needle string) bool {
	var name string
	if db.QueryRow(`SELECT lower(name) FROM mods WHERE id=?`, id).Scan(&name) != nil {
		return false
	}
	return strings.Contains(name, needle)
}
