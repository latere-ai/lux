// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

func disableBody(t *testing.T, key *v1.Key) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"metadata": key.Metadata, "spec": key.Spec})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestExactDisableAfterAuthorityWithdrawal(t *testing.T) {
	for _, scenario := range []string{"model denied", "budget denied", "references deleted", "expired", "ceilings reduced"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, nil)
			h.seed()
			payload := keyJSON
			if scenario == "expired" {
				payload = `{"spec":{"models":["gpt-5"],"budget":"team","expiresAt":"2026-09-14T12:00:01Z"}}`
			}
			made := h.request(http.MethodPut, "/v1/keys/cleanup", payload)
			if made.Code != http.StatusCreated {
				t.Fatal(made.Code, made.Body)
			}
			old := h.objectOf(v1.KindKey, "cleanup").(*v1.Key)
			next := *old
			next.Spec.Disabled = true
			switch scenario {
			case "model denied":
				h.stub.Deny(stub.Rule{Action: authorizer.ActionModelUse}, "withdrawn")
			case "budget denied":
				h.stub.Deny(stub.Rule{Action: authorizer.ActionBudgetDraw}, "withdrawn")
			case "references deleted":
				for _, path := range []string{"/v1/models/gpt-5"} {
					got := h.request(http.MethodDelete, path, "")
					if got.Code != http.StatusNoContent {
						t.Fatal(got.Code, got.Body)
					}
				}
			case "expired":
				h.advance(2 * time.Second)
			case "ceilings reduced":
				h.stub.Allow(stub.Rule{Action: authorizer.ActionKeyUpdate, Limits: map[string]any{"max_key_requests_per_minute": 1}})
			}
			h.advance(2 * time.Minute)
			h.stub.ClearRequests()
			got := h.request(http.MethodPut, "/v1/keys/cleanup", disableBody(t, &next), "If-Match", made.Header().Get("ETag"))
			if got.Code != http.StatusOK {
				t.Fatal(got.Code, got.Body)
			}
			persisted := h.objectOf(v1.KindKey, "cleanup").(*v1.Key)
			if !store.DisableOnly(old, persisted) || !reflect.DeepEqual(old.Status.Budget, persisted.Status.Budget) {
				t.Fatal("disable changed stored authority")
			}
			for _, r := range h.stub.Requests() {
				if r.Action != authorizer.ActionKeyUpdate {
					t.Fatal("cleanup requested extra authority", r.Action)
				}
			}
			rows, err := h.st.Journal().Since(t.Context(), 0, 100)
			if err != nil || rows[len(rows)-1].Type != "key.updated" {
				t.Fatal(rows, err)
			}
			wantCode(t, h.request(http.MethodPut, "/v1/keys/cleanup", disableBody(t, &next), "If-Match", made.Header().Get("ETag")), CodeConflict)
			h.stub.Deny(stub.Rule{Action: authorizer.ActionKeyUpdate}, "refused")
			h.advance(2 * time.Minute)
			wantCode(t, h.request(http.MethodPut, "/v1/keys/cleanup", disableBody(t, &next)), CodeForbidden)
		})
	}
}

func TestExactDisableCannotChangePolicy(t *testing.T) {
	edits := map[string]func(*v1.Key){
		"models":   func(k *v1.Key) { k.Spec.Models = []string{"other"} },
		"budget":   func(k *v1.Key) { k.Spec.Budget = "other" },
		"ttl":      func(k *v1.Key) { k.Spec.TTL = "2h" },
		"expiry":   func(k *v1.Key) { k.Spec.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) },
		"labels":   func(k *v1.Key) { k.Metadata.Labels = map[string]string{"extra": "label"} },
		"requests": func(k *v1.Key) { n := 999; k.Spec.Limits.RequestsPerMinute = &n },
		"tokens":   func(k *v1.Key) { n := 999; k.Spec.Limits.TokensPerMinute = &n },
		"spend": func(k *v1.Key) {
			n := v1.Money(99)
			k.Spec.Limits.Spend = &v1.Spend{Amount: &n, Currency: "USD", Window: "month"}
		},
		"unpriced":    func(k *v1.Key) { k.Spec.AllowUnpriced = !k.Spec.AllowUnpriced },
		"passthrough": func(k *v1.Key) { k.Spec.Passthrough = !k.Spec.Passthrough },
		"enabled":     func(k *v1.Key) { k.Spec.Disabled = false },
	}
	h := newHarness(t, nil)
	h.seed()
	if got := h.request(http.MethodPut, "/v1/keys/cleanup", keyJSON); got.Code != 201 {
		t.Fatal(got.Body)
	}
	h.stub.Deny(stub.Rule{Action: authorizer.ActionBudgetDraw}, "withdrawn")
	h.advance(2 * time.Minute)
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			old := h.objectOf(v1.KindKey, "cleanup").(*v1.Key)
			next := *old
			next.Spec.Disabled = true
			edit(&next)
			if exactKeyDisable(&next, old) != nil {
				t.Fatal("accepted policy expansion")
			}
			got := h.request(http.MethodPut, "/v1/keys/cleanup", disableBody(t, &next))
			if got.Code < 400 {
				t.Fatal(got.Code, got.Body)
			}
		})
	}
	old := h.objectOf(v1.KindKey, "cleanup").(*v1.Key)
	for _, edit := range []func(*v1.Key){func(k *v1.Key) { k.Spec.SetValue("new") }, func(k *v1.Key) { k.Spec.SetValueSHA256("new") }, func(k *v1.Key) { k.Spec.ValueFrom = &v1.ValueFrom{} }} {
		next := *old
		next.Spec.Disabled = true
		edit(&next)
		if exactKeyDisable(&next, old) != nil {
			t.Fatal("accepted credential input")
		}
	}
	for _, obj := range []v1.Object{nil, (*v1.Key)(nil), &v1.Model{}} {
		if exactKeyDisable(obj, old) != nil || exactKeyDisable(old, obj) != nil {
			t.Fatal("accepted non-key")
		}
	}
}
