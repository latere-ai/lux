// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// bucketIdle is how long an idle pair of rate buckets is kept before
// eviction, spec 007's ten minutes.
const bucketIdle = 10 * time.Minute

// budgetSource is the seam the Limiter reads a Budget through, which
// *KeyCache satisfies: the Budget by id, nil for one that is gone.
type budgetSource interface {
	Budget(ctx context.Context, id string) (*v1.Budget, error)
}

// LimiterOptions is what the Limiter runs under.
type LimiterOptions struct {
	Store store.Store
	// Budgets answers the Budget a Key draws from; nil reads the store
	// on every request, which a test may want and a replica does not.
	Budgets budgetSource
	// Defaults are LUX_DEFAULT_REQUESTS_PER_MINUTE and
	// LUX_DEFAULT_TOKENS_PER_MINUTE, the rates a Key gets when its spec
	// names none; zero is no bucket.
	Defaults manifest.Defaults
	// Flush is how often the spend deltas are written to the store and
	// lastUsedAt is stamped; zero is metering.DefaultFlush.
	Flush time.Duration
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints event ids; nil mints evt_ ULIDs from Now.
	NewID func() string
}

// Limiter is stage 7 of the pipeline on one replica, spec 004's Limiter:
// the Key's two rate buckets, which are this replica's alone, then the
// money questions in the pipeline's order, model_unpriced,
// currency_mismatch, spend_exceeded, budget_exhausted, against the
// store's counters as this replica sees them. A request that is
// admitted holds a Lease whose Settle replaces the reservation with the
// measured count and cost, or refunds it whole when nothing was
// measured. Flush writes the deltas, announces a soft Budget's
// exhaustion, and stamps lastUsedAt at most once a minute per Key.
type Limiter struct {
	o        LimiterOptions
	buckets  *ratelimit.Buckets
	counters *metering.Counters

	mu      sync.Mutex
	used    map[string]time.Time  // key id to the minute of its last admitted request
	written map[string]time.Time  // key id to the minute lastUsedAt was last written
	soft    map[string]softWindow // a soft Budget's spend key to the window Flush checks
}

// softWindow is one soft Budget's current window, checked at flush for
// the once-per-window announcement.
type softWindow struct {
	budget       *v1.Budget
	at, resetsAt time.Time // an instant inside the window, and its reset
}

// NewLimiter constructs the Limiter; Run starts its flush.
func NewLimiter(o LimiterOptions) *Limiter {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = newEventID(o.Now)
	}
	if o.Flush <= 0 {
		o.Flush = metering.DefaultFlush
	}
	return &Limiter{
		o:        o,
		buckets:  ratelimit.New(ratelimit.Config{Idle: bucketIdle, Now: o.Now}),
		counters: metering.NewCounters(o.Store.Counters(), o.Flush, metering.WithClock(o.Now)),
		used:     map[string]time.Time{},
		written:  map[string]time.Time{},
		soft:     map[string]softWindow{},
	}
}

// The bucket keys: one pair per Key id.
func requestsBucket(id string) string { return "requests:" + id }
func tokensBucket(id string) string   { return "tokens:" + id }

// rates are the Key's per-minute rates: its spec's, or the defaults
// where the spec names none. Zero is no bucket.
func (l *Limiter) rates(k *v1.Key) (requests, tokens int) {
	requests, tokens = l.o.Defaults.RequestsPerMinute, l.o.Defaults.TokensPerMinute
	if k.Spec.Limits.RequestsPerMinute != nil {
		requests = *k.Spec.Limits.RequestsPerMinute
	}
	if k.Spec.Limits.TokensPerMinute != nil {
		tokens = *k.Spec.Limits.TokensPerMinute
	}
	return requests, tokens
}

