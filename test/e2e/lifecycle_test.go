// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/test/stubs/provider"
)

// TestE2ELifecycle starts luxd as a process on port 0 over the stubs and
// covers every door, a translation between two of them, the four kinds'
// grammar through /v1, the jobs reaching a stub, and a clean shutdown:
// readiness answers 503 while draining and the process exits 0.
func TestE2ELifecycle(t *testing.T) {
	// The health job publishes a new Model's availability at its tick, so
	// the door's model list fills within the shortest interval allowed.
	s := newStack(t, map[string]string{"LUX_HEALTH_INTERVAL": "5s"})
	// The openai Provider keeps the jobs on, so discovery and the probe
	// reach its stub; the other three keep them off.
	s.provider(t, "openai", "openai", true, "")
	s.model(t, "chat", "openai", "gpt-stub", true)
	s.provider(t, "anthropic", "anthropic", false, "")
	s.model(t, "claude", "anthropic", "claude-stub", true)
	s.provider(t, "gemini", "gemini", false, "")
	s.model(t, "gem", "gemini", "gemini-stub", false)
	s.provider(t, "lux", "lux", false, "")
	s.model(t, "lx", "lux", "lux-stub", false)
	s.apply(t, "budget", "team", budgetYAML("team"))
	key := s.key(t, "dev", "*")

	// Every door, passthrough.
	for _, tc := range []struct {
		door, path, body, want string
		header                 http.Header
	}{
		{"openai", "/openai/v1/chat/completions", `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`, provider.Content("openai", "gpt-stub", "hi"), jsonBearer(key)},
		{"anthropic", "/anthropic/v1/messages", `{"model":"claude","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, provider.Content("anthropic", "claude-stub", "hi"), http.Header{"x-api-key": {key}, "Content-Type": {"application/json"}}},
		{"gemini", "/gemini/v1beta/models/gem:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, provider.Content("gemini", "gemini-stub", "hi"), http.Header{"x-goog-api-key": {key}, "Content-Type": {"application/json"}}},
		{"lux", "/lux/v1/generate", `{"model":"lx","messages":[{"role":"user","blocks":[{"type":"text","text":"hi"}]}]}`, provider.Content("lux", "lux-stub", "hi"), jsonBearer(key)},
	} {
		resp := do(t, http.MethodPost, s.gw.public+tc.path, tc.header, tc.body)
		if resp.status != http.StatusOK || !strings.Contains(string(resp.body), tc.want) {
			t.Fatalf("%s door: %d %s %s", tc.door, resp.status, resp.code(), resp.body)
		}
	}
	// A translation: the /openai door toward the anthropic target, whole
	// and streamed, with the Model's name written back.
	resp := s.chat(t, key, "claude", "hi", false)
	if resp.status != http.StatusOK || !strings.Contains(string(resp.body), provider.Content("anthropic", "claude-stub", "hi")) || !strings.Contains(string(resp.body), `"model":"claude"`) {
		t.Fatalf("translated: %d %s %s", resp.status, resp.code(), resp.body)
	}
	resp = s.chat(t, key, "claude", "hi", true)
	if resp.status != http.StatusOK || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/event-stream") || !strings.HasSuffix(string(resp.body), "data: [DONE]\n\n") {
		t.Fatalf("translated stream: %d %s\n%s", resp.status, resp.header.Get("Content-Type"), resp.body)
	}
	// The door's own model list is served from the catalogue, once the
	// health job has published each Model's availability.
	eventually(t, "the door's model list carrying the declared Models", 15*time.Second, func() bool {
		resp = do(t, http.MethodGet, s.gw.public+"/openai/v1/models", bearer(key), "")
		return resp.status == http.StatusOK && strings.Contains(string(resp.body), `"id":"chat"`) && strings.Contains(string(resp.body), `"id":"lx"`)
	})

	// The four kinds' grammar: list, read with its ETag, update under
	// If-Match, and delete, plus a Key rotation and /v1/self.
	for _, k := range []struct{ plural, name string }{{"providers", "gemini"}, {"models", "gem"}, {"keys", "dev"}, {"budgets", "team"}} {
		list := do(t, http.MethodGet, s.gw.public+"/v1/"+k.plural, bearer(s.token), "")
		if list.status != http.StatusOK || !strings.Contains(string(list.body), `"name":"`+k.name+`"`) {
			t.Fatalf("GET /v1/%s = %d %s", k.plural, list.status, list.body)
		}
		read := do(t, http.MethodGet, s.gw.public+"/v1/"+k.plural+"/"+k.name, bearer(s.token), "")
		if read.status != http.StatusOK || read.header.Get("ETag") == "" {
			t.Fatalf("GET /v1/%s/%s = %d ETag %q", k.plural, k.name, read.status, read.header.Get("ETag"))
		}
		h := bearer(s.token)
		h.Set("Content-Type", "application/json")
		h.Set("If-Match", read.header.Get("ETag"))
		update := do(t, http.MethodPut, s.gw.public+"/v1/"+k.plural+"/"+k.name, h, string(read.body))
		if update.status != http.StatusOK {
			t.Fatalf("PUT /v1/%s/%s under If-Match = %d %s", k.plural, k.name, update.status, update.body)
		}
		h.Set("If-Match", `"1"`)
		if stale := do(t, http.MethodPut, s.gw.public+"/v1/"+k.plural+"/"+k.name, h, string(read.body)); stale.status != http.StatusConflict || stale.code() != "conflict" {
			t.Fatalf("PUT /v1/%s/%s under a stale If-Match = %d %s %s", k.plural, k.name, stale.status, stale.code(), stale.body)
		}
	}
	rotated := do(t, http.MethodPost, s.gw.public+"/v1/keys/dev/rotate", bearer(s.token), "")
	if rotated.status != http.StatusOK || !strings.Contains(string(rotated.body), `"value":"lux_`) {
		t.Fatalf("POST /v1/keys/dev/rotate = %d %s", rotated.status, rotated.body)
	}
	if resp := do(t, http.MethodGet, s.gw.public+"/v1/self", bearer(s.token), ""); resp.status != http.StatusOK || !strings.Contains(string(resp.body), `"policy":"authorizer"`) {
		t.Fatalf("GET /v1/self = %d %s", resp.status, resp.body)
	}
	for _, k := range []struct{ plural, name string }{{"keys", "dev"}, {"budgets", "team"}, {"models", "gem"}, {"providers", "gemini"}} {
		if resp := do(t, http.MethodDelete, s.gw.public+"/v1/"+k.plural+"/"+k.name, bearer(s.token), ""); resp.status != http.StatusNoContent {
			t.Fatalf("DELETE /v1/%s/%s = %d %s", k.plural, k.name, resp.status, resp.body)
		}
		if resp := do(t, http.MethodGet, s.gw.public+"/v1/"+k.plural+"/"+k.name, bearer(s.token), ""); resp.status != http.StatusNotFound {
			t.Fatalf("GET after DELETE /v1/%s/%s = %d", k.plural, k.name, resp.status)
		}
	}

	// The jobs reached the openai stub's models route.
	eventually(t, "discovery or the probe listing the openai stub", 10*time.Second, func() bool {
		for _, r := range s.received(t, "openai") {
			if r.Method == http.MethodGet && r.Path == "/v1/models" {
				return true
			}
		}
		return false
	})

	// A clean shutdown: readiness fails while draining, the process
	// exits 0, and the stubs are left alone.
	if resp := do(t, http.MethodGet, s.gw.internal+"/readyz", nil, ""); resp.status != http.StatusOK {
		t.Fatalf("GET /readyz before the stop = %d", resp.status)
	}
	stopped := make(chan int, 1)
	go func() { stopped <- s.gw.stop(t, 90*time.Second) }()
	eventually(t, "readiness failing while draining", 5*time.Second, func() bool {
		if s.gw.exited() {
			return true
		}
		return do(t, http.MethodGet, s.gw.internal+"/readyz", nil, "").status == http.StatusServiceUnavailable
	})
	if code := <-stopped; code != 0 {
		t.Fatalf("luxd exited %d on SIGTERM; stderr:\n%s", code, s.gw.errOut.String())
	}
	if strings.Contains(s.gw.errOut.String(), "level=ERROR") {
		t.Fatalf("luxd logged an error:\n%s", s.gw.errOut.String())
	}
	if resp := do(t, http.MethodGet, s.stubs.urls["openai"]+"/_received", nil, ""); resp.status != http.StatusOK {
		t.Fatalf("the stubs did not survive luxd's stop: %d", resp.status)
	}
}
