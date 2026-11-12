package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInventoryAuditRecognizesFilesAndRetainsUnmanaged(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('pc','Gaming PC')`); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	if _, err := app.db.Exec(`INSERT INTO mod_versions(mod_id,version,sha256,file_name) VALUES(1,'1.0',?,'Known.package')`, hash); err != nil {
		t.Fatal(err)
	}
	body := `{"files":[{"path":"Known.package","sha256":"` + hash + `","enabled":true},{"path":"Loose/Unknown.ts4script.disabled","sha256":"` + strings.Repeat("b", 64) + `","enabled":false}]}`
	request := httptest.NewRequest("POST", "/api/clients/pc/audit", bytes.NewBufferString(body))
	request.SetPathValue("clientID", "pc")
	recorder := httptest.NewRecorder()
	app.auditInventory(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("audit returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var counts map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &counts); err != nil {
		t.Fatal(err)
	}
	if counts["recognized"] != float64(1) || counts["unmanaged"] != float64(1) {
		t.Fatalf("unexpected audit match counts: %#v", counts)
	}
	var knownID sql.NullInt64
	if err := app.db.QueryRow(`SELECT mod_id FROM client_inventory WHERE client_id='pc' AND file_name='Known.package'`).Scan(&knownID); err != nil || !knownID.Valid || knownID.Int64 != 1 {
		t.Fatalf("known file was not matched: id=%+v err=%v", knownID, err)
	}
	var unmanagedID sql.NullInt64
	var enabled int
	if err := app.db.QueryRow(`SELECT mod_id,enabled FROM client_inventory WHERE client_id='pc' AND file_name='Unknown.ts4script'`).Scan(&unmanagedID, &enabled); err != nil || unmanagedID.Valid || enabled != 0 {
		t.Fatalf("unknown disabled file was not retained as unmanaged: id=%+v enabled=%d err=%v", unmanagedID, enabled, err)
	}
	clear := httptest.NewRequest("POST", "/api/clients/pc/audit", bytes.NewBufferString(`{"files":[]}`))
	clear.SetPathValue("clientID", "pc")
	clearRecorder := httptest.NewRecorder()
	app.auditInventory(clearRecorder, clear)
	var remaining, itemCount int
	if err := app.db.QueryRow(`SELECT count(*) FROM client_inventory WHERE client_id='pc'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if err := app.db.QueryRow(`SELECT item_count FROM client_inventory_audits WHERE client_id='pc'`).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if clearRecorder.Code != 200 || remaining != 0 || itemCount != 0 {
		t.Fatalf("re-audit did not replace snapshot: status=%d remaining=%d items=%d", clearRecorder.Code, remaining, itemCount)
	}
}

func TestCrossClientInventoryComparison(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('pc','PC'),('laptop','Laptop')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO mods(id,external_id,name,source,url) VALUES(2,'other','Observed Mod','test','https://example.test/observed')`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO installed_mods(mod_id,client_id,version,desired) VALUES(1,NULL,'latest',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO client_inventory_audits(client_id,item_count) VALUES('pc',1),('laptop',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec(`INSERT INTO client_inventory(client_id,path,file_name,sha256,mod_id,enabled) VALUES('pc','Known.package','Known.package',? ,1,1),('laptop','Known.package.disabled','Known.package',?,1,0),('laptop','Observed.package','Observed.package',?,2,1)`, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	app.compareInventory(recorder, httptest.NewRequest("GET", "/api/inventory/compare", nil))
	if recorder.Code != 200 {
		t.Fatalf("compare returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Mods []struct {
			ModID   int64 `json:"mod_id"`
			Clients []struct {
				ClientID string `json:"client_id"`
				State    string `json:"state"`
			} `json:"clients"`
		} `json:"mods"`
		Unmanaged []map[string]any `json:"unmanaged"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	states := map[int64]map[string]string{}
	for _, mod := range result.Mods {
		states[mod.ModID] = map[string]string{}
		for _, client := range mod.Clients {
			states[mod.ModID][client.ClientID] = client.State
		}
	}
	if states[1]["pc"] != "enabled" || states[1]["laptop"] != "disabled" || states[2]["pc"] != "missing" || states[2]["laptop"] != "enabled" {
		t.Fatalf("unexpected client comparison states: %#v", states)
	}
	if len(result.Unmanaged) != 0 {
		t.Fatalf("unexpected unmanaged files: %#v", result.Unmanaged)
	}
}

