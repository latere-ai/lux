// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestDeclaringOverADiscoveredModel: an apply of a discovered Model's name
// under If-None-Match: * declares a Model that replaces it in place (spec
// 005): the id is kept, the source is declared, and the spec is the
// apply's, its disabled flag included.
func TestDeclaringOverADiscoveredModel(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	pid := h.objectOf(v1.KindProvider, "openai").ID()
	discovered := &v1.Model{Metadata: v1.ObjectMeta{Name: "openai/o5"}, Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: pid, Model: "o5"}}, Disabled: true},
		Status: v1.ModelStatus{ID: h.newID(v1.PrefixModel), Owner: h.subject(), Source: v1.SourceDiscovered, Warnings: []string{}}}
	if _, err := h.st.Objects().Put(bg(), discovered, 0); err != nil {
		t.Fatal(err)
	}
	rec := h.request(http.MethodPut, "/v1/models/openai/o5", `{"spec": {"targets": [{"provider": "openai", "model": "o5"}], "pricing": {"currency": "USD", "per": 1000000, "input": "1", "output": "2"}}}`, "If-None-Match", "*")
	if rec.Code != http.StatusCreated {
		t.Fatalf("a declare over a discovered Model: %d %s", rec.Code, rec.Body.String())
	}
	m, ok := h.objectOf(v1.KindModel, "openai/o5").(*v1.Model)
	if !ok || m.Status.Source != v1.SourceDeclared || m.Status.ID != discovered.Status.ID || m.Spec.Disabled || m.Spec.Pricing == nil {
		t.Fatalf("the Model after the declare: %+v", m)
	}
	// A second create of the name, now declared, is refused.
	if rec := h.request(http.MethodPut, "/v1/models/openai/o5", `{"spec": {"targets": [{"provider": "openai", "model": "o5"}]}}`, "If-None-Match", "*"); rec.Code != http.StatusConflict {
		t.Fatalf("a create over a declared Model: %d %s", rec.Code, rec.Body.String())
	}
}
