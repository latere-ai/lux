// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
)

// TestClientAddressRule is spec 011's address rule: the peer is the
// client unless it sits in a trusted range, in which case the last
// forwarded entry outside every range is; a forwarded header from an
// untrusted peer is ignored, and so is every header when nothing is
// trusted; an entry that is not an address leaves the peer standing.
func TestClientAddressRule(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	for _, tc := range []struct {
		name, peer string
		xff        []string
		trusted    []netip.Prefix
		want       string
	}{
		{"no proxy trusted", "10.1.2.3:4444", []string{"203.0.113.9"}, nil, "10.1.2.3"},
		{"an untrusted peer forwarding", "198.51.100.7:1234", []string{"203.0.113.9"}, trusted, "198.51.100.7"},
		{"a trusted peer forwarding one", "10.1.2.3:4444", []string{"203.0.113.9"}, trusted, "203.0.113.9"},
		{"a trusted peer forwarding a chain", "10.1.2.3:4444", []string{"203.0.113.9, 10.0.0.5", "10.0.0.6"}, trusted, "203.0.113.9"},
		{"the last entry outside the ranges", "10.1.2.3:4444", []string{"203.0.113.9, 198.51.100.7, 10.0.0.5"}, trusted, "198.51.100.7"},
		{"every entry inside the ranges", "10.1.2.3:4444", []string{"10.0.0.5, 10.0.0.6"}, trusted, "10.1.2.3"},
		{"no header from a trusted peer", "10.1.2.3:4444", nil, trusted, "10.1.2.3"},
		{"an entry that is not an address", "10.1.2.3:4444", []string{"203.0.113.9, unknown"}, trusted, "10.1.2.3"},
		{"an IPv6 peer in a range", "[fd00::1]:4444", []string{"[2001:db8::9]:8080"}, trusted, "2001:db8::9"},
		{"a mapped IPv4 peer", "[::ffff:10.1.2.3]:4444", []string{"203.0.113.9"}, trusted, "203.0.113.9"},
		{"a peer with no port", "10.1.2.3", []string{"203.0.113.9"}, trusted, "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/keys", nil)
			r.RemoteAddr = tc.peer
			for _, h := range tc.xff {
				r.Header.Add("X-Forwarded-For", h)
			}
			if got := ClientAddress(r, tc.trusted); got != tc.want {
				t.Fatalf("ClientAddress() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLimitUnauthenticatedCountsRefusedCredentials: the per-address
// bucket is charged before the handler and refunded unless the handler
// answered 401, so a caller who authenticates is never limited by it
// while a flood of bad credentials from one address is refused
// rate_limited in the plane's shape with Retry-After and the RateLimit
// headers; another address has its own bucket; a bucket of zero is no
// middleware at all.
func TestLimitUnauthenticatedCountsRefusedCredentials(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	var status int
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("body"))
	})
	h := LimitUnauthenticated(inner, AddressLimiterOptions{PerMinute: 3, Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, Now: func() time.Time { return now }, NewID: func() string { return "req_test" }})
	send := func(peer, xff, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.RemoteAddr = peer
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	status = http.StatusOK
	for range 10 {
		if rec := send("203.0.113.9:1", "", "/v1/keys"); rec.Code != 200 {
			t.Fatalf("an authenticated request was refused: %d %s", rec.Code, rec.Body.String())
		}
	}
	status = http.StatusUnauthorized
	for i := range 3 {
		if rec := send("203.0.113.9:1", "", "/v1/keys"); rec.Code != 401 {
			t.Fatalf("bad credential %d: %d", i+1, rec.Code)
		}
	}
	rec := send("203.0.113.9:1", "", "/v1/keys")
	if rec.Code != 429 {
		t.Fatalf("the fourth bad credential: %d %s", rec.Code, rec.Body.String())
	}
	hdr := rec.Header()
	if hdr.Get("Retry-After") == "" || hdr.Get("RateLimit-Limit") != "3" || hdr.Get("RateLimit-Remaining") != "0" || hdr.Get("RateLimit-Reset") == "" || hdr.Get(gateway.HeaderRequestID) != "req_test" {
		t.Errorf("headers %v", hdr)
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "rate_limited" || env.Error.Message != gateway.CodeRateLimited.Message() || env.Error.Details["request_id"] != "req_test" || !strings.Contains(env.Error.Details["detail"].(string), "203.0.113.9") {
		t.Errorf("envelope %+v", env)
	}
	// Another address, and the same address behind a trusted proxy, are
	// other buckets; the door answers in its own shape.
	if rec := send("203.0.113.10:1", "", "/v1/keys"); rec.Code != 401 {
		t.Errorf("another address shares the bucket: %d", rec.Code)
	}
	if rec := send("10.0.0.1:1", "198.51.100.7", "/openai/v1/chat/completions"); rec.Code != 401 {
		t.Errorf("a forwarded address shares the bucket: %d", rec.Code)
	}
	for range 3 {
		send("10.0.0.1:1", "198.51.100.7", "/openai/v1/chat/completions")
	}
	if rec := send("10.0.0.1:1", "198.51.100.7", "/openai/v1/chat/completions"); rec.Code != 429 || !strings.Contains(rec.Body.String(), `"type":"rate_limited"`) {
		t.Errorf("the door's refusal: %d %s", rec.Code, rec.Body.String())
	}
	// The window refills with the clock.
	now = now.Add(time.Minute)
	if rec := send("203.0.113.9:1", "", "/v1/keys"); rec.Code != 401 {
		t.Errorf("after a minute: %d", rec.Code)
	}
	// A bucket of zero is the handler itself: every request is admitted
	// however many were refused before it.
	none := LimitUnauthenticated(inner, AddressLimiterOptions{})
	for range 5 {
		r := httptest.NewRequest(http.MethodGet, "/v1/keys", nil)
		r.RemoteAddr = "203.0.113.12:1"
		rec := httptest.NewRecorder()
		none.ServeHTTP(rec, r)
		if rec.Code != 401 {
			t.Fatalf("a bucket of zero refused: %d", rec.Code)
		}
	}
	// A handler that writes without WriteHeader commits 200, which is not
	// a refused credential.
	plain := LimitUnauthenticated(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }), AddressLimiterOptions{PerMinute: 1})
	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/v1/self", nil)
		r.RemoteAddr = "203.0.113.11:1"
		rec := httptest.NewRecorder()
		plain.ServeHTTP(rec, r)
		if rec.Code != 200 {
			t.Fatalf("a served request was charged: %d", rec.Code)
		}
	}
}

func TestStatusWriterUnwraps(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusWriter{ResponseWriter: rec}
	if w.Unwrap() != rec {
		t.Fatal("Unwrap does not reach the connection")
	}
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through the wrapper: %v", err)
	}
	if secondsUp(0) != 1 || secondsUp(1500*time.Millisecond) != 2 || secondsUp(time.Minute) != 60 {
		t.Error("secondsUp rounds wrongly")
	}
}
