// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
)

func TestRegisteredCredentialAdmissionAcrossMutationAndReferences(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	verifier := sha256.Sum256([]byte("registered-secret-canary"))
	hash := hex.EncodeToString(verifier[:])
	commitment := sha256.Sum256([]byte(hash))
	want := hex.EncodeToString(commitment[:])
	seen := map[string]bool{}
	check := func(r authz.Request) any {
		proposed, _ := r.Resource.Fields["proposed"].(map[string]any)
		if binding, ok := r.Resource.Fields["binding"].(map[string]any); ok {
			proposed, _ = binding["proposed"].(map[string]any)
		}
		credential, _ := proposed["credential"].(map[string]any)
		allowed := credential["mode"] == "sha256" && credential["commitment"] == want
		if r.Action == authorizer.ActionKeyUpdate {
			allowed = credential["mode"] == "absent"
		}
		seen[r.Action] = true
		return map[string]any{"allow": allowed, "reason": "unregistered_credential"}
	}
	s := stub.New(t, stub.WithAction(authorizer.ActionKeyCreate, check), stub.WithAction(authorizer.ActionKeyUpdate, check), stub.WithAction(authorizer.ActionOwnerAssign, check), stub.WithAction(authorizer.ActionModelUse, check), stub.WithAction(authorizer.ActionBudgetDraw, check))
	z, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(z)
	body := fmt.Sprintf(`{"spec":{"models":["gpt-5"],"budget":"team","valueSHA256":%q}}`, hash)
	got := h.request(http.MethodPut, "/v1/keys/registered", body, "Lux-Owner", h.iss.URL()+"|target")
	if got.Code != http.StatusCreated {
		t.Fatal(got.Code, got.Body)
	}
	for _, action := range []string{authorizer.ActionKeyCreate, authorizer.ActionOwnerAssign, authorizer.ActionModelUse, authorizer.ActionBudgetDraw} {
		if !seen[action] {
			t.Errorf("missing %s", action)
		}
	}
	for _, credential := range []string{``, `,"valueSHA256":"` + strings.Repeat("f", 64) + `"`, `,"value":"other-secret"`, `,"valueFrom":{"env":"OTHER"}`} {
		denied := h.request(http.MethodPut, "/v1/keys/unregistered", `{"spec":{"models":["gpt-5"]`+credential+`}}`)
		wantCode(t, denied, CodeForbidden)
	}
	updated := h.request(http.MethodPut, "/v1/keys/registered", `{"spec":{"disabled":true}}`)
	if updated.Code != http.StatusOK {
		t.Fatal(updated.Code, updated.Body)
	}

	for _, req := range s.Requests() {
		raw, _ := json.Marshal(req)
		if strings.Contains(string(raw), hash) || strings.Contains(string(raw), "registered-secret-canary") || strings.Contains(string(raw), "other-secret") {
			t.Fatal("credential or verifier leaked")
		}
	}
}
