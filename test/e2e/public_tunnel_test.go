// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	luxclient "latere.ai/x/lux/client"
	"latere.ai/x/lux/client/tunnel"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/test/stubs/provider"
)

// This consumer imports only public Lux packages and drives a real core process.
func TestE2EPublicTunnelClient(t *testing.T) {
	s := newStack(t, map[string]string{"LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_REGISTRY_TTL": "5s"})
	control := &luxclient.Client{BaseURL: s.gw.public, Token: luxclient.StaticToken(s.token)}
	_, err := control.Apply(t.Context(), "providers", "laptop", []byte(`{"spec":{"dialect":"openai","tunnel":true,"discovery":{"mode":"none"},"health":{"mode":"none"}}}`), "application/json", "")
	if err != nil {
		t.Fatal(err)
	}
	runtime := provider.New(provider.Options{Dialect: v1.DialectOpenAI})
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("identity or inference bearer reached runtime")
		}
		r.Header.Set("Authorization", "Bearer "+provider.DefaultCredential)
		runtime.ServeHTTP(w, r)
	}))
	t.Cleanup(local.Close)
	expires := time.Now().Add(8 * time.Second)
	var token atomic.Value
	token.Store(mintClaims(t, s, issuertest.Claims{Sub: "dev", Exp: expires.Unix()}))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Run(ctx, tunnel.Options{Gateway: s.gw.public, Provider: "laptop", Upstream: local.URL + "/v1", Token: func() (string, error) { return token.Load().(string), nil }, Logger: slog.New(slog.DiscardHandler)})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := do(t, http.MethodGet, s.gw.public+"/v1/providers/laptop", bearer(s.token), "")
		if bytes.Contains(got.body, []byte(`"state":"Connected"`)) {
			break
		}
		select {
		case err := <-done:
			t.Fatal("tunnel ended", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel did not connect", string(got.body))
		}
		time.Sleep(25 * time.Millisecond)
	}
	token.Store(s.token)
	s.model(t, "laptop-model", "laptop", "stub-openai", true)
	key := s.key(t, "local-work", "laptop-model")
	for _, stream := range []bool{false, true} {
		got := s.chat(t, key, "laptop-model", "hello", stream)
		if got.status != http.StatusOK {
			t.Fatal(got.status, string(got.body))
		}
		if stream && !bytes.Contains(got.body, []byte("[DONE]")) {
			t.Fatal("stream lost terminator", string(got.body))
		}
	}
	// Stay attached beyond the original bearer expiry: the public token source
	// must reach the core in a heartbeat without reconnecting.
	for time.Now().Before(expires.Add(time.Second)) {
		select {
		case err := <-done:
			t.Fatal("tunnel ended before refresh", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	got := s.chat(t, key, "laptop-model", "refreshed", true)
	if got.status != http.StatusOK {
		t.Fatal("refreshed session failed", got.status, string(got.body))
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("public client did not stop")
	}
}