// Reserve implements gateway.Limiter.
func (l *Limiter) Reserve(ctx context.Context, r gateway.Reservation) (gateway.Lease, error) {
	k := r.Key
	if k == nil {
		return nil, errors.New("limits: a reservation without a Key")
	}
	id := k.Status.ID
	now := l.o.Now()
	le := &lease{l: l, key: k}
	requests, tokens := l.rates(k)
	if requests > 0 {
		l.buckets.SetRate(requestsBucket(id), requests)
		if a := l.buckets.Allow(requestsBucket(id)); !a.OK {
			return nil, &gateway.Refusal{Code: gateway.CodeRateLimited, RetryAfter: a.Retry,
				Detail: "Key " + id + ": requestsPerMinute " + strconv.Itoa(requests) + " is spent; the next request is admitted in " + a.Retry.String()}
		}
		le.requests = 1
	}
	if reserved := metering.Reserved(r.InputTokens, r.OutputTokens); tokens > 0 && reserved > 0 {
		// A reservation is bounded by the rate, which is the burst, so a
		// request larger than a whole minute's tokens is admitted when
		// the bucket is full and its measured count settles into a
		// deficit, rather than refused on every call.
		n := int(min(reserved, int64(tokens)))
		l.buckets.SetRate(tokensBucket(id), tokens)
		if a := l.buckets.AllowN(tokensBucket(id), n); !a.OK {
			le.refund()
			return nil, &gateway.Refusal{Code: gateway.CodeRateLimited, RetryAfter: a.Retry,
				Detail: "Key " + id + ": tokensPerMinute " + strconv.Itoa(tokens) + " cannot cover a reservation of " + strconv.Itoa(n) + " tokens, " + strconv.Itoa(a.Remaining) + " remain; they are admitted in " + a.Retry.String()}
		}
		le.tokens = n
	}
	if err := l.reserveSpend(ctx, r, le, now); err != nil {
		le.refund()
		return nil, err
	}
	l.touch(id, now)
	return le, nil
}

// spendLimit is the Key's spend limit, or nil when it names none.
func spendLimit(k *v1.Key) *v1.Spend {
	if s := k.Spec.Limits.Spend; s != nil && s.Amount != nil {
		return s
	}
	return nil
}

