// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"

	agent "latere.ai/x/lux/client/tunnel"
	"latere.ai/x/lux/internal/luxcli"
)

// mounts are the two ways a listener sits under a base path, each with
// the address its control plane answers at below the base.
var mounts = []struct {
	mode    string
	control string
}{
	{"prefix", "/v1"},
	{"replace", ""},
}

// TestCLIUnderReplacedBasePath is spec 040's lux command: given only the
// installation's public URL, it finds the control plane under either
// mount from /.well-known/lux and applies, reads, and lists through it.
func TestCLIUnderReplacedBasePath(t *testing.T) {
	const base = "/v1/models"
	for _, m := range mounts {
		t.Run(m.mode, func(t *testing.T) {
			iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
			srv := startServe(t, map[string]string{
				"LUX_OIDC_ISSUERS": iss.URL(), "LUX_BASE_PATH": base, "LUX_BASE_PATH_MODE": m.mode,
				"LUX_PUBLIC_URL": "https://api.example.com" + base,
			})
			defer srv.stop()
			env := map[string]string{"LUX_URL": srv.publicURL + base, "LUX_TOKEN": iss.Mint(issuertest.Claims{Sub: "alice"})}
			lux := func(args ...string) string {
				t.Helper()
				var out, errOut bytes.Buffer
				code := luxcli.Run(t.Context(), luxcli.Options{
					Args: args, Getenv: func(k string) string { return env[k] }, Stdout: &out, Stderr: &errOut, Version: "test",
				})
				if code != 0 {
					t.Fatalf("lux %s: exit %d\nstdout %s\nstderr %s", strings.Join(args, " "), code, out.String(), errOut.String())
				}
				return out.String()
			}
			if out := lux("whoami"); !strings.Contains(out, iss.URL()+"|alice") {
				t.Errorf("whoami: %s", out)
			}
			if out := lux("budgets", "create", "team", "-amount", "5"); !strings.Contains(out, `"name":"team"`) {
				t.Errorf("budgets create: %s", out)
			}
			if out := lux("get", "budget", "team"); !strings.Contains(out, `"name":"team"`) {
				t.Errorf("get: %s", out)
			}
			if out := lux("list", "budgets"); !strings.Contains(out, `"name":"team"`) {
				t.Errorf("list: %s", out)
			}
			// The Budget is where the mount says the control plane is.
			bearer := []string{"Authorization", "Bearer " + env["LUX_TOKEN"]}
			if resp, body := do(t, http.MethodGet, srv.publicURL+base+m.control+"/budgets/team", "", bearer...); resp.StatusCode != http.StatusOK {
				t.Errorf("the Budget at %s%s/budgets/team = %d: %s", base, m.control, resp.StatusCode, body)
			}
		})
	}
}

// TestTunnelUnderReplacedBasePath is spec 040's tunnel agent: given only
// the installation's public URL, it finds the session and carrier routes
// under either mount from /.well-known/lux, attaches a runtime, and a
// Key reaches the runtime through the door under the base.
func TestTunnelUnderReplacedBasePath(t *testing.T) {
	const base = "/v1/models"
	for _, m := range mounts {
		t.Run(m.mode, func(t *testing.T) {
			iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
			srv := startServe(t, map[string]string{
				"LUX_OIDC_ISSUERS": iss.URL(), "LUX_TUNNEL_ENABLED": "1", "LUX_HEALTH_INTERVAL": "5s",
				"LUX_BASE_PATH": base, "LUX_BASE_PATH_MODE": m.mode, "LUX_PUBLIC_URL": "http://localhost" + base,
			})
			defer srv.stop()
			token := iss.Mint(issuertest.Claims{Sub: "alice"})
			bearer := []string{"Authorization", "Bearer " + token}
			control := srv.publicURL + base + m.control
			if resp, body := do(t, http.MethodPut, control+"/providers/laptop", `{"spec": {"dialect": "openai", "tunnel": true}}`, bearer...); resp.StatusCode != http.StatusCreated {
				t.Fatalf("apply: %d %s", resp.StatusCode, body)
			}

			rt := newFakeRuntime(t)
			ctx, stopAgent := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() {
				result <- agent.Run(ctx, agent.Options{
					Gateway: srv.publicURL + base, Provider: "laptop", Upstream: rt.srv.URL + "/v1", UserAgent: "lux/test",
					Token:  func() (string, error) { return token, nil },
					Logger: slog.New(slog.DiscardHandler),
				})
			}()
			waitUntil(t, "the session to be Connected", func() bool {
				_, body := do(t, http.MethodGet, control+"/providers/laptop", "", bearer...)
				return strings.Contains(body, `"state":"Connected"`)
			})
			waitUntil(t, "the discovered Model", func() bool {
				_, body := do(t, http.MethodGet, control+"/models", "", bearer...)
				return strings.Contains(body, `"name":"laptop/llama3.1"`)
			})
			resp, body := do(t, http.MethodPut, control+"/keys/mine", `{"spec": {"models": ["laptop/*"]}}`, bearer...)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("key: %d %s", resp.StatusCode, body)
			}
			var key struct {
				Status struct {
					Value string `json:"value"`
				} `json:"status"`
			}
			if err := json.Unmarshal([]byte(body), &key); err != nil {
				t.Fatal(err)
			}
			resp, body = do(t, http.MethodPost, srv.publicURL+base+"/openai/v1/chat/completions",
				`{"model":"laptop/llama3.1","messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer "+key.Status.Value)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hello from the laptop") {
				t.Fatalf("through the door under the base: %d %s", resp.StatusCode, body)
			}
			stopAgent()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("the agent stopped with %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the agent did not stop")
			}
		})
	}
}
