// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestProviderRequestFieldsRoundTrip: a Provider's requestFields applied
// through PUT are what GET returns, change and go away on later applies
// because the field is mutable, and a reserved member is reserved_prefix
// at its path.
func TestProviderRequestFieldsRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	with := func(fields string) string {
		return strings.Replace(providerJSON, `"discovery"`, `"requestFields": `+fields+`, "discovery"`, 1)
	}
	read := func() any {
		t.Helper()
		rec := h.request(http.MethodGet, "/v1/providers/openai", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		return body(t, rec)["spec"].(map[string]any)["requestFields"]
	}
	if rec := h.request(http.MethodPut, "/v1/providers/openai", with(`{"provider": {"zdr": true, "data_collection": "deny"}}`)); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got, want := read(), map[string]any{"provider": map[string]any{"zdr": true, "data_collection": "deny"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after create %v, want %v", got, want)
	}
	if rec := h.request(http.MethodPut, "/v1/providers/openai", with(`{"provider": {"zdr": false}}`)); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if got, want := read(), map[string]any{"provider": map[string]any{"zdr": false}}; !reflect.DeepEqual(got, want) {
		t.Errorf("after update %v, want %v", got, want)
	}
	if rec := h.request(http.MethodPut, "/v1/providers/openai", providerJSON); rec.Code != http.StatusOK {
		t.Fatalf("update without: %d %s", rec.Code, rec.Body.String())
	}
	if got := read(); got != nil {
		t.Errorf("after an apply without the field %v", got)
	}
	details := wantCode(t, h.request(http.MethodPut, "/v1/providers/openai", with(`{"Model": "gpt-5"}`)), CodeReservedPrefix)
	if got := paths(details); !slices.Equal(got, []string{`spec.requestFields["Model"]`}) {
		t.Errorf("paths %v", got)
	}
}
