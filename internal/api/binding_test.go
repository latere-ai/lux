// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
)

func TestReferencesCarryTheEnclosingMutation(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	check := func(r authz.Request) any {
		binding, _ := r.Resource.Fields["binding"].(map[string]any)
		proposed, _ := binding["proposed"].(map[string]any)
		spec, _ := proposed["spec"].(map[string]any)
		return map[string]any{"allow": binding["kind"] == "Key" && spec["budget"] == "team", "reason": "wrong_grant"}
	}
	s := stub.New(t, stub.WithAction(authorizer.ActionModelUse, check), stub.WithAction(authorizer.ActionBudgetDraw, check))
	z, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(z)
	got := h.request(http.MethodPut, "/v1/keys/bound", keyJSON)
	if got.Code != http.StatusCreated {
		t.Fatal(got.Code, got.Body)
	}
	secret, _ := status(t, got)["value"].(string)
	wantCode(t, h.request(http.MethodPut, "/v1/keys/unbound", `{"spec":{"models":["gpt-5"]}}`), CodeNotFound)
	for _, r := range s.Requests() {
		b, _ := json.Marshal(r)
		if secret != "" && strings.Contains(string(b), secret) {
			t.Fatal("credential leaked into authorization")
		}
	}
	// A direct read is independent of any previous apply's grant context.
	h.request(http.MethodGet, "/v1/budgets/team", "")
	for _, r := range s.Requests() {
		if r.Action == authorizer.ActionBudgetRead && r.Resource.Fields["binding"] != nil {
			t.Fatal("binding leaked into direct read")
		}
	}
}

func TestRotationCarriesUnchangedProposal(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	got := h.request(http.MethodPut, "/v1/keys/rotate-proposal", keyJSON)
	if got.Code != http.StatusCreated {
		t.Fatal(got.Body)
	}
	s := stub.New(t, stub.WithAction(authorizer.ActionKeyUpdate, func(r authz.Request) any {
		p, _ := r.Resource.Fields["proposed"].(map[string]any)
		spec, _ := p["spec"].(map[string]any)
		return map[string]any{"allow": r.Resource.String("operation") == "rotate" && p["owner"] == h.subject() && spec["budget"] == "team", "reason": "missing_proposal"}
	}))
	z, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(z)
	rotated := h.request(http.MethodPost, "/v1/keys/rotate-proposal/rotate", "")
	if rotated.Code != http.StatusOK {
		t.Fatal(rotated.Code, rotated.Body)
	}
	if status(t, rotated)["value"] == status(t, got)["value"] {
		t.Fatal("rotation kept old credential")
	}
	wantCode(t, h.request(http.MethodPut, "/v1/keys/rotate-proposal", keyJSON), CodeForbidden)
}

func TestProviderReferenceCarriesModelProposal(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	s := stub.New(t, stub.WithAction(authorizer.ActionProviderRead, func(r authz.Request) any {
		binding, _ := r.Resource.Fields["binding"].(map[string]any)
		proposed, _ := binding["proposed"].(map[string]any)
		metadata, _ := proposed["metadata"].(map[string]any)
		return map[string]any{"allow": binding["kind"] == "Model" && metadata["name"] == "bound-model", "reason": "missing_model"}
	}))
	z, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(z)
	got := h.request(http.MethodPut, "/v1/models/bound-model", `{"spec":{"targets":[{"provider":"openai","model":"gpt-5"}]}}`)
	if got.Code != http.StatusCreated {
		t.Fatal(got.Code, got.Body)
	}
}
