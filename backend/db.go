package main

import "database/sql"

func migrate(db *sql.DB) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS repos(id INTEGER PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL, url TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, config TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS mods(id INTEGER PRIMARY KEY, repo_id INTEGER REFERENCES repos(id), external_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL, creator TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, url TEXT NOT NULL DEFAULT '', tags TEXT NOT NULL DEFAULT '[]', last_seen TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(source, external_id))`,
		`CREATE TABLE IF NOT EXISTS mod_versions(id INTEGER PRIMARY KEY, mod_id INTEGER NOT NULL REFERENCES mods(id) ON DELETE CASCADE, version TEXT NOT NULL, download_url TEXT NOT NULL DEFAULT '', sha256 TEXT NOT NULL DEFAULT '', file_name TEXT NOT NULL DEFAULT '', published_at TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(mod_id, version))`,
		`CREATE TABLE IF NOT EXISTS clients(id TEXT PRIMARY KEY, name TEXT NOT NULL, last_seen TEXT, local_path TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS profiles(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS profile_mods(profile_id INTEGER NOT NULL REFERENCES profiles(id) ON DELETE CASCADE, mod_id INTEGER NOT NULL REFERENCES mods(id) ON DELETE CASCADE, version TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, desired INTEGER NOT NULL DEFAULT 1, notes TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(profile_id,mod_id))`,
		`CREATE TABLE IF NOT EXISTS installed_mods(id INTEGER PRIMARY KEY, mod_id INTEGER NOT NULL REFERENCES mods(id), client_id TEXT REFERENCES clients(id), version TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, desired INTEGER NOT NULL DEFAULT 1, notes TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(mod_id, client_id))`,
		`CREATE TABLE IF NOT EXISTS conflicts(id INTEGER PRIMARY KEY, client_id TEXT REFERENCES clients(id), type TEXT NOT NULL, severity TEXT NOT NULL, message TEXT NOT NULL, mod_ids TEXT NOT NULL DEFAULT '[]', resolved INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS annotations(id INTEGER PRIMARY KEY, mod_id INTEGER NOT NULL REFERENCES mods(id), kind TEXT NOT NULL CHECK(kind IN ('blacklist','flag','note')), value TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, UNIQUE(mod_id, kind))`,
		`CREATE TABLE IF NOT EXISTS sync_jobs(id INTEGER PRIMARY KEY, client_id TEXT REFERENCES clients(id), mod_id INTEGER REFERENCES mods(id), action TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', payload TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS client_inventory(client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE, path TEXT NOT NULL, file_name TEXT NOT NULL, sha256 TEXT NOT NULL DEFAULT '', mod_id INTEGER REFERENCES mods(id), enabled INTEGER NOT NULL DEFAULT 1, observed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY(client_id,path))`,
		`CREATE INDEX IF NOT EXISTS idx_client_inventory_mod ON client_inventory(mod_id,client_id)`,
		`CREATE TABLE IF NOT EXISTS client_curseforge_addons(client_id TEXT NOT NULL REFERENCES clients(id) ON DELETE CASCADE,addon_id INTEGER NOT NULL,name TEXT NOT NULL DEFAULT '',file_id INTEGER NOT NULL,file_name TEXT NOT NULL DEFAULT '',download_url TEXT NOT NULL DEFAULT '',fingerprint INTEGER NOT NULL,mod_folder TEXT NOT NULL,module_folder_name TEXT NOT NULL DEFAULT '',mod_id INTEGER REFERENCES mods(id),fingerprint_matched INTEGER NOT NULL DEFAULT 0,fingerprint_checked_at TEXT,observed_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,PRIMARY KEY(client_id,addon_id,file_id,fingerprint,module_folder_name))`,
		`CREATE INDEX IF NOT EXISTS idx_client_curseforge_addons_mod ON client_curseforge_addons(mod_id,client_id)`,
		`CREATE TABLE IF NOT EXISTS client_inventory_audits(client_id TEXT PRIMARY KEY REFERENCES clients(id) ON DELETE CASCADE, audited_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, item_count INTEGER NOT NULL DEFAULT 0)`,
		`INSERT OR IGNORE INTO schema_migrations(version) VALUES(1)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	columns, err := db.Query(`PRAGMA table_info(mods)`)
	if err != nil {
		return err
	}
	hasThumbnail := false
	for columns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var dflt any
		if err := columns.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
			columns.Close()
			return err
		}
		if name == "thumbnail_url" {
			hasThumbnail = true
		}
	}
	if err := columns.Close(); err != nil {
		return err
	}
	if !hasThumbnail {
		if _, err := db.Exec(`ALTER TABLE mods ADD COLUMN thumbnail_url TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	modColumns, err := db.Query(`PRAGMA table_info(mods)`)
	if err != nil {
		return err
	}
	hasSourceDownloadURL := false
	hasCurrentVersion, hasPreviousVersion := false, false
	hasCatalogActive := false
	hasSourceDetailsChecked := false
	for modColumns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var dflt any
		if err := modColumns.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
			modColumns.Close()
			return err
		}
		if name == "source_download_url" {
			hasSourceDownloadURL = true
		}
		if name == "current_version" {
			hasCurrentVersion = true
		}
		if name == "previous_version" {
			hasPreviousVersion = true
		}
		if name == "catalog_active" {
			hasCatalogActive = true
		}
		if name == "source_details_checked" {
			hasSourceDetailsChecked = true
		}
	}
	if err := modColumns.Close(); err != nil {
		return err
	}
	if !hasSourceDownloadURL {
		if _, err := db.Exec(`ALTER TABLE mods ADD COLUMN source_download_url TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasCurrentVersion {
		if _, err := db.Exec(`ALTER TABLE mods ADD COLUMN current_version TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasPreviousVersion {
		if _, err := db.Exec(`ALTER TABLE mods ADD COLUMN previous_version TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasCatalogActive {
		if _, err := db.Exec(`ALTER TABLE mods ADD COLUMN catalog_active INTEGER NOT NULL DEFAULT 1`); err != nil {
			return err
		}
	}
	if !hasSourceDetailsChecked {
		if _, err := db.Exec(`ALTER TABLE mods ADD COLUMN source_details_checked INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	versionColumns, err := db.Query(`PRAGMA table_info(mod_versions)`)
	if err != nil {
		return err
	}
	hasTrackingSource := false
	for versionColumns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var dflt any
		if err := versionColumns.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
			versionColumns.Close()
			return err
		}
		if name == "tracking_source" {
			hasTrackingSource = true
		}
	}
	if err := versionColumns.Close(); err != nil {
		return err
	}
	if !hasTrackingSource {
		if _, err := db.Exec(`ALTER TABLE mod_versions ADD COLUMN tracking_source TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	clientColumns, err := db.Query(`PRAGMA table_info(clients)`)
	if err != nil {
		return err
	}
	hasProfileID := false
	for clientColumns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var dflt any
		if err := clientColumns.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
			clientColumns.Close()
			return err
		}
		if name == "profile_id" {
			hasProfileID = true
		}
	}
	if err := clientColumns.Close(); err != nil {
		return err
	}
	if !hasProfileID {
		if _, err := db.Exec(`ALTER TABLE clients ADD COLUMN profile_id INTEGER REFERENCES profiles(id) ON DELETE SET NULL`); err != nil {
			return err
		}
	}
	return nil
}
