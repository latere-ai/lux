// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestE2EKeyFenceLifecycle(t *testing.T) {
	s := planeStack(t)
	s.provider(t, "openai", "openai", false, "")
	s.model(t, "fence-model", "openai", "stub-openai", true)
	manifest := keySpec("fenced", "  models: [fence-model]\n  ttl: 1h\n")
	made := s.apply(t, "key", "fenced", manifest)
	status, ok := made.json(t)["status"].(map[string]any)
	if !ok {
		t.Fatal("missing status")
	}
	value, ok := status["value"].(string)
	if !ok || value == "" {
		t.Fatal("missing credential")
	}
	if got := s.chat(t, value, "fence-model", "before", false); got.status != http.StatusOK {
		t.Fatal(got.status, string(got.body))
	}
	assertion, err := json.Marshal(map[string]any{"owner": status["owner"], "labels": map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	headers := bearer(s.token)
	headers.Set("Content-Type", "application/json")
	endpoint := s.gw.public + "/v1/keys/fenced/fence"
	fence := do(t, http.MethodPost, endpoint, headers, string(assertion))
	if fence.status != http.StatusOK {
		t.Fatal(fence.status, string(fence.body))
	}
	replay := do(t, http.MethodPost, endpoint, headers, string(assertion))
	read := do(t, http.MethodGet, endpoint, headers, "")
	if replay.status != http.StatusOK || read.status != http.StatusOK || !bytes.Equal(fence.body, replay.body) || !bytes.Equal(fence.body, read.body) {
		t.Fatal("fence replay/read changed")
	}
	rotated := do(t, http.MethodPost, s.gw.public+"/v1/keys/fenced/rotate", bearer(s.token), "")
	if rotated.status != http.StatusConflict || !bytes.Contains(rotated.body, []byte("key_fenced")) {
		t.Fatal(rotated.status, string(rotated.body))
	}
	disabled := applyWith(t, s, s.token, "key", "fenced", keySpec("fenced", "  models: [fence-model]\n  ttl: 1h\n  disabled: true\n"))
	if disabled.status != http.StatusOK {
		t.Fatal(disabled.status, string(disabled.body))
	}
	// The cache has a finite grace window; the fence alone made no inference claim.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.chat(t, value, "fence-model", "after", false)
		if got.status == http.StatusForbidden && bytes.Contains(got.body, []byte("key_disabled")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(got.status, string(got.body))
		}
		time.Sleep(50 * time.Millisecond)
	}
	enabled := applyWith(t, s, s.token, "key", "fenced", manifest)
	if enabled.status != http.StatusConflict || !bytes.Contains(enabled.body, []byte("key_fenced")) {
		t.Fatal(enabled.status, string(enabled.body))
	}
	deleted := do(t, http.MethodDelete, s.gw.public+"/v1/keys/fenced", bearer(s.token), "")
	if deleted.status != http.StatusNoContent {
		t.Fatal(deleted.status, string(deleted.body))
	}
	recreated := applyWith(t, s, s.token, "key", "fenced", manifest)
	if recreated.status != http.StatusConflict || !bytes.Contains(recreated.body, []byte("key_fenced")) {
		t.Fatal(recreated.status, string(recreated.body))
	}
}
