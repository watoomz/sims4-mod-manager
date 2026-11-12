package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"
)

func testApp(t *testing.T) *App {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO mods(id,external_id,name,source,url) VALUES(1,'test-url','Test Mod','test','https://example.test/mod')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &App{db: db}
}

func TestDesiredStateAndClientOverride(t *testing.T) {
	app := testApp(t)
	install := httptest.NewRequest("POST", "/api/desired", bytes.NewBufferString(`{"mod_id":1}`))
	installRecorder := httptest.NewRecorder()
	app.setDesired(installRecorder, install)
	if installRecorder.Code != 200 {
		t.Fatalf("install returned %d: %s", installRecorder.Code, installRecorder.Body.String())
	}
	var clientID string
	if err := app.db.QueryRow(`INSERT INTO clients(id,name) VALUES('client-1','PC') RETURNING id`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	globalAgain := httptest.NewRequest("POST", "/api/desired", bytes.NewBufferString(`{"mod_id":1}`))
	globalAgainRecorder := httptest.NewRecorder()
	app.setDesired(globalAgainRecorder, globalAgain)
	if globalAgainRecorder.Code != 200 {
		t.Fatalf("repeated install returned %d: %s", globalAgainRecorder.Code, globalAgainRecorder.Body.String())
	}
	override := httptest.NewRequest("POST", "/api/desired", bytes.NewBufferString(`{"mod_id":1,"client_id":"client-1","enabled":false}`))
	overrideRecorder := httptest.NewRecorder()
	app.setDesired(overrideRecorder, override)
	if overrideRecorder.Code != 200 {
		t.Fatalf("client override returned %d: %s", overrideRecorder.Code, overrideRecorder.Body.String())
	}
	clientRequest := httptest.NewRequest("GET", "/api/clients/client-1/desired", nil)
	clientRequest.SetPathValue("clientID", clientID)
	clientRecorder := httptest.NewRecorder()
	app.clientDesired(clientRecorder, clientRequest)
	var rows []map[string]any
	if err := json.Unmarshal(clientRecorder.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["enabled"] != false || rows[0]["desired"] != true {
		t.Fatalf("unexpected effective client state: %#v", rows)
	}
	var pending int
	if err := app.db.QueryRow(`SELECT count(*) FROM sync_jobs WHERE client_id='client-1' AND mod_id=1 AND status='pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("expected one superseding pending sync action, got %d", pending)
	}
}

func TestUninstallKeepsDesiredTombstoneAndQueuesJob(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('client-1','PC')`); err != nil {
		t.Fatal(err)
	}
	install := httptest.NewRequest("POST", "/api/desired", bytes.NewBufferString(`{"mod_id":1}`))
	installRecorder := httptest.NewRecorder()
	app.setDesired(installRecorder, install)
	remove := httptest.NewRequest("DELETE", "/api/desired/1", nil)
	remove.SetPathValue("modID", "1")
	removeRecorder := httptest.NewRecorder()
	app.uninstallDesired(removeRecorder, remove)
	if removeRecorder.Code != 200 {
		t.Fatalf("uninstall returned %d: %s", removeRecorder.Code, removeRecorder.Body.String())
	}
	var desired, jobCount int
	if err := app.db.QueryRow(`SELECT desired FROM installed_mods WHERE mod_id=1 AND client_id IS NULL`).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM sync_jobs WHERE client_id='client-1' AND mod_id=1 AND action='uninstall'`).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if desired != 0 || jobCount != 1 {
		t.Fatalf("expected uninstall tombstone and queued job; desired=%d jobs=%d", desired, jobCount)
	}
}

func TestUpdateListAndClientJobAcknowledgement(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('client-1','PC')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO mod_versions(mod_id,version,sha256) VALUES(1,'1.0','old'),(1,'2.0','new')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO installed_mods(mod_id,client_id,version) VALUES(1,NULL,'1.0')`); err != nil {
		t.Fatal(err)
	}
	updates := httptest.NewRecorder()
	app.updates(updates, httptest.NewRequest("GET", "/api/updates", nil))
	if updates.Code != 200 || !bytes.Contains(updates.Body.Bytes(), []byte(`"latest_version":"2.0"`)) {
		t.Fatalf("expected update from 1.0 to 2.0, got %d: %s", updates.Code, updates.Body.String())
	}
	if _, err := app.db.Exec(`INSERT INTO sync_jobs(client_id,mod_id,action) VALUES('client-1',1,'install')`); err != nil {
		t.Fatal(err)
	}
	ack := httptest.NewRequest("POST", "/api/clients/client-1/sync/ack", nil)
	ack.SetPathValue("clientID", "client-1")
	recorder := httptest.NewRecorder()
	app.acknowledgeSync(recorder, ack)
	var status string
	if err := app.db.QueryRow(`SELECT status FROM sync_jobs WHERE client_id='client-1'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 200 || status != "complete" {
		t.Fatalf("sync job was not acknowledged: status=%s response=%d %s", status, recorder.Code, recorder.Body.String())
	}
}
