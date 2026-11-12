package main

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrateCreatesCatalogTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"repos", "mods", "mod_versions", "clients", "installed_mods", "conflicts", "annotations", "sync_jobs"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("table %s was not created", name)
		}
	}
}
