package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestEffectiveDesiredPrecedenceAndProfileQueues(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('pc','PC'),('laptop','Laptop')`); err != nil {
		t.Fatal(err)
	}
	profile, err := app.db.Exec(`INSERT INTO profiles(name) VALUES('Desktop')`)
	if err != nil {
		t.Fatal(err)
	}
	profileID, _ := profile.LastInsertId()
	if _, err := app.db.Exec(`INSERT INTO installed_mods(mod_id,client_id,version,enabled,desired,notes) VALUES(1,NULL,'global',1,1,'global'),(1,'pc','own',0,1,'own')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO profile_mods(profile_id,mod_id,version,enabled,desired,notes) VALUES(?,?, 'profile',0,1,'profile')`, profileID, 1); err != nil {
		t.Fatal(err)
	}
	assign := httptest.NewRequest("POST", "/api/clients/laptop/profile", bytes.NewBufferString(`{"profile_id":`+jsonNumber(profileID)+`}`))
	assign.SetPathValue("clientID", "laptop")
	assigned := httptest.NewRecorder()
	app.assignClientProfile(assigned, assign)
	if assigned.Code != 200 {
		t.Fatalf("assignment returned %d: %s", assigned.Code, assigned.Body.String())
	}
	for _, tc := range []struct {
		client, version, notes string
		enabled                bool
	}{{"pc", "own", "own", false}, {"laptop", "profile", "profile", false}} {
		r := httptest.NewRequest("GET", "/api/clients/"+tc.client+"/desired", nil)
		r.SetPathValue("clientID", tc.client)
		w := httptest.NewRecorder()
		app.clientDesired(w, r)
		var rows []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["version"] != tc.version || rows[0]["notes"] != tc.notes || rows[0]["enabled"] != tc.enabled {
			t.Fatalf("%s resolved unexpected state: %s", tc.client, w.Body.String())
		}
	}
	var jobs int
	if err := app.db.QueryRow(`SELECT count(*) FROM sync_jobs WHERE client_id='laptop' AND status='pending'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("profile assignment should queue laptop sync, got %d jobs", jobs)
	}
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source,url) VALUES(2,'test-url-2','Second','test','https://example.test/second')`); err != nil {
		t.Fatal(err)
	}
	modReq := httptest.NewRequest("POST", "/api/profiles/1/mods", bytes.NewBufferString(`{"mod_id":2,"version":"latest","enabled":true,"desired":true}`))
	modReq.SetPathValue("profileID", "1")
	modW := httptest.NewRecorder()
	app.saveProfileMod(modW, modReq)
	if modW.Code != 200 {
		t.Fatalf("profile mod returned %d: %s", modW.Code, modW.Body.String())
	}
	if err := app.db.QueryRow(`SELECT count(*) FROM sync_jobs WHERE client_id='laptop' AND mod_id=2 AND status='pending'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("profile edit should queue assigned laptop, got %d jobs", jobs)
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
