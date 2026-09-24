// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// cent is one cent in micro-units.
const cent = v1.Money(10_000)

// priced is a Model priced per token, so one input token under "0.01"
// costs one cent and the figures in the tests are whole cents.
func priced(name, input, output, currency string) *v1.Model {
	in, err := v1.ParseMoney(input)
	if err != nil {
		panic(err)
	}
	out, err := v1.ParseMoney(output)
	if err != nil {
		panic(err)
	}
	return &v1.Model{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ModelSpec{Pricing: &v1.Pricing{Currency: currency, Per: 1, Input: &in, Output: &out, CachedInput: &in, CacheWrite: &in}},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, time.Now(), nil), Owner: subject, Warnings: []string{}},
	}
}

// unpriced is a Model with no pricing.
func unpriced(name string) *v1.Model {
	return &v1.Model{Metadata: v1.ObjectMeta{Name: name}, Status: v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, time.Now(), nil), Owner: subject, Warnings: []string{}}}
}

// rates is the edit that gives a Key its two rates.
func rates(requests, tokens int) func(k *v1.Key) {
	return func(k *v1.Key) {
		k.Spec.Limits.RequestsPerMinute, k.Spec.Limits.TokensPerMinute = &requests, &tokens
	}
}

// spends is the edit that gives a Key a spend limit.
func spends(amount, currency string, window v1.Window) func(k *v1.Key) {
	return func(k *v1.Key) {
		m, err := v1.ParseMoney(amount)
		if err != nil {
			panic(err)
		}
		k.Spec.Limits.Spend = &v1.Spend{Amount: &m, Currency: currency, Window: window}
	}
}

// edits joins several edits.
func edits(fns ...func(k *v1.Key)) func(k *v1.Key) {
	return func(k *v1.Key) {
		for _, fn := range fns {
			fn(k)
		}
	}
}

func (h *harness) limiter(st store.Store, budgets budgetSource, defaults manifest.Defaults) *Limiter {
	return NewLimiter(LimiterOptions{Store: st, Budgets: budgets, Defaults: defaults, Flush: time.Second, Logger: h.logger, Now: h.clock})
}

// reserve asks l to admit one request of in and out tokens under k for
// m and returns the lease, or the refusal.
func reserve(t *testing.T, l *Limiter, k *v1.Key, m *v1.Model, in, out int64) (gateway.Lease, *gateway.Refusal) {
	t.Helper()
	lease, err := l.Reserve(t.Context(), gateway.Reservation{Key: k, Model: m, InputTokens: in, OutputTokens: out})
	if ref, ok := errors.AsType[*gateway.Refusal](err); ok {
		return nil, ref
	}
	if err != nil {
		t.Fatal(err)
	}
	return lease, nil
}

// spendOne admits one one-cent request under k and settles it measured.
func spendOne(t *testing.T, l *Limiter, k *v1.Key, m *v1.Model) *gateway.Refusal {
	t.Helper()
	lease, ref := reserve(t, l, k, m, 1, 0)
	if ref != nil {
		return ref
	}
	lease.Settle(t.Context(), gateway.Tokens{Input: 1})
	return nil
}

// counter reads one store counter.
func (h *harness) counter(t *testing.T, key string) int64 {
	t.Helper()
	m, err := h.st.Counters().Read(t.Context(), []string{key})
	if err != nil {
		t.Fatal(err)
	}
	return m[key]
}

// eventData decodes one event's data block.
func eventData(t *testing.T, e store.Event) (eventRecord, map[string]any) {
	t.Helper()
	var rec eventRecord
	if err := json.Unmarshal(e.Payload, &rec); err != nil {
		t.Fatal(err)
	}
	data, ok := rec.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is a %T", rec.Data)
	}
	return rec, data
}

