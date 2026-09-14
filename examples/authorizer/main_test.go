// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
)

// The document's authorizer is held to the contract it implements,
// latere.ai/x/pkg/authz/conformance, under the gateway's own action
// vocabulary, and to the four properties docs/plane.md says to keep as
// the policy grows.

// token is the bearer this endpoint requires in the tests.
const token = "the-platform-authorizer-token"

// vocabulary is the action table of spec 006, which is what the
// gateway sends and what an authorizer answers.
var vocabulary = []conformance.Action{
	{Name: "provider.create", Kind: "Provider"}, {Name: "provider.read", Kind: "Provider"},
	{Name: "provider.update", Kind: "Provider"}, {Name: "provider.delete", Kind: "Provider"},
	{Name: "provider.tunnel", Kind: "Provider"}, {Name: "provider.list", Kind: "Provider"},
	{Name: "model.create", Kind: "Model"}, {Name: "model.read", Kind: "Model"},
	{Name: "model.update", Kind: "Model"}, {Name: "model.delete", Kind: "Model"},
	{Name: "model.list", Kind: "Model"}, {Name: "model.use", Kind: "Model"},
	{Name: "key.create", Kind: "Key"}, {Name: "key.read", Kind: "Key"},
	{Name: "key.update", Kind: "Key"}, {Name: "key.delete", Kind: "Key"}, {Name: "key.list", Kind: "Key"},
	{Name: "budget.create", Kind: "Budget"}, {Name: "budget.read", Kind: "Budget"},
	{Name: "budget.update", Kind: "Budget"}, {Name: "budget.delete", Kind: "Budget"},
	{Name: "budget.list", Kind: "Budget"}, {Name: "budget.draw", Kind: "Budget"},
	{Name: "usage.read", Kind: "Usage"},
}

// TestPlaneDocAuthorizerConforms runs the conformance suite every
// authorizer passes against the endpoint docs/plane.md prints, under
// the gateway's twenty-four actions and two subjects: the probe is
// denied for every subject and action, a wrong bearer and no bearer are
// refused, and every well-formed request is answered with a decision of
// the contract's shape.
func TestPlaneDocAuthorizerConforms(t *testing.T) {
	server := httptest.NewServer(handler(token))
	t.Cleanup(server.Close)
	conformance.Run(t, server.URL, token,
		conformance.WithActions(vocabulary...),
		conformance.WithSubjects("https://login.example.com|alice", "https://login.example.com|bob"),
	)
}

// TestTheMinimalAuthorizerDecides is the policy itself, row by row: the
// probe first, the catalogue readable by everyone and declared by an
// administrator, an object to its owner alone, and a ceiling and a
// filter on everything else.
func TestTheMinimalAuthorizerDecides(t *testing.T) {
	const alice, bob = "https://login.example.com|alice", "https://login.example.com|bob"
	for _, tc := range []struct {
		name    string
		in      req
		allow   bool
		reason  string
		limits  bool
		filters bool
	}{
		{"the probe, whatever the plan", req{Subject: alice, Claims: map[string]any{"plan": "admin"}, Action: "key.read", Resource: map[string]any{"id": probeID}}, false, "the probe id is reserved", false, false},
		{"the probe, anonymous", req{Action: "model.use", Resource: map[string]any{"id": probeID}}, false, "the probe id is reserved", false, false},
		{"the catalogue is read by everyone", req{Subject: alice, Action: "provider.read", Resource: map[string]any{"id": "prv_1", "owner": bob}}, true, "", false, false},
		{"a Model is used by everyone", req{Subject: alice, Action: "model.use", Resource: map[string]any{"selector": "*"}}, true, "", false, false},
		{"the catalogue is declared by an administrator", req{Subject: alice, Claims: map[string]any{"plan": "admin"}, Action: "provider.create", Resource: map[string]any{"name": "openai"}}, true, "", false, false},
		{"and by nobody else", req{Subject: alice, Claims: map[string]any{"plan": "team"}, Action: "model.create", Resource: map[string]any{"name": "gpt-5"}}, false, "the catalogue is declared by the platform", false, false},
		{"another subject's Key", req{Subject: alice, Claims: map[string]any{"plan": "team"}, Action: "key.delete", Resource: map[string]any{"id": "key_1", "owner": bob}}, false, "not yours", false, false},
		{"a subject's own Key", req{Subject: alice, Claims: map[string]any{"plan": "team"}, Action: "key.delete", Resource: map[string]any{"id": "key_1", "owner": alice}}, true, "", true, true},
		{"a create under a plan's ceiling", req{Subject: alice, Claims: map[string]any{"plan": "free"}, Action: "key.create", Resource: map[string]any{"name": "run-42"}}, true, "", true, true},
		{"an administrator is under no ceiling", req{Subject: alice, Claims: map[string]any{"plan": "admin"}, Action: "key.create", Resource: map[string]any{"name": "run-42"}}, true, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decide(tc.in)
			if got.Allow != tc.allow || got.Reason != tc.reason {
				t.Errorf("allow %v reason %q, want %v %q", got.Allow, got.Reason, tc.allow, tc.reason)
			}
			if (got.Limits != nil) != tc.limits {
				t.Errorf("limits %v", got.Limits)
			}
			if (got.Filter != nil) != tc.filters {
				t.Errorf("filter %v", got.Filter)
			}
			if !got.Allow && got.Reason == "" {
				t.Error("a deny with no reason; the reason is the developer detail of the gateway's 403")
			}
		})
	}
	// The plan's ceiling is the one a Key may ask for, and it grows with
	// the plan.
	free := decide(req{Subject: alice, Claims: map[string]any{"plan": "free"}, Action: "key.create", Resource: map[string]any{}})
	team := decide(req{Subject: alice, Claims: map[string]any{"plan": "team"}, Action: "key.create", Resource: map[string]any{}})
	if free.Limits["max_key_spend"] != "5" || team.Limits["max_key_spend"] != "50" {
		t.Errorf("the ceilings are %v and %v", free.Limits, team.Limits)
	}
}

