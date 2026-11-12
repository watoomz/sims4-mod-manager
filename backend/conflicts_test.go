package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestInventoryConflictDetectionAndResolution(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('client-1','PC')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source) VALUES(2,'m2','Second Mod','test'),(3,'m3','WickedWhims','test'),(4,'m4','WonderfulWhims','test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO annotations(mod_id,kind,value) VALUES(2,'blacklist','broken')`); err != nil {
		t.Fatal(err)
	}
	body := `{"files":[{"path":"Shared.package","mod_id":1},{"path":"shared.package","mod_id":2},{"path":"Folder/Deep/script.ts4script","mod_id":1},{"path":"wicked.ts4script","mod_id":3},{"path":"wonderful.ts4script","mod_id":4}]}`
	r := httptest.NewRequest("POST", "/api/clients/client-1/inventory", bytes.NewBufferString(body))
	r.SetPathValue("clientID", "client-1")
	w := httptest.NewRecorder()
	app.reportInventory(w, r)
	if w.Code != 200 {
		t.Fatalf("inventory returned %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Conflicts []detectedConflict `json:"conflicts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, c := range response.Conflicts {
		kinds[c.Kind] = true
	}
	for _, kind := range []string{"filename_collision", "script_depth", "known_incompatible", "blacklist"} {
		if !kinds[kind] {
			t.Errorf("missing %s conflict in %#v", kind, response.Conflicts)
		}
	}
	clear := httptest.NewRequest("POST", "/api/clients/client-1/inventory", bytes.NewBufferString(`{"files":[]}`))
	clear.SetPathValue("clientID", "client-1")
	clearRecorder := httptest.NewRecorder()
	app.reportInventory(clearRecorder, clear)
	if clearRecorder.Code != 200 {
		t.Fatalf("empty inventory returned %d: %s", clearRecorder.Code, clearRecorder.Body.String())
	}
	var active int
	if err := app.db.QueryRow(`SELECT count(*) FROM conflicts WHERE client_id='client-1' AND resolved=0`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("expected old conflicts resolved, found %d", active)
	}
}

func TestInventoryRejectsTraversal(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('client-1','PC')`); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/clients/client-1/inventory", bytes.NewBufferString(`{"files":[{"path":"../../outside.package"}]}`))
	r.SetPathValue("clientID", "client-1")
	w := httptest.NewRecorder()
	app.reportInventory(w, r)
	if w.Code != 400 {
		t.Fatalf("expected path traversal to be rejected, got %d", w.Code)
	}
}
