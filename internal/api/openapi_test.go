// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// committedOpenAPI reads api/openapi.yaml.
func committedOpenAPI(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(specsDir(t), "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading the committed document: %v; run go run ./tools/apidoc", err)
	}
	return data
}

// TestOpenAPIIsCurrent: the committed api/openapi.yaml equals a fresh
// generation byte for byte, so a route or a field added without
// regenerating is a red build.
func TestOpenAPIIsCurrent(t *testing.T) {
	fresh := OpenAPIYAML()
	if committed := committedOpenAPI(t); !bytes.Equal(committed, fresh) {
		t.Fatalf("api/openapi.yaml differs from a fresh generation; run go run ./tools/apidoc\n%s", excerptDiff(committed, fresh))
	}
	if again := OpenAPIYAML(); !bytes.Equal(fresh, again) {
		t.Fatal("two generations differ")
	}
}

// excerptDiff is the first differing line of two documents.
func excerptDiff(a, b []byte) string {
	al, bl := strings.Split(string(a), "\n"), strings.Split(string(b), "\n")
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + itoa(i+1) + ":\n  committed: " + x + "\n  fresh:     " + y
		}
	}
	return "no differing line"
}

// TestOpenAPIServedMatchesCommitted: GET /v1/openapi.json is the
// committed YAML as JSON, member for member; the document names every
// route of the table, every kind with apiVersion and kind, the
// write-only values as writeOnly, the error table, and no example of a
// Key value.
func TestOpenAPIServedMatchesCommitted(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.request(http.MethodGet, "/v1/openapi.json", "", "Authorization", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var served any
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	var fromYAML any
	if err := yaml.Unmarshal(committedOpenAPI(t), &fromYAML); err != nil {
		t.Fatal(err)
	}
	// Numbers decode differently from YAML and from JSON; a JSON round
	// trip of the YAML's value gives both one shape.
	normalised, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	var committed any
	_ = json.Unmarshal(normalised, &committed)
	if !reflect.DeepEqual(served, committed) {
		t.Fatalf("the served document differs from the committed one")
	}
	doc := served.(map[string]any)
	paths := doc["paths"].(map[string]any)
	for _, p := range []string{"/v1/providers", "/v1/providers/{name}", "/v1/models", "/v1/models/{name}", "/v1/keys", "/v1/keys/{name}", "/v1/keys/{name}/rotate", "/v1/budgets", "/v1/budgets/{name}", "/v1/usage", "/v1/requests", "/v1/self", "/v1/openapi.json", "/.well-known/lux"} {
		if paths[p] == nil {
			t.Errorf("no path %s", p)
		}
	}
	if op := paths["/v1/requests"].(map[string]any)["get"].(map[string]any); op["x-lux-action"] != "usage.read" {
		t.Errorf("the requests action %v", op["x-lux-action"])
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, kind := range []string{"Provider", "Model", "Key", "Budget"} {
		s := schemas[kind].(map[string]any)
		props := s["properties"].(map[string]any)
		if props["apiVersion"].(map[string]any)["const"] != "lux.latere.ai/v1beta1" || props["kind"].(map[string]any)["const"] != kind {
			t.Errorf("%s lacks apiVersion and kind: %v", kind, props)
		}
		if props["status"].(map[string]any)["readOnly"] != true {
			t.Errorf("%s status is not read-only", kind)
		}
		if schemas[kind+"List"] == nil {
			t.Errorf("no %sList", kind)
		}
	}
	if v := schemas["KeySpec"].(map[string]any)["properties"].(map[string]any)["value"].(map[string]any); v["writeOnly"] != true {
		t.Errorf("KeySpec.value %v", v)
	}
	if v := schemas["Credential"].(map[string]any)["properties"].(map[string]any)["value"].(map[string]any); v["writeOnly"] != true {
		t.Errorf("Credential.value %v", v)
	}
	if enum := schemas["Error"].(map[string]any)["properties"].(map[string]any)["error"].(map[string]any)["properties"].(map[string]any)["code"].(map[string]any)["enum"].([]any); len(enum) != len(codes) {
		t.Errorf("the Error code enum has %d entries, the table %d", len(enum), len(codes))
	}
	if errs := doc["x-lux-errors"].([]any); len(errs) != len(codes) || errs[0].(map[string]any)["code"] != "malformed_body" {
		t.Errorf("x-lux-errors %v", errs)
	}
	if strings.Contains(rec.Body.String(), "lux_") && strings.Contains(rec.Body.String(), `"example"`) {
		t.Error("the document carries a Key value example")
	}
	if op := paths["/v1/keys/{name}"].(map[string]any)["put"].(map[string]any); op["x-lux-action"] != "key.create or key.update" {
		t.Errorf("apply action %v", op["x-lux-action"])
	}
}

// TestOrderedJSON: an ordered object writes its members in order and
// set replaces in place.
func TestOrderedJSON(t *testing.T) {
	o := obj("b", 1, "a", []string{"x"}, "c", obj("z", true)).set("a", "y").set("d", nil)
	data, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"b":1,"a":"y","c":{"z":true},"d":null}` {
		t.Errorf("%s", data)
	}
	if _, err := json.Marshal(obj("f", func() {})); err == nil {
		t.Error("a value that does not marshal was accepted")
	}
	y, _ := yaml.Marshal(toYAML(obj("b", 1, "a", []any{obj("k", "v")}, "s", []string{"p", "q"})))
	if string(y) != "b: 1\na:\n- k: v\ns:\n- p\n- q\n" {
		t.Errorf("%q", y)
	}
}

// TestSchemaOfEveryShape: the generator has a shape for every Go type
// the kinds use and panics on one it does not, so a new field of a new
// type is a decision and not a silent hole.
func TestSchemaOfEveryShape(t *testing.T) {
	s := newSchemas()
	if got := s.of(reflect.TypeFor[map[string]any]()); got.set("x", 1) == nil {
		t.Error()
	}
	if got := s.of(reflect.TypeFor[*int]()); len(got) != 1 || got[0].value != "integer" {
		t.Errorf("*int: %v", got)
	}
	if got := s.of(reflect.TypeFor[[]bool]()); got[0].value != "array" {
		t.Errorf("[]bool: %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("a channel has a shape")
		}
	}()
	s.of(reflect.TypeFor[chan int]())
}
