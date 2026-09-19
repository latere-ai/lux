// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/serve"
	v1 "latere.ai/x/lux/manifest/v1"
)

func TestExplicitOwnerAssignmentPreservesWriterAndChargesTargetQuota(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	target := h.iss.URL() + "|service-a"
	h.stub.SetRules(stub.Rule{Action: authorizer.ActionKeyCreate, Allow: true, Limits: map[string]any{"max_keys": 1}}, stub.Rule{Action: authorizer.ActionOwnerAssign, Allow: true})
	created := h.request(http.MethodPut, "/v1/keys/assigned-one", keyJSON, "Lux-Owner", target)
	if created.Code != http.StatusCreated || status(t, created)["owner"] != target {
		t.Fatalf("assigned create: %d %s", created.Code, created.Body)
	}
	for _, r := range h.stub.Requests() {
		if r.Action == authorizer.ActionOwnerAssign {
			if r.Subject != h.subject() || r.Resource.String("owner") != target || r.Resource.Kind != "Ownership" {
				t.Fatal("assignment impersonated target", r)
			}
		}
	}
	rows, err := h.st.Journal().Since(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Type != "key.created" {
			continue
		}
		var ev struct {
			Subject string `json:"subject"`
			Object  struct {
				Owner string `json:"owner"`
			} `json:"object"`
		}
		if err := json.Unmarshal(row.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Object.Owner == target {
			found = true
			if ev.Subject != h.subject() {
				t.Fatal("audit actor became owner")
			}
		}
	}
	if !found {
		t.Fatal("assigned creation not journaled")
	}
	// A second authenticated writer cannot evade the target owner's quota.
	wantCode(t, h.request(http.MethodPut, "/v1/keys/assigned-two", keyJSON, "Lux-Owner", target, "Authorization", "Bearer "+h.bob), CodeCeilingExceeded)
	other := h.request(http.MethodPut, "/v1/keys/other-owner", keyJSON, "Lux-Owner", h.iss.URL()+"|service-b")
	if other.Code != http.StatusCreated {
		t.Fatalf("independent owner quota: %d %s", other.Code, other.Body)
	}
	updated := h.request(http.MethodPut, "/v1/keys/assigned-one", keyJSON, "Lux-Owner", target)
	if updated.Code != http.StatusOK || status(t, updated)["owner"] != target {
		t.Fatalf("same-owner retry: %d %s", updated.Code, updated.Body)
	}
	wantCode(t, h.request(http.MethodPut, "/v1/keys/assigned-one", keyJSON, "Lux-Owner", h.subject()), CodeInvalidField)
	updated = h.request(http.MethodPut, "/v1/keys/assigned-one", keyJSON)
	if updated.Code != http.StatusOK || status(t, updated)["owner"] != target {
		t.Fatal("omitted owner changed existing ownership")
	}
}
func TestOwnerAssignmentRequiresBothPermissions(t *testing.T) {
	for _, denied := range []string{authorizer.ActionKeyCreate, authorizer.ActionOwnerAssign} {
		t.Run(denied, func(t *testing.T) {
			h := newHarness(t, nil)
			h.seed()
			h.stub.Deny(stub.Rule{Action: denied}, "assignment_denied")
			wantCode(t, h.request(http.MethodPut, "/v1/keys/assigned", keyJSON, "Lux-Owner", h.iss.URL()+"|service"), CodeForbidden)
			// Neither failed permission may write an object.
			if _, _, err := h.st.Objects().ByName(t.Context(), "Key", "assigned"); err == nil {
				t.Fatal("denied assignment wrote object")
			}
		})
	}
	h := newHarness(t, nil)
	h.seed()
	for _, owner := range []string{"bare-subject", "issuer|", "|subject", " issuer|subject"} {
		wantCode(t, h.request(http.MethodPut, "/v1/keys/invalid-owner", keyJSON, "Lux-Owner", owner), CodeInvalidField)
	}
	created := h.request(http.MethodPut, "/v1/keys/ordinary", keyJSON)
	if created.Code != http.StatusCreated || status(t, created)["owner"] != h.subject() {
		t.Fatal("ordinary create changed owner")
	}
	for _, r := range h.stub.Requests() {
		if r.Action == authorizer.ActionOwnerAssign {
			t.Fatal("ordinary create asked assignment")
		}
	}
}

func TestChangedProposalCannotReuseWarmUpdateAllow(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	want := h.request(http.MethodPut, "/v1/keys/admission", keyJSON)
	if want.Code != http.StatusCreated {
		t.Fatal(want.Body)
	}
	s := stub.New(t, stub.WithAction(authorizer.ActionKeyUpdate, func(req authz.Request) any {
		proposed, _ := req.Resource.Fields["proposed"].(map[string]any)
		metadata, _ := proposed["metadata"].(map[string]any)
		labels, _ := metadata["labels"].(map[string]any)
		return map[string]any{"allow": labels["tenant"] == "alice", "reason": "foreign_tenant", "ttl": 60}
	}))
	client, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}, Now: h.clock})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(client)
	allowed := `{"metadata":{"labels":{"tenant":"alice"}},"spec":{"models":["gpt-5"],"budget":"team"}}`
	if r := h.request(http.MethodPut, "/v1/keys/admission", allowed); r.Code != http.StatusOK {
		t.Fatal(r.Body)
	}
	// Warm the decision for the current stored object, then change only the proposal.
	if r := h.request(http.MethodPut, "/v1/keys/admission", allowed); r.Code != http.StatusOK {
		t.Fatal(r.Body)
	}
	wantCode(t, h.request(http.MethodPut, "/v1/keys/admission", strings.ReplaceAll(allowed, "alice", "bob")), CodeForbidden)
	obj, _, err := h.st.Objects().ByName(t.Context(), "Key", "admission")
	if err != nil || obj.(*v1.Key).Metadata.Labels["tenant"] != "alice" {
		t.Fatal("denied proposal changed storage", err)
	}
}