// TestRateLimitPackageShape: ratelimit.Buckets offers AllowN, Adjust,
// and per-key rates without a default bucket, and the Limiter's buckets
// run in that mode: a Key with no rate is unlimited.
func TestRateLimitPackageShape(t *testing.T) {
	var _ interface {
		AllowN(key string, n int) ratelimit.Allowance
		Adjust(key string, delta int)
		SetRate(key string, perMinute int)
		Allow(key string) ratelimit.Allowance
	} = (*ratelimit.Buckets)(nil)
	h := newHarness(t)
	b := ratelimit.New(ratelimit.Config{Now: h.clock})
	for range 1000 {
		if !b.Allow("free").OK {
			t.Fatal("a key with no rate was limited in the per-key mode")
		}
	}
	b.SetRate("paced", 2)
	if !b.AllowN("paced", 2).OK || b.Allow("paced").OK {
		t.Fatal("SetRate did not limit the key")
	}
	b.Adjust("paced", 1)
	if !b.Allow("paced").OK {
		t.Fatal("Adjust did not refund a token")
	}
	l := h.limiter(h.st, nil, manifest.Defaults{})
	k, _ := h.key(t, "free", nil)
	for range 500 {
		if _, ref := reserve(t, l, k, unpriced("m"), 100, 100); ref != nil {
			t.Fatalf("a Key without rates was refused: %v", ref)
		}
	}
}

// TestRequestBucket: a Key with requestsPerMinute 60 admits 60 at once,
// refuses the 61st with Retry-After 1, and admits one more after a
// second; zero is no limit; a Key naming no rate takes the default.
func TestRequestBucket(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{RequestsPerMinute: 2})
	m := unpriced("m")
	k, _ := h.key(t, "sixty", rates(60, 0))
	for i := range 60 {
		if _, ref := reserve(t, l, k, m, 10, 10); ref != nil {
			t.Fatalf("request %d refused: %v", i+1, ref)
		}
	}
	_, ref := reserve(t, l, k, m, 10, 10)
	if ref == nil || ref.Code != gateway.CodeRateLimited || ref.RetryAfter != time.Second || !strings.Contains(ref.Detail, "requestsPerMinute 60") {
		t.Fatalf("the 61st = %+v", ref)
	}
	h.advance(time.Second)
	if _, ref := reserve(t, l, k, m, 10, 10); ref != nil {
		t.Fatalf("after a second: %v", ref)
	}
	if _, ref := reserve(t, l, k, m, 10, 10); ref == nil {
		t.Fatal("a second token was admitted after one second")
	}
	unlimited, _ := h.key(t, "unlimited", rates(0, 0))
	for range 200 {
		if _, ref := reserve(t, l, unlimited, m, 10, 10); ref != nil {
			t.Fatalf("zero is a limit: %v", ref)
		}
	}
	// No rate in the spec is the operator's default of two.
	defaulted, _ := h.key(t, "defaulted", nil)
	for range 2 {
		if _, ref := reserve(t, l, defaulted, m, 1, 1); ref != nil {
			t.Fatalf("within the default: %v", ref)
		}
	}
	if _, ref := reserve(t, l, defaulted, m, 1, 1); ref == nil || ref.Code != gateway.CodeRateLimited {
		t.Fatalf("the third under a default of two = %+v", ref)
	}
}

// TestTokenReservationSettles: the token bucket is charged the
// reservation before the request and settled to the measured count
// after, refunding an over-estimate and debiting an under-estimate into
// a deficit the refill covers first; a reservation above the whole
// minute is bounded by it and admitted when the bucket is full.
func TestTokenReservationSettles(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{})
	m := unpriced("m")
	k, _ := h.key(t, "thousand", rates(0, 1000))
	lease, ref := reserve(t, l, k, m, 100, 500)
	if ref != nil {
		t.Fatal(ref)
	}
	if _, ref := reserve(t, l, k, m, 401, 0); ref == nil {
		t.Fatal("401 tokens were admitted with 400 left")
	}
	lease.Settle(t.Context(), gateway.Tokens{Input: 100, Output: 200})
	if _, ref := reserve(t, l, k, m, 700, 0); ref != nil {
		t.Fatalf("the refund of 300 was not given back: %v", ref)
	}
	_, ref = reserve(t, l, k, m, 1, 0)
	if ref == nil || ref.Code != gateway.CodeRateLimited || ref.RetryAfter != 60*time.Millisecond || !strings.Contains(ref.Detail, "tokensPerMinute 1000") {
		t.Fatalf("on an empty bucket = %+v", ref)
	}
	// An under-estimate: reserve 200, measure 1000, and the bucket is
	// 800 in deficit, covered by the refill before the next token.
	under, _ := h.key(t, "under", rates(0, 1000))
	lease, _ = reserve(t, l, under, m, 100, 100)
	lease.Settle(t.Context(), gateway.Tokens{Input: 600, Output: 400})
	if _, ref := reserve(t, l, under, m, 1, 0); ref == nil || ref.RetryAfter != 60*time.Millisecond {
		t.Fatalf("after settling into a deficit = %+v", ref)
	}
	h.advance(30 * time.Second)
	if _, ref := reserve(t, l, under, m, 500, 0); ref != nil {
		t.Fatalf("half a minute refilled 500: %v", ref)
	}
	// A request larger than a minute's tokens is bounded to the burst,
	// admitted when the bucket is full, and its measured count settles
	// into a deficit the refill pays off.
	small, _ := h.key(t, "small", rates(0, 100))
	lease, ref = reserve(t, l, small, m, 1000, 0)
	if ref != nil {
		t.Fatalf("a reservation above the minute was refused: %v", ref)
	}
	lease.Settle(t.Context(), gateway.Tokens{Input: 1000})
	if _, ref := reserve(t, l, small, m, 1, 0); ref == nil || ref.RetryAfter != 9*time.Minute+600*time.Millisecond {
		t.Fatalf("in a deficit of 900 at 100 a minute = %+v", ref)
	}
	// Settle is once: a second settle changes nothing.
	lease.Settle(t.Context(), gateway.Tokens{})
	if _, ref := reserve(t, l, small, m, 1, 0); ref == nil {
		t.Fatal("a second Settle refunded the reservation")
	}
}

