// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authkit/jwt"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// alice is a caller as the verifier renders one, with claims of the kind
// an issuer stamps and a platform adds.
var alice = Caller{
	Subject: fixtureSubject, Issuer: fixtureIssuer, Sub: "alice",
	Claims: map[string]any{"iss": fixtureIssuer, "sub": "alice", "aud": []any{"lux"}, "exp": float64(4102444800),
		"email": "alice@example.com", "groups": []any{"research"}, "plan": "team"},
}

// info is what the authorizer learns about the request.
var info = authz.Caller{ID: "req_01J9TESTREQUEST000000000000", IP: "203.0.113.4", UserAgent: "lux/0.1"}

// clock is a settable clock for the shared client's cache.
type clock struct{ now atomic.Pointer[time.Time] }

func newClock() *clock {
	c := &clock{}
	t := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	c.now.Store(&t)
	return c
}

func (c *clock) Now() time.Time { return *c.now.Load() }

func (c *clock) Advance(d time.Duration) {
	t := c.Now().Add(d)
	c.now.Store(&t)
}

// newClient builds the shared client over a stub with the given options.
func newClient(t *testing.T, url, token string, o authz.Options) *authz.Client {
	t.Helper()
	o.URL, o.Token = url, token
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	c, err := authz.NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newStubAuthorizer is the Authorizer over a fresh stub.
func newStubAuthorizer(t *testing.T, opts ...stub.Option) (*stub.Server, *Authorizer) {
	t.Helper()
	s := stub.New(t, opts...)
	return s, NewAuthorizer(newClient(t, s.URL(), s.Token(), authz.Options{}))
}

// wantCode asserts err is an *Error of the code and returns it.
func wantCode(t *testing.T, err error, code Code) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	if e.Message != code.Message() {
		t.Errorf("message %q is not the fixed sentence of %s", e.Message, code)
	}
	return e
}

// TestAuthorizerRequestShapes: every action of the table reaches the
// stub with the table's resource shape, and the request carries subject,
// issuer, sub, and every claim of the verified token verbatim, with the
// request's own id, address, and user agent.
func TestAuthorizerRequestShapes(t *testing.T) {
	iss := newIssuer(t)
	v := newVerifier(t, iss.URL())
	token := iss.Mint(issuertest.Claims{Sub: "alice", Extra: map[string]any{
		"email": "alice@example.com", "groups": []string{"research"}, "plan": map[string]any{"name": "team", "seats": 5},
	}})
	caller, err := v.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := jwt.DecodePayload(token, &payload); err != nil {
		t.Fatal(err)
	}

	s, z := newStubAuthorizer(t)
	want := shapes(t)
	for _, action := range authorizer.Actions() {
		t.Run(action, func(t *testing.T) {
			s.ClearRequests()
			if _, err := z.Decide(t.Context(), caller, action, resourceOf(t, action), info); err != nil {
				t.Fatal(err)
			}
			reqs := s.Requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want one", len(reqs))
			}
			got := reqs[0]
			if got.Subject != iss.URL()+"|alice" || got.Issuer != iss.URL() || got.Sub != "alice" {
				t.Errorf("subject fields %q %q %q", got.Subject, got.Issuer, got.Sub)
			}
			if !reflect.DeepEqual(got.Claims, payload) {
				t.Errorf("claims\n got %v\nwant %v", got.Claims, payload)
			}
			if got.Action != action {
				t.Errorf("action %q", got.Action)
			}
			if res := flat(t, got.Resource); !reflect.DeepEqual(res, want[action]) {
				t.Errorf("resource\n got %v\nwant %v", res, want[action])
			}
			if got.Request != info {
				t.Errorf("request %+v, want %+v", got.Request, info)
			}
			if got.Workload != nil {
				t.Errorf("workload %v was sent; luxd sends none", got.Workload)
			}
		})
	}
}

