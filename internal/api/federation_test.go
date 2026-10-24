// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestFederationDropsTheStoredValue is spec 051's second criterion: an
// apply that sets credential.federation deletes the stored value in the
// same write and leaves status.credential unset at the value's version;
// switching back seals the next version; and a gateway without a
// workload identity refuses the apply.
func TestFederationDropsTheStoredValue(t *testing.T) {
	const keyed = `{"spec": {"dialect": "anthropic", "baseURL": "https://api.example.com/v1", "credential": {"value": "` + canary + `"}, "discovery": {"mode": "none"}, "health": {"mode": "none"}}}`
	const federated = `{"spec": {"dialect": "anthropic", "baseURL": "https://api.example.com/v1", "credential": {"federation": {"ruleID": "fdrl_01", "organizationID": "4f6c1d2a-8e3b-4c7d-9a1e-2b5f6c7d8e9f", "serviceAccountID": "svac_01"}}, "discovery": {"mode": "none"}, "health": {"mode": "none"}}}`
	h := newHarness(t, func(o *Options) { o.WorkloadIdentity = true })
	if rec := h.request(http.MethodPut, "/v1/providers/anthropic", keyed); rec.Code != http.StatusCreated {
		t.Fatalf("a keyed Provider: %d %s", rec.Code, rec.Body.String())
	}
	id := h.objectOf(v1.KindProvider, "anthropic").ID()
	h.advance(time.Minute)
	rec := h.request(http.MethodPut, "/v1/providers/anthropic", federated)
	if rec.Code != http.StatusOK {
		t.Fatalf("to federation: %d %s", rec.Code, rec.Body.String())
	}
	if cred, _ := status(t, rec)["credential"].(map[string]any); cred["set"] != false || cred["version"] != float64(1) {
		t.Errorf("credential status after federation %v", cred)
	}
	if _, err := h.st.Credentials().Get(bg(), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the stored value is still there: %v", err)
	}
	p := h.objectOf(v1.KindProvider, "anthropic").(*v1.Provider)
	if c := p.Spec.Credential; c.Federation == nil || c.Header != "Authorization" || c.Scheme != v1.SchemeBearer {
		t.Errorf("the stored credential block %+v", c)
	}
	// Applied again, nothing is deleted and the status holds.
	if rec := h.request(http.MethodPut, "/v1/providers/anthropic", federated); rec.Code != http.StatusOK {
		t.Fatalf("federation again: %d %s", rec.Code, rec.Body.String())
	}
	h.advance(time.Minute)
	rec = h.request(http.MethodPut, "/v1/providers/anthropic", keyed)
	if cred, _ := status(t, rec)["credential"].(map[string]any); rec.Code != http.StatusOK || cred["set"] != true || cred["version"] != float64(2) {
		t.Errorf("back to a value: %d %v", rec.Code, cred)
	}
	if _, err := h.st.Credentials().Get(bg(), id); err != nil {
		t.Errorf("the new value was not sealed: %v", err)
	}

	off := newHarness(t, nil)
	if rec := off.request(http.MethodPut, "/v1/providers/anthropic", federated); rec.Code != http.StatusBadRequest {
		t.Fatalf("without a workload identity: %d %s", rec.Code, rec.Body.String())
	}
}
