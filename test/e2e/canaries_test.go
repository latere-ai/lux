// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/lux/test/stubs/provider"
)

// TestE2EProviderCredentialNeverLeavesTheGateway is the process half of
// the custody canary: the request the stub received carries the
// Provider's credential and not the caller's Key, no answer of the
// gateway carries the credential, and a Provider holding the wrong
// credential is refused by the stub and reported as the provider's
// error, so the positive case is proved beside the negative one.
func TestE2EProviderCredentialNeverLeavesTheGateway(t *testing.T) {
	s := newStack(t, nil)
	key := s.fixtures(t)
	s.provider(t, "wrong", "openai", false, "not-the-stub-credential")
	s.model(t, "wrong-model", "wrong", "gpt-stub", false)

	resp := s.chat(t, key, "openai-model", "hi", false)
	if resp.status != http.StatusOK {
		t.Fatalf("chat = %d %s %s", resp.status, resp.code(), resp.body)
	}
	got := s.received(t, "openai")
	if len(got) != 1 {
		t.Fatalf("the stub received %d requests, want 1", len(got))
	}
	if auth := got[0].Header.Get("Authorization"); auth != "Bearer "+provider.DefaultCredential {
		t.Fatalf("the upstream saw Authorization %q, want the Provider's credential", auth)
	}
	recorded := marshal(got[0])
	if strings.Contains(recorded, key) {
		t.Fatal("the caller's Key reached the provider")
	}
	for _, name := range []string{"Lux-Labels", "X-Forwarded-For", "Cookie"} {
		if got[0].Header.Get(name) != "" {
			t.Fatalf("the upstream saw %s", name)
		}
	}
	if strings.Contains(string(resp.body), provider.DefaultCredential) || strings.Contains(marshal(resp.header), provider.DefaultCredential) {
		t.Fatal("the door's answer carries the Provider's credential")
	}
	for _, path := range []string{"/v1/providers/openai", "/v1/providers", "/v1/requests?key=dev"} {
		read := do(t, http.MethodGet, s.gw.public+path, bearer(s.token), "")
		if read.status != http.StatusOK || strings.Contains(string(read.body), provider.DefaultCredential) {
			t.Fatalf("GET %s = %d and carries the credential: %s", path, read.status, read.body)
		}
	}

	resp = s.chat(t, key, "wrong-model", "hi", false)
	if resp.status != http.StatusBadGateway || resp.code() != "upstream_error" {
		t.Fatalf("a Provider with the wrong credential: %d %s %s", resp.status, resp.code(), resp.body)
	}
	got = s.received(t, "openai")
	if len(got) != 2 || got[1].Header.Get("Authorization") != "Bearer not-the-stub-credential" {
		t.Fatalf("the refused request was not recorded with its credential: %+v", got)
	}
}

// TestE2EEveryRequestHasOneUsageRecord is the process half of the
// metering canary: served, refused, failed, and streamed requests each
// leave exactly one record, and no request leaves two.
func TestE2EEveryRequestHasOneUsageRecord(t *testing.T) {
	s := newStack(t, nil)
	key := s.fixtures(t)
	s.model(t, "failing", "openai", "fail-500", false)
	requests := []struct {
		path, body string
		header     http.Header
		status     string
	}{
		{"/openai/v1/chat/completions", `{"model":"openai-model","messages":[{"role":"user","content":"a"}]}`, jsonBearer(key), "ok"},
		{"/openai/v1/chat/completions", `{"model":"openai-model","stream":true,"messages":[{"role":"user","content":"b"}]}`, jsonBearer(key), "ok"},
		{"/anthropic/v1/messages", `{"model":"anthropic-model","max_tokens":8,"messages":[{"role":"user","content":"c"}]}`, http.Header{"x-api-key": {key}, "Content-Type": {"application/json"}}, "ok"},
		{"/gemini/v1beta/models/gemini-model:generateContent", `{"contents":[{"parts":[{"text":"d"}]}]}`, http.Header{"x-goog-api-key": {key}, "Content-Type": {"application/json"}}, "ok"},
		{"/lux/v1/generate", `{"model":"lux-model","messages":[{"role":"user","blocks":[{"type":"text","text":"e"}]}]}`, jsonBearer(key), "ok"},
		{"/openai/v1/chat/completions", `{"model":"no-such-model","messages":[]}`, jsonBearer(key), "refused"},
		{"/openai/v1/chat/completions", `{"model":"failing","messages":[{"role":"user","content":"f"}]}`, jsonBearer(key), "failed"},
	}
	for i, r := range requests {
		resp := do(t, http.MethodPost, s.gw.public+r.path, r.header, r.body)
		if (r.status == "ok") != (resp.status == http.StatusOK) {
			t.Fatalf("request %d: %d %s %s", i, resp.status, resp.code(), resp.body)
		}
	}
	records := s.records(t, "dev")
	if len(records) != len(requests) {
		t.Fatalf("%d records for %d requests", len(records), len(requests))
	}
	ids := map[string]bool{}
	statuses := map[string]int{}
	for _, r := range records {
		id, _ := r["id"].(string)
		if ids[id] {
			t.Fatalf("record %s appears twice", id)
		}
		ids[id] = true
		status, _ := r["status"].(string)
		statuses[status]++
	}
	if statuses["ok"] != 5 || statuses["refused"] != 1 || statuses["failed"] != 1 {
		t.Fatalf("statuses = %v", statuses)
	}
	// A request with no Key leaves a record nobody can read by Key; the
	// count for the Key is unchanged.
	if resp := s.chat(t, "lux_not_a_key_at_all_0123456789012345678", "openai-model", "g", false); resp.status != http.StatusUnauthorized {
		t.Fatalf("an unknown Key = %d", resp.status)
	}
	if got := len(s.records(t, "dev")); got != len(requests) {
		t.Fatalf("%d records after a request with another Key, want %d", got, len(requests))
	}
}

