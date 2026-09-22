// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"testing"
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
	if mountAt("", mux) != http.Handler(mux) {
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
