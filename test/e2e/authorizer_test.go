// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	vocabulary "latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
)

// TestE2EAuthorizerUnavailability: each of spec 006's six forms of
// unavailability, driven through latere.ai/x/pkg/authz/stub where it has
// the form and through a listener of the test's own where it does not,
// is authorizer_unavailable at the gateway, and none of them is an allow.
func TestE2EAuthorizerUnavailability(t *testing.T) {
	// The four forms the stub has, against one luxd with a short decision
	// deadline so the hang is a timeout and not a wait.
	s := newStack(t, map[string]string{"LUX_AUTHORIZER_TIMEOUT": "500ms"})
	authorizer := s.stubs.urls["authorizer"]
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"timeout", http.MethodPost, "/hang", ""},
		{"non-200", http.MethodPut, "/fail", `{"status":500}`},
		{"body that does not parse", http.MethodPut, "/fail", `{"body":"malformed"}`},
		{"body without allow", http.MethodPut, "/fail", `{"body":"no-allow"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := do(t, tc.method, authorizer+tc.path, nil, tc.body); resp.status != http.StatusNoContent {
				t.Fatalf("%s %s = %d", tc.method, tc.path, resp.status)
			}
			expectUnavailable(t, s.gw.public, s.token, "b-"+strings.ReplaceAll(tc.name, " ", "-"))
			if resp := do(t, http.MethodPost, authorizer+"/resume", nil, ""); resp.status != http.StatusNoContent {
				t.Fatalf("POST /resume = %d", resp.status)
			}
			if resp := do(t, http.MethodGet, s.gw.public+"/v1/budgets/b-"+strings.ReplaceAll(tc.name, " ", "-"), bearer(s.token), ""); resp.status != http.StatusNotFound {
				t.Fatalf("the Budget exists after an unavailable decision: %d %s", resp.status, resp.body)
			}
		})
	}

	// A refused connection: a luxd whose authorizer URL names a port
	// nothing listens at.
	t.Run("refused connection", func(t *testing.T) {
		gw := startLuxd(t, serverEnv(t, s.stubs, map[string]string{"LUX_AUTHORIZER_URL": "http://127.0.0.1:" + strconv.Itoa(freePort(t))}))
		expectUnavailable(t, gw.public, s.token, "b-refused")
	})

	// A TLS failure: a listener that answers the handshake with bytes that
	// are no TLS record.
	t.Run("TLS failure", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte("this is not a TLS record\n"))
				_ = conn.Close()
			}
		}()
		gw := startLuxd(t, serverEnv(t, s.stubs, map[string]string{"LUX_AUTHORIZER_URL": "https://" + ln.Addr().String()}))
		expectUnavailable(t, gw.public, s.token, "b-tls")
	})
}

// expectUnavailable applies a Budget and asserts the refusal is
// authorizer_unavailable, 503, and nothing that reads as an allow.
func expectUnavailable(t *testing.T, public, token, name string) {
	t.Helper()
	h := bearer(token)
	h.Set("Content-Type", "application/yaml")
	resp := do(t, http.MethodPut, public+"/v1/budgets/"+name, h, budgetYAML(name))
	if resp.status != http.StatusServiceUnavailable || resp.code() != "authorizer_unavailable" {
		t.Fatalf("PUT /v1/budgets/%s = %d %s %s, want 503 authorizer_unavailable", name, resp.status, resp.code(), resp.body)
	}
	if !strings.Contains(string(resp.body), `"code":"authorizer_unavailable"`) {
		t.Fatalf("the envelope does not carry the code: %s", resp.body)
	}
}

// TestE2ECheckAgainstTheStubs: the stub authorizer denies authz.ProbeID
// whatever rules are set, so the authorizer row of luxd check passes
// against it. luxd check is spec 017's and not in this build; the row's
// call, auth.Authorizer.Check over the shared client, is made here
// against the stub as a process, with an allow-everything table in
// force.
func TestE2ECheckAgainstTheStubs(t *testing.T) {
	s := startStubs(t)
	authorizer := s.urls["authorizer"]
	if resp := do(t, http.MethodPut, authorizer+"/rules", nil, `{"rules":[{"subject":"*","action":"*","resource":"*","allow":true}]}`); resp.status != http.StatusNoContent {
		t.Fatalf("PUT /rules = %d", resp.status)
	}
	c, err := authz.NewClient(authz.Options{URL: authorizer, Token: stub.DefaultToken, HTTP: &http.Client{}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.NewAuthorizer(c).Check(t.Context()); err != nil {
		t.Fatalf("the check's authorizer row failed against the stub: %v", err)
	}
	// The table was in force: an ordinary decision is the allow it names.
	d, err := c.Authorize(t.Context(), authz.Request{Subject: "s", Action: vocabulary.ActionProviderRead, Resource: authz.NewResource("Provider", "prv_1", nil)})
	if err != nil || !d.Allow {
		t.Fatalf("an ordinary decision under the allow-everything table: %+v, %v", d, err)
	}
	resp := do(t, http.MethodGet, authorizer+"/requests", nil, "")
	if !strings.Contains(string(resp.body), authz.ProbeID) {
		t.Fatalf("the probe did not reach the stub: %s", resp.body)
	}
	// A wrong bearer is refused before any decision.
	if resp := do(t, http.MethodPost, authorizer, bearer("not-the-token"), `{"subject":"s","action":"key.read","resource":{"kind":"Key"}}`); resp.status != http.StatusUnauthorized {
		t.Fatalf("a wrong bearer = %d", resp.status)
	}
}
