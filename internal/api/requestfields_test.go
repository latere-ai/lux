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

// TestZeroRetentionRoundTrip: a Key's spec.zeroRetention renders false
// when unset, is what GET returns after a PUT sets it, and changes on a
// later apply; a Provider's spec.zeroRetention is absent, {}, or carries
// requestFields as applied, and its fields are refused at their own
// paths under the rules of requestFields.
func TestZeroRetentionRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	readKey := func() any {
		t.Helper()
		rec := h.request(http.MethodGet, "/v1/keys/zr", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		return body(t, rec)["spec"].(map[string]any)["zeroRetention"]
	}
	if rec := h.request(http.MethodPut, "/v1/keys/zr", keyJSON); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := readKey(); got != false {
		t.Errorf("an unset flag reads %v", got)
	}
	with := strings.Replace(keyJSON, `"budget"`, `"zeroRetention": true, "budget"`, 1)
	if rec := h.request(http.MethodPut, "/v1/keys/zr", with); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if got := readKey(); got != true {
		t.Errorf("after setting the flag it reads %v", got)
	}
	if rec := h.request(http.MethodPut, "/v1/keys/zr", keyJSON); rec.Code != http.StatusOK {
		t.Fatalf("update without: %d %s", rec.Code, rec.Body.String())
	}
	if got := readKey(); got != false {
		t.Errorf("after an apply without the flag it reads %v", got)
	}

	provider := func(declaration string) string {
		return strings.Replace(providerJSON, `"discovery"`, `"zeroRetention": `+declaration+`, "discovery"`, 1)
	}
	readProvider := func() any {
		t.Helper()
		rec := h.request(http.MethodGet, "/v1/providers/openai", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		return body(t, rec)["spec"].(map[string]any)["zeroRetention"]
	}
	if got := readProvider(); got != nil {
		t.Errorf("a Provider that declares nothing reads %v", got)
	}
	for _, c := range []struct {
		declaration string
		want        any
	}{
		{`{}`, map[string]any{}},
		{`{"requestFields": {"provider": {"zdr": true}}}`, map[string]any{"requestFields": map[string]any{"provider": map[string]any{"zdr": true}}}},
		{`{"requestFields": {}}`, map[string]any{}},
	} {
		if rec := h.request(http.MethodPut, "/v1/providers/openai", provider(c.declaration)); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.declaration, rec.Code, rec.Body.String())
		}
		if got := readProvider(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s reads %v, want %v", c.declaration, got, c.want)
		}
	}
	if rec := h.request(http.MethodPut, "/v1/providers/openai", providerJSON); rec.Code != http.StatusOK {
		t.Fatalf("update without: %d %s", rec.Code, rec.Body.String())
	}
	if got := readProvider(); got != nil {
		t.Errorf("after an apply without the declaration it reads %v", got)
	}
	for _, c := range []struct {
		declaration string
		code        Code
		path        string
	}{
		{`{"requestFields": {"STREAM": true}}`, CodeReservedPrefix, `spec.zeroRetention.requestFields["STREAM"]`},
		{`{"requestFields": {"provider": {"zdr": null}}}`, CodeInvalidField, `spec.zeroRetention.requestFields["provider"]["zdr"]`},
		{`{"fields": {}}`, CodeUnknownField, "spec.zeroRetention.fields"},
	} {
		details := wantCode(t, h.request(http.MethodPut, "/v1/providers/openai", provider(c.declaration)), c.code)
		if got := paths(details); !slices.Equal(got, []string{c.path}) {
			t.Errorf("%s: paths %v", c.declaration, got)
		}
	}
}
