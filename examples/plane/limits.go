// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The windows and the record: what a platform supplies the data plane
// beside its store. The arithmetic is metering's, the refusal codes are
// the contract's, and what is kept afterwards is the platform's own
// ledger, here a slice.

// limiter admits a request against the Key's rates and the spend
// windows, in the pipeline's order: the two rate buckets, the price the
// Model carries, the Key's spend window, the Budget's.
type limiter struct {
	store    *store
	counters *metering.Counters
	buckets  *ratelimit.Buckets
	defaults manifest.Defaults
	now      func() time.Time
}

func newLimiter(s *store, defaults manifest.Defaults, flush time.Duration, now func() time.Time) *limiter {
	return &limiter{
		store:    s,
		counters: metering.NewCounters(s, flush, metering.WithClock(now)),
		buckets:  ratelimit.New(ratelimit.Config{Idle: 10 * time.Minute, Now: now}),
		defaults: defaults,
		now:      now,
	}
}

// Run flushes the deltas to the store on the interval, which is what
// lets several replicas agree on a window.
func (l *limiter) Run(ctx context.Context) { _ = l.counters.Run(ctx) }

// rates are the Key's per-minute rates, its own or the installation's
// defaults; zero is no bucket.
func (l *limiter) rates(k *v1.Key) (requests, tokens int) {
	requests, tokens = l.defaults.RequestsPerMinute, l.defaults.TokensPerMinute
	if k.Spec.Limits.RequestsPerMinute != nil {
		requests = *k.Spec.Limits.RequestsPerMinute
	}
	if k.Spec.Limits.TokensPerMinute != nil {
		tokens = *k.Spec.Limits.TokensPerMinute
	}
	return requests, tokens
}

// Reserve implements gateway.Limiter.
func (l *limiter) Reserve(ctx context.Context, r gateway.Reservation) (gateway.Lease, error) {
	k := r.Key
	if k == nil {
		return nil, errors.New("plane: a reservation without a Key")
	}
	id := k.Status.ID
	now := l.now()
	requests, tokens := l.rates(k)
	le := &lease{l: l, id: id}
	if requests > 0 {
		l.buckets.SetRate("requests:"+id, requests)
		if a := l.buckets.Allow("requests:" + id); !a.OK {
			return nil, &gateway.Refusal{Code: gateway.CodeRateLimited, RetryAfter: a.Retry,
				Detail: "Key " + id + ": requestsPerMinute " + strconv.Itoa(requests) + " is spent"}
		}
		le.requests = 1
	}
	if reserved := metering.Reserved(r.InputTokens, r.OutputTokens); tokens > 0 && reserved > 0 {
		n := int(min(reserved, int64(tokens)))
		l.buckets.SetRate("tokens:"+id, tokens)
		if a := l.buckets.AllowN("tokens:"+id, n); !a.OK {
			le.refund()
			return nil, &gateway.Refusal{Code: gateway.CodeRateLimited, RetryAfter: a.Retry,
				Detail: "Key " + id + ": tokensPerMinute " + strconv.Itoa(tokens) + " cannot cover " + strconv.Itoa(n) + " tokens"}
		}
		le.tokens = n
	}
	if err := l.reserveSpend(ctx, r, le, now); err != nil {
		le.refund()
		return nil, err
	}
	return le, nil
}

