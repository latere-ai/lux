// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

// TestE2EFailureInjectionReachesTheDoors: the failure table selected by
// upstream model name works through a Model's target, and each row
// answers at the door as spec 004's error table says.
func TestE2EFailureInjectionReachesTheDoors(t *testing.T) {
	s := newStack(t, nil)
	key := s.fixtures(t)
	for _, tc := range []struct {
		upstream string
		status   int
		code     string
	}{
		{"fail-500", http.StatusBadGateway, "upstream_error"},
		{"fail-429", http.StatusBadGateway, "upstream_error"},
		{"fail-529", http.StatusBadGateway, "upstream_error"},
		{"fail-401", http.StatusBadGateway, "upstream_error"},
		{"redirect", http.StatusBadGateway, "upstream_error"},
		{"fail-400", http.StatusBadRequest, "upstream_rejected"},
	} {
		name := "m-" + tc.upstream
		s.model(t, name, "openai", tc.upstream, false)
		resp := s.chat(t, key, name, "hi", false)
		if resp.status != tc.status || resp.code() != tc.code {
			t.Errorf("%s: %d %s, want %d %s: %s", tc.upstream, resp.status, resp.code(), tc.status, tc.code, resp.body)
		}
	}
	s.model(t, "m-html", "openai", "fail-html", false)
	if resp := s.chat(t, key, "m-html", "hi", false); resp.status != http.StatusOK || resp.header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("fail-html: %d %s", resp.status, resp.header.Get("Content-Type"))
	}
	s.model(t, "m-mid", "openai", "fail-stream-mid", false)
	if resp := s.chat(t, key, "m-mid", "hi", true); resp.status != http.StatusOK || !strings.Contains(string(resp.body), `data: {"error":`) || strings.Contains(string(resp.body), "[DONE]") {
		t.Errorf("fail-stream-mid: %d\n%s", resp.status, resp.body)
	}
	s.model(t, "m-events", "openai", "events-3", false)
	if resp := s.chat(t, key, "m-events", "hi", true); resp.status != http.StatusOK || strings.Count(string(resp.body), `"content":`) != 3 {
		t.Errorf("events-3: %d\n%s", resp.status, resp.body)
	}
	records := s.records(t, "dev")
	var failed int
	for _, r := range records {
		if r["status"] == "failed" {
			failed++
		}
	}
	if failed != 7 {
		t.Fatalf("%d failed records, want 7 across %d", failed, len(records))
	}
}
