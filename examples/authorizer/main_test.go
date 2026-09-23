// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"

	"latere.ai/x/lux/authorizer"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The document's authorizer is held to the contract it implements,
// latere.ai/x/pkg/authz/conformance, under the gateway's own action
// vocabulary, and to the four properties docs/plane.md says to keep as
// the policy grows.

// token is the bearer this endpoint requires in the tests.
const token = "the-platform-authorizer-token"

// TestPlaneDocAuthorizerConforms runs the conformance suite every
// authorizer passes against the endpoint docs/plane.md prints, driven
// from the declared table rather than from a list written out here: the
// probe is denied for every subject and action, a wrong bearer and no
// bearer are refused, an action outside the table is a 400, and every
// well-formed request is answered with a decision of the contract's
// shape.
func TestPlaneDocAuthorizerConforms(t *testing.T) {
	server := httptest.NewServer(handler(token))
	t.Cleanup(server.Close)
	conformance.Run(t, server.URL, token,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects("https://login.example.com|alice", "https://login.example.com|bob"),
	)
}

// ask is one envelope for the decider: the subject, the plan its issuer
// stamped, the action, and the resource.
func ask(subject, plan, action string, res authz.Resource) authz.Request {
	claims := map[string]any{}
	if plan != "" {
		claims["plan"] = plan
	}
	return authz.Request{Subject: subject, Claims: claims, Action: action, Resource: res}
}

// TestTheMinimalAuthorizerDecides is the policy itself, row by row: the
// catalog readable by everyone and declared by an administrator, an
// object to its owner alone, and a ceiling and a filter on everything
// else. The probe and an action outside the vocabulary are not rows
// here: the scaffold answers both before the policy is called, and
// TestTheEndpointReadsItsBearerAndItsBody holds it to that.
func TestTheMinimalAuthorizerDecides(t *testing.T) {
	const alice, bob = "https://login.example.com|alice", "https://login.example.com|bob"
	provider := func(owner string) authz.Resource {
		return authz.NewResource("Provider", "prv_1", map[string]any{"owner": owner})
	}
	key := func(owner string) authz.Resource {
		return authz.NewResource("Key", "key_1", map[string]any{"owner": owner})
	}
	create := func(kind, name string) authz.Resource {
		return authz.NewResource(kind, "", map[string]any{"name": name})
	}
	for _, tc := range []struct {
		name    string
		in      authz.Request
		allow   bool
		reason  string
		limits  bool
		filters bool
	}{
		{"the catalog is read by everyone", ask(alice, "", authorizer.ActionProviderRead, provider(bob)), true, "", false, false},
		{"a Model is used by everyone", ask(alice, "", authorizer.ActionModelUse, authz.NewResource("Model", "", map[string]any{"selector": "*"})), true, "", false, false},
		{"the catalog is declared by an administrator", ask(alice, "admin", authorizer.ActionProviderCreate, create("Provider", "openai")), true, "", false, false},
		{"and by nobody else", ask(alice, "team", authorizer.ActionModelCreate, create("Model", "gpt-5")), false, "the catalog is declared by the platform", false, false},
		{"another subject's Key", ask(alice, "team", authorizer.ActionKeyDelete, key(bob)), false, "not yours", false, false},
		{"a subject's own Key", ask(alice, "team", authorizer.ActionKeyDelete, key(alice)), true, "", true, true},
		{"a create under a plan's ceiling", ask(alice, "free", authorizer.ActionKeyCreate, create("Key", "run-42")), true, "", true, true},
		{"an administrator is under no ceiling", ask(alice, "admin", authorizer.ActionKeyCreate, create("Key", "run-42")), true, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := policy{}.Decide(t.Context(), tc.in)
			if err != nil {
				t.Fatalf("the decider failed: %v", err)
			}
			if got.Allow != tc.allow || got.Reason != tc.reason {
				t.Errorf("allow %v reason %q, want %v %q", got.Allow, got.Reason, tc.allow, tc.reason)
			}
			if (got.Limits != nil) != tc.limits {
				t.Errorf("limits %s", got.Limits)
			}
			if (got.Filter != nil) != tc.filters {
				t.Errorf("filter %v", got.Filter)
			}
			if !got.Allow && got.Reason == "" {
				t.Error("a deny with no reason; the reason is the developer detail of the gateway's 403")
			}
			if got.Allow && tc.filters && (len(got.Filter.Owners) != 1 || got.Filter.Owners[0] != alice) {
				t.Errorf("the filter reads as %v; a list returns the subject's own", got.Filter)
			}
		})
	}
	// The plan's ceiling is the one a Key may ask for, and it grows with
	// the plan. It reads back through the package luxd decodes it with.
	spend := func(plan string) string {
		t.Helper()
		d, err := policy{}.Decide(t.Context(), ask(alice, plan, authorizer.ActionKeyCreate, create("Key", "run-42")))
		if err != nil {
			t.Fatalf("the %s ceiling: %v", plan, err)
		}
		limits, err := authorizer.DecodeLimits(d)
		if err != nil {
			t.Fatalf("the %s ceiling does not decode: %v", plan, err)
		}
		return limits.Key.MaxSpend.String()
	}
	if free, team := spend("free"), spend("team"); free != "5" || team != "50" {
		t.Errorf("the ceilings are %s and %s, want 5 and 50", free, team)
	}
}

// TestTheEndpointReadsItsBearerAndItsBody: the scaffold around the
// policy answers a decision to a POST under its bearer and nothing else,
// refuses an action outside the vocabulary as a malformed request rather
// than deciding it, and denies the reserved probe id before the policy
// is reached.
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
	if resp := post(t, token, `{"subject":"a","action":"key.rotate","resource":{"kind":"Key"}}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an action outside the vocabulary = %d, want a 400 and no verdict", resp.StatusCode)
	}
	if resp := post(t, token, `{"subject":"a","action":"key.read","resource":{"kind":"Provider","id":"prv_1"}}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a resource of another kind = %d, want a 400", resp.StatusCode)
	}
	probe := post(t, token, `{"action":"key.read","resource":{"kind":"Key","id":"`+authz.ProbeID+`"}}`)
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("the probe = %d, want a 200 carrying a deny", probe.StatusCode)
	}
	var answer struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(probe.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if answer.Allow || answer.Reason != authz.ReasonProbe {
		t.Errorf("the probe answered %+v; it is denied for every subject and action", answer)
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
	limits, err := authorizer.DecodeLimits(d)
	if err != nil || limits.Key.MaxSpend != v1.Money(5_000_000) || limits.MaxKeys != 100 {
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

func TestOwnerAssignmentIsExplicitlyAdminOnly(t *testing.T) {
	for _, plan := range []string{"free", "team", "admin"} {
		req := authz.Request{Subject: "https://login.example.com|writer", Claims: map[string]any{"plan": plan}, Action: authorizer.ActionOwnerAssign, Resource: authorizer.OwnerAssignment("Key", "managed", "https://login.example.com|target", nil)}
		d, err := (policy{}).Decide(t.Context(), req)
		if err != nil || d.Allow != (plan == "admin") {
			t.Fatalf("%s: %+v %v", plan, d, err)
		}
	}
}