// TestE2EHotPathDialsNoWebhook is spec 001's third invariant across a
// process boundary: after the records of both stubs are cleared, two
// hundred data plane requests leave the issuer's and the authorizer's
// request logs empty.
func TestE2EHotPathDialsNoWebhook(t *testing.T) {
	s := newStack(t, nil)
	key := s.fixtures(t)
	for _, url := range []string{s.stubs.urls["issuer"] + "/requests", s.stubs.urls["authorizer"] + "/requests"} {
		if resp := do(t, http.MethodDelete, url, nil, ""); resp.status != http.StatusNoContent {
			t.Fatalf("DELETE %s = %d", url, resp.status)
		}
	}
	for i := range 200 {
		model := []string{"openai-model", "anthropic-model", "lux-model"}[i%3]
		if resp := s.chat(t, key, model, "hi", i%7 == 0); resp.status != http.StatusOK {
			t.Fatalf("request %d: %d %s %s", i, resp.status, resp.code(), resp.body)
		}
	}
	for _, url := range []string{s.stubs.urls["issuer"] + "/requests", s.stubs.urls["authorizer"] + "/requests"} {
		resp := do(t, http.MethodGet, url, nil, "")
		if body := strings.TrimSpace(string(resp.body)); resp.status != http.StatusOK || (body != "[]" && body != "null") {
			t.Fatalf("GET %s after two hundred data plane requests = %d %s", url, resp.status, body)
		}
	}
}

// TestE2ECostIsExact: a Model whose target is tokens-1000-500 leaves a
// record of exactly 1000 and 500 tokens, priced by spec 009's arithmetic
// at 2.50 in and 10 out per million: 7500 micro-units of USD.
func TestE2ECostIsExact(t *testing.T) {
	s := newStack(t, nil)
	key := s.fixtures(t)
	s.model(t, "exact", "openai", "tokens-1000-500", true)
	resp := s.chat(t, key, "exact", "count me", false)
	if resp.status != http.StatusOK {
		t.Fatalf("chat = %d %s %s", resp.status, resp.code(), resp.body)
	}
	doc := resp.json(t)
	if in, out := number(t, doc, "usage.prompt_tokens"), number(t, doc, "usage.completion_tokens"); in != 1000 || out != 500 {
		t.Fatalf("the answer reports %d/%d tokens", in, out)
	}
	records := s.records(t, "dev")
	if len(records) != 1 {
		t.Fatalf("%d records, want 1", len(records))
	}
	r := records[0]
	if number(t, r, "tokens.input") != 1000 || number(t, r, "tokens.output") != 500 || number(t, r, "tokens.cachedInput") != 0 {
		t.Fatalf("tokens = %v", r["tokens"])
	}
	if number(t, r, "cost.amount") != 7500 || r["cost"].(map[string]any)["currency"] != "USD" || r["cost"].(map[string]any)["priced"] != true {
		t.Fatalf("cost = %v, want 7500 micro-units of USD", r["cost"])
	}
	if r["upstreamModel"] != "tokens-1000-500" || r["model"].(map[string]any)["name"] != "exact" {
		t.Fatalf("model = %v, upstreamModel = %v", r["model"], r["upstreamModel"])
	}
}