// reserveSpend is the money half: a price that cannot be counted, the
// Key's window, the Budget's.
func (l *limiter) reserveSpend(ctx context.Context, r gateway.Reservation, le *lease, now time.Time) error {
	k := r.Key
	id := k.Status.ID
	var spend *v1.Spend
	if s := k.Spec.Limits.Spend; s != nil && s.Amount != nil {
		spend = s
	}
	var budgets []*v1.Budget
	for _, ref := range v1.KeyBudgets(k) {
		b, err := l.store.Budget(ctx, ref.ID)
		if err != nil {
			return err
		}
		if b != nil {
			budgets = append(budgets, b)
		}
	}
	var pricing *v1.Pricing
	if !r.Opaque && r.Model != nil {
		pricing = r.Model.Spec.Pricing
	}
	if (spend != nil || len(budgets) > 0) && pricing == nil && !k.Spec.AllowUnpriced {
		what := "an opaque route is unpriced"
		if r.Model != nil {
			what = "Model " + r.Model.Metadata.Name + " has no pricing"
		}
		return &gateway.Refusal{Code: gateway.CodeModelUnpriced, Detail: what + ", and Key " + id + " spends under a limit without allowUnpriced"}
	}
	var estimate v1.Money
	if pricing != nil {
		estimate, _ = metering.Cost(metering.Tokens{Input: r.InputTokens, Output: r.OutputTokens}, pricing)
		if spend != nil && spend.Currency != "" && spend.Currency != pricing.Currency {
			return &gateway.Refusal{Code: gateway.CodeCurrencyMismatch, Detail: "Key " + id + " limits its spend in " + spend.Currency + " and the Model is priced in " + pricing.Currency}
		}
		for _, b := range budgets {
			if b.Spec.Currency != pricing.Currency {
				return &gateway.Refusal{Code: gateway.CodeCurrencyMismatch, Detail: "Budget " + b.Metadata.Name + " is in " + b.Spec.Currency + " and the Model is priced in " + pricing.Currency}
			}
		}
	}
	le.pricing, le.estimate = pricing, estimate
	le.countRows = []row{{key: metering.TotalKey(metering.ScopeKeyRequests, id)}}
	le.tokenRows = []row{{key: metering.TotalKey(metering.ScopeKeyTokens, id)}}
	le.spendRows = []row{{key: metering.TotalKey(metering.ScopeKeySpend, id)}}
	if spend != nil {
		_, resetsAt := metering.Window(spend.Window, now, k.Status.CreatedAt)
		key := metering.CounterKey(metering.ScopeKeySpend, id, spend.Window, now, k.Status.CreatedAt)
		known, pending := l.counters.Known(key), l.counters.Pending(key)
		if metering.Exceeds(metering.Projected(known, pending, int64(estimate)), int64(*spend.Amount)) {
			return &gateway.Refusal{Code: gateway.CodeSpendExceeded, RetryAfter: metering.RetryAfter(now, resetsAt),
				Detail: "Key " + id + " has spent " + v1.Money(known+pending).String() + " of " + spend.Amount.String() + " " + spend.Currency + " in its " + string(spend.Window) + " window"}
		}
		le.countRows = append(le.countRows, row{key: metering.CounterKey(metering.ScopeKeyRequests, id, spend.Window, now, k.Status.CreatedAt), resetsAt: resetsAt})
		le.tokenRows = append(le.tokenRows, row{key: metering.CounterKey(metering.ScopeKeyTokens, id, spend.Window, now, k.Status.CreatedAt), resetsAt: resetsAt})
		le.spendRows = append(le.spendRows, row{key: key, resetsAt: resetsAt})
	}
	// Every Budget the Key lists is a gate: any hard one the request
	// would pass refuses it, and Retry-After is the latest reset among
	// those, none when one never resets.
	var refusing *gateway.Refusal
	var retry time.Time
	never := false
	var rows []row
	for _, b := range budgets {
		hard := b.Spec.Hard == nil || *b.Spec.Hard
		_, resetsAt := metering.BudgetWindow(b, now)
		key := metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, now)
		known, pending := l.counters.Known(key), l.counters.Pending(key)
		if hard && metering.Exceeds(metering.Projected(known, pending, int64(estimate)), int64(*b.Spec.Amount)) {
			detail := "Budget " + b.Metadata.Name + " has spent " + v1.Money(known+pending).String() + " of " + b.Spec.Amount.String() + " " + b.Spec.Currency + " in its " + string(b.Spec.Window) + " window"
			if refusing == nil {
				refusing = &gateway.Refusal{Code: gateway.CodeBudgetExhausted, Detail: detail}
			} else {
				refusing.Detail += "; " + detail
			}
			if resetsAt.IsZero() {
				never = true
			} else if resetsAt.After(retry) {
				retry = resetsAt
			}
			continue
		}
		rows = append(rows, row{key: key, resetsAt: resetsAt})
	}
	if refusing != nil {
		if !never {
			refusing.RetryAfter = metering.RetryAfter(now, retry)
		}
		return refusing
	}
	le.spendRows = append(le.spendRows, rows...)
	le.admit()
	return nil
}

// row is one counter this lease adds to and when its window resets.
type row struct {
	key      string
	resetsAt time.Time
}

// lease is one admitted request: the rate tokens it took and the
// estimate it added to the spend rows, replaced by the measured cost at
// the settle. The rows are the Key's lifetime totals, its spend window
// when it has a spend limit, and the Budget's window when it draws from
// one, so a read of either renders what this replica has counted.
type lease struct {
	l         *limiter
	id        string
	requests  int
	tokens    int
	pricing   *v1.Pricing
	estimate  v1.Money
	spendRows []row
	countRows []row // the request counters
	tokenRows []row // the token counters
	admitted  bool
}

// admit counts one request and adds the estimate to the spend rows.
func (le *lease) admit() {
	le.admitted = true
	for _, r := range le.countRows {
		le.l.counters.Add(r.key, 1, r.resetsAt)
	}
	le.addSpend(int64(le.estimate))
}

// addSpend writes one delta to every spend row.
func (le *lease) addSpend(delta int64) {
	if delta == 0 {
		return
	}
	for _, r := range le.spendRows {
		le.l.counters.Add(r.key, delta, r.resetsAt)
	}
}