// TestRefusalDebitsNothing: a refusal at this stage debits nothing, the
// request token included when the token bucket refuses after it, and a
// refusal at a later stage refunds the whole reservation.
func TestRefusalDebitsNothing(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	l := h.limiter(h.st, nil, manifest.Defaults{})
	m := unpriced("m")
	k, _ := h.key(t, "two-by-hundred", rates(2, 100))
	if _, ref := reserve(t, l, k, m, 60, 0); ref != nil {
		t.Fatal(ref)
	}
	if _, ref := reserve(t, l, k, m, 60, 0); ref == nil || ref.Code != gateway.CodeRateLimited {
		t.Fatalf("60 tokens with 40 left = %+v", ref)
	}
	// The request token of the refused request was given back: the
	// second admitted request is this one, and the third is refused.
	if _, ref := reserve(t, l, k, m, 10, 0); ref != nil {
		t.Fatalf("the request token was not refunded: %v", ref)
	}
	if _, ref := reserve(t, l, k, m, 10, 0); ref == nil || !strings.Contains(ref.Detail, "requestsPerMinute") {
		t.Fatalf("the third request = %+v", ref)
	}
	// A refusal in the money half refunds both buckets.
	limited, _ := h.key(t, "limited", edits(rates(1, 100), spends("1", "USD", "1h")))
	if _, ref := reserve(t, l, limited, m, 50, 0); ref == nil || ref.Code != gateway.CodeModelUnpriced {
		t.Fatalf("an unpriced Model under a spend limit = %+v", ref)
	}
	if _, ref := reserve(t, l, limited, priced("p", "0.01", "0", "USD"), 100, 0); ref != nil {
		t.Fatalf("the buckets were debited by the money refusal: %v", ref)
	}
	// A refusal at a later stage settles with zero tokens: the whole
	// reservation comes back to the token bucket, and the estimate
	// leaves the window.
	later, _ := h.key(t, "later", edits(rates(0, 5), spends("0.05", "USD", "1h")))
	p := priced("p", "0.01", "0", "USD")
	lease, ref := reserve(t, l, later, p, 5, 0)
	if ref != nil {
		t.Fatal(ref)
	}
	if _, ref := reserve(t, l, later, p, 1, 0); ref == nil || ref.Code != gateway.CodeRateLimited {
		t.Fatalf("with the bucket and the window reserved whole = %+v", ref)
	}
	lease.Settle(ctx, gateway.Tokens{})
	if _, ref := reserve(t, l, later, p, 5, 0); ref != nil {
		t.Fatalf("after the whole refund: %v", ref)
	}
	if n := l.counters.Total(metering.CounterKey(metering.ScopeKeyRequests, later.Status.ID, v1.WindowNone, h.clock(), later.Status.CreatedAt)); n != 2 {
		t.Fatalf("admitted requests counted = %d, want 2", n)
	}
}

