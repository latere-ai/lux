// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"

	"latere.ai/x/lux/internal/serve"
	v1 "latere.ai/x/lux/manifest/v1"
)

func TestKeyTTLDoesNotMoveAfterSlowCreation(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	h.h.o.MintKeyValue = func() (string, error) { h.advance(time.Second); return serve.MintKeyValue() }
	manifest := `{"spec":{"models":["gpt-5"],"ttl":"1h"}}`
	made := h.request(http.MethodPut, "/v1/keys/slow", manifest)
	if made.Code != http.StatusCreated {
		t.Fatal(made.Code, made.Body)
	}
	expires := status(t, made)["expiresAt"]
	updated := h.request(http.MethodPut, "/v1/keys/slow", manifest)
	if updated.Code != http.StatusOK {
		t.Fatal(updated.Code, updated.Body)
	}
	if got := status(t, updated)["expiresAt"]; got != expires {
		t.Fatalf("update moved original TTL expiry from %v to %v", expires, got)
	}
}

func TestKeyTTLUpdatePreservesLegacyResolvedExpiry(t *testing.T) {
	h := newHarness(t, nil)
	h.seed()
	manifest := `{"spec":{"models":["gpt-5"],"ttl":"1h"}}`
	made := h.request(http.MethodPut, "/v1/keys/legacy", manifest)
	if made.Code != http.StatusCreated {
		t.Fatal(made.Code, made.Body)
	}
	obj, version, err := h.st.Objects().ByName(t.Context(), v1.KindKey, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	key, ok := obj.(*v1.Key)
	if !ok {
		t.Fatal("not a key")
	}
	// Older writers computed expiry before persisting a later creation time.
	key.Status.ExpiresAt = key.Status.ExpiresAt.Add(-time.Second)
	if _, err := h.st.Objects().Put(t.Context(), key, version); err != nil {
		t.Fatal(err)
	}
	updated := h.request(http.MethodPut, "/v1/keys/legacy", manifest)
	if updated.Code != http.StatusOK {
		t.Fatal(updated.Code, updated.Body)
	}
	if got := status(t, updated)["expiresAt"]; got != key.Status.ExpiresAt.Format(time.RFC3339Nano) {
		t.Fatalf("legacy expiry moved: %v != %v", got, key.Status.ExpiresAt)
	}
}