// TestTheEndpointReadsItsBearerAndItsBody: the handler answers a
// decision to a POST under its bearer and nothing else.
func TestTheEndpointReadsItsBearerAndItsBody(t *testing.T) {
	server := httptest.NewServer(handler(token))
	t.Cleanup(server.Close)
	post := func(t *testing.T, bearer, body string) *http.Response {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	if resp := post(t, token, `{"subject":"a","action":"key.read","resource":{"owner":"a"}}`); resp.StatusCode != http.StatusOK {
		t.Errorf("a decision request = %d", resp.StatusCode)
	}
	if resp := post(t, token, "not a decision request"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a body that is no request = %d", resp.StatusCode)
	}
	if resp := post(t, "", `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no bearer = %d", resp.StatusCode)
	}
	get, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a GET = %d", resp.StatusCode)
	}
}

// TestTheEnvelopeIsTheContracts: the four fields the endpoint reads are
// the ones latere.ai/x/pkg/authz sends, so a request the gateway builds
// decodes here.
func TestTheEnvelopeIsTheContracts(t *testing.T) {
	server := httptest.NewServer(handler(token))
	t.Cleanup(server.Close)
	client, err := authz.NewClient(authz.Options{URL: server.URL, Token: token, HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d, err := client.Authorize(ctx, authz.Request{
		Subject: "https://login.example.com|alice", Issuer: "https://login.example.com", Sub: "alice",
		Claims: map[string]any{"plan": "free"}, Action: "key.create",
		Resource: authz.NewResource("Key", "", map[string]any{"name": "run-42", "labels": map[string]string{}}),
	})
	if err != nil || !d.Allow {
		t.Fatalf("the gateway's own client reads %v %v", d, err)
	}
	var limits struct {
		MaxKeySpend string `json:"max_key_spend"`
		MaxKeys     int    `json:"max_keys"`
	}
	if err := d.DecodeLimits(&limits); err != nil || limits.MaxKeySpend != "5" || limits.MaxKeys != 100 {
		t.Errorf("the ceilings read as %+v: %v", limits, err)
	}
	if d.Filter == nil || len(d.Filter.Owners) != 1 || d.Filter.Owners[0] != "https://login.example.com|alice" {
		t.Errorf("the filter reads as %v", d.Filter)
	}
	if err := authz.Check(ctx, client, "key.read", "Key"); err != nil {
		t.Errorf("the probe: %v", err)
	}
}

// TestRunRefusesWithoutATokenAndServes: the command says what it needs,
// serves when it has it, and stops when its context ends.
func TestRunRefusesWithoutATokenAndServes(t *testing.T) {
	out := &syncBuffer{}
	if code := run(t.Context(), nil, out); code != 2 || !strings.Contains(out.String(), "-token") {
		t.Fatalf("run with no token = %d: %s", code, out.String())
	}
	out.Reset()
	if code := run(t.Context(), []string{"-nonesuch"}, out); code != 2 {
		t.Errorf("run with an unknown flag = %d", code)
	}
	out.Reset()
	if code := run(t.Context(), []string{"-token", token, "-addr", "256.256.256.256:1"}, out); code != 1 {
		t.Errorf("run on an address that cannot be bound = %d: %s", code, out.String())
	}
	out.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"-token", token, "-addr", "127.0.0.1:0"}, out) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "deciding at") {
		if time.Now().After(deadline) {
			t.Fatalf("the endpoint never reported its address: %s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run = %d: %s", code, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the endpoint did not stop")
	}
}

// syncBuffer lets a test read what a command is still writing, which a
// strings.Builder read from two goroutines is not.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}
