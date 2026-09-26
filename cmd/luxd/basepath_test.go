// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
)

// publicRoutes is every route the public mux registers, one request each,
// with the status it answers when reached: the build identity, the three
// probes, the discovery document, the control plane without a bearer, and
// the four doors without a Key.
var publicRoutes = []struct {
	name   string
	path   string
	status int
}{
	{"the build identity", "/", http.StatusOK},
	{"livez", "/livez", http.StatusOK},
	{"readyz", "/readyz", http.StatusOK},
	{"version", "/version", http.StatusOK},
	{"the discovery document", "/.well-known/lux", http.StatusOK},
	{"the control plane", "/v1/self", http.StatusUnauthorized},
	{"the openai door", "/openai/v1/models", http.StatusUnauthorized},
	{"the anthropic door", "/anthropic/v1/models", http.StatusUnauthorized},
	{"the gemini door", "/gemini/v1beta/models", http.StatusUnauthorized},
	{"the lux door", "/lux/v1/models", http.StatusUnauthorized},
}

// TestBasePathMovesThePublicListener is spec 034's mount: with
// LUX_BASE_PATH set every route of the public listener answers under the
// base, the base itself answers what / answers, and nothing answers at the
// root but the mux's bare 404.
func TestBasePathMovesThePublicListener(t *testing.T) {
	const base = "/v1/models"
	srv := startServe(t, map[string]string{"LUX_BASE_PATH": base, "LUX_PUBLIC_URL": "https://api.example.com" + base})
	for _, rt := range publicRoutes {
		t.Run(rt.name, func(t *testing.T) {
			under := srv.publicURL + base + rt.path
			if rt.path == "/" {
				under = srv.publicURL + base
			}
			if resp, body := do(t, http.MethodGet, under, ""); resp.StatusCode != rt.status {
				t.Errorf("GET %s = %d, want %d: %s", under, resp.StatusCode, rt.status, body)
			}
			resp, body := do(t, http.MethodGet, srv.publicURL+rt.path, "")
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Lux-Error") != "" {
				t.Errorf("GET %s at the root = %d %q, want a bare 404: %s", rt.path, resp.StatusCode, resp.Header.Get("Lux-Error"), body)
			}
		})
	}
	if resp, body := do(t, http.MethodGet, srv.publicURL+base+"/", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s/ = %d: %s", base, resp.StatusCode, body)
	}
	if resp, body := do(t, http.MethodGet, srv.publicURL+"/v1/keys", ""); resp.StatusCode != http.StatusNotFound || resp.Header.Get("Lux-Error") != "" {
		t.Errorf("a path under /v1 outside the base = %d %q: %s", resp.StatusCode, resp.Header.Get("Lux-Error"), body)
	}
}

// TestBasePathEmptyIsTheRoot is spec 034's default: with LUX_BASE_PATH
// unset no wrapper is installed and every route answers at the root, as
// it did before the variable existed.
func TestBasePathEmptyIsTheRoot(t *testing.T) {
	mux := http.NewServeMux()
	if mountAt("", false, mux) != http.Handler(mux) {
		t.Fatal("an empty base path installed a wrapper")
	}
	srv := startServe(t, map[string]string{"LUX_PUBLIC_URL": "https://lux.example.com"})
	for _, rt := range publicRoutes {
		t.Run(rt.name, func(t *testing.T) {
			if resp, body := do(t, http.MethodGet, srv.publicURL+rt.path, ""); resp.StatusCode != rt.status {
				t.Errorf("GET %s = %d, want %d: %s", rt.path, resp.StatusCode, rt.status, body)
			}
		})
	}
}

// noFollow sends one request without following a redirect, so a case
// reads what the listener itself answered.
func noFollow(t *testing.T, method, url string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(data)
}

