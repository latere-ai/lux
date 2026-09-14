// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The client address rule and the per-address bucket of spec 011, which
// run here before the door handler of spec 004 and before the /v1
// handler, so both planes agree on who the caller is and how many bad
// credentials one address may present.

// ClientAddress is the caller's address for the rate bucket and for the
// authorizer's request.ip: the peer's, or, when the peer is inside one
// of the trusted ranges, the last X-Forwarded-For entry outside every
// range, which is the address the nearest untrusted hop presented. A
// forwarded header from a peer outside the ranges is ignored, and so is
// every header when no range is trusted, so a caller cannot choose the
// address it is limited under. An entry that is not an address ends the
// walk and the peer stands.
func ClientAddress(r *http.Request, trusted []netip.Prefix) string {
	peer := hostOf(r.RemoteAddr)
	addr, err := netip.ParseAddr(peer)
	if err != nil || !inRanges(addr, trusted) {
		return peer
	}
	var entries []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for e := range strings.SplitSeq(h, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
	}
	for _, entrie := range slices.Backward(entries) {
		a, err := netip.ParseAddr(hostOf(entrie))
		if err != nil {
			return peer
		}
		if !inRanges(a, trusted) {
			return a.String()
		}
	}
	return peer
}

// hostOf strips a port from host:port and the brackets from an IPv6
// literal; a bare host is returned as it is.
func hostOf(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return strings.Trim(s, "[]")
}

func inRanges(a netip.Addr, ranges []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range ranges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// AddressLimiterOptions is what LimitUnauthenticated runs under.
type AddressLimiterOptions struct {
	// PerMinute is LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE, the bucket
	// each client address draws from; 0 is no bucket.
	PerMinute int
	// Trusted is LUX_TRUSTED_PROXIES.
	Trusted []netip.Prefix
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints the request id of a refusal, req_ and a ULID; nil uses
	// v1.NewID.
	NewID func() string
}

// addressIdle is how long an idle address bucket is kept, ten minutes as
// the Key buckets are.
const addressIdle = 10 * time.Minute

// LimitUnauthenticated bounds the requests one client address may send
// before authentication, on both planes: a token is taken from the
// address's bucket before the handler runs and given back after it
// unless the handler answered 401, so the bucket counts refused
// credentials, a flood of bad tokens or guessed Key values, and not the
// requests of a caller behind one address who authenticates. A refusal
// is rate_limited in the shape of the plane the path names, with
// Retry-After and the RateLimit headers, before the store, the verifier,
// or the handler is touched.
func LimitUnauthenticated(next http.Handler, o AddressLimiterOptions) http.Handler {
	if o.PerMinute <= 0 {
		return next
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = func() string { return v1.NewID(v1.PrefixRequest, o.Now(), nil) }
	}
	buckets := ratelimit.New(ratelimit.Config{Idle: addressIdle, Now: o.Now})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := ClientAddress(r, o.Trusted)
		buckets.SetRate(addr, o.PerMinute)
		a := buckets.Allow(addr)
		if !a.OK {
			id := o.NewID()
			h := w.Header()
			h.Set(gateway.HeaderRequestID, id)
			h.Set("RateLimit-Limit", strconv.Itoa(a.PerMinute))
			h.Set("RateLimit-Remaining", "0")
			h.Set("RateLimit-Reset", strconv.FormatInt(secondsUp(a.Retry), 10))
			gateway.WriteRefusal(w, r.URL.Path, id, gateway.CodeRateLimited,
				"client address "+addr+": "+strconv.Itoa(a.PerMinute)+" unauthenticated requests a minute are spent; the next is admitted in "+a.Retry.String(), a.Retry)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		if sw.status != http.StatusUnauthorized {
			buckets.Adjust(addr, 1)
		}
	})
}

// secondsUp is d in whole seconds, rounded up, at least one.
func secondsUp(d time.Duration) int64 {
	return max(int64((d+time.Second-1)/time.Second), 1)
}

// statusWriter notes the status the handler committed. Unwrap lets the
// door's ResponseController reach the connection for its flushes.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
