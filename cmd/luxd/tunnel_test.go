// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	agent "latere.ai/x/lux/client/tunnel"
)

// forwardSecret is a 32-byte secret for the forward route.
const forwardSecret = "fwd-0123456789abcdef0123456789ab"

// fakeRuntime is a local model server on loopback: the openai models
// route, a chat completion with usage, and a record of every header it
// saw.
type fakeRuntime struct {
	srv     *httptest.Server
	mu      sync.Mutex
	headers []http.Header
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	rt := &fakeRuntime{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		rt.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"llama3.1"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		rt.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"llama3.1","choices":[{"index":0,"message":{"role":"assistant","content":"hello from the laptop"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`)
	})
	rt.srv = httptest.NewServer(mux)
	t.Cleanup(rt.srv.Close)
	return rt
}

func (rt *fakeRuntime) record(r *http.Request) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.headers = append(rt.headers, r.Header.Clone())
}

func (rt *fakeRuntime) seen() []http.Header {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]http.Header(nil), rt.headers...)
}

// TestServeTunnelsARuntime is spec 013 through run: with the tunnel on,
// a subject under the owner policy applies a tunnel: true Provider and
// attaches a runtime through the agent package over plaintext HTTP/2;
// the models are discovered at connect, a Key reaches one through the
// openai door, the runtime sees neither the Key nor an issuer token,
// the request is counted against the Provider, the sessions gauge moves,
// the forward route is on the internal listener under the secret and
// nowhere else, and the agent's stop makes the Provider Unreachable and
// Disconnected within the registry TTL.
func TestServeTunnelsARuntime(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	srv := startServe(t, map[string]string{
		"LUX_OIDC_ISSUERS": iss.URL(), "LUX_TUNNEL_ENABLED": "1", "LUX_TUNNEL_REGISTRY_TTL": "5s", "LUX_HEALTH_INTERVAL": "5s",
		"LUX_TUNNEL_FORWARD_ADDR": "127.0.0.1:8081", "LUX_TUNNEL_FORWARD_SECRET": forwardSecret,
	})
	defer srv.stop()
	if !strings.Contains(srv.out.String(), "luxd: tunnel: on, registry TTL 5s; other replicas forward to this one at 127.0.0.1:8081 with 1 secret(s)") {
		t.Fatalf("no tunnel line in:\n%s", srv.out.String())
	}
	token := iss.Mint(issuertest.Claims{Sub: "alice"})
	bearer := []string{"Authorization", "Bearer " + token}

	// alice is no admin, and applies a tunneled Provider all the same.
	resp, body := do(t, http.MethodPut, srv.publicURL+"/v1/providers/laptop", `{"spec": {"dialect": "openai", "tunnel": true}}`, bearer...)
	if resp.StatusCode != 201 || !strings.Contains(body, `"tunnel":true`) {
		t.Fatalf("apply: %d %s", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodPut, srv.publicURL+"/v1/providers/openai", `{"spec": {"dialect": "openai", "baseURL": "https://api.example.com/v1", "credential": {"value": "sk-x"}}}`, bearer...)
	if resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("a dialed Provider by a non-admin: %d %s", resp.StatusCode, body)
	}

	// The agent attaches over h2c.
	rt := newFakeRuntime(t)
	ctx, stopAgent := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- agent.Run(ctx, agent.Options{
			Gateway: srv.publicURL, Provider: "laptop", Upstream: rt.srv.URL + "/v1", UserAgent: "lux/test",
			Token:  func() (string, error) { return token, nil },
			Logger: slog.New(slog.DiscardHandler),
		})
	}()
	provider := func() map[string]any {
		_, body := do(t, http.MethodGet, srv.publicURL+"/v1/providers/laptop", "", bearer...)
		var obj map[string]any
		_ = json.Unmarshal([]byte(body), &obj)
		st, _ := obj["status"].(map[string]any)
		return st
	}
	tunnelState := func() string {
		tun, _ := provider()["tunnel"].(map[string]any)
		s, _ := tun["state"].(string)
		return s
	}
	waitUntil(t, "the session to be Connected", func() bool { return tunnelState() == "Connected" })
	// The health job probes on LUX_HEALTH_INTERVAL, a tick apart from the
	// connect, so the state is read when it has run and not the instant the
	// session is up, when status.health is still empty on a slow runner.
	waitUntil(t, "the runtime to be Healthy", func() bool {
		h, _ := provider()["health"].(map[string]any)
		return h["state"] == "Healthy"
	})
	st := provider()
	tun := st["tunnel"].(map[string]any)
	if tun["agent"] != "lux/test" || tun["subject"] != iss.URL()+"|alice" || !strings.HasPrefix(tun["session"].(string), "tun_") {
		t.Errorf("status.tunnel %v", tun)
	}
	// Discovery at connect declared the runtime's model under alice.
	waitUntil(t, "the discovered Model", func() bool {
		_, body := do(t, http.MethodGet, srv.publicURL+"/v1/models", "", bearer...)
		return strings.Contains(body, `"name":"laptop/llama3.1"`) && strings.Contains(body, `"source":"discovered"`)
	})

	// A Key reaches the model through the openai door.
	resp, body = do(t, http.MethodPut, srv.publicURL+"/v1/keys/mine", `{"spec": {"models": ["laptop/*"]}}`, bearer...)
	if resp.StatusCode != 201 {
		t.Fatalf("key: %d %s", resp.StatusCode, body)
	}
	var key struct {
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &key); err != nil || !strings.HasPrefix(key.Status.Value, "lux_") {
		t.Fatalf("key value: %v %s", err, body)
	}
	resp, body = do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model":"laptop/llama3.1","messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer "+key.Status.Value)
	if resp.StatusCode != 200 || !strings.Contains(body, "hello from the laptop") || !strings.Contains(body, `"model":"laptop/llama3.1"`) {
		t.Fatalf("through the door: %d %s", resp.StatusCode, body)
	}
	// TestTunnelCarriesNoCredential: the runtime saw no Key, no issuer
	// token, and no provider credential, and saw the gateway's own
	// header set.
	seen := rt.seen()
	if len(seen) < 2 {
		t.Fatalf("%d requests reached the runtime", len(seen))
	}
	for _, h := range seen {
		for name, values := range h {
			joined := strings.Join(values, " ")
			if strings.Contains(joined, key.Status.Value) || strings.Contains(joined, token) || strings.Contains(joined, "sk-") {
				t.Errorf("the runtime saw a credential in %s", name)
			}
		}
		for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Lux-Tunnel-Session", "X-Forwarded-For"} {
			if h.Get(name) != "" {
				t.Errorf("the runtime saw %s", name)
			}
		}
		if !strings.HasPrefix(h.Get("User-Agent"), "luxd") || !strings.HasPrefix(h.Get("Lux-Request-Id"), "req_") {
			t.Errorf("the runtime saw %v", h)
		}
	}
	// TestTunnelRequestsAreMetered at the wiring: the request counted
	// against the Provider with the runtime's tokens, and the gauge holds
	// one session.
	waitUntil(t, "the metrics", func() bool {
		_, metrics := do(t, http.MethodGet, srv.internalURL+"/metrics", "")
		return strings.Contains(metrics, `provider="laptop"`) && strings.Contains(metrics, "lux_tunnel_sessions 1") && strings.Contains(metrics, "lux_tokens_total")
	})

	// The forward route: on the internal listener under the secret, over
	// HTTP/2, and nowhere else.
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	h2c := &http.Client{Transport: &http.Transport{Protocols: protocols}}
	forward := func(client *http.Client, base, secret string) (int, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/internal/tunnel/prv_nobody", strings.NewReader(""))
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	if code, body := forward(h2c, srv.internalURL, forwardSecret); code != 503 || errorCode(t, body) != "provider_unavailable" {
		t.Errorf("the forward route with the secret: %d %s", code, body)
	}
	if code, body := forward(h2c, srv.internalURL, ""); code != 401 || errorCode(t, body) != "unauthenticated" {
		t.Errorf("the forward route without the secret: %d %s", code, body)
	}
	if code, body := forward(h2c, srv.internalURL, "wrong"); code != 401 || errorCode(t, body) != "unauthenticated" {
		t.Errorf("the forward route with a wrong secret: %d %s", code, body)
	}
	if code, _ := forward(http.DefaultClient, srv.internalURL, forwardSecret); code != 404 {
		t.Errorf("the forward route over HTTP/1.1: %d", code)
	}
	if code, _ := forward(h2c, srv.publicURL, forwardSecret); code != 404 {
		t.Errorf("the forward route on the public listener: %d", code)
	}

	// The agent stops: the row goes at once, and the holder's next tick
	// makes the Provider Unreachable and Disconnected within the TTL.
	stopAgent()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the agent stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not stop")
	}
	deadline := time.Now().Add(12 * time.Second)
	for tunnelState() != "Disconnected" {
		if time.Now().After(deadline) {
			t.Fatalf("the Provider is still %s after the agent stopped: %v", tunnelState(), provider())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if health, _ := provider()["health"].(map[string]any); health["state"] != "Unreachable" {
		t.Errorf("status.health after the loss %v", health)
	}
	_, metrics := do(t, http.MethodGet, srv.internalURL+"/metrics", "")
	if !strings.Contains(metrics, "lux_tunnel_sessions 0") {
		t.Errorf("the gauge after the loss:\n%s", metrics)
	}
	resp, body = do(t, http.MethodPost, srv.publicURL+"/openai/v1/chat/completions", `{"model":"laptop/llama3.1","messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer "+key.Status.Value)
	if resp.StatusCode != 503 || !strings.Contains(body, "provider_unavailable") {
		t.Errorf("the door after the loss: %d %s", resp.StatusCode, body)
	}
}

// TestTunnelOffNotices: without LUX_TUNNEL_ENABLED the start-up line
// says the tunnel is off, the routes are not_found, and the sessions
// gauge of spec 019 is scraped at zero all the same; with it in the
// file mode the line says why the tunnel stays off.
func TestTunnelOffNotices(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	srv := startServe(t, map[string]string{"LUX_OIDC_ISSUERS": iss.URL()})
	if !strings.Contains(srv.out.String(), "luxd: tunnel: off; LUX_TUNNEL_ENABLED=1 serves the tunnel routes") {
		t.Fatalf("no tunnel line in:\n%s", srv.out.String())
	}
	resp, body := do(t, http.MethodPost, srv.publicURL+"/v1/providers/laptop/tunnel", "", "Authorization", "Bearer "+iss.Mint(issuertest.Claims{Sub: "alice"}))
	if resp.StatusCode != 404 || errorCode(t, body) != "not_found" {
		t.Errorf("the session route with the tunnel off: %d %s", resp.StatusCode, body)
	}
	if _, metrics := do(t, http.MethodGet, srv.internalURL+"/metrics", ""); !strings.Contains(metrics, "lux_tunnel_sessions 0") {
		t.Errorf("the gauge with the tunnel off:\n%s", metrics)
	}
	srv.stop()

	dir := t.TempDir()
	writeFile(t, dir, "provider.yaml", providerYAML)
	file := startServe(t, map[string]string{"LUX_MANIFEST_DIR": dir, "OPENAI_KEY": "sk-live", "LUX_OIDC_ISSUERS": "", "LUX_SECRETS_KEK": "", "LUX_TUNNEL_ENABLED": "1"})
	defer file.stop()
	if !strings.Contains(file.out.String(), "luxd: tunnel: off in the file mode") {
		t.Fatalf("no tunnel line in:\n%s", file.out.String())
	}
}

// waitUntil polls cond for up to five seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