// TestAuthorizerUnavailability: a refused connection, a TLS failure, a
// non-200, a body that does not parse, a body without allow, and a
// timeout are each authorizer_unavailable, none is an allow, and the
// fixed sentence carries none of the detail.
func TestAuthorizerUnavailability(t *testing.T) {
	refused := stub.New(t)
	url, token := refused.URL(), refused.Token()
	refused.Close()

	tls := httptest.NewTLSServer(stub.NewHandler().Handler())
	t.Cleanup(tls.Close)

	for _, tc := range []struct {
		name  string
		build func(t *testing.T) *Authorizer
		want  string
	}{
		{"a refused connection", func(t *testing.T) *Authorizer {
			return NewAuthorizer(newClient(t, url, token, authz.Options{}))
		}, "connection refused"},
		{"a TLS failure", func(t *testing.T) *Authorizer {
			return NewAuthorizer(newClient(t, tls.URL, stub.DefaultToken, authz.Options{}))
		}, "certificate"},
		{"a non-200 status", func(t *testing.T) *Authorizer {
			s, z := newStubAuthorizer(t)
			s.Fail(http.StatusInternalServerError)
			return z
		}, "answered 500"},
		{"a wrong bearer", func(t *testing.T) *Authorizer {
			s := stub.New(t)
			return NewAuthorizer(newClient(t, s.URL(), "not-the-token", authz.Options{}))
		}, "answered 401"},
		{"a body that does not parse", func(t *testing.T) *Authorizer {
			s, z := newStubAuthorizer(t)
			s.FailBody(stub.BodyMalformed)
			return z
		}, "body: "},
		{"a body without allow", func(t *testing.T) *Authorizer {
			s, z := newStubAuthorizer(t)
			s.FailBody(stub.BodyNoAllow)
			return z
		}, "no allow field"},
		{"a timeout", func(t *testing.T) *Authorizer {
			s := stub.New(t)
			s.Hang()
			return NewAuthorizer(newClient(t, s.URL(), s.Token(), authz.Options{Timeout: 100 * time.Millisecond}))
		}, "deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := tc.build(t)
			_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info)
			e := wantCode(t, err, CodeAuthorizerUnavailable)
			if !strings.Contains(e.Detail, tc.want) {
				t.Errorf("detail %q lacks %q", e.Detail, tc.want)
			}
			if e.Message != "The permission service is unavailable; retry shortly." {
				t.Errorf("message %q", e.Message)
			}
			// The Lookup gives the same answer as manifest's code.
			var me *manifest.Error
			if _, err := z.Lookup(alice, info, catalogOf(fixtureProvider())).Provider(t.Context(), "team-openai"); !errors.As(err, &me) || me.Code != manifest.CodeAuthorizerUnavailable {
				t.Errorf("Lookup.Provider = %v, want authorizer_unavailable", err)
			}
		})
	}
}

// TestAuthorizerRetriesOnlyBeforeAResponseLine: a connection closed
// before any response line is retried once and no more; a non-200 and a
// timeout after the request was sent are asked once.
func TestAuthorizerRetriesOnlyBeforeAResponseLine(t *testing.T) {
	t.Run("a connection closed before a response line", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		var accepted atomic.Int32
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				accepted.Add(1)
				_ = conn.Close()
			}
		}()
		z := NewAuthorizer(newClient(t, "http://"+ln.Addr().String(), "t", authz.Options{}))
		_, err = z.Decide(t.Context(), alice, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info)
		wantCode(t, err, CodeAuthorizerUnavailable)
		if n := accepted.Load(); n != 2 {
			t.Errorf("%d connection(s), want the call and one retry", n)
		}
	})
	t.Run("a non-200", func(t *testing.T) {
		s, z := newStubAuthorizer(t)
		s.Fail(http.StatusBadGateway)
		_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info)
		wantCode(t, err, CodeAuthorizerUnavailable)
		if n := len(s.Requests()); n != 1 {
			t.Errorf("%d request(s), want one and no retry", n)
		}
	})
	t.Run("a timeout after the request was sent", func(t *testing.T) {
		s := stub.New(t)
		s.Hang()
		z := NewAuthorizer(newClient(t, s.URL(), s.Token(), authz.Options{Timeout: 100 * time.Millisecond}))
		_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info)
		wantCode(t, err, CodeAuthorizerUnavailable)
		s.Resume()
		if n := len(s.Requests()); n != 1 {
			t.Errorf("%d request(s), want one and no retry", n)
		}
	})
}

