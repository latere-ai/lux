// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/config"
)

// env is a Getenv over a map.
func env(m map[string]string) config.Getenv {
	return func(k string) string { return m[k] }
}

// load is config.Load over the map, failing the test on a problem. The
// key of spec 005 is added in server mode, since Load requires it there
// and no case here is about it.
func load(t *testing.T, m map[string]string) config.Config {
	t.Helper()
	if m["LUX_MANIFEST_DIR"] == "" {
		m["LUX_SECRETS_KEK"] = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	}
	if m["LUX_PUBLIC_URL"] == "" {
		m["LUX_PUBLIC_URL"] = "https://lux.example.com"
	}
	cfg, err := config.Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestStartupBuildsEachPolicy: from the configuration, no issuer is the
// file mode with nothing to authenticate or authorize, an issuer without
// an authorizer is the owner policy, and an issuer with an authorizer is
// the shared client; the start-up line names each.
func TestStartupBuildsEachPolicy(t *testing.T) {
	iss := newIssuer(t)
	s := stub.New(t)
	objects := objectsOf(fixtureKey())
	bob := Caller{Subject: otherSubject, Issuer: fixtureIssuer, Sub: "bob"}

	t.Run("the file mode", func(t *testing.T) {
		a, err := Startup(t.Context(), load(t, map[string]string{"LUX_MANIFEST_DIR": t.TempDir()}), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.Policy != PolicyFile || a.Verifier != nil || a.Client != nil || a.Authorizer(objects) != nil {
			t.Fatalf("auth = %+v", a)
		}
		if got := a.String(); !strings.Contains(got, "file mode") {
			t.Errorf("String() = %q", got)
		}
	})
	t.Run("the owner policy", func(t *testing.T) {
		a, err := Startup(t.Context(), load(t, map[string]string{
			"LUX_OIDC_ISSUERS": iss.URL(), "LUX_ADMIN_SUBJECTS": adminSubject,
		}), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.Policy != PolicyOwner || a.Verifier == nil || a.Client != nil {
			t.Fatalf("auth = %+v", a)
		}
		if got := a.String(); got != "identity: issuers "+iss.URL()+"; audience lux; owner policy with 1 admin subject(s)" {
			t.Errorf("String() = %q", got)
		}
		z := a.Authorizer(objects)
		if _, err := z.Decide(t.Context(), bob, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info); err == nil {
			t.Error("the owner policy let bob read alice's key")
		}
		if _, err := z.Decide(t.Context(), Caller{Subject: adminSubject}, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info); err != nil {
			t.Errorf("the owner policy refused the admin: %v", err)
		}
		if _, err := a.Verifier.Verify(iss.Mint(issuertest.Claims{Sub: "bob"})); err != nil {
			t.Errorf("the verifier refused the issuer's token: %v", err)
		}
	})
	t.Run("an authorizer", func(t *testing.T) {
		a, err := Startup(t.Context(), load(t, map[string]string{
			"LUX_OIDC_ISSUERS": iss.URL(), "LUX_AUTHORIZER_URL": s.URL(), "LUX_AUTHORIZER_TOKEN": s.Token(),
			"LUX_AUTHORIZER_TIMEOUT": "2s", "LUX_ADMIN_SUBJECTS": adminSubject,
		}), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if a.Policy != PolicyAuthorizer || a.Verifier == nil || a.Client == nil || a.Client.URL() != s.URL() {
			t.Fatalf("auth = %+v", a)
		}
		got := a.String()
		if got != "identity: issuers "+iss.URL()+"; audience lux; authorizer "+s.URL()+"; LUX_ADMIN_SUBJECTS read and unused (1 admin subject(s))" {
			t.Errorf("String() = %q", got)
		}
		if strings.Contains(got, s.Token()) {
			t.Error("the start-up line carries the authorizer's token")
		}
		if _, err := a.Authorizer(nil).Decide(t.Context(), bob, authorizer.ActionKeyRead, authorizer.KeyObject(fixtureKey()), info); err != nil {
			t.Errorf("the stub refused bob: %v", err)
		}
	})
}

// TestStartupRefusals: an issuer that cannot be read and an authorizer
// URL without its token are start-up failures naming the variable.
func TestStartupRefusals(t *testing.T) {
	iss := newIssuer(t)
	for _, tc := range []struct {
		name string
		o    Options
		want string
	}{
		{"an unreachable issuer", Options{Issuers: []string{"http://127.0.0.1:1"}, Audiences: []string{audience}}, "LUX_OIDC_ISSUERS: issuer http://127.0.0.1:1"},
		{"a URL without its token", Options{Issuers: []string{iss.URL()}, Audiences: []string{audience}, AuthorizerURL: "http://127.0.0.1:1"}, "LUX_AUTHORIZER_TOKEN is unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(t.Context(), tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New() = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestAdminSubjectsIgnoredUnderAnAuthorizer: with an authorizer set, a
// subject in LUX_ADMIN_SUBJECTS receives no allow the authorizer did not
// give, and the start-up line reports the variable as read and unused;
// the same subject under the owner policy is allowed everything.
func TestAdminSubjectsIgnoredUnderAnAuthorizer(t *testing.T) {
	iss := newIssuer(t)
	s := stub.New(t)
	s.Deny(stub.Rule{Subject: adminSubject}, "no rule for root")
	root := Caller{Subject: adminSubject, Issuer: fixtureIssuer, Sub: "root"}
	res := authorizer.ProviderCreate(fixtureProvider())

	withAuthorizer, err := New(t.Context(), Options{
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, AuthorizerURL: s.URL(), AuthorizerToken: s.Token(),
		AdminSubjects: []string{adminSubject}, HTTP: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = withAuthorizer.Authorizer(objectsOf()).Decide(t.Context(), root, authorizer.ActionProviderCreate, res, info)
	if e := wantCode(t, err, CodeForbidden); !strings.Contains(e.Detail, "no rule for root") {
		t.Errorf("detail %q", e.Detail)
	}
	if !strings.Contains(withAuthorizer.String(), "LUX_ADMIN_SUBJECTS read and unused") {
		t.Errorf("String() = %q", withAuthorizer.String())
	}
	if n := len(s.Requests()); n != 1 {
		t.Errorf("the authorizer saw %d request(s); the admin list short-circuited nothing", n)
	}

	withoutAuthorizer, err := New(t.Context(), Options{
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, AdminSubjects: []string{adminSubject}, HTTP: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutAuthorizer.Authorizer(objectsOf()).Decide(t.Context(), root, authorizer.ActionProviderCreate, res, info); err != nil {
		t.Errorf("the owner policy refused the admin: %v", err)
	}
}

// recorder is a transport that keeps every request it sent.
type recorder struct {
	mu   sync.Mutex
	sent []*http.Request
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.sent = append(r.sent, req.Clone(req.Context()))
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

// TestAuthorizerTokenStaysOnItsEndpoint: LUX_AUTHORIZER_TOKEN is the
// bearer of every authorizer call and of no other, so the issuer's
// discovery and key set requests carry no Authorization header. The
// sink's requests are spec 012's and join the e2e capture with it.
func TestAuthorizerTokenStaysOnItsEndpoint(t *testing.T) {
	iss := newIssuer(t)
	s := stub.New(t, stub.WithToken("authorizer-bearer-0123"))
	rec := &recorder{}
	a, err := New(t.Context(), Options{
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, AuthorizerURL: s.URL(), AuthorizerToken: s.Token(),
		HTTP: &http.Client{Transport: rec},
	})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := a.Verifier.Verify(iss.Mint(issuertest.Claims{Sub: "alice"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authorizer(nil).Decide(t.Context(), caller, authorizer.ActionKeyCreate, authorizer.KeyCreate(fixtureKey()), info); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	toIssuer, toAuthorizer := 0, 0
	for _, r := range rec.sent {
		got := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(r.URL.String(), s.URL()):
			toAuthorizer++
			if got != "Bearer "+s.Token() {
				t.Errorf("%s %s carried %q, want the authorizer's bearer", r.Method, r.URL, got)
			}
		case strings.HasPrefix(r.URL.String(), iss.URL()):
			toIssuer++
			if got != "" {
				t.Errorf("%s %s carried %q; the issuer gets no bearer", r.Method, r.URL, got)
			}
		default:
			t.Errorf("%s %s: a request to neither endpoint", r.Method, r.URL)
		}
	}
	// Discovery and the key set at start, the key set again at the first
	// token, and one decision.
	if toIssuer < 3 || toAuthorizer != 1 {
		t.Errorf("%d request(s) to the issuer and %d to the authorizer", toIssuer, toAuthorizer)
	}
}

// TestAuthorizerObservesEveryCall: the Observe hook of the options
// receives the result of every call the shared client makes, for the
// metric of spec 019.
func TestAuthorizerObservesEveryCall(t *testing.T) {
	iss := newIssuer(t)
	s := stub.New(t)
	s.Deny(stub.Rule{Action: authorizer.ActionModelCreate}, "no")
	var mu sync.Mutex
	var results []string
	a, err := New(t.Context(), Options{
		Issuers: []string{iss.URL()}, Audiences: []string{audience}, AuthorizerURL: s.URL(), AuthorizerToken: s.Token(), HTTP: &http.Client{},
		Observe: func(result string, _ float64) { mu.Lock(); results = append(results, result); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	z := a.Authorizer(nil)
	_, _ = z.Decide(t.Context(), alice, authorizer.ActionKeyCreate, authorizer.KeyCreate(fixtureKey()), info)
	_, _ = z.Decide(t.Context(), alice, authorizer.ActionModelCreate, authorizer.ModelCreate(fixtureModel()), info)
	s.Fail(http.StatusInternalServerError)
	_, _ = z.Decide(t.Context(), alice, authorizer.ActionBudgetCreate, authorizer.BudgetCreate(fixtureBudget(t)), info)
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(results, ",") != "allow,deny,error" {
		t.Errorf("results %v", results)
	}
	var _ authz.Authorizer = a.Client
}

// TestAnUnknownActionCostsNoRoundTrip: the client Startup builds carries
// the vocabulary, so an action outside spec 006's table is refused in
// the gateway and never reaches the authorizer. A string the gateway
// never declared is a mistake in luxd, not a question an endpoint is
// asked to answer.
func TestAnUnknownActionCostsNoRoundTrip(t *testing.T) {
	const rotate = "key.rotate"
	iss := newIssuer(t)
	s := stub.New(t)
	a, err := Startup(t.Context(), load(t, map[string]string{
		"LUX_OIDC_ISSUERS": iss.URL(), "LUX_AUTHORIZER_URL": s.URL(), "LUX_AUTHORIZER_TOKEN": s.Token(),
	}), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	caller := Caller{Subject: fixtureSubject, Issuer: fixtureIssuer, Sub: "alice"}
	res := authorizer.KeyObject(fixtureKey())

	var unknown *authz.UnknownAction
	if _, err := a.Client.Authorize(t.Context(), Request(caller, rotate, res, info)); !errors.As(err, &unknown) {
		t.Fatalf("Authorize(%s) = %v, want an *authz.UnknownAction", rotate, err)
	}
	if unknown.Core != "lux" || unknown.Action != rotate {
		t.Errorf("the refusal names %s's %q", unknown.Core, unknown.Action)
	}
	if authz.Retryable(unknown) {
		t.Error("an unknown action reads as retryable; it is no outage at the endpoint")
	}
	if _, err := a.Authorizer(nil).Decide(t.Context(), caller, rotate, res, info); err == nil {
		t.Errorf("the gateway's seam answered %s with an allow", rotate)
	}
	if n := len(s.Requests()); n != 0 {
		t.Fatalf("%d request(s) reached the authorizer; an unknown action costs no round trip", n)
	}

	// Every action of the table still travels.
	if _, err := a.Authorizer(nil).Decide(t.Context(), caller, authorizer.ActionKeyRead, res, info); err != nil {
		t.Fatalf("%s: %v", authorizer.ActionKeyRead, err)
	}
	if n := len(s.Requests()); n != 1 {
		t.Errorf("%d request(s) after one action of the table, want one", n)
	}
}