func TestTunnelOwnerCannotConvertProviderToRemote(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.TunnelEnabled = true })
	h.h.o.Authorizer = auth.NewAuthorizer(&auth.OwnerPolicy{Objects: &serve.ObjectOwners{Objects: h.st.Objects()}})
	created := h.request(http.MethodPut, "/v1/providers/laptop", `{"spec":{"dialect":"openai","tunnel":true}}`)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Code, created.Body)
	}
	wantCode(t, h.request(http.MethodPut, "/v1/providers/laptop", `{"spec":{"dialect":"openai","baseURL":"https://upstream.example.com"}}`), CodeForbidden)
	obj, _, err := h.st.Objects().ByName(t.Context(), "Provider", "laptop")
	if err != nil || !obj.(*v1.Provider).Spec.Tunnel {
		t.Fatal("refused conversion changed provider", err)
	}
}

func TestOwnerHeaderMustBeUniqueAndBounded(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	for _, values := range [][]string{{h.subject(), h.subject()}, {"issuer|" + strings.Repeat("a", 2048)}} {
		r := httptest.NewRequest(http.MethodPut, "/v1/keys/assigned", strings.NewReader(keyJSON))
		r.Header.Set("Authorization", "Bearer "+h.alice)
		r.Header.Set("Content-Type", "application/json")
		for _, value := range values {
			r.Header.Add("Lux-Owner", value)
		}
		rec := httptest.NewRecorder()
		h.h.ServeHTTP(rec, r)
		wantCode(t, rec, CodeInvalidField)
	}
}

func TestLegacyAuthorizerRefusesUnknownAssignment(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	legacy := authorizer.Vocabulary()
	legacy.Actions = legacy.Actions[:len(legacy.Actions)-1]
	s := stub.New(t, stub.WithVocabulary(legacy))
	client, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.h.o.Authorizer = auth.NewAuthorizer(client)
	wantCode(t, h.request(http.MethodPut, "/v1/keys/assigned", keyJSON, "Lux-Owner", h.iss.URL()+"|service"), CodeAuthorizerUnavailable)
}
