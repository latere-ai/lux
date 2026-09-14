// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/lux/authorizer"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/serve"
	v1 "latere.ai/x/lux/manifest/v1"
)

// authenticate is the bearer through internal/auth and the per-subject
// rate bucket after it: LUX_REQUESTS_PER_MINUTE, or the
// requests_per_minute the authorizer last granted this subject, as the
// burst and the rate; every response past this point carries the three
// RateLimit headers and a refusal adds Retry-After. In the file mode
// there is no bearer to read and no subject to limit.
func (c *call) authenticate() *Error {
	c.addr = serve.ClientAddress(c.r, c.h.o.TrustedProxies)
	if c.h.fileMode() {
		return nil
	}
	caller, err := c.h.o.Auth.Verifier.Authenticate(c.r)
	if err != nil {
		return mapError(err)
	}
	c.caller = caller
	limit := c.h.o.RequestsPerMinute
	if g, ok := c.h.grants.get(caller.Subject); ok && g.limits.RequestsPerMinute > 0 {
		limit = g.limits.RequestsPerMinute
	}
	if limit <= 0 {
		return nil
	}
	c.h.subjects.SetRate(caller.Subject, limit)
	a := c.h.subjects.Allow(caller.Subject)
	h := c.w.Header()
	h.Set("RateLimit-Limit", strconv.Itoa(limit))
	h.Set("RateLimit-Remaining", strconv.Itoa(a.Remaining))
	if !a.OK {
		h.Set("RateLimit-Reset", strconv.FormatInt(secondsUp(a.Retry), 10))
		return &Error{Code: CodeRateLimited, RetryAfter: a.Retry,
			Detail: "subject " + caller.Subject + ": " + strconv.Itoa(limit) + " requests a minute are spent; the next is admitted in " + a.Retry.String()}
	}
	// The reset is when the bucket is full again at the rate.
	reset := time.Duration(float64(limit-a.Remaining) / float64(limit) * float64(time.Minute))
	h.Set("RateLimit-Reset", strconv.FormatInt(max(int64((reset+time.Second-1)/time.Second), 0), 10))
	return nil
}

// info is what the authorizer learns about the request itself.
func (c *call) info() authz.Caller {
	return authz.Caller{ID: c.id, IP: c.addr, UserAgent: c.r.UserAgent()}
}

// authorize asks the request's own action and remembers what the allow
// granted the subject. A deny is forbidden with the reason in the
// detail; no decision is authorizer_unavailable.
func (c *call) authorize(ctx context.Context, action string, res authz.Resource) (auth.Decision, *Error) {
	c.action, c.kind = action, res.Kind
	d, err := c.h.o.Authorizer.Decide(ctx, c.caller, action, res, c.info())
	if err != nil {
		return auth.Decision{}, mapError(err)
	}
	c.h.grants.put(c.caller.Subject, d)
	return d, nil
}

// authorizeTunnel asks provider.tunnel for one Provider, whose resource
// is the object shape spec 006's table names, the same one a read is
// asked with. It sits here beside the other asks so one file names the
// vocabulary for this package.
func (c *call) authorizeTunnel(ctx context.Context, p *v1.Provider) *Error {
	res, _ := authorizer.ResourceFor(authorizer.ActionProviderTunnel, p)
	_, err := c.authorize(ctx, authorizer.ActionProviderTunnel, res)
	return err
}

// lookup is the Lookup Resolve asks for this request: the store's
// objects under the authorizer's provider.read, budget.draw, and
// model.use, as the caller. refs captures the Budget it resolved so the
// Key's status.budget names it by id.
func (c *call) lookup(refs *references) *auth.Lookup {
	return c.h.o.Authorizer.Lookup(c.caller, c.info(), refs)
}

// grants is what this replica remembers of the authorizer's allows per
// subject: the limits and the filter of the last allow, for the allow's
// ttl. /v1/self reports it and the rate bucket reads
// requests_per_minute from it before the request's own decision is
// made, since the bucket runs right after authentication. The shared
// client's cache is keyed by action and resource and cannot be read by
// subject, which is why the memo is kept here. It holds at most
// grantEntries subjects and drops expired ones, then any one, past it.
type grants struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]grant
}

// grant is one subject's last allow.
type grant struct {
	limits authorizer.Limits
	filter *authz.Filter
	until  time.Time
}

// grantEntries bounds the memo, the shared client's own bound.
const grantEntries = authz.CacheEntries

func newGrants(now func() time.Time) *grants {
	return &grants{now: now, m: map[string]grant{}}
}

// put remembers the decision for its ttl, DefaultTTL when it names none.
func (g *grants) put(subject string, d auth.Decision) {
	ttl := d.TTL
	if ttl <= 0 {
		ttl = authz.DefaultTTL
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.m[subject]; !ok && len(g.m) >= grantEntries {
		now := g.now()
		for s, e := range g.m {
			if !now.Before(e.until) {
				delete(g.m, s)
			}
		}
		for s := range g.m {
			if len(g.m) < grantEntries {
				break
			}
			delete(g.m, s)
		}
	}
	g.m[subject] = grant{limits: d.Limits, filter: d.Filter, until: g.now().Add(ttl)}
}

// get is the subject's unexpired grant.
func (g *grants) get(subject string) (grant, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.m[subject]
	if !ok || !g.now().Before(e.until) {
		delete(g.m, subject)
		return grant{}, false
	}
	return e, true
}