// TestDenyReasonStaysOutOfTheUserSentence: a deny on the request's own
// action is forbidden with the fixed sentence, and the authorizer's
// reason is in the developer detail alone.
func TestDenyReasonStaysOutOfTheUserSentence(t *testing.T) {
	s, z := newStubAuthorizer(t)
	const reason = "plan free does not include budgets, upgrade at /billing"
	s.Deny(stub.Rule{Subject: fixtureSubject, Action: authorizer.ActionBudgetCreate}, reason)
	_, err := z.Decide(t.Context(), alice, authorizer.ActionBudgetCreate, authorizer.BudgetCreate(fixtureBudget(t)), info)
	e := wantCode(t, err, CodeForbidden)
	if e.Message != "You do not have permission to do this." {
		t.Errorf("message %q", e.Message)
	}
	if !strings.Contains(e.Detail, reason) || !strings.Contains(e.Detail, "action=budget.create") || !strings.Contains(e.Detail, "subject="+fixtureSubject) {
		t.Errorf("detail %q lacks the reason, the action, or the subject", e.Detail)
	}
	if strings.Contains(e.Message, "plan") || strings.Contains(e.Message, "billing") {
		t.Errorf("the reason leaked into the sentence %q", e.Message)
	}
}

// allowAllLookup is a manifest.Lookup that resolves every reference, for
// a Resolve whose subject is the ceilings and not the references.
type allowAllLookup struct{}

func (allowAllLookup) Provider(context.Context, string) (*v1.Provider, error) {
	return fixtureProvider(), nil
}
func (allowAllLookup) Budget(context.Context, string) (*v1.Budget, error) {
	return &v1.Budget{Metadata: v1.ObjectMeta{Name: "team-research"}}, nil
}
func (allowAllLookup) Models(context.Context, string) ([]v1.ModelRef, error) { return nil, nil }

// TestAuthorizerLimitsReachTheirConsumers: every limits field of the
// answer decodes into its figure, the control plane rate, the four Key
// ceilings that Resolve refuses with ceiling_exceeded, and max_keys; an
// answer without limits grants none.
func TestAuthorizerLimitsReachTheirConsumers(t *testing.T) {
	s, z := newStubAuthorizer(t)
	s.Allow(stub.Rule{Subject: fixtureSubject, Limits: map[string]any{
		"requests_per_minute": 1200, "max_key_requests_per_minute": 600, "max_key_tokens_per_minute": 1000000,
		"max_key_spend": "50", "max_key_ttl": "720h", "max_keys": 100,
	}})
	d, err := z.Decide(t.Context(), alice, authorizer.ActionKeyCreate, authorizer.KeyCreate(fixtureKey()), info)
	if err != nil {
		t.Fatal(err)
	}
	want := authorizer.Limits{
		RequestsPerMinute: 1200,
		Key:               manifest.Limits{MaxRequestsPerMinute: 600, MaxTokensPerMinute: 1000000, MaxSpend: *money(t, "50"), MaxTTL: 720 * time.Hour},
		MaxKeys:           100,
	}
	if d.Limits != want {
		t.Fatalf("limits = %+v, want %+v", d.Limits, want)
	}

	// The four ceilings reach Resolve as its Limits and refuse a Key
	// above each, and the same Key resolves under no ceiling.
	rate := 601
	key := &v1.Key{Metadata: v1.ObjectMeta{Name: "run-42"}, Spec: v1.KeySpec{
		Models: []string{"gpt-5"}, TTL: "1h",
		Limits: v1.KeyLimits{RequestsPerMinute: &rate, TokensPerMinute: &rate, Spend: &v1.Spend{Amount: money(t, "10"), Window: "month"}},
	}}
	opts := manifest.Options{Actor: manifest.Actor{Subject: fixtureSubject}, Lookup: allowAllLookup{}, Limits: d.Limits.Key}
	_, err = manifest.Resolve(t.Context(), key, opts)
	var me *manifest.Error
	if !errors.As(err, &me) || me.Code != manifest.CodeCeilingExceeded || !reflect.DeepEqual(me.Paths, []string{"spec.limits.requestsPerMinute"}) {
		t.Fatalf("Resolve under the ceilings = %v, want ceiling_exceeded at spec.limits.requestsPerMinute", err)
	}
	opts.Limits = manifest.Limits{}
	if _, err := manifest.Resolve(t.Context(), key, opts); err != nil {
		t.Fatalf("Resolve under no ceiling = %v", err)
	}

	t.Run("an answer without limits", func(t *testing.T) {
		s.SetRules()
		d, err := z.Decide(t.Context(), alice, authorizer.ActionBudgetCreate, authorizer.BudgetCreate(fixtureBudget(t)), info)
		if err != nil {
			t.Fatal(err)
		}
		if d.Limits != (authorizer.Limits{}) || d.Filter != nil || d.TTL != authz.DefaultTTL {
			t.Errorf("decision = %+v", d)
		}
	})
}