// refund gives the buckets and the counters back what a refusal after a
// partial reservation took.
func (le *lease) refund() {
	if le.requests > 0 {
		le.l.buckets.Adjust("requests:"+le.id, le.requests)
		le.requests = 0
	}
	if le.tokens > 0 {
		le.l.buckets.Adjust("tokens:"+le.id, le.tokens)
		le.tokens = 0
	}
	if le.admitted {
		le.addSpend(-int64(le.estimate))
		for _, r := range le.countRows {
			le.l.counters.Add(r.key, -1, r.resetsAt)
		}
		le.admitted = false
	}
}

// Settle implements gateway.Lease: the estimate is replaced by what the
// request actually cost and the measured tokens are counted, so a
// request that measured nothing is refunded whole.
func (le *lease) Settle(_ context.Context, t gateway.Tokens) {
	if !le.admitted {
		return
	}
	measured := v1.Money(0)
	if le.pricing != nil {
		measured, _ = metering.Cost(metering.Tokens{
			Input: t.Input, Output: t.Output, CachedInput: t.CachedInput, CacheWrite: t.CacheWrite,
		}, le.pricing)
	}
	le.addSpend(int64(measured) - int64(le.estimate))
	le.estimate = measured
	if n := t.Input + t.Output; n > 0 {
		for _, r := range le.tokenRows {
			le.l.counters.Add(r.key, n, r.resetsAt)
		}
	}
}

// recorder is the platform's ledger: one metering.Record per request,
// priced by the Model's own pricing, kept for the usage surface. A
// platform writes them to its warehouse here.
type recorder struct {
	store   *store
	catalog gateway.Catalog
}

// Record implements gateway.Recorder.
func (r *recorder) Record(rec gateway.Record) {
	tokens := metering.Tokens{
		Input: rec.Tokens.Input, Output: rec.Tokens.Output, CachedInput: rec.Tokens.CachedInput,
		CacheWrite: rec.Tokens.CacheWrite, Reasoning: rec.Tokens.Reasoning, Estimated: rec.Tokens.Estimated,
	}
	m := metering.Record{
		ID: rec.ID, At: rec.At.UTC(), EndedAt: rec.EndedAt.UTC(),
		Key: metering.KeyRef{ID: rec.KeyID, Prefix: rec.KeyPrefix}, Owner: rec.Owner,
		Model: metering.Ref{Name: rec.Model, ID: rec.ModelID}, Provider: metering.Ref{Name: rec.Provider, ID: rec.ProviderID},
		UpstreamModel: rec.UpstreamModel,
		Door:          rec.Door, TargetDialect: rec.TargetDialect, Route: rec.Route, Translated: rec.Translated,
		Loss: append([]string{}, rec.Loss...), Attempts: make([]metering.Attempt, 0, len(rec.Attempts)),
		Status: metering.Status(rec.Status), Error: string(rec.Error), UpstreamStatus: rec.UpstreamStatus,
		LatencyMs: rec.Latency.Milliseconds(), TTFBMs: rec.TTFB.Milliseconds(),
		Tokens: tokens, Cost: metering.Charged(tokens, r.pricing(rec)), Stream: rec.Stream,
		Labels: maps.Clone(rec.Labels), RequestLabels: maps.Clone(rec.RequestLabels),
	}
	for _, a := range rec.Attempts {
		m.Attempts = append(m.Attempts, metering.Attempt{
			Provider: a.Provider, UpstreamModel: a.UpstreamModel, Status: metering.Status(a.Status),
			HTTPStatus: a.HTTPStatus, Error: string(a.Error), DurationMs: a.Duration.Milliseconds(),
		})
	}
	if m.Labels == nil {
		m.Labels = map[string]string{}
	}
	if m.RequestLabels == nil {
		m.RequestLabels = map[string]string{}
	}
	r.store.append(m)
}

// pricing is the Model's price for one record, and nil where there is
// nothing to price: a count route, an opaque route, or a Model with no
// pricing.
func (r *recorder) pricing(rec gateway.Record) *v1.Pricing {
	if rec.Model == "" || countRoutes[rec.Route] {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := r.catalog.Model(ctx, rec.Model)
	if err != nil || m == nil {
		return nil
	}
	return m.Spec.Pricing
}

// countRoutes are the door table's token counts, which are answered
// from an estimate and are never priced.
var countRoutes = map[string]bool{
	"/anthropic/v1/messages/count_tokens":       true,
	"/gemini/v1beta/models/{model}:countTokens": true,
	"/lux/v1/count_tokens":                      true,
}

// The seams: the two interfaces the doors take from a platform.
var (
	_ gateway.Limiter  = (*limiter)(nil)
	_ gateway.Recorder = (*recorder)(nil)
)
