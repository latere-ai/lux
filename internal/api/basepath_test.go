// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestRoute is spec 040's one rule: the root leaves every route as it
// is, prefix puts the base in front of every route, and replace puts it
// in the place of a control plane route's /v1 and in front of the rest.
func TestRoute(t *testing.T) {
	const base = "/v1/models"
	for _, tc := range []struct {
		base    string
		replace bool
		route   string
		want    string
	}{
		{"", false, "/v1/keys", "/v1/keys"},
		{"", true, "/v1/keys", "/v1/keys"},
		{base, false, "/v1/keys", "/v1/models/v1/keys"},
		{base, false, "/v1/models/openai/gpt-5", "/v1/models/v1/models/openai/gpt-5"},
		{base, false, "/.well-known/lux", "/v1/models/.well-known/lux"},
		{base, true, "/v1/keys", "/v1/models/keys"},
		{base, true, "/v1/keys/{name}/rotate", "/v1/models/keys/{name}/rotate"},
		{base, true, "/v1/models/openai/gpt-5", "/v1/models/models/openai/gpt-5"},
		{base, true, "/v1/openapi.json", "/v1/models/openapi.json"},
		{base, true, "/v1", "/v1/models"},
		{base, true, "/.well-known/lux", "/v1/models/.well-known/lux"},
		{base, true, "/openai/v1/chat/completions", "/v1/models/openai/v1/chat/completions"},
		{base, true, "/version", "/v1/models/version"},
		// A segment that begins with v1 is not the version segment.
		{base, true, "/v1beta/models", "/v1/models/v1beta/models"},
		{"/lux/v1", true, "/v1/self", "/lux/v1/self"},
	} {
		if got := Route(tc.base, tc.replace, tc.route); got != tc.want {
			t.Errorf("Route(%q, %v, %q) = %q, want %q", tc.base, tc.replace, tc.route, got, tc.want)
		}
	}
}

// TestReplacedControlPlaneShadowsNoOtherRoute is spec 040's collision
// guard: under replace the first segment after the base selects either a
// control plane route or one of the routes that keep their own path, the
// four doors, the discovery document, and the build identity. It reads
// the control plane's first segments from the route table, so a route
// added under /v1 that would take one of those names fails here.
func TestReplacedControlPlaneShadowsNoOtherRoute(t *testing.T) {
	h := newHarness(t, nil)
	kept := []string{".well-known", "version", "livez", "readyz"}
	for _, d := range []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux} {
		kept = append(kept, string(d))
	}
	var first []string
	for _, p := range h.h.patterns {
		rest, ok := strings.CutPrefix(p, versionSegment+"/")
		if !ok {
			continue
		}
		seg, _, _ := strings.Cut(rest, "/")
		if seg == "" {
			t.Errorf("the route %s has no segment after /v1, which under replace is the base itself and its build identity", p)
			continue
		}
		if slices.Contains(kept, seg) {
			t.Errorf("the route %s would answer at <base>/%s under replace, where a route that keeps its own path answers", p, seg)
		}
		first = append(first, seg)
	}
	for _, want := range []string{"providers", "models", "keys", "budgets", "usage", "requests", "self", "openapi.json"} {
		if !slices.Contains(first, want) {
			t.Errorf("the route table has no route under /v1/%s; the guard is not reading it", want)
		}
	}
}

// TestServedDocumentUnderReplace is spec 040's served document: every
// path is the address Route gives it under replace, so no path carries
// the doubled version segment, servers names the public URL, and nothing
// outside paths and servers differs from the committed document.
func TestServedDocumentUnderReplace(t *testing.T) {
	const base = "/v1/models"
	public, err := url.Parse("https://api.example.com" + base)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.BasePath, o.BaseReplacesV1, o.PublicURL = base, true, public })
	rec := h.request(http.MethodGet, "/v1/openapi.json", "", "Authorization", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var served, committed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(openAPIJSON(), &committed); err != nil {
		t.Fatal(err)
	}
	if want := []any{map[string]any{"url": public.String()}}; !reflect.DeepEqual(served["servers"], want) {
		t.Errorf("servers %v, want %v", served["servers"], want)
	}
	paths, _ := served["paths"].(map[string]any)
	rooted, _ := committed["paths"].(map[string]any)
	if len(paths) != len(rooted) || len(paths) == 0 {
		t.Fatalf("%d paths served, %d committed", len(paths), len(rooted))
	}
	for p, item := range rooted {
		moved := Route(base, true, p)
		if strings.HasPrefix(moved, base+"/v1/") || !strings.HasPrefix(moved, base+"/") {
			t.Errorf("%s is served as %s", p, moved)
		}
		if !reflect.DeepEqual(paths[moved], item) {
			t.Errorf("the served document lacks %s as the committed %s", moved, p)
		}
	}
	for _, want := range []string{base + "/keys", base + "/models/{name}", base + "/openapi.json", base + "/.well-known/lux"} {
		if paths[want] == nil {
			t.Errorf("the served document names no %s", want)
		}
	}
	delete(served, "servers")
	delete(served, "paths")
	delete(committed, "paths")
	if !reflect.DeepEqual(served, committed) {
		t.Error("the served document differs from the committed one outside paths and servers")
	}
}

// TestWellKnownUnderReplace is spec 040's discovery document: under
// replace the control plane is the public URL itself and the document
// beside it is under that address, while each door is the public URL
// plus the dialect under both mounts, which is how a client tells them
// apart.
func TestWellKnownUnderReplace(t *testing.T) {
	const base = "/v1/models"
	public, err := url.Parse("https://api.example.com" + base)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		replace bool
		api     string
	}{
		{"prefix", false, public.String() + "/v1"},
		{"replace", true, public.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.BasePath, o.BaseReplacesV1, o.PublicURL = base, tc.replace, public })
			doc := body(t, h.request(http.MethodGet, "/.well-known/lux", "", "Authorization", ""))
			if doc["api"] != tc.api || doc["openapi"] != tc.api+"/openapi.json" {
				t.Errorf("api %v openapi %v, want %s and %s/openapi.json", doc["api"], doc["openapi"], tc.api, tc.api)
			}
			doors, _ := doc["doors"].(map[string]any)
			for _, d := range []string{"openai", "anthropic", "gemini", "lux"} {
				if want := public.String() + "/" + d; doors[d] != want {
					t.Errorf("the %s door is %v, want %s", d, doors[d], want)
				}
			}
		})
	}
}