// TestRateIsPerReplica: two replicas each admit the configured rate, so
// the installation admits twice it and no more.
func TestRateIsPerReplica(t *testing.T) {
	h := newHarness(t)
	a, b := h.limiter(h.st, nil, manifest.Defaults{}), h.limiter(h.st, nil, manifest.Defaults{})
	k, _ := h.key(t, "sixty", rates(60, 0))
	m := unpriced("m")
	admitted := 0
	for _, l := range []*Limiter{a, b} {
		for range 61 {
			if _, ref := reserve(t, l, k, m, 1, 1); ref == nil {
				admitted++
			}
		}
	}
	if admitted != 120 {
		t.Fatalf("two replicas admitted %d, want 120", admitted)
	}
}

// TestSpendWindow: a spend window refuses when known + pending +
// estimate exceeds the amount, an over-estimate settled to less frees
// its room, Retry-After names the reset, and a none window carries none.
func TestSpendWindow(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	l := h.limiter(h.st, nil, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "nickel", spends("0.05", "USD", "1h"))
	for i := range 5 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatalf("request %d: %v", i+1, ref)
		}
	}
	ref := spendOne(t, l, k, p)
	if ref == nil || ref.Code != gateway.CodeSpendExceeded || ref.RetryAfter != time.Hour || !strings.Contains(ref.Detail, "has spent 0.05 of 0.05 USD in its 1h window") {
		t.Fatalf("the sixth cent = %+v", ref)
	}
	if h.counter(t, metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "1h", h.clock(), k.Status.CreatedAt)) != 0 {
		t.Fatal("a delta reached the store before the flush")
	}
	l.Flush(ctx)
	if got := h.counter(t, metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "1h", h.clock(), k.Status.CreatedAt)); got != int64(5*cent) {
		t.Fatalf("the flushed window holds %d", got)
	}
	// An over-estimate: five cents reserved lands on the amount and is
	// admitted; settled at two, three more fit.
	k2, _ := h.key(t, "over", spends("0.05", "USD", "1h"))
	lease, ref := reserve(t, l, k2, p, 5, 0)
	if ref != nil {
		t.Fatal(ref)
	}
	lease.Settle(ctx, gateway.Tokens{Input: 2})
	for i := range 3 {
		if ref := spendOne(t, l, k2, p); ref != nil {
			t.Fatalf("after the settle, request %d: %v", i+1, ref)
		}
	}
	if ref := spendOne(t, l, k2, p); ref == nil {
		t.Fatal("the sixth cent of the second Key was admitted")
	}
	// A none window never resets and names no Retry-After.
	forever, _ := h.key(t, "forever", spends("0.01", "USD", v1.WindowNone))
	if ref := spendOne(t, l, forever, p); ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, forever, p); ref == nil || ref.Code != gateway.CodeSpendExceeded || ref.RetryAfter != 0 {
		t.Fatalf("under a none window = %+v", ref)
	}
}

// TestWindowReset: a refused Key serves again after the window number
// changes, on the clock alone and without a write; status renders
// Exhausted before and Active after.
func TestWindowReset(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	l := h.limiter(st, nil, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "penny", spends("0.01", "USD", "1h"))
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, k, p); ref == nil || ref.RetryAfter != time.Hour {
		t.Fatalf("the second cent = %+v", ref)
	}
	l.Flush(ctx)
	if err := RenderKey(ctx, st, k, h.clock()); err != nil || k.Status.State != v1.KeyExhausted {
		t.Fatalf("before the reset: %s, %v", k.Status.State, err)
	}
	h.advance(time.Hour)
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("after the reset: %v", ref)
	}
	if err := RenderKey(ctx, st, k, h.clock()); err != nil || k.Status.State != v1.KeyActive || k.Status.Usage.Window.Requests != 0 {
		t.Fatalf("after the reset: %s, %+v, %v", k.Status.State, k.Status.Usage.Window, err)
	}
	if ops(reg, "Objects.Put") != 0 || ops(reg, "Objects.PutStatus") > 1 {
		t.Fatalf("the reset wrote: %d Put, %d PutStatus", ops(reg, "Objects.Put"), ops(reg, "Objects.PutStatus"))
	}
}