// budgetOf is the Budget the Key draws from, nil when it names none or
// the Budget is gone.
func (l *Limiter) budgetOf(ctx context.Context, k *v1.Key) (*v1.Budget, error) {
	if k.Status.Budget == nil || k.Status.Budget.ID == "" {
		return nil, nil
	}
	if l.o.Budgets != nil {
		return l.o.Budgets.Budget(ctx, k.Status.Budget.ID)
	}
	obj, _, err := l.o.Store.Objects().Get(ctx, v1.KindBudget, k.Status.Budget.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, ok := obj.(*v1.Budget)
	if !ok {
		return nil, fmt.Errorf("Budget %s: the store returned a %T", k.Status.Budget.ID, obj)
	}
	return b, nil
}

// isHard reports whether a Budget refuses for its amount.
func isHard(b *v1.Budget) bool { return b.Spec.Hard == nil || *b.Spec.Hard }

// reserveSpend is the money half of stage 7, in the pipeline's order.
// It fills the lease's windows and, when the request is admitted, adds
// the estimate and one request to the deltas.
func (l *Limiter) reserveSpend(ctx context.Context, r gateway.Reservation, le *lease, now time.Time) error {
	k := r.Key
	id := k.Status.ID
	spend := spendLimit(k)
	budget, err := l.budgetOf(ctx, k)
	if err != nil {
		return fmt.Errorf("Budget of Key %s: %w", id, err)
	}
	var pricing *v1.Pricing
	if !r.Opaque && r.Model != nil {
		pricing = r.Model.Spec.Pricing
	}
	if (spend != nil || budget != nil) && pricing == nil && !k.Spec.AllowUnpriced {
		what := "an opaque route is unpriced"
		if r.Model != nil {
			what = "Model " + r.Model.Metadata.Name + " has no pricing"
		}
		return &gateway.Refusal{Code: gateway.CodeModelUnpriced, Detail: what + ", and Key " + id + " spends under a limit without allowUnpriced"}
	}
	var estimate v1.Money
	if pricing != nil {
		estimate = costOf(gateway.Tokens{Input: r.InputTokens, Output: r.OutputTokens}, pricing)
		if spend != nil && spend.Currency != "" && spend.Currency != pricing.Currency {
			return &gateway.Refusal{Code: gateway.CodeCurrencyMismatch, Detail: "Key " + id + " limits its spend in " + spend.Currency + " and Model " + r.Model.Metadata.Name + " is priced in " + pricing.Currency}
		}
		if budget != nil && budget.Spec.Currency != pricing.Currency {
			return &gateway.Refusal{Code: gateway.CodeCurrencyMismatch, Detail: "Budget " + budget.Metadata.Name + " is in " + budget.Spec.Currency + " and Model " + r.Model.Metadata.Name + " is priced in " + pricing.Currency}
		}
	}
	le.pricing, le.estimate = pricing, estimate
	if spend != nil {
		_, resetsAt := metering.Window(spend.Window, now, k.Status.CreatedAt)
		key := metering.CounterKey(metering.ScopeKeySpend, id, spend.Window, now, k.Status.CreatedAt)
		known, pending := l.counters.Known(key), l.counters.Pending(key)
		if metering.Exceeds(metering.Projected(known, pending, int64(estimate)), int64(*spend.Amount)) {
			l.announceKey(ctx, k, spend, resetsAt, v1.Money(known+pending), now)
			return &gateway.Refusal{Code: gateway.CodeSpendExceeded, RetryAfter: metering.RetryAfter(now, resetsAt),
				Detail: "Key " + id + " has spent " + v1.Money(known+pending).String() + " of " + spend.Amount.String() + " " + spend.Currency + " in its " + string(spend.Window) + " window, and this request is estimated at " + estimate.String()}
		}
		le.window = &counterWindow{w: spend.Window, at: now, resetsAt: resetsAt}
	}
	if budget != nil {
		_, resetsAt := metering.Window(budget.Spec.Window, now, budget.Status.CreatedAt)
		key := metering.CounterKey(metering.ScopeBudgetSpend, budget.Status.ID, budget.Spec.Window, now, budget.Status.CreatedAt)
		known, pending := l.counters.Known(key), l.counters.Pending(key)
		if isHard(budget) && metering.Exceeds(metering.Projected(known, pending, int64(estimate)), int64(*budget.Spec.Amount)) {
			l.announceBudget(ctx, budget, now, resetsAt, v1.Money(known+pending), now)
			return &gateway.Refusal{Code: gateway.CodeBudgetExhausted, RetryAfter: metering.RetryAfter(now, resetsAt),
				Detail: "Budget " + budget.Metadata.Name + " has spent " + v1.Money(known+pending).String() + " of " + budget.Spec.Amount.String() + " " + budget.Spec.Currency + " in its " + string(budget.Spec.Window) + " window, and this request is estimated at " + estimate.String()}
		}
		le.budget = &budgetWindow{key: key, resetsAt: resetsAt}
		if !isHard(budget) {
			l.mu.Lock()
			l.soft[key] = softWindow{budget: budget, at: now, resetsAt: resetsAt}
			l.mu.Unlock()
		}
	}
	le.admit()
	return nil
}

// costOf prices tokens under a Model's pricing, spec 009's rule: money
// per Per tokens in micro-units, one rounding half up over the whole
// sum. Spec 009's metering.Cost takes this over when it lands; the
// arithmetic is written there in the same words.
func costOf(t gateway.Tokens, p *v1.Pricing) v1.Money {
	per := int64(p.Per)
	if per <= 0 {
		per = 1
	}
	price := func(m *v1.Money) int64 {
		if m == nil {
			return 0
		}
		return int64(*m)
	}
	n := t.Input*price(p.Input) + t.Output*price(p.Output) + t.CachedInput*price(p.CachedInput) + t.CacheWrite*price(p.CacheWrite)
	return v1.Money((n + per/2) / per)
}

// announceKey claims the window's marker and, first, raises key.exhausted.
func (l *Limiter) announceKey(ctx context.Context, k *v1.Key, spend *v1.Spend, resetsAt time.Time, spent v1.Money, now time.Time) {
	marker := metering.CounterKey(metering.ScopeKeyExhausted, k.Status.ID, spend.Window, now, k.Status.CreatedAt)
	first, err := metering.Claim(ctx, l.o.Store.Counters(), marker, resetsAt)
	if err != nil {
		l.o.Logger.ErrorContext(ctx, "limits: claiming the exhaustion marker", "key", k.Status.ID, "err", err)
		return
	}
	if !first {
		return
	}
	data := map[string]any{"window": spend.Window, "amount": *spend.Amount, "spent": spent, "currency": spend.Currency}
	if !resetsAt.IsZero() {
		data["resetsAt"] = resetsAt
	}
	if err := appendEvent(ctx, l.o.Store.Journal(), eventKeyExhausted, reasonLimit, k, data, now, l.o.NewID); err != nil {
		l.o.Logger.ErrorContext(ctx, "limits: raising the event", "key", k.Status.ID, "err", err)
	}
}

// announceBudget claims the window's marker and, first, raises
// budget.exhausted. at is an instant inside the window announced.
func (l *Limiter) announceBudget(ctx context.Context, b *v1.Budget, at, resetsAt time.Time, spent v1.Money, now time.Time) {
	marker := metering.CounterKey(metering.ScopeBudgetExhausted, b.Status.ID, b.Spec.Window, at, b.Status.CreatedAt)
	first, err := metering.Claim(ctx, l.o.Store.Counters(), marker, resetsAt)
	if err != nil {
		l.o.Logger.ErrorContext(ctx, "limits: claiming the exhaustion marker", "budget", b.Status.ID, "err", err)
		return
	}
	if !first {
		return
	}
	data := map[string]any{"window": b.Spec.Window, "amount": *b.Spec.Amount, "spent": spent, "currency": b.Spec.Currency, "hard": isHard(b)}
	if !resetsAt.IsZero() {
		data["resetsAt"] = resetsAt
	}
	if err := appendEvent(ctx, l.o.Store.Journal(), eventBudgetExhausted, reasonLimit, b, data, now, l.o.NewID); err != nil {
		l.o.Logger.ErrorContext(ctx, "limits: raising the event", "budget", b.Status.ID, "err", err)
	}
}

// touch notes the Key was used in this minute.
func (l *Limiter) touch(id string, now time.Time) {
	minute := now.UTC().Truncate(time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used[id] = minute
}

// Flush writes this replica's deltas to the store, announces a soft
// Budget whose window the flushed total shows at or over its amount,
// and writes lastUsedAt for every Key used in a minute it has not yet
// written. A store that cannot answer is logged; the deltas wait for
// the next flush.
func (l *Limiter) Flush(ctx context.Context) {
	if err := l.counters.Flush(ctx); err != nil {
		l.o.Logger.ErrorContext(ctx, "limits: flushing the spend counters", "err", err)
	}
	now := l.o.Now()
	l.mu.Lock()
	soft := make(map[string]softWindow, len(l.soft))
	for key, w := range l.soft {
		if !w.resetsAt.IsZero() && !w.resetsAt.After(now) {
			delete(l.soft, key)
			continue
		}
		soft[key] = w
	}
	used := l.used
	l.used = map[string]time.Time{}
	l.mu.Unlock()
	for key, w := range soft {
		if l.counters.Total(key) < int64(*w.budget.Spec.Amount) {
			continue
		}
		l.announceBudget(ctx, w.budget, w.at, w.resetsAt, v1.Money(l.counters.Total(key)), now)
		l.mu.Lock()
		delete(l.soft, key)
		l.mu.Unlock()
	}
	l.writeLastUsed(ctx, used, now)
}

// writeLastUsed stamps lastUsedAt once per Key per minute, rounded down
// to the minute, so a busy Key costs one status write a minute.
func (l *Limiter) writeLastUsed(ctx context.Context, used map[string]time.Time, now time.Time) {
	l.mu.Lock()
	for id, at := range l.written {
		if now.Sub(at) > time.Hour {
			delete(l.written, id)
		}
	}
	l.mu.Unlock()
	for id, minute := range used {
		l.mu.Lock()
		last := l.written[id]
		l.mu.Unlock()
		if !minute.After(last) {
			continue
		}
		err := l.o.Store.Objects().PutStatus(ctx, v1.KindKey, id, store.KeyObserved{LastUsedAt: minute})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			l.o.Logger.ErrorContext(ctx, "limits: writing lastUsedAt", "key", id, "err", err)
			continue
		}
		l.mu.Lock()
		l.written[id] = minute
		l.mu.Unlock()
	}
}

// Run flushes on the interval until ctx ends, then once more on a
// context that outlives the cancellation, so a stopping replica leaves
// no delta behind.
func (l *Limiter) Run(ctx context.Context) {
	t := time.NewTicker(l.o.Flush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.o.Flush)
			defer cancel()
			l.Flush(last)
			return
		case <-t.C:
			l.Flush(ctx)
		}
	}
}

