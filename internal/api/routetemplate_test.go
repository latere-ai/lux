// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRouteTemplate: every pattern the route table registers but the
// catch-all is the template of a request to it, with a name, a name
// holding slashes, or none; the catch-all, a path that is not clean, and
// a path outside /v1 name no route, so the value never carries the
// caller's path.
func TestRouteTemplate(t *testing.T) {
	h := newHarness(t, nil)
	fill := strings.NewReplacer("{name...}", "org-raw/model-raw", "{name}", "name-raw", "{id}", "id-raw")
	for _, pattern := range h.h.patterns {
		if pattern == "/" {
			continue
		}
		r := httptest.NewRequest(http.MethodGet, fill.Replace(pattern), nil)
		if got := h.h.RouteTemplate(r); got != pattern {
			t.Errorf("RouteTemplate(%s) = %q, want %q", r.URL.Path, got, pattern)
		}
	}
	for _, p := range []string{"/v1/nothing-raw", "/v1//keys", "/v1/keys/../self", "/v1/keys/", "/openai/v1/chat/completions", "/"} {
		r := httptest.NewRequest(http.MethodGet, "http://lux.example"+p, nil)
		if got := h.h.RouteTemplate(r); got != "" {
			t.Errorf("RouteTemplate(%s) = %q, want empty", p, got)
		}
	}
}
