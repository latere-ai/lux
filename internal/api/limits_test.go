// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
)

// TestRateLimits: the 601st authenticated request of one subject in a
// minute is rate_limited with the four headers, the window refills with
// the clock, another subject has its own bucket, and the authorizer's
// requests_per_minute overrides the configured rate for its subject
// once a decision carried it; a rate of 0 is no limit and no headers.
func TestRateLimits(t *testing.T) {
	h := newHarness(t, nil)
	for i := range 600 {
		rec := h.request(http.MethodGet, "/v1/budgets/none", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
		if rec.Header().Get("RateLimit-Limit") != "600" || rec.Header().Get("RateLimit-Remaining") != strconv.Itoa(599-i) {
			t.Fatalf("request %d headers %v", i+1, rec.Header())
		}
	}
	rec := h.request(http.MethodGet, "/v1/budgets/none", "")
	wantCode(t, rec, CodeRateLimited)
	hd := rec.Header()
	if hd.Get("RateLimit-Limit") != "600" || hd.Get("RateLimit-Remaining") != "0" || hd.Get("RateLimit-Reset") == "" || hd.Get("Retry-After") == "" {
		t.Errorf("refusal headers %v", hd)
	}
	if rec := h.request(http.MethodGet, "/v1/budgets/none", "", as(h.bob)...); rec.Code != http.StatusNotFound {
		t.Errorf("another subject shares the bucket: %d", rec.Code)
	}
	h.advance(time.Minute)
	if rec := h.request(http.MethodGet, "/v1/budgets/none", ""); rec.Code != http.StatusNotFound || rec.Header().Get("RateLimit-Remaining") != "599" {
		t.Errorf("after a minute: %d %v", rec.Code, rec.Header())
	}
	// The override: bob's next decision grants 2 a minute; the request
	// that carried it is under the old rate, the ones after under his.
	h.stub.Allow(stub.Rule{Subject: h.iss.URL() + "|bob", Limits: map[string]any{"requests_per_minute": 2}})
	h.request(http.MethodPut, "/v1/budgets/bobs", budgetJSON, as(h.bob)...)
	for i := range 2 {
		if rec := h.request(http.MethodGet, "/v1/budgets/bobs", "", as(h.bob)...); rec.Code != http.StatusOK || rec.Header().Get("RateLimit-Limit") != "2" {
			t.Fatalf("bob's request %d: %d %v", i+1, rec.Code, rec.Header())
		}
	}
	rec = h.request(http.MethodGet, "/v1/budgets/bobs", "", as(h.bob)...)
	if d := wantCode(t, rec, CodeRateLimited); rec.Header().Get("RateLimit-Limit") != "2" || d["detail"] == nil {
		t.Errorf("bob's override: %v %v", rec.Header(), d)
	}
	if rec := h.request(http.MethodGet, "/v1/budgets/bobs", ""); rec.Code != http.StatusOK || rec.Header().Get("RateLimit-Limit") != "600" {
		t.Errorf("alice under the configured rate: %d %v", rec.Code, rec.Header())
	}
	// An unauthenticated request carries no subject headers.
	rec = h.request(http.MethodGet, "/v1/budgets/bobs", "", "Authorization", "")
	if rec.Header().Get("RateLimit-Limit") != "" {
		t.Error("an unauthenticated response carries RateLimit headers")
	}
	none := newHarness(t, func(o *Options) { o.RequestsPerMinute = 0 })
	for range 5 {
		if rec := none.request(http.MethodGet, "/v1/budgets/none", ""); rec.Code != http.StatusNotFound || rec.Header().Get("RateLimit-Limit") != "" {
			t.Errorf("no limit: %d %v", rec.Code, rec.Header())
		}
	}
}

// decisionWithTTL is an allow granting one request a minute for ttl.
func decisionWithTTL(ttl time.Duration) auth.Decision {
	return auth.Decision{Limits: authorizer.Limits{RequestsPerMinute: 1}, TTL: ttl}
}

// TestGrantsMemo: the memo remembers a subject's last allow for its ttl,
// forgets it after, and holds at most grantEntries subjects.
func TestGrantsMemo(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	g := newGrants(func() time.Time { return now })
	g.put("s1", decisionWithTTL(0))
	if _, ok := g.get("s1"); !ok {
		t.Fatal("a grant was not remembered")
	}
	now = now.Add(61 * time.Second)
	if _, ok := g.get("s1"); ok {
		t.Fatal("a grant outlived the default ttl")
	}
	for i := range grantEntries {
		g.put("s"+strconv.Itoa(i), decisionWithTTL(time.Hour))
	}
	g.put("one-more", decisionWithTTL(time.Hour))
	if len(g.m) > grantEntries {
		t.Errorf("the memo holds %d subjects, the bound is %d", len(g.m), grantEntries)
	}
	if _, ok := g.get("one-more"); !ok {
		t.Error("the newest grant was the one evicted")
	}
}

// TestTrustedProxies: behind a peer in LUX_TRUSTED_PROXIES the
// authorizer's request.ip is the last forwarded entry outside the
// ranges; a forwarded header from a peer outside them is ignored and the
// peer is the client; unset, every header is ignored.
func TestTrustedProxies(t *testing.T) {
	trusted := newHarness(t, func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	for _, tc := range []struct{ peer, xff, want string }{
		{"10.1.2.3:4444", "203.0.113.9, 10.0.0.5", "203.0.113.9"},
		{"10.1.2.3:4444", "198.51.100.7, 203.0.113.9, 10.0.0.5", "203.0.113.9"},
		{"198.51.100.7:4444", "203.0.113.9", "198.51.100.7"},
		{"10.1.2.3:4444", "", "10.1.2.3"},
	} {
		trusted.stub.ClearRequests()
		r := httptest.NewRequest(http.MethodGet, "/v1/budgets", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("Authorization", "Bearer "+trusted.alice)
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		w := httptest.NewRecorder()
		trusted.h.ServeHTTP(w, r)
		reqs := trusted.stub.Requests()
		if w.Code != http.StatusOK || len(reqs) != 1 || reqs[0].Request.IP != tc.want {
			t.Errorf("peer %s xff %q: %d, request.ip %v, want %s", tc.peer, tc.xff, w.Code, reqs, tc.want)
		}
	}
	none := newHarness(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/v1/budgets", nil)
	r.RemoteAddr = "10.1.2.3:4444"
	r.Header.Set("Authorization", "Bearer "+none.alice)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	w := httptest.NewRecorder()
	none.h.ServeHTTP(w, r)
	if reqs := none.stub.Requests(); len(reqs) != 1 || reqs[0].Request.IP != "10.1.2.3" {
		t.Errorf("no proxy trusted: request.ip %v", reqs)
	}
}
