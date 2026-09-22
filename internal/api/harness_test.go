// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

const (
	audience  = "lux"
	kek       = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	canary    = "sk-canary-7f3c9a-never-in-any-encoding"
	publicURL = "https://lux.example.com"
)

// harness is one surface over a memory store, a stub issuer, and a stub
// authorizer, with a clock the tests move and the ids the tests read.
type harness struct {
	t     *testing.T
	st    *memory.Store
	iss   *issuertest.Server
	stub  *stub.Server
	auth  *auth.Auth
	h     *Handler
	reg   *metrics.Registry
	log   *bytes.Buffer
	keys  *secrets.Keyring
	alice string // alice's bearer
	bob   string // bob's bearer

	mu  sync.Mutex
	now time.Time
	ids int
}

// newHarness builds the surface; edit changes the options before New.
func newHarness(t *testing.T, edit func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, log: &bytes.Buffer{}, now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	h.st = memory.New(memory.WithClock(h.clock))
	h.iss = issuertest.New(t, issuertest.WithDefaultAudience(audience))
	h.stub = stub.New(t)
	keys, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	h.keys = keys
	a, err := auth.New(t.Context(), auth.Options{
		Issuers: []string{h.iss.URL()}, Audiences: []string{audience},
		AuthorizerURL: h.stub.URL(), AuthorizerToken: h.stub.Token(), AuthorizerTimeout: 2 * time.Second,
		HTTP: &http.Client{}, Now: h.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.auth = a
	h.reg = metrics.NewRegistry()
	base, _ := url.Parse(publicURL)
	o := Options{
		Store: h.st, Auth: a, Authorizer: a.Authorizer(&serve.ObjectOwners{Objects: h.st.Objects()}),
		PublicURL: base, Version: "0.1.0-test", RequestsPerMinute: 600, MaxManifestBytes: 4096,
		Defaults: manifest.Defaults{RequestsPerMinute: 60, TokensPerMinute: 1000, Timeout: 10 * time.Minute},
		Keys:     keys, Metrics: h.reg, Logger: slog.New(slog.NewJSONHandler(h.log, nil)),
		Now: h.clock, NewID: h.newID,
	}
	if edit != nil {
		edit(&o)
	}
	h.h = New(o)
	h.alice = h.iss.Mint(issuertest.Claims{Sub: "alice", Extra: map[string]any{"email": "alice@example.com", "groups": []string{"research"}}})
	h.bob = h.iss.Mint(issuertest.Claims{Sub: "bob"})
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// newID mints readable, ordered ids, so a test can tell them apart.
func (h *harness) newID(prefix string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids++
	n := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(
		"0000000000000000000000000000"[:26-len(itoa(h.ids))]+itoa(h.ids), "I", "1"), "L", "1"), "O", "0"))
	return prefix + n
}

func itoa(n int) string { return strings.TrimLeft(strings.Repeat("0", 26)+intToString(n), "0") }

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// subject is alice's rendered subject.
func (h *harness) subject() string { return h.iss.URL() + "|alice" }

// request is one request through the handler, with alice's bearer
// unless headers set Authorization, and a JSON content type when a body
// is sent and none is set.
func (h *harness) request(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "203.0.113.9:4444"
	r.Header.Set("Authorization", "Bearer "+h.alice)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			r.Header.Del(headers[i])
			continue
		}
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec
}

// as is the Authorization header pair for a bearer.
func as(token string) []string { return []string{"Authorization", "Bearer " + token} }

// body decodes a JSON response body into a map.
func body(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not a JSON object: %v\n%s", err, rec.Body.String())
	}
	return m
}

// envelope reads an error response: the code and its details.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) (code string, details map[string]any) {
	t.Helper()
	m := body(t, rec)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %s", rec.Body.String())
	}
	code, _ = e["code"].(string)
	details, _ = e["details"].(map[string]any)
	if msg, _ := e["message"].(string); msg != Code(code).Message() {
		t.Errorf("message %q is not the table's sentence for %s", msg, code)
	}
	if details["request_id"] == nil || details["request_id"] != rec.Header().Get("Lux-Request-Id") {
		t.Errorf("details.request_id %v is not the response's Lux-Request-Id %q", details["request_id"], rec.Header().Get("Lux-Request-Id"))
	}
	return code, details
}

// wantCode asserts the response is the code at the status the table
// gives it and returns the details.
func wantCode(t *testing.T, rec *httptest.ResponseRecorder, code Code) map[string]any {
	t.Helper()
	got, details := envelope(t, rec)
	if got != string(code) || rec.Code != code.Status() {
		t.Fatalf("%d %s, want %d %s\n%s", rec.Code, got, code.Status(), code, rec.Body.String())
	}
	return details
}

// paths reads details.paths.
func paths(details map[string]any) []string {
	raw, _ := details["paths"].([]any)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		out = append(out, p.(string))
	}
	return out
}

// status reads the status block of an object response.
func status(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	s, _ := body(t, rec)["status"].(map[string]any)
	if s == nil {
		t.Fatalf("no status in %s", rec.Body.String())
	}
	return s
}

// The fixtures: one manifest per kind, as a caller would PUT them.

const providerJSON = `{"spec": {"dialect": "openai", "baseURL": "https://api.example.com/v1", "credential": {"value": "` + canary + `"}, "discovery": {"mode": "none"}, "health": {"mode": "none"}}}`

const modelJSON = `{"spec": {"targets": [{"provider": "openai", "model": "gpt-5"}], "pricing": {"currency": "USD", "input": "1", "output": "2"}}}`

const budgetJSON = `{"spec": {"amount": "50", "currency": "USD", "window": "month"}}`

const keyJSON = `{"spec": {"models": ["gpt-5"], "budget": "team"}}`

// seed applies the Provider, the Model, and the Budget, so a Key can
// name them.
func (h *harness) seed() {
	h.t.Helper()
	for _, s := range []struct{ path, body string }{
		{"/v1/providers/openai", providerJSON}, {"/v1/models/gpt-5", modelJSON}, {"/v1/budgets/team", budgetJSON},
	} {
		if rec := h.request(http.MethodPut, s.path, s.body); rec.Code != http.StatusCreated {
			h.t.Fatalf("seed PUT %s: %d %s", s.path, rec.Code, rec.Body.String())
		}
	}
}

// bg is a context for the store outside a request.
func bg() context.Context { return context.Background() }

// objectOf reads one object from the store by kind and name.
func (h *harness) objectOf(kind, name string) v1.Object {
	h.t.Helper()
	obj, _, err := h.st.Objects().ByName(bg(), kind, name)
	if err != nil {
		h.t.Fatalf("%s %q: %v", kind, name, err)
	}
	return obj
}