func TestInventoryAuditCrossReferencesCurseForgeDesktopFingerprint(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('pc','Gaming PC')`); err != nil {
		t.Fatal(err)
	}
	var fingerprintCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fingerprints" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		fingerprintCalls++
		var body struct {
			Fingerprints []uint32 `json:"fingerprints"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Fingerprints) != 1 || body.Fingerprints[0] != 987654 {
			t.Errorf("unexpected fingerprint request: %#v err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"exactMatches":[{"id":42,"fingerprint":987654,"file":{"id":43,"fileFingerprint":987654}}]}}`))
	}))
	defer api.Close()
	app.api = NewCurseForgeAdapter("test-key", api.Client())
	app.api.baseURL = api.URL

	body := `{"files":[{"path":"Packages/Local CurseForge/Example.package","sha256":"` + strings.Repeat("a", 64) + `","enabled":true}],"curseforge_addons":[{"addonID":42,"name":"Inventory Mod","fileId":43,"fileName":"inventory.zip","downloadUrl":"https://edge.forgecdn.net/files/inventory.zip","fingerprint":987654,"modFolder":"Packages/Local CurseForge","moduleFolderName":"Example.package"}]}`
	request := httptest.NewRequest("POST", "/api/clients/pc/audit", bytes.NewBufferString(body))
	request.SetPathValue("clientID", "pc")
	response := httptest.NewRecorder()
	app.auditInventory(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("audit returned %d: %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["recognized"] != float64(1) || result["unmanaged"] != float64(0) {
		t.Fatalf("CurseForge install was not recognized: %#v", result)
	}
	var modID sql.NullInt64
	if err := app.db.QueryRow(`SELECT mod_id FROM client_inventory WHERE client_id='pc' AND file_name='Example.package'`).Scan(&modID); err != nil || !modID.Valid {
		t.Fatalf("CurseForge inventory row was not identified: id=%+v err=%v", modID, err)
	}
	var name string
	if err := app.db.QueryRow(`SELECT name FROM mods WHERE id=?`, modID.Int64).Scan(&name); err != nil || name != "Inventory Mod" {
		t.Fatalf("identified catalog row name=%q err=%v", name, err)
	}
	var fingerprintMatched int
	if err := app.db.QueryRow(`SELECT fingerprint_matched FROM client_curseforge_addons WHERE client_id='pc' AND fingerprint=987654`).Scan(&fingerprintMatched); err != nil || fingerprintMatched != 1 {
		t.Fatalf("fingerprint match was not recorded: matched=%d err=%v", fingerprintMatched, err)
	}

	// A normal inventory sync carries the same local metadata. Its cached
	// fingerprint result should avoid another upstream lookup.
	syncRequest := httptest.NewRequest("POST", "/api/clients/pc/inventory", bytes.NewBufferString(`{"files":[],"curseforge_addons":[{"addonID":42,"name":"Inventory Mod","fileId":43,"fileName":"inventory.zip","downloadUrl":"https://edge.forgecdn.net/files/inventory.zip","fingerprint":987654,"modFolder":"Packages/Local CurseForge","moduleFolderName":"Example.package"}]}`))
	syncRequest.SetPathValue("clientID", "pc")
	syncResponse := httptest.NewRecorder()
	app.reportInventory(syncResponse, syncRequest)
	if syncResponse.Code != http.StatusOK || fingerprintCalls != 1 {
		t.Fatalf("normal inventory status=%d fingerprint calls=%d: %s", syncResponse.Code, fingerprintCalls, syncResponse.Body.String())
	}
}

func TestInventoryAuditKeepsCurseForgeMetadataWhenFingerprintAPIUnavailable(t *testing.T) {
	app := testApp(t)
	if _, err := app.db.Exec(`INSERT INTO clients(id,name) VALUES('pc','Gaming PC')`); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusForbidden)
	}))
	defer api.Close()
	app.api = NewCurseForgeAdapter("test-key", api.Client())
	app.api.baseURL = api.URL
	body := `{"files":[{"path":"Packages/Locally Installed/Example.package","sha256":"` + strings.Repeat("b", 64) + `","enabled":true}],"curseforge_addons":[{"addonID":77,"name":"Optional Local Mod","fileId":78,"fileName":"mod.zip","fingerprint":789,"modFolder":"Packages/Locally Installed"}]}`
	request := httptest.NewRequest("POST", "/api/clients/pc/audit", bytes.NewBufferString(body))
	request.SetPathValue("clientID", "pc")
	response := httptest.NewRecorder()
	app.auditInventory(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("audit failed with optional fingerprint API unavailable: %d: %s", response.Code, response.Body.String())
	}
	var modID sql.NullInt64
	if err := app.db.QueryRow(`SELECT mod_id FROM client_inventory WHERE client_id='pc' AND file_name='Example.package'`).Scan(&modID); err != nil || !modID.Valid {
		t.Fatalf("local addon ID did not identify the inventory file: id=%+v err=%v", modID, err)
	}
}