// TestOvershootBound: three replicas, a one-second flush, ten requests
// a second per replica, and a one-cent request never exceed a hard
// limit by more than (R − 1) × F × T × C + C, twenty-one cents, over a
// hundred runs; one replica never overshoots by more than one request.
func TestOvershootBound(t *testing.T) {
	const replicas, perSecond = 3, 10
	amount := 100 * cent
	bound := metering.Overshoot(replicas, time.Second, perSecond, cent)
	if bound != 21*cent {
		t.Fatalf("the bound is %s", bound)
	}
	worst := v1.Money(0)
	for run := range 100 {
		h := newHarness(t)
		ctx := t.Context()
		p := priced("p", "0.01", "0", "USD")
		k, _ := h.key(t, "dollar", spends("1", "USD", "24h"))
		ls := make([]*Limiter, replicas)
		for i := range ls {
			ls[i] = h.limiter(h.st, nil, manifest.Defaults{})
		}
		rng := rand.New(rand.NewPCG(uint64(run), 7))
		refused := make([]bool, replicas)
		for done := 0; done < replicas; {
			for _, i := range rng.Perm(replicas) {
				for range perSecond {
					if refused[i] {
						break
					}
					if spendOne(t, ls[i], k, p) != nil {
						refused[i] = true
						done++
					}
				}
			}
			for _, l := range ls {
				l.Flush(ctx)
			}
			h.advance(time.Second)
		}
		spent := v1.Money(h.counter(t, metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "24h", h.clock(), k.Status.CreatedAt)))
		if over := spent - amount; over > worst {
			worst = over
		}
		if spent > amount+bound {
			t.Fatalf("run %d: spent %s against %s, over the bound of %s", run, spent, amount, bound)
		}
	}
	if worst <= 0 {
		t.Fatal("no run overshot at all; the simulation is not driving the replicas apart")
	}
	t.Logf("worst overshoot over a hundred runs: %s of a bound of %s", worst, bound)
	// One replica: the projection includes its own pending spend, so it
	// stops at the amount and never by more than one request.
	h := newHarness(t)
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "dollar", spends("1", "USD", "24h"))
	l := h.limiter(h.st, nil, manifest.Defaults{})
	for spendOne(t, l, k, p) == nil {
	}
	l.Flush(t.Context())
	spent := v1.Money(h.counter(t, metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "24h", h.clock(), k.Status.CreatedAt)))
	if spent > amount+cent {
		t.Fatalf("one replica spent %s against %s", spent, amount)
	}
}

// TestExhaustionIsAnnouncedOnce: six replicas driving one Key's spend
// window and one Budget's window past their amounts emit exactly one
// key.exhausted and one budget.exhausted per window, through the marker
// counter, for a hard limit at the first refusal and for a soft Budget
// at the first flush that observes it.
func TestExhaustionIsAnnouncedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := priced("p", "0.01", "0", "USD")
	hard := h.budget(t, "hard", "0.10", "USD", "1h", true)
	soft := h.budget(t, "soft", "0.03", "USD", "1h", false)
	limited, _ := h.key(t, "limited", edits(spends("0.05", "USD", "1h"), draws(hard)))
	free, _ := h.key(t, "free", draws(hard))
	softly, _ := h.key(t, "softly", draws(soft))
	ls := make([]*Limiter, 6)
	for i := range ls {
		ls[i] = h.limiter(h.st, nil, manifest.Defaults{})
	}
	flushAll := func() {
		for _, l := range ls {
			l.Flush(ctx)
		}
	}
	// Round-robin one-cent requests until every replica has refused, so
	// every replica observes the exhausted window.
	drive := func(k *v1.Key, code gateway.Code) {
		t.Helper()
		refused := 0
		for n := 0; refused < len(ls) && n < 100; n++ {
			l := ls[n%len(ls)]
			if ref := spendOne(t, l, k, p); ref != nil {
				if ref.Code != code {
					t.Fatalf("%s refused %s, want %s", k.Metadata.Name, ref.Code, code)
				}
				refused++
			}
			flushAll()
		}
		if refused < len(ls) {
			t.Fatalf("%s: only %d replicas refused", k.Metadata.Name, refused)
		}
	}
	drive(limited, gateway.CodeSpendExceeded)
	if n := len(h.events(t, eventKeyExhausted)); n != 1 {
		t.Fatalf("%d key.exhausted events, want 1", n)
	}
	rec, data := eventData(t, h.events(t, eventKeyExhausted)[0])
	if rec.Reason != reasonLimit || rec.Subject != "" || rec.Object.Kind != v1.KindKey || rec.Object.ID != limited.Status.ID || rec.Object.Name != "limited" {
		t.Fatalf("event = %+v", rec)
	}
	if data["window"] != "1h" || data["amount"] != "0.05" || data["spent"] != "0.05" || data["currency"] != "USD" || data["resetsAt"] == nil {
		t.Fatalf("data = %v", data)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 0 {
		t.Fatalf("the hard Budget at five of ten cents raised %d events", n)
	}
	drive(free, gateway.CodeBudgetExhausted)
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted events, want 1", n)
	}
	rec, data = eventData(t, h.events(t, eventBudgetExhausted)[0])
	if rec.Object.Kind != v1.KindBudget || rec.Object.ID != hard.Status.ID || data["hard"] != true || data["amount"] != "0.1" || data["spent"] != "0.1" {
		t.Fatalf("budget event = %+v %v", rec, data)
	}
	// The soft Budget: no refusal, one event at the first flush that
	// observes the window at its amount, on every replica at once.
	for n := range 12 {
		if ref := spendOne(t, ls[n%len(ls)], softly, p); ref != nil {
			t.Fatalf("a soft Budget refused: %v", ref)
		}
	}
	flushAll()
	flushAll()
	events := h.events(t, eventBudgetExhausted)
	if len(events) != 2 {
		t.Fatalf("%d budget.exhausted events after the soft Budget, want 2", len(events))
	}
	rec, data = eventData(t, events[1])
	if rec.Object.ID != soft.Status.ID || data["hard"] != false || data["amount"] != "0.03" {
		t.Fatalf("soft event = %+v %v", rec, data)
	}
	// The announcing replica reports the total its flush observed, at or
	// over the amount and no more than everything spent.
	if spent, err := v1.ParseMoney(data["spent"].(string)); err != nil || spent < 3*cent || spent > 12*cent {
		t.Fatalf("soft event spent = %v, %v", data["spent"], err)
	}
	// Every replica whose flush showed the window at or over the amount
	// claimed the marker; one whose last flush left it under has no
	// delta to flush and does not look again, and one of the claimants
	// was first.
	if n := h.counter(t, metering.MarkerKey(metering.BudgetCounterKey(metering.ScopeBudgetExhausted, soft, h.clock()), *soft.Spec.Amount)); n < 1 || n > 6 {
		t.Fatalf("the soft marker was claimed %d times", n)
	}
	// The next window announces again, once.
	h.advance(time.Hour)
	drive(limited, gateway.CodeSpendExceeded)
	if n := len(h.events(t, eventKeyExhausted)); n != 2 {
		t.Fatalf("%d key.exhausted events after the reset, want 2", n)
	}
}

