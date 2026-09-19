// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"net/http"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
)

func TestE2EKeyGrantBindingAndRotation(t *testing.T) {
	policy := stub.New(t, stub.WithAction(authorizer.ActionModelUse, func(r authz.Request) any {
		binding, _ := r.Resource.Fields["binding"].(map[string]any)
		p, _ := binding["proposed"].(map[string]any)
		spec, _ := p["spec"].(map[string]any)
		return map[string]any{"allow": binding["kind"] == "Key" && spec["budget"] == "funded", "reason": "wrong_grant"}
	}), stub.WithAction(authorizer.ActionKeyUpdate, func(r authz.Request) any {
		p, _ := r.Resource.Fields["proposed"].(map[string]any)
		spec, _ := p["spec"].(map[string]any)
		return map[string]any{"allow": r.Resource.String("operation") == "rotate" && spec["budget"] == "funded", "reason": "missing_rotation_state"}
	}))
	s := newStack(t, map[string]string{"LUX_AUTHORIZER_URL": policy.URL(), "LUX_AUTHORIZER_TOKEN": policy.Token()})
	s.provider(t, "openai", "openai", false, "")
	s.model(t, "paid-model", "openai", "stub-openai", true)
	s.apply(t, "budget", "funded", budgetYAML("funded"))
	made := s.apply(t, "key", "allowed", keySpec("allowed", "  models: [paid-model]\n  budget: funded\n"))
	key, _ := made.json(t)["status"].(map[string]any)["value"].(string)
	if got := s.chat(t, key, "paid-model", "hello", false); got.status != http.StatusOK {
		t.Fatal(got.status, string(got.body))
	}
	denied := applyWith(t, s, s.token, "key", "wrong", keySpec("wrong", "  models: [paid-model]\n"))
	if denied.status != http.StatusNotFound {
		t.Fatal("unbound grant admitted", denied.status, string(denied.body))
	}
	rotated := do(t, http.MethodPost, s.gw.public+"/v1/keys/allowed/rotate", bearer(s.token), "")
	if rotated.status != http.StatusOK {
		t.Fatal(rotated.status, string(rotated.body))
	}
	next, _ := rotated.json(t)["status"].(map[string]any)["value"].(string)
	if next == key || next == "" {
		t.Fatal("rotation produced no new credential")
	}
	if got := s.chat(t, next, "paid-model", "rotated", false); got.status != http.StatusOK {
		t.Fatal(got.status, string(got.body))
	}
}
