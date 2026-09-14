// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"encoding/json"
	"net/http"
	"testing"
)

// stubs is the document a lux-stubs instance serves at GET /: where each
// stub listens, because the binary of spec 015 gives every stub a
// listener of its own and one address cannot name seven. The suite reads
// it once at the start of a run and never guesses a port.
type stubs struct {
	// Providers is one stub provider per dialect, by dialect name.
	Providers map[string]string `json:"providers"`
	// Authorizer is the stub authorizer of latere.ai/x/pkg/authz/stub,
	// whose PUT /fail and POST /resume the identity group drives.
	Authorizer string `json:"authorizer"`
	// Issuer is the stub issuer, named for completeness; the suite mints
	// through Config.Token and reads nothing here.
	Issuer string `json:"issuer"`
	// Sink is the stub event sink of spec 012; empty until it exists.
	Sink string `json:"sink"`
	// Credential is the value every stub provider checks its dialect's
	// credential header for, which the suite writes into the Providers
	// it applies, so a request the gateway forwards without the
	// Provider's credential is a 401 the stub records.
	Credential string `json:"credential"`
}

// received is one request a stub provider recorded, as GET /_received
// answers it: the method, the path, the query, every header, and the
// body as text.
type received struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

// loadStubs reads the stubs document. A URL that is set and does not
// answer fails the run: a partial run must never look like a full one.
func (c *client) loadStubs(t testing.TB) {
	t.Helper()
	if c.cfg.StubsURL == "" {
		return
	}
	resp := c.request(t, http.MethodGet, c.cfg.StubsURL+"/", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("%s: GET %s/ answered %d: %s", EnvStubsURL, c.cfg.StubsURL, resp.Status, excerpt(resp.Body))
	}
	var s stubs
	if err := json.Unmarshal(resp.Body, &s); err != nil || len(s.Providers) == 0 {
		t.Fatalf("%s: GET %s/ is not the stubs document: %v\n%s", EnvStubsURL, c.cfg.StubsURL, err, excerpt(resp.Body))
	}
	for _, d := range c.well.Dialects {
		if s.Providers[d] == "" {
			t.Fatalf("%s: the stubs document names no %s provider", EnvStubsURL, d)
		}
	}
	c.stubs = &s
}

// received reads what one stub provider recorded since it was last
// cleared.
func (c *client) received(t testing.TB, provider string) []received {
	t.Helper()
	resp := c.request(t, http.MethodGet, provider+"/_received", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET %s/_received: %d %s", provider, resp.Status, excerpt(resp.Body))
	}
	var out []received
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("GET %s/_received: %v\n%s", provider, err, excerpt(resp.Body))
	}
	return out
}

// clearReceived forgets what one stub provider recorded.
func (c *client) clearReceived(t testing.TB, provider string) {
	t.Helper()
	if resp := c.request(t, http.MethodDelete, provider+"/_received", nil); resp.Status != http.StatusNoContent {
		t.Fatalf("DELETE %s/_received: %d %s", provider, resp.Status, excerpt(resp.Body))
	}
}

// lastReceived is the newest request a stub provider recorded, or a
// failure when it recorded none.
func (c *client) lastReceived(t testing.TB, provider string) received {
	t.Helper()
	all := c.received(t, provider)
	if len(all) == 0 {
		t.Fatalf("the stub provider at %s received nothing", provider)
	}
	return all[len(all)-1]
}

// authorizerFail drives the stub authorizer's outage: status above zero
// makes every decision that status, zero restores the rule table.
func (c *client) authorizerFail(t testing.TB, status int) {
	t.Helper()
	resp := c.request(t, http.MethodPut, c.stubs.Authorizer+"/fail", map[string]any{"status": status})
	if resp.Status != http.StatusNoContent {
		t.Fatalf("PUT %s/fail: %d %s", c.stubs.Authorizer, resp.Status, excerpt(resp.Body))
	}
}
