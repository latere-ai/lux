// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestE2EEventsReachTheSink: a mutation through /v1 is delivered to the
// stub sink signed, the sink verifies it, a delivery the sink refuses
// once arrives on the retry with two attempts on its record, and nothing
// sits among the rejected deliveries.
func TestE2EEventsReachTheSink(t *testing.T) {
	s := newStack(t, nil, "-fail-first", "1")
	s.provider(t, "openai", "openai", false, "")
	var created map[string]any
	eventually(t, "the provider.created event reaching the sink", 15*time.Second, func() bool {
		for _, e := range s.events(t) {
			var body map[string]any
			if err := json.Unmarshal(e.Body, &body); err != nil {
				t.Fatalf("an event body: %v", err)
			}
			if body["type"] == "provider.created" {
				if e.Attempts != 2 {
					t.Fatalf("the event arrived with %d attempt(s), want 2 after one refusal", e.Attempts)
				}
				created = body
				return true
			}
		}
		return false
	})
	obj, _ := created["object"].(map[string]any)
	if obj["kind"] != "Provider" || obj["name"] != "openai" || created["reason"] != "request" {
		t.Fatalf("event = %v", created)
	}
	if resp := do(t, http.MethodGet, s.stubs.urls["sink"]+"/_events?invalid=1", nil, ""); string(resp.body) != "[]" {
		t.Fatalf("rejected deliveries: %s", resp.body)
	}
	// A second mutation is delivered once, at the first attempt.
	s.apply(t, "budget", "team", budgetYAML("team"))
	eventually(t, "the budget.created event", 10*time.Second, func() bool {
		for _, e := range s.events(t) {
			var body map[string]any
			_ = json.Unmarshal(e.Body, &body)
			if body["type"] == "budget.created" {
				return e.Attempts == 1
			}
		}
		return false
	})
}
