// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestKeyDoorsRoundTrip: spec.doors is applied and read back through
// /v1 as written, is mutable, is written only when set, and an empty
// list or an unknown door is refused invalid_field at its path before
// anything is stored (spec 049).
func TestKeyDoorsRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	readDoors := func() any {
		t.Helper()
		rec := h.request(http.MethodGet, "/v1/keys/zr", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		return body(t, rec)["spec"].(map[string]any)["doors"]
	}
	with := func(doors string) string {
		return strings.Replace(keyJSON, `"budget"`, `"doors": `+doors+`, "budget"`, 1)
	}
	if rec := h.request(http.MethodPut, "/v1/keys/zr", keyJSON); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := readDoors(); got != nil {
		t.Errorf("a Key without doors reads %v", got)
	}
	for _, c := range []struct {
		doors string
		want  any
	}{
		{`["lux"]`, []any{"lux"}},
		{`["anthropic", "openai"]`, []any{"anthropic", "openai"}},
	} {
		if rec := h.request(http.MethodPut, "/v1/keys/zr", with(c.doors)); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.doors, rec.Code, rec.Body.String())
		}
		if got := readDoors(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s reads %v, want %v", c.doors, got, c.want)
		}
	}
	for _, c := range []struct{ doors, path string }{
		{`[]`, "spec.doors"},
		{`["azure"]`, "spec.doors[0]"},
		{`["lux", "lux"]`, "spec.doors[1]"},
	} {
		rec := h.request(http.MethodPut, "/v1/keys/zr", with(c.doors))
		if d := wantCode(t, rec, CodeInvalidField); !reflect.DeepEqual(paths(d), []string{c.path}) {
			t.Errorf("%s: paths %v", c.doors, paths(d))
		}
	}
	if got := readDoors(); !reflect.DeepEqual(got, []any{"anthropic", "openai"}) {
		t.Errorf("a refused apply changed the stored doors to %v", got)
	}
	if rec := h.request(http.MethodPut, "/v1/keys/zr", keyJSON); rec.Code != http.StatusOK {
		t.Fatalf("update without: %d %s", rec.Code, rec.Body.String())
	}
	if got := readDoors(); got != nil {
		t.Errorf("after an apply without doors it reads %v", got)
	}
}