// counterWindow is the Key's current spend window: the rule, an instant
// inside it, and its reset.
type counterWindow struct {
	w            v1.Window
	at, resetsAt time.Time
}

// budgetWindow is the Budget's current window.
type budgetWindow struct {
	key      string
	resetsAt time.Time
}

// row is one counter this lease adds to, with the expiry of its window.
type row struct {
	key       string
	expiresAt time.Time
}

// rows are the Key's counters of one scope this lease adds to: the
// lifetime total, and the current spend window when the Key has a
// spend limit and it is not the total itself, which a none window is.
func (le *lease) rows(s metering.Scope) []row {
	k := le.key
	out := []row{{key: metering.TotalKey(s, k.Status.ID)}}
	if w := le.window; w != nil {
		if key := metering.CounterKey(s, k.Status.ID, w.w, w.at, k.Status.CreatedAt); key != out[0].key {
			out = append(out, row{key: key, expiresAt: w.resetsAt})
		}
	}
	return out
}

// lease is one admitted reservation: what was charged to the buckets and
// the deltas, so Settle can replace it with what was measured.
type lease struct {
	l        *Limiter
	key      *v1.Key
	requests int // 1 when the request bucket was charged
	tokens   int // the tokens charged to the token bucket
	pricing  *v1.Pricing
	estimate v1.Money
	window   *counterWindow // the Key's spend window, nil without a spend limit
	budget   *budgetWindow  // the Budget's window, nil without a Budget
	once     sync.Once
}

