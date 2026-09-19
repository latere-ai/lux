// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"bytes"
	"net/http"
	"testing"
	"time"
)

func TestE2EDisabledKeyWaitsForModelAssignment(t *testing.T) {
	s := planeStack(t)
	made := s.apply(t, "key", "waiting", keySpec("waiting", "  disabled: true\n"))
	value, _ := made.json(t)["status"].(map[string]any)["value"].(string)
	if value == "" {
		t.Fatal("no credential")
	}
	s.provider(t, "openai", "openai", false, "")
	s.model(t, "new-model", "openai", "stub-openai", true)
	refused := s.chat(t, value, "new-model", "hello", false)
	if refused.status != http.StatusForbidden || !bytes.Contains(refused.body, []byte("key_disabled")) {
		t.Fatal(refused.status, refused.body)
	}
	empty := applyWith(t, s, s.token, "key", "waiting", keySpec("waiting", "  disabled: false\n"))
	if empty.status != http.StatusBadRequest || !bytes.Contains(empty.body, []byte("missing_field")) {
		t.Fatal(empty.status, empty.body)
	}
	s.apply(t, "key", "waiting", keySpec("waiting", "  models: [new-model]\n  disabled: false\n"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		response := s.chat(t, value, "new-model", "hello", false)
		if response.status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(response.status, response.body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