// TestUnpricedRule: an unpriced Model and an opaque route are
// model_unpriced under a spend limit or a Budget, hard or soft, without
// allowUnpriced, served with it, and served under neither.
func TestUnpricedRule(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{})
	hard := h.budget(t, "hard", "1", "USD", v1.WindowMonth, true)
	soft := h.budget(t, "soft", "1", "USD", v1.WindowMonth, false)
	allow := func(k *v1.Key) { k.Spec.AllowUnpriced = true }
	for _, c := range []struct {
		name   string
		edit   func(k *v1.Key)
		opaque bool
		want   gateway.Code // "" for admitted
	}{
		{"spend limit", spends("1", "USD", "1h"), false, gateway.CodeModelUnpriced},
		{"hard budget", draws(hard), false, gateway.CodeModelUnpriced},
		{"soft budget", draws(soft), false, gateway.CodeModelUnpriced},
		{"opaque under a budget", draws(hard), true, gateway.CodeModelUnpriced},
		{"spend limit with allowUnpriced", edits(spends("1", "USD", "1h"), allow), false, ""},
		{"opaque under a budget with allowUnpriced", edits(draws(soft), allow), true, ""},
		{"neither", nil, false, ""},
		{"opaque under neither", nil, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			k, _ := h.key(t, strings.ReplaceAll(c.name, " ", "-"), c.edit)
			res := gateway.Reservation{Key: k, Opaque: c.opaque}
			if !c.opaque {
				res.Model = unpriced("bare")
				res.InputTokens, res.OutputTokens = 10, 10
			}
			_, err := l.Reserve(t.Context(), res)
			var ref *gateway.Refusal
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && !errors.As(err, &ref):
				t.Fatalf("admitted, want %s", c.want)
			case c.want != "" && ref.Code != c.want:
				t.Fatalf("refused %s, want %s", ref.Code, c.want)
			case c.want != "" && ref.RetryAfter != 0:
				t.Fatalf("Retry-After %s on %s", ref.RetryAfter, ref.Code)
			}
			if ref != nil {
				what := "Model bare has no pricing"
				if c.opaque {
					what = "an opaque route is unpriced"
				}
				if !strings.Contains(ref.Detail, what) || !strings.Contains(ref.Detail, "without allowUnpriced") {
					t.Fatalf("detail = %q", ref.Detail)
				}
			}
		})
	}
}

