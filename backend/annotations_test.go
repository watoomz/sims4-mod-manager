package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestAnnotationUpsertAndClear(t *testing.T) {
	app := testApp(t)
	request := func(value string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/mods/1/annotations/flag", bytes.NewBufferString(`{"value":"`+value+`"}`))
		r.SetPathValue("modID", "1")
		r.SetPathValue("kind", "flag")
		w := httptest.NewRecorder()
		app.setAnnotation(w, r)
		return w
	}
	if got := request("Needs review"); got.Code != 200 {
		t.Fatalf("set flag returned %d: %s", got.Code, got.Body.String())
	}
	w := httptest.NewRecorder()
	app.listAnnotations(w, httptest.NewRequest("GET", "/api/annotations", nil))
	var items []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["value"] != "Needs review" {
		t.Fatalf("unexpected annotations: %#v", items)
	}
	if got := request(""); got.Code != 200 {
		t.Fatalf("clear flag returned %d: %s", got.Code, got.Body.String())
	}
	w = httptest.NewRecorder()
	app.listAnnotations(w, httptest.NewRequest("GET", "/api/annotations", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected cleared annotation, got %#v", items)
	}
}