// TestBasePathReplaceMovesTheControlPlane is spec 040's mount: with
// LUX_BASE_PATH_MODE=replace every control plane route answers at the
// base with its /v1 removed, the doors, the discovery document, /version
// and the build identity answer at the base plus their own path, the
// probes answer on the internal listener alone, and nothing answers at
// the root or at the doubled address spec 034 serves.
func TestBasePathReplaceMovesTheControlPlane(t *testing.T) {
	const base = "/v1/models"
	const public = "https://api.example.com" + base
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	srv := startServe(t, map[string]string{
		"LUX_OIDC_ISSUERS": iss.URL(), "LUX_BASE_PATH": base, "LUX_BASE_PATH_MODE": "replace", "LUX_PUBLIC_URL": public,
	})
	defer srv.stop()
	for _, line := range []string{
		"luxd: control plane at " + public + " on the public listener\n",
		"luxd: the public listener answers under " + base + ", in the place of the control plane's /v1\n",
	} {
		if !strings.Contains(srv.out.String(), line) {
			t.Errorf("the start-up output lacks %q:\n%s", line, srv.out.String())
		}
	}
	bearer := []string{"Authorization", "Bearer " + iss.Mint(issuertest.Claims{Sub: "alice"})}
	under := srv.publicURL + base
	// envelope begins the control plane's error details, which a door's
	// dialect envelope does not carry: a case holding it was answered by
	// the control plane.
	const envelope = `"details":{"detail":"`
	if resp, body := do(t, http.MethodPut, under+"/budgets/team", `{"spec": {"amount": "50"}}`, bearer...); resp.StatusCode != http.StatusCreated {
		t.Fatalf("apply under the base = %d: %s", resp.StatusCode, body)
	}

	for _, rt := range []struct {
		name   string
		path   string
		bearer bool
		status int
		holds  string // what the body contains, "" for no check
		code   string // the envelope's code, "" for a body that is not one
	}{
		{"the build identity at the base", "", false, http.StatusOK, "luxd dev (", ""},
		{"the build identity at the base with its slash", "/", false, http.StatusOK, "luxd dev (", ""},
		{"version", "/version", false, http.StatusOK, `"version":"dev"`, ""},
		{"the discovery document", "/.well-known/lux", false, http.StatusOK, `"api":"` + public + `","openapi":"` + public + `/openapi.json"`, ""},
		{"the served document", "/openapi.json", false, http.StatusOK, `"` + base + `/keys/{name}/rotate":`, ""},
		{"self without a bearer", "/self", false, http.StatusUnauthorized, envelope, "unauthenticated"},
		{"self", "/self", true, http.StatusOK, `"policy":"owner"`, ""},
		{"a Budget", "/budgets/team", true, http.StatusOK, `"name":"team"`, ""},
		{"a Budget named with an escape", "/budgets/te%61m", true, http.StatusOK, `"name":"team"`, ""},
		{"the Budgets", "/budgets", true, http.StatusOK, `"items"`, ""},
		{"the Keys", "/keys", true, http.StatusOK, `"items"`, ""},
		{"the Providers", "/providers", true, http.StatusOK, `"items"`, ""},
		{"the Models", "/models", true, http.StatusOK, `"items"`, ""},
		// A Model named like a door is the control plane's to answer.
		{"a Model named like a door", "/models/openai/gpt-5", true, http.StatusNotFound, envelope, "not_found"},
		{"usage", "/usage", true, http.StatusOK, `"items"`, ""},
		{"requests", "/requests", true, http.StatusOK, `"items"`, ""},
		// The doubled address of spec 034 is not an alias.
		{"the prefix address", "/v1/self", true, http.StatusNotFound, envelope + "GET /v1/v1/self is not in the route table", "not_found"},
		// The probes are the internal listener's: under the base they are
		// paths the control plane does not route.
		{"livez", "/livez", true, http.StatusNotFound, envelope + "GET /v1/livez is not in the route table", "not_found"},
		{"readyz", "/readyz", true, http.StatusNotFound, envelope + "GET /v1/readyz is not in the route table", "not_found"},
	} {
		t.Run(rt.name, func(t *testing.T) {
			var headers []string
			if rt.bearer {
				headers = bearer
			}
			resp, body := noFollow(t, http.MethodGet, under+rt.path, headers...)
			if resp.StatusCode != rt.status || !strings.Contains(body, rt.holds) {
				t.Fatalf("GET %s = %d %.200q, want %d holding %q", base+rt.path, resp.StatusCode, body, rt.status, rt.holds)
			}
			if rt.code != "" {
				if got := errorCode(t, body); got != rt.code {
					t.Errorf("GET %s answered the code %q, want %q", base+rt.path, got, rt.code)
				}
			}
		})
	}
	for _, door := range []string{"/openai/v1/models", "/anthropic/v1/models", "/gemini/v1beta/models", "/lux/v1/models"} {
		if resp, body := do(t, http.MethodGet, under+door, ""); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Lux-Error") != "unauthenticated" {
			t.Errorf("the door %s under the base = %d %q: %s", door, resp.StatusCode, resp.Header.Get("Lux-Error"), body)
		}
	}
	if resp, body := do(t, http.MethodGet, under+"/budgets/team", "", bearer...); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"team"`) {
		t.Errorf("the Budget applied under the base = %d: %s", resp.StatusCode, body)
	}
	for _, p := range []string{"/", "/v1/self", "/v1/budgets/team", "/.well-known/lux", "/openai/v1/models", "/version", "/livez", "/readyz", "/v1/modelsx/self"} {
		resp, body := noFollow(t, http.MethodGet, srv.publicURL+p, bearer...)
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Lux-Error") != "" || strings.HasPrefix(body, "{") {
			t.Errorf("GET %s at the root = %d %q, want a bare 404: %s", p, resp.StatusCode, resp.Header.Get("Lux-Error"), body)
		}
	}
	for _, p := range []string{"/livez", "/readyz"} {
		if code, body := get(t, srv.internalURL+p); code != http.StatusOK || body != "ok\n" {
			t.Errorf("the internal listener answered %s with %d %q", p, code, body)
		}
	}
	if code, _ := get(t, srv.internalURL+"/version"); code != http.StatusOK {
		t.Errorf("the internal listener answered /version with %d", code)
	}
}
