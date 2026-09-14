// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"testing"
)

// crafted is one answer built by hand for the readers below.
func crafted(status int, id string, headers map[string]string, body string) *response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &response{Status: status, Header: h, Body: []byte(body), ID: id}
}

// TestDoorCodeReadsEveryShape: the door reader accepts each dialect's
// own error shape with the code in the shape's member and in Lux-Error,
// and fails on a body whose members disagree with the header, a shape
// with a member missing, a Content-Type that is not JSON, a message that
// is not one sentence, a status outside the table, and no header at
// all.
func TestDoorCodeReadsEveryShape(t *testing.T) {
	c := &client{rep: newReport("crafted"), sentences: map[string]string{}}
	json := map[string]string{"Content-Type": "application/json", "Lux-Error": "not_found"}
	good := map[string]string{
		"openai":    `{"error":{"message":"There is no such object.","type":"not_found","code":"not_found","param":null}}`,
		"anthropic": `{"type":"error","error":{"type":"not_found","message":"There is no such object."},"request_id":"req_1"}`,
		"gemini":    `{"error":{"code":404,"message":"There is no such object.","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"not_found","domain":"lux"}]}}`,
		"lux":       `{"error":{"code":"not_found","message":"There is no such object.","details":{"request_id":"req_1"}}}`,
	}
	for dialect, body := range good {
		if f := drive(t, dialect, func(t testing.TB) { c.expectDoor(t, dialect, crafted(404, "req_1", json, body), "not_found") }); f.Failed() {
			t.Errorf("%s: a good refusal failed:\n%s", dialect, f.output())
		}
	}
	bad := map[string]string{
		"openai":    `{"error":{"message":"Two. Sentences.","type":"other","code":"other"}}`,
		"anthropic": `{"type":"nope","error":{"type":"other","message":"x"},"request_id":"req_2"}`,
		"gemini":    `{"error":{"code":500,"message":"x","status":"","details":[{"@type":"x","reason":"other","domain":"y"}]}}`,
		"lux":       `{"error":{"code":"other","message":"x","details":{"request_id":"req_2"}}}`,
	}
	for dialect, body := range bad {
		if f := drive(t, dialect, func(t testing.TB) { c.expectDoor(t, dialect, crafted(404, "req_1", json, body), "not_found") }); !f.Failed() {
			t.Errorf("%s: a bad refusal passed", dialect)
		}
	}
	if f := drive(t, "gemini", func(t testing.TB) {
		c.doorCode(t, "gemini", crafted(404, "req_1", json, `{"error":{"code":404,"message":"There is no such object.","status":"NOT_FOUND"}}`))
	}); !f.Failed() {
		t.Error("a gemini refusal without details passed")
	}
	if f := drive(t, "no header", func(t testing.TB) { c.doorCode(t, "openai", crafted(500, "req_1", nil, `{}`)) }); !f.Failed() {
		t.Error("a refusal without Lux-Error passed")
	}
	if f := drive(t, "text", func(t testing.TB) {
		c.doorCode(t, "openai", crafted(404, "req_1", map[string]string{"Content-Type": "text/plain", "Lux-Error": "not_found"}, good["openai"]))
	}); !f.Failed() {
		t.Error("a text/plain refusal passed")
	}
	if f := drive(t, "status", func(t testing.TB) { c.doorCode(t, "openai", crafted(418, "req_1", json, good["openai"])) }); !f.Failed() {
		t.Error("a status outside the table passed")
	}
}

// TestEnvelopeReadsTheRefusal: the /v1 reader holds request_id to
// Lux-Request-Id, the code to Lux-Error and to the status the table
// gives it, the message to one sentence, and one code to one sentence
// across a run.
func TestEnvelopeReadsTheRefusal(t *testing.T) {
	c := &client{rep: newReport("crafted"), sentences: map[string]string{}}
	json := map[string]string{"Content-Type": "application/json", "Lux-Error": "not_found"}
	good := `{"error":{"code":"not_found","message":"There is no such object.","details":{"request_id":"req_1","paths":["a"],"detail":"d"}}}`
	f := drive(t, "good", func(t testing.TB) {
		e := c.expect(t, crafted(404, "req_1", json, good), "not_found")
		if e.requestID != "req_1" || len(e.paths) != 1 || e.detail != "d" {
			t.Errorf("envelope %+v", e)
		}
	})
	if f.Failed() {
		t.Errorf("a good envelope failed:\n%s", f.output())
	}
	for name, resp := range map[string]*response{
		"another id":     crafted(404, "req_2", json, good),
		"another header": crafted(404, "req_1", map[string]string{"Content-Type": "application/json", "Lux-Error": "other"}, good),
		"another status": crafted(409, "req_1", json, good),
		"two sentences":  crafted(404, "req_1", json, `{"error":{"code":"not_found","message":"Two. Sentences.","details":{"request_id":"req_1"}}}`),
		"no envelope":    crafted(404, "req_1", json, `{"items":[]}`),
		"another code":   crafted(404, "req_1", json, `{"error":{"code":"other","message":"Other.","details":{"request_id":"req_1"}}}`),
	} {
		if f := drive(t, name, func(t testing.TB) { c.expect(t, resp, "not_found") }); !f.Failed() {
			t.Errorf("%s passed", name)
		}
	}
	if f := drive(t, "etag", func(t testing.TB) {
		etagOf(t, crafted(200, "req_1", map[string]string{"ETag": `"2"`}, `{"status":{"version":1}}`))
	}); !f.Failed() {
		t.Error("an ETag that is not the version passed")
	}
	if f := drive(t, "items", func(t testing.TB) { c.items(t, crafted(500, "req_1", nil, `{}`)) }); !f.Failed() {
		t.Error("a list that is not a 200 passed")
	}
	if f := drive(t, "listed", func(t testing.TB) {
		listedNames(t, "anthropic", crafted(200, "req_1", nil, `{"data":[{"type":"x","id":"a","display_name":"b"}],"has_more":true,"first_id":"z","last_id":"z"}`))
		listedNames(t, "gemini", crafted(200, "req_1", nil, `{"models":[{"name":"models/a"}]}`))
		listedNames(t, "openai", crafted(200, "req_1", nil, `{"object":"x","data":[{"id":"a","object":"y","owned_by":"z"}]}`))
	}); !f.Failed() {
		t.Error("a list outside the dialect's shape passed")
	}
	if f := drive(t, "listed status", func(t testing.TB) { listedNames(t, "openai", crafted(500, "req_1", nil, `{}`)) }); !f.Failed() {
		t.Error("a list that is not a 200 passed")
	}
}
