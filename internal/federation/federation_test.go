// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// counted is an identity source that hands out jwt-1, jwt-2, ... so each
// exchange's assertion is told apart.
type counted struct {
	mu sync.Mutex
	n  int
}

func (c *counted) IdentityToken(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return fmt.Sprintf("jwt-%d", c.n), nil
}

// endpoint is Anthropic's token endpoint as the test sets it: each grant
// it is posted is recorded, and it answers sk-ant-oat01-<n> for an hour,
// or the status and body set, after hold is closed when set.
type endpoint struct {
	mu     sync.Mutex
	grants []map[string]string
	status int
	body   string
	hold   chan struct{}
}

func (e *endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var grant map[string]string
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &grant)
	e.mu.Lock()
	e.grants = append(e.grants, grant)
	n, status, body, hold := len(e.grants), e.status, e.body, e.hold
	e.mu.Unlock()
	if r.URL.Path != "/v1/oauth/token" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "wrong route", http.StatusTeapot)
		return
	}
	if hold != nil {
		<-hold
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "$assertion", grant["assertion"]))
		return
	}
	_, _ = fmt.Fprintf(w, `{"access_token":"sk-ant-oat01-%d","token_type":"Bearer","expires_in":3600,"scope":"workspace:developer"}`, n)
}

func (e *endpoint) set(status int, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.body = status, body
}

func (e *endpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.grants)
}

type clients struct{ c *http.Client }

func (c clients) Client(context.Context, *v1.Provider) (*http.Client, error) { return c.c, nil }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func federated(baseURL string) *v1.Provider {
	return &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "anthropic"},
		Spec: v1.ProviderSpec{Dialect: v1.DialectAnthropic, BaseURL: baseURL, Credential: &v1.Credential{
			Header: "Authorization", Scheme: v1.SchemeBearer,
			Federation: &v1.Federation{RuleID: "fdrl_01", OrganizationID: "4f6c1d2a-8e3b-4c7d-9a1e-2b5f6c7d8e9f", ServiceAccountID: "svac_01"},
		}},
		Status: v1.ProviderStatus{ID: "prv_01ANTHROPIC"},
	}
}