// TestCurrencyMismatch: a Budget or a spend limit in another currency
// than the Model's pricing is currency_mismatch, and nothing converts.
func TestCurrencyMismatch(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{})
	eur := h.budget(t, "eur", "1", "EUR", v1.WindowMonth, true)
	usd := h.budget(t, "usd", "1", "USD", v1.WindowMonth, true)
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "eur-budget", draws(eur))
	_, ref := reserve(t, l, k, p, 1, 0)
	if ref == nil || ref.Code != gateway.CodeCurrencyMismatch || !strings.Contains(ref.Detail, "Budget eur is in EUR and Model p is priced in USD") {
		t.Fatalf("a EUR Budget under a USD Model = %+v", ref)
	}
	k, _ = h.key(t, "eur-spend", edits(spends("1", "EUR", "1h"), draws(usd)))
	_, ref = reserve(t, l, k, p, 1, 0)
	if ref == nil || ref.Code != gateway.CodeCurrencyMismatch || !strings.Contains(ref.Detail, "limits its spend in EUR and Model p is priced in USD") {
		t.Fatalf("a EUR spend limit under a USD Model = %+v", ref)
	}
	k, _ = h.key(t, "agree", edits(spends("1", "USD", "1h"), draws(usd)))
	if _, ref := reserve(t, l, k, p, 1, 0); ref != nil {
		t.Fatalf("agreeing currencies: %v", ref)
	}
}

// TestRefusalOrderAmongLimits: with every condition present, the
// refusal is the pipeline's first: rate_limited, then model_unpriced,
// currency_mismatch, spend_exceeded, budget_exhausted.
func TestRefusalOrderAmongLimits(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{})
	empty := h.budget(t, "empty", "0", "USD", v1.WindowMonth, true)
	usd, eur := priced("usd", "0.01", "0", "USD"), priced("eur", "0.01", "0", "EUR")
	for _, s := range []struct {
		name  string
		edit  func(k *v1.Key)
		model *v1.Model
		want  gateway.Code
	}{
		{"every condition", edits(rates(1, 0), spends("0", "USD", "1h"), draws(empty)), unpriced("bare"), gateway.CodeRateLimited},
		{"without the rate", edits(spends("0", "USD", "1h"), draws(empty)), unpriced("bare"), gateway.CodeModelUnpriced},
		{"with a price in another currency", edits(spends("0", "USD", "1h"), draws(empty)), eur, gateway.CodeCurrencyMismatch},
		{"with the currencies agreeing", edits(spends("0", "USD", "1h"), draws(empty)), usd, gateway.CodeSpendExceeded},
		{"without the spend limit", draws(empty), usd, gateway.CodeBudgetExhausted},
		{"without the budget", nil, usd, ""},
	} {
		k, _ := h.key(t, strings.ReplaceAll(s.name, " ", "-"), s.edit)
		if s.want == gateway.CodeRateLimited {
			// The one request the rate admits is spent first, by a view
			// of the same Key without the money conditions, because a
			// money refusal would give the request token back.
			prime := *k
			prime.Spec.Limits.Spend, prime.Status.Budget = nil, nil
			if _, ref := reserve(t, l, &prime, unpriced("x"), 0, 0); ref != nil {
				t.Fatalf("%s: priming = %+v", s.name, ref)
			}
		}
		_, ref := reserve(t, l, k, s.model, 1, 0)
		switch {
		case s.want == "" && ref != nil:
			t.Fatalf("%s: refused %s", s.name, ref.Code)
		case s.want != "" && ref == nil:
			t.Fatalf("%s: admitted, want %s", s.name, s.want)
		case s.want != "" && ref.Code != s.want:
			t.Fatalf("%s: %s, want %s", s.name, ref.Code, s.want)
		}
	}
}