// TestDecodeLimitsThroughDecide: an answer whose limits the gateway
// cannot read is no decision at all, which Decide reports as
// authorizer_unavailable. The decoding itself is the authorizer
// package's.
func TestDecodeLimitsThroughDecide(t *testing.T) {
	s, z := newStubAuthorizer(t)
	s.Allow(stub.Rule{Limits: map[string]any{"max_key_spend": 50}})
	_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyCreate, authorizer.KeyCreate(fixtureKey()), info)
	e := wantCode(t, err, CodeAuthorizerUnavailable)
	if !strings.Contains(e.Detail, "limits this gateway cannot read") {
		t.Errorf("detail %q", e.Detail)
	}
}

// TestAuthorizerFilter: the filter of an allow on a list action reaches
// the decision as the owners and labels named, and an allow without one
// carries none.
func TestAuthorizerFilter(t *testing.T) {
	s, z := newStubAuthorizer(t)
	want := &authz.Filter{Owners: []string{fixtureSubject}, Labels: map[string]string{"team": "research"}}
	s.Allow(stub.Rule{Action: authorizer.ActionKeyList, Filter: want})
	for _, tc := range []struct {
		action string
		res    authz.Resource
		want   *authz.Filter
	}{
		{authorizer.ActionKeyList, authorizer.KeyList(), want},
		{authorizer.ActionUsageRead, authorizer.UsageRead(nil, nil), nil},
	} {
		d, err := z.Decide(t.Context(), alice, tc.action, tc.res, info)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d.Filter, tc.want) {
			t.Errorf("%s: filter %+v, want %+v", tc.action, d.Filter, tc.want)
		}
	}
}

// TestDecisionCache, through the shared client's clock: an allow is held
// for its ttl, 60 s when it names none, and at most 600 s; a deny for
// 5 s; an unavailable answer never; an answer about a resource with no id
// never; and a subject revoked at the authorizer is refused once the
// allow's ttl has passed.
func TestDecisionCache(t *testing.T) {
	type step struct {
		advance time.Duration
		asked   int  // requests the stub has seen after this step
		allowed bool // the answer this step
	}
	object := authorizer.KeyObject(fixtureKey())
	for _, tc := range []struct {
		name  string
		setup func(s *stub.Server)
		res   authz.Resource
		steps []step
	}{
		{"an allow for its ttl", func(s *stub.Server) { s.Allow(stub.Rule{TTL: 30}) }, object,
			[]step{{0, 1, true}, {29 * time.Second, 1, true}, {2 * time.Second, 2, true}}},
		{"an allow for 60 s by default", func(*stub.Server) {}, object,
			[]step{{0, 1, true}, {59 * time.Second, 1, true}, {2 * time.Second, 2, true}}},
		{"an allow capped at 600 s", func(s *stub.Server) { s.Allow(stub.Rule{TTL: 3600}) }, object,
			[]step{{0, 1, true}, {599 * time.Second, 1, true}, {2 * time.Second, 2, true}}},
		{"a deny for 5 s", func(s *stub.Server) { s.Deny(stub.Rule{}, "no") }, object,
			[]step{{0, 1, false}, {4 * time.Second, 1, false}, {2 * time.Second, 2, false}}},
		{"a resource with no id never", func(*stub.Server) {}, authorizer.KeyCreate(fixtureKey()),
			[]step{{0, 1, true}, {0, 2, true}, {0, 3, true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := stub.New(t)
			tc.setup(s)
			clk := newClock()
			z := NewAuthorizer(newClient(t, s.URL(), s.Token(), authz.Options{Now: clk.Now}))
			for i, st := range tc.steps {
				clk.Advance(st.advance)
				_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, tc.res, info)
				if allowed := err == nil; allowed != st.allowed {
					t.Fatalf("step %d: err = %v, want allowed %v", i, err, st.allowed)
				}
				if n := len(s.Requests()); n != st.asked {
					t.Fatalf("step %d: the stub saw %d request(s), want %d", i, n, st.asked)
				}
			}
		})
	}
	t.Run("an unavailable answer never", func(t *testing.T) {
		s := stub.New(t)
		s.Fail(http.StatusServiceUnavailable)
		z := NewAuthorizer(newClient(t, s.URL(), s.Token(), authz.Options{Now: newClock().Now}))
		for i := 1; i <= 2; i++ {
			_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, object, info)
			wantCode(t, err, CodeAuthorizerUnavailable)
			if n := len(s.Requests()); n != i {
				t.Fatalf("the stub saw %d request(s), want %d", n, i)
			}
		}
		s.Resume()
		if _, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, object, info); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
	})
	t.Run("a revocation takes effect within the ttl", func(t *testing.T) {
		s := stub.New(t)
		s.Allow(stub.Rule{TTL: 10})
		clk := newClock()
		z := NewAuthorizer(newClient(t, s.URL(), s.Token(), authz.Options{Now: clk.Now}))
		if _, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, object, info); err != nil {
			t.Fatal(err)
		}
		s.Deny(stub.Rule{Subject: fixtureSubject}, "revoked")
		clk.Advance(9 * time.Second)
		if _, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, object, info); err != nil {
			t.Fatalf("within the ttl the cached allow holds: %v", err)
		}
		clk.Advance(2 * time.Second)
		_, err := z.Decide(t.Context(), alice, authorizer.ActionKeyRead, object, info)
		wantCode(t, err, CodeForbidden)
	})
}

