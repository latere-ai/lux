// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestBaseURLChangeIsAuthorizedAndAudited is the row of spec 016 for a
// subject that moves a Provider's baseURL to a host it controls: the
// change is a provider.update decision carrying the object as it was
// stored, so an authorizer sees the host it is being moved away from;
// the new baseURL meets the upstream host rule again at resolve, so a
// private address is refused on an update as on a create; and the
// change raises provider.updated naming spec.baseURL, so the operator's
// sink sees which field moved.
func TestBaseURLChangeIsAuthorizedAndAudited(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	h.stub.ClearRequests()

	// The decision is asked with the old object: the resource carries
	// the baseURL that is stored, not the one the body asks for.
	moved := strings.Replace(providerJSON, "https://api.example.com/v1", "https://api.attacker.example/v1", 1)
	if rec := h.request(http.MethodPut, "/v1/providers/openai", moved); rec.Code != http.StatusOK {
		t.Fatalf("the update: %d %s", rec.Code, rec.Body.String())
	}
	reqs := h.stub.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d decisions, want one", len(reqs))
	}
	if reqs[0].Action != "provider.update" {
		t.Errorf("action %q, want provider.update", reqs[0].Action)
	}
	if got := reqs[0].Resource.String("baseURL"); got != "https://api.example.com/v1" {
		t.Errorf("the resource carries baseURL %q, want the stored one", got)
	}

	// The host rule runs again on the update: a private address is
	// invalid_field at spec.baseURL and the stored object does not move.
	inward := strings.Replace(providerJSON, "https://api.example.com/v1", "https://127.0.0.1:9000/v1", 1)
	rec := h.request(http.MethodPut, "/v1/providers/openai", inward)
	details := wantCode(t, rec, CodeInvalidField)
	if p := paths(details); !reflect.DeepEqual(p, []string{"spec.baseURL"}) {
		t.Errorf("paths %v, want spec.baseURL", p)
	}
	if got := h.objectOf(v1.KindProvider, "openai").(*v1.Provider).Spec.BaseURL; got != "https://api.attacker.example/v1" {
		t.Errorf("the refused update moved the stored baseURL to %q", got)
	}

	// The audit: one provider.updated naming the field that moved, and
	// the credential's value in no payload.
	rows, err := h.st.Journal().Since(bg(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, r := range rows {
		if strings.Contains(string(r.Payload), canary) {
			t.Error("a journal payload carries the credential value")
		}
		if r.Type != "provider.updated" {
			continue
		}
		updates++
		var rec map[string]any
		if err := json.Unmarshal(r.Payload, &rec); err != nil {
			t.Fatal(err)
		}
		data, _ := rec["data"].(map[string]any)
		if p, _ := data["paths"].([]any); !reflect.DeepEqual(p, []any{"spec.baseURL"}) {
			t.Errorf("provider.updated paths %v, want spec.baseURL", data["paths"])
		}
		if rec["object"].(map[string]any)["name"] != "openai" {
			t.Errorf("provider.updated names %v, want the Provider that moved", rec["object"])
		}
	}
	if updates != 1 {
		t.Errorf("%d provider.updated rows, want one; the refused change raises none", updates)
	}
}
