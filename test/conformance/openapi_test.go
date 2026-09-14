// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"strings"
	"testing"
)

// doc is a small document in the generator's vocabulary.
const doc = `{
  "openapi": "3.1.0",
  "paths": {
    "/v1/things": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/ThingList"}}}},
                                          "default": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}}}},
    "/v1/things/{name}": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}},
                          "delete": {"responses": {"204": {"description": "gone"}}}},
    "/v1/models/{name}": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}}},
    "/v1/models/{name}/rotate": {"post": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Thing"}}}}}}}
  },
  "components": {"schemas": {
    "Thing": {"type": "object", "required": ["name", "size"], "additionalProperties": false, "properties": {
      "name": {"type": "string", "pattern": "^[a-z]+$"},
      "size": {"type": "integer", "minimum": 1, "maximum": 10},
      "kind": {"type": "string", "const": "Thing"},
      "state": {"type": "string", "enum": ["Open", "Closed"]},
      "at": {"type": "string", "format": "date-time"},
      "flag": {"type": "boolean"},
      "ratio": {"type": "number"},
      "tags": {"type": "array", "items": {"type": "string"}},
      "extra": {"type": "object", "additionalProperties": {"type": "integer"}},
      "any": {"type": "object", "additionalProperties": true},
      "missing": {"$ref": "#/components/schemas/Nowhere"}
    }},
    "ThingList": {"type": "object", "required": ["items"], "properties": {"items": {"type": "array", "items": {"$ref": "#/components/schemas/Thing"}}}},
    "Error": {"type": "object", "required": ["error"], "properties": {"error": {"type": "object", "required": ["code"], "properties": {"code": {"type": "string"}}}}}
  }}
}`

func mustParse(t *testing.T) *document {
	t.Helper()
	d, err := parseDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestParseDocumentRefusesWhatIsNotADocument: bad JSON and a JSON body
// without paths are errors.
func TestParseDocumentRefusesWhatIsNotADocument(t *testing.T) {
	for _, bad := range []string{"{", `{"openapi":"2.0","paths":{"/a":{}}}`, `{"openapi":"3.1.0"}`} {
		if _, err := parseDocument([]byte(bad)); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
	mustParse(t)
}

// TestMatchTemplate: a literal segment matches itself, {name} one
// segment, and under /v1/models every remaining segment; the most
// literal template wins.
func TestMatchTemplate(t *testing.T) {
	d := mustParse(t)
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/things", true}, {"GET", "/v1/things/a", true}, {"GET", "/v1/things/a/b", false},
		{"GET", "/v1/models/a/b/c", true}, {"POST", "/v1/models/a/rotate", true}, {"GET", "/v1/nowhere", false},
		{"DELETE", "/v1/things/a", true}, {"PUT", "/v1/things/a", false},
	} {
		if got := d.operation(tc.method, tc.path) != nil; got != tc.want {
			t.Errorf("%s %s: matched %v", tc.method, tc.path, got)
		}
	}
	if op := d.operation("POST", "/v1/models/a/rotate"); op == nil || op["responses"] == nil {
		t.Error("the rotate template did not win over the model template")
	}
	if _, ok := matchTemplate("/v1/things/{name}", "/v1"); ok {
		t.Error("a short path matched a longer template")
	}
}

// TestValidateHoldsEveryKeyword: each keyword of the vocabulary refuses
// the value outside it and admits the one inside.
func TestValidateHoldsEveryKeyword(t *testing.T) {
	d := mustParse(t)
	good := `{"name":"abc","size":3,"kind":"Thing","state":"Open","at":"2026-09-14T12:00:00Z","flag":true,"ratio":1.5,"tags":["a"],"extra":{"n":1},"any":{"x":[1]}}`
	if p := d.validate("GET", "/v1/things/abc", 200, "application/json", []byte(good)); len(p) != 0 {
		t.Errorf("a good body: %v", p)
	}
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"name":"abc"}`, "required member size"},
		{`{"name":"ABC","size":3}`, "does not match"},
		{`{"name":"abc","size":0}`, "below the minimum"},
		{`{"name":"abc","size":11}`, "above the maximum"},
		{`{"name":"abc","size":1.5}`, "not an integer"},
		{`{"name":"abc","size":"3"}`, "not a number"},
		{`{"name":"abc","size":3,"kind":"Other"}`, "not the constant"},
		{`{"name":"abc","size":3,"state":"Ajar"}`, "not in the enum"},
		{`{"name":"abc","size":3,"at":"yesterday"}`, "not RFC 3339"},
		{`{"name":"abc","size":3,"flag":"yes"}`, "not a boolean"},
		{`{"name":"abc","size":3,"tags":"a"}`, "not an array"},
		{`{"name":"abc","size":3,"tags":[1]}`, "not a string"},
		{`{"name":"abc","size":3,"extra":{"n":"x"}}`, "not a number"},
		{`{"name":"abc","size":3,"other":1}`, "not in the schema"},
		{`{"name":"abc","size":3,"missing":1}`, "resolves to nothing"},
		{`[]`, "not an object"},
		{`{`, "not JSON"},
	} {
		p := d.validate("GET", "/v1/things/abc", 200, "application/json", []byte(tc.body))
		if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), tc.want) {
			t.Errorf("%s: %v, want %q", tc.body, p, tc.want)
		}
	}
}

// TestValidateReadsTheResponseTable: the status picks the response, a
// refusal falls to default, a 204 carries no body, a route the document
// lacks holds a refusal to the Error schema and a success to nothing,
// and a Content-Type the document does not describe is a finding.
func TestValidateReadsTheResponseTable(t *testing.T) {
	d := mustParse(t)
	if p := d.validate("GET", "/v1/things", 404, "application/json", []byte(`{"error":{"code":"not_found"}}`)); len(p) != 0 {
		t.Errorf("a refusal on the default response: %v", p)
	}
	if p := d.validate("GET", "/v1/things", 404, "application/json", []byte(`{"error":{}}`)); len(p) == 0 {
		t.Error("a refusal without a code passed")
	}
	if p := d.validate("DELETE", "/v1/things/a", 204, "", nil); len(p) != 0 {
		t.Errorf("a 204: %v", p)
	}
	if p := d.validate("DELETE", "/v1/things/a", 204, "", []byte("x")); len(p) == 0 {
		t.Error("a 204 with a body passed")
	}
	if p := d.validate("GET", "/v1/things/a", 418, "application/json", []byte(`{}`)); len(p) == 0 {
		t.Error("a status the operation does not describe passed")
	}
	if p := d.validate("GET", "/v1/nowhere", 404, "application/json", []byte(`{"error":{"code":"not_found"}}`)); len(p) != 0 {
		t.Errorf("a refusal on a route the document lacks: %v", p)
	}
	if p := d.validate("GET", "/v1/nowhere", 200, "application/json", []byte(`{}`)); len(p) == 0 {
		t.Error("a success on a route the document lacks passed")
	}
	if p := d.validate("GET", "/v1/things/a", http.StatusOK, "text/plain", []byte(`{"name":"a","size":1}`)); len(p) == 0 {
		t.Error("a text/plain answer passed as application/json")
	}
	list := `{"items":[{"name":"a","size":1},{"name":"b","size":20}]}`
	p := d.validate("GET", "/v1/things", 200, "application/json", []byte(list))
	if len(p) != 1 || !strings.HasPrefix(p[0], "$.items[1].size") {
		t.Errorf("the list: %v", p)
	}
	if indexless(p[0]) != "$.items[].size: 20 is above the maximum" {
		t.Errorf("indexless %q", indexless(p[0]))
	}
}