// allowEverything is an authorizer that does not read the request.
type allowEverything struct{}

func (allowEverything) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Allow: true}, nil
}

// TestProbeIdIsAlwaysDenied: the stub authorizer and the owner policy
// deny the probe id for every action and every subject, the anonymous
// one and an admin included, whatever rules are set; Check reads a deny
// as the right answer and an allow as an endpoint that does not read the
// request; and the stub conforms to the shared contract under Lux's
// vocabulary.
func TestProbeIdIsAlwaysDenied(t *testing.T) {
	admin := fixtureIssuer + "|root"
	s, z := newStubAuthorizer(t, stub.WithAllow("*"))
	s.Allow(stub.Rule{Resource: authz.ProbeID})
	policy := NewAuthorizer(&OwnerPolicy{Admins: []string{admin}, Objects: objectsOf()})
	for _, tc := range []struct {
		name string
		z    *Authorizer
	}{{"the stub", z}, {"the owner policy", policy}} {
		for _, subject := range []string{"", fixtureSubject, admin} {
			c := Caller{Subject: subject}
			c.Issuer, c.Sub, _ = authz.SplitSubject(subject)
			for _, action := range authorizer.Actions() {
				res := resourceOf(t, action)
				res.ID = authz.ProbeID
				_, err := tc.z.Decide(t.Context(), c, action, res, info)
				if e := wantCode(t, err, CodeForbidden); tc.name == "the owner policy" && !strings.HasSuffix(e.Detail, authz.ReasonProbe) {
					t.Errorf("%s: %s as %q: reason %q", tc.name, action, subject, e.Detail)
				}
			}
		}
		if err := tc.z.Check(t.Context()); err != nil {
			t.Errorf("%s: Check = %v", tc.name, err)
		}
	}
	if err := NewAuthorizer(allowEverything{}).Check(t.Context()); !errors.Is(err, authz.ErrProbeAllowed) {
		t.Errorf("Check against an authorizer that allows the probe = %v", err)
	}
	// A fresh client, because the shared client holds the probe's deny
	// like any other deny for five seconds.
	closed := stub.New(t)
	fresh := NewAuthorizer(newClient(t, closed.URL(), closed.Token(), authz.Options{}))
	closed.Close()
	if err := fresh.Check(t.Context()); err == nil {
		t.Error("Check against a closed authorizer answered nothing wrong")
	}

	// The suite drives a case per row of the declared table rather than
	// from a list written out here, so it covers the whole vocabulary and
	// holds an action outside it to the one answer, a 400. Lux names no
	// page action: every list answers a decision the filter narrows.
	t.Run("the stub conforms under the vocabulary", func(t *testing.T) {
		s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
		conformance.Run(t, s.URL(), s.Token(), conformance.WithVocabulary(authorizer.Vocabulary()),
			conformance.WithSubjects(fixtureSubject, fixtureIssuer+"|bob"), conformance.WithHTTPClient(&http.Client{}))
	})
}