func token(t *testing.T, x *Exchanger, p *v1.Provider) string {
	t.Helper()
	b, err := x.Token(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestExchange is spec 051's third criterion: the grant carries the
// Provider's identifiers and a fresh identity token; a token is kept
// until 120 seconds before it expires, refreshed in the window with one
// exchange while the others keep it, kept when the refresh fails, and
// past 30 seconds before a request fails with the exchange, which is not
// asked again for five seconds; a changed Provider and a refused token
// exchange again; an error names the upstream's message and never the
// assertion.
func TestExchange(t *testing.T) {
	ep := &endpoint{}
	srv := httptest.NewServer(ep)
	t.Cleanup(srv.Close)
	ids, c := &counted{}, &clock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	x := New(Options{Identity: ids, Clients: clients{srv.Client()}, Now: c.Now, Logger: slog.New(slog.DiscardHandler)})
	p := federated(srv.URL + "/v1")

	if got := token(t, x, p); got != "sk-ant-oat01-1" {
		t.Fatalf("first token %q", got)
	}
	want := map[string]string{"grant_type": grantType, "assertion": "jwt-1", "federation_rule_id": "fdrl_01",
		"organization_id": "4f6c1d2a-8e3b-4c7d-9a1e-2b5f6c7d8e9f", "service_account_id": "svac_01"}
	if fmt.Sprint(ep.grants[0]) != fmt.Sprint(want) {
		t.Fatalf("the grant %v, want %v without a workspace", ep.grants[0], want)
	}

	// Fresh until 120 seconds before its hour is up.
	c.advance(time.Hour - RefreshBefore - time.Second)
	if got := token(t, x, p); got != "sk-ant-oat01-1" || ep.count() != 1 {
		t.Fatalf("a fresh token: %q after %d exchanges", got, ep.count())
	}

	// In the window one request exchanges while another keeps the token.
	c.advance(2 * time.Second)
	ep.mu.Lock()
	ep.hold = make(chan struct{})
	ep.mu.Unlock()
	done := make(chan string)
	go func() {
		b, err := x.Token(context.Background(), p)
		if err != nil {
			done <- err.Error()
			return
		}
		done <- string(b)
	}()
	waitFor(t, func() bool { return ep.count() == 2 })
	if got := token(t, x, p); got != "sk-ant-oat01-1" {
		t.Fatalf("a request while another refreshes: %q", got)
	}
	close(ep.hold)
	if got := <-done; got != "sk-ant-oat01-2" || ep.grants[1]["assertion"] != "jwt-2" {
		t.Fatalf("the refresh: %q with %v", got, ep.grants[1])
	}
	ep.mu.Lock()
	ep.hold = nil
	ep.mu.Unlock()

	// A refresh that fails keeps the token it held.
	c.advance(time.Hour - RefreshBefore + time.Second)
	ep.set(http.StatusServiceUnavailable, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	if got := token(t, x, p); got != "sk-ant-oat01-2" {
		t.Fatalf("a failed refresh: %q", got)
	}

	// Past 30 seconds before, a failure is the request's, and is answered
	// again without an exchange for five seconds.
	c.advance(RefreshBefore - MandatoryBefore)
	ep.set(http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"Authentication failed for $assertion"}}`)
	before := ep.count()
	_, err := x.Token(t.Context(), p)
	if err == nil || !strings.Contains(err.Error(), "the token exchange answered 401: authentication_error: Authentication failed for [redacted]") || strings.Contains(err.Error(), "jwt-") {
		t.Fatalf("a failed exchange: %v", err)
	}
	if _, again := x.Token(t.Context(), p); again == nil || ep.count() != before+1 {
		t.Fatalf("a request right after a failure exchanged again: %d exchanges, %v", ep.count()-before, again)
	}
	c.advance(RetryAfter)
	ep.set(0, "")
	if got := token(t, x, p); got != fmt.Sprintf("sk-ant-oat01-%d", before+2) {
		t.Fatalf("after the retry window: %q", got)
	}

	// A Provider that names another rule exchanges again at once.
	changed := federated(p.Spec.BaseURL)
	changed.Spec.Credential.Federation.RuleID = "fdrl_02"
	changed.Spec.Credential.Federation.WorkspaceID = "wrkspc_01"
	n := ep.count()
	if got := token(t, x, changed); got != fmt.Sprintf("sk-ant-oat01-%d", n+1) || ep.grants[n]["federation_rule_id"] != "fdrl_02" || ep.grants[n]["workspace_id"] != "wrkspc_01" {
		t.Fatalf("a changed Provider: %q with %v", got, ep.grants[n])
	}

	// A refused token is dropped; another value is not.
	x.Refused(changed, []byte("sk-ant-oat01-other"))
	if token(t, x, changed); ep.count() != n+1 {
		t.Fatal("a value other than the held token was dropped")
	}
	x.Refused(changed, []byte(fmt.Sprintf("sk-ant-oat01-%d", n+1)))
	if got := token(t, x, changed); got != fmt.Sprintf("sk-ant-oat01-%d", n+2) {
		t.Fatalf("after a refusal: %q", got)
	}

	// An answer that is no token response is a failure.
	c.advance(2 * time.Hour)
	ep.set(http.StatusOK, `{"token_type":"Bearer"}`)
	if _, err := x.Token(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "without an access_token") {
		t.Fatalf("a 200 without a token: %v", err)
	}
	if _, err := x.Token(t.Context(), &v1.Provider{Spec: v1.ProviderSpec{Dialect: v1.DialectAnthropic}}); err == nil {
		t.Fatal("a Provider with no federation was exchanged for")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTokenURL: the token endpoint is the base URL, which names the API
// version, with oauth/token after it.
func TestTokenURL(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.anthropic.com/v1":  "https://api.anthropic.com/v1/oauth/token",
		"https://api.anthropic.com/v1/": "https://api.anthropic.com/v1/oauth/token",
	} {
		if got := TokenURL(federated(base)); got != want {
			t.Errorf("%s: %s, want %s", base, got, want)
		}
	}
}

// failing is an identity source that fails.
type failing struct{}

func (failing) IdentityToken(context.Context) (string, error) {
	return "", errors.New("no token on disk")
}

type noClient struct{}

func (noClient) Client(context.Context, *v1.Provider) (*http.Client, error) {
	return nil, errors.New("the base URL is refused")
}

// TestExchangeFailures: every way an exchange fails is the request's
// error, named, with the defaults New fills in: an identity source that
// fails, a Provider with no client, an endpoint that does not answer,
// one that answers no JSON, and the upstream's message read from its
// error shape or quoted from its body.
func TestExchangeFailures(t *testing.T) {
	p := federated("http://127.0.0.1:1/v1")
	for name, tc := range map[string]struct {
		o    Options
		want string
	}{
		"identity":  {Options{Identity: failing{}, Clients: clients{http.DefaultClient}}, "the workload identity token: no token on disk"},
		"client":    {Options{Identity: staticToken("jwt"), Clients: noClient{}}, "client: the base URL is refused"},
		"transport": {Options{Identity: staticToken("jwt"), Clients: clients{http.DefaultClient}}, "POST http://127.0.0.1:1/v1/oauth/token"},
	} {
		if _, err := New(tc.o).Token(t.Context(), p); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	answer := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if body, ok := strings.CutPrefix(answer, "200 "); ok {
			_, _ = io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	for body, want := range map[string]string{
		"200 not json": "a body that is not a token response",
		`{"error":{"message":"federation_rule_id is malformed"}}`: "answered 400: federation_rule_id is malformed",
		"bad request":            `answered 400: "bad request"`,
		"":                       "answered 400: an empty body",
		strings.Repeat("x", 300): "…",
	} {
		answer = body
		x := New(Options{Identity: staticToken("jwt"), Clients: clients{srv.Client()}})
		if _, err := x.Token(t.Context(), federated(srv.URL+"/v1")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
	// Refused of nothing drops nothing.
	New(Options{Identity: staticToken("jwt"), Clients: clients{srv.Client()}}).Refused(p, nil)
}

type staticToken string

func (s staticToken) IdentityToken(context.Context) (string, error) { return string(s), nil }