// refund gives the buckets back what a refusal after them charged.
func (le *lease) refund() {
	id := le.key.Status.ID
	if le.requests > 0 {
		le.l.buckets.Adjust(requestsBucket(id), le.requests)
	}
	if le.tokens > 0 {
		le.l.buckets.Adjust(tokensBucket(id), le.tokens)
	}
}

// admit adds the reservation to the deltas: one request and the
// estimate under the Key's totals, under its spend window when it has
// one, and under the Budget's window when it draws from one.
func (le *lease) admit() {
	c := le.l.counters
	for _, r := range le.rows(metering.ScopeKeyRequests) {
		c.Add(r.key, 1, r.expiresAt)
	}
	for _, r := range le.rows(metering.ScopeKeySpend) {
		c.Add(r.key, int64(le.estimate), r.expiresAt)
	}
	if b := le.budget; b != nil {
		c.Add(b.key, int64(le.estimate), b.resetsAt)
	}
}

// Settle implements gateway.Lease: the token bucket is adjusted by the
// reservation less the measured count, the measured cost replaces the
// estimate in every delta the reservation entered, and the measured
// tokens count under the Key. Zero measured tokens is the whole refund.
func (le *lease) Settle(_ context.Context, t gateway.Tokens) {
	le.once.Do(func() {
		c := le.l.counters
		settled := metering.Settled(t.Input, t.Output)
		if le.tokens > 0 {
			le.l.buckets.Adjust(tokensBucket(le.key.Status.ID), int(metering.Adjustment(int64(le.tokens), settled)))
		}
		var measured v1.Money
		if le.pricing != nil {
			measured = costOf(t, le.pricing)
		}
		delta := int64(measured) - int64(le.estimate)
		for _, r := range le.rows(metering.ScopeKeyTokens) {
			c.Add(r.key, settled, r.expiresAt)
		}
		for _, r := range le.rows(metering.ScopeKeySpend) {
			c.Add(r.key, delta, r.expiresAt)
		}
		if b := le.budget; b != nil {
			c.Add(b.key, delta, b.resetsAt)
		}
	})
}