// TestSoftBudget: a soft Budget never refuses for its amount, emits
// budget.exhausted exactly once per window when reached, renders
// Exhausted until the reset, and never exhausts its Keys.
func TestSoftBudget(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	l := h.limiter(h.st, nil, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	soft := h.budget(t, "soft", "0.03", "USD", "1h", false)
	k, _ := h.key(t, "softly", draws(soft))
	for i := range 5 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatalf("request %d under a soft Budget: %v", i+1, ref)
		}
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 0 {
		t.Fatalf("%d events before the flush", n)
	}
	for range 3 {
		l.Flush(ctx)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted events, want 1", n)
	}
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("after the announcement: %v", ref)
	}
	l.Flush(ctx)
	if err := RenderBudget(ctx, h.st, soft, h.clock()); err != nil {
		t.Fatal(err)
	}
	if soft.Status.State != v1.BudgetExhausted || soft.Status.Spent.String() != "0.06" || soft.Status.Remaining.String() != "0" || !soft.Status.ResetsAt.Equal(h.clock().Add(time.Hour)) || *soft.Status.Keys != 1 {
		t.Fatalf("budget status = %+v", soft.Status)
	}
	if err := RenderKey(ctx, h.st, k, h.clock()); err != nil || k.Status.State != v1.KeyActive {
		t.Fatalf("a soft Budget exhausted its Key: %s, %v", k.Status.State, err)
	}
	h.advance(time.Hour)
	if err := RenderBudget(ctx, h.st, soft, h.clock()); err != nil || soft.Status.State != v1.BudgetOpen || *soft.Status.Spent != 0 {
		t.Fatalf("after the reset: %+v, %v", soft.Status, err)
	}
	// A watched window that reset without reaching its amount is dropped
	// at the next flush and announces nothing.
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	h.advance(time.Hour)
	l.Flush(ctx)
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d events after a window that never filled", n)
	}
}

// TestBudgetLifecycle: raising an exhausted Budget's amount serves at
// the next request once the journal row is consumed; status.keys,
// spent, remaining, resetsAt, and state render from the live Keys and
// the current window's counter. The delete refusal is spec 011's.
func TestBudgetLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	cache := h.cache(st, reg, time.Hour)
	l := h.limiter(st, cache, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	b := h.budget(t, "team", "0.02", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "one", draws(b))
	h.key(t, "two", draws(b))
	h.key(t, "elsewhere", nil)
	for range 2 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatal(ref)
		}
	}
	ref := spendOne(t, l, k, p)
	if ref == nil || ref.Code != gateway.CodeBudgetExhausted || ref.RetryAfter != time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Sub(h.clock()) {
		t.Fatalf("the third cent = %+v", ref)
	}
	l.Flush(ctx)
	if err := RenderBudget(ctx, st, b, h.clock()); err != nil {
		t.Fatal(err)
	}
	if *b.Status.Keys != 2 || b.Status.Spent.String() != "0.02" || b.Status.Remaining.String() != "0" || b.Status.State != v1.BudgetExhausted || !b.Status.ResetsAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("budget status = %+v", b.Status)
	}
	if err := RenderKey(ctx, st, k, h.clock()); err != nil || k.Status.State != v1.KeyExhausted {
		t.Fatalf("the Key under the exhausted Budget: %s, %v", k.Status.State, err)
	}
	// The amount is raised: the cached Budget serves until the journal
	// row is consumed, then the next request runs.
	amount := 5 * cent
	b.Spec.Amount = &amount
	if _, err := h.st.Objects().Put(ctx, b, b.Status.Version); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventBudgetUpdated, b)
	if ref := spendOne(t, l, k, p); ref == nil {
		t.Fatal("the raised amount was seen before the tail")
	}
	cache.Tail(ctx)
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("after the raise: %v", ref)
	}
	l.Flush(ctx)
	if err := RenderBudget(ctx, st, b, h.clock()); err != nil || b.Status.State != v1.BudgetOpen || b.Status.Spent.String() != "0.03" || b.Status.Remaining.String() != "0.02" {
		t.Fatalf("after the raise: %+v, %v", b.Status, err)
	}
	if err := RenderKey(ctx, st, k, h.clock()); err != nil || k.Status.State != v1.KeyActive {
		t.Fatalf("the Key after the raise: %s, %v", k.Status.State, err)
	}
	// A none Budget renders no reset.
	forever := h.budget(t, "forever", "1", "USD", v1.WindowNone, true)
	if err := RenderBudget(ctx, st, forever, h.clock()); err != nil || !forever.Status.ResetsAt.IsZero() || *forever.Status.Keys != 0 || forever.Status.Remaining.String() != "1" {
		t.Fatalf("a none Budget = %+v, %v", forever.Status, err)
	}
}
