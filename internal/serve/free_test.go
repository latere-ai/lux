// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// pricedAt is a Model priced per token at the four prices given, each a
// money string, in currency.
func pricedAt(name, currency, input, output, cachedInput, cacheWrite string) *v1.Model {
	m := priced(name, input, output, currency)
	for _, p := range []struct {
		to    **v1.Money
		value string
	}{{&m.Spec.Pricing.CachedInput, cachedInput}, {&m.Spec.Pricing.CacheWrite, cacheWrite}} {
		v, err := v1.ParseMoney(p.value)
		if err != nil {
			panic(err)
		}
		*p.to = &v
	}
	return m
}

// zeroPriced is a Model whose every price is zero.
func zeroPriced(name string) *v1.Model { return pricedAt(name, "USD", "0", "0", "0", "0") }

// overspend carries k's windows past their amount: one request whose
// one-cent estimate lands on a one-cent window is admitted, and settles
// measured at three cents, the overshoot a hard window allows.
func overspend(t *testing.T, l *Limiter, k *v1.Key, p *v1.Model) {
	t.Helper()
	lease, ref := reserve(t, l, k, p, 1, 0)
	if ref != nil {
		t.Fatalf("the request that crosses was refused: %+v", ref)
	}
	lease.Settle(t.Context(), gateway.Tokens{Input: 3})
}

// TestZeroPricedModelOnASpentBudget: a Budget spent past its amount
// admits a request for a Model whose every price is zero, counts the
// request and adds nothing to the Budget; the next request for a priced
// Model is refused budget_exhausted with the detail and Retry-After it
// had before; the Budget renders Exhausted and its Key Exhausted, which
// a zero-priced request is served through.
func TestZeroPricedModelOnASpentBudget(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	l := h.limiter(h.st, nil, manifest.Defaults{})
	p, free := priced("p", "0.01", "0", "USD"), zeroPriced("free")
	wallet := h.budget(t, "wallet", "0.01", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "person", draws(wallet))
	overspend(t, l, k, p)
	for i := range 3 {
		lease, ref := reserve(t, l, k, free, 1000, 1000)
		if ref != nil {
			t.Fatalf("zero-priced request %d on a spent Budget = %+v", i+1, ref)
		}
		lease.Settle(ctx, gateway.Tokens{Input: 1000, Output: 1000, CachedInput: 500, CacheWrite: 100})
	}
	_, ref := reserve(t, l, k, p, 1, 0)
	_, resetsAt := metering.BudgetWindow(wallet, h.clock())
	want := "Budget wallet has spent 0.03 of 0.01 USD in its month window, and this request is estimated at 0.01"
	if ref == nil || ref.Code != gateway.CodeBudgetExhausted || ref.Detail != want || ref.RetryAfter != metering.RetryAfter(h.clock(), resetsAt) {
		t.Fatalf("a priced request on the spent Budget = %+v", ref)
	}
	l.Flush(ctx)
	if got := h.counter(t, metering.BudgetCounterKey(metering.ScopeBudgetSpend, wallet, h.clock())); got != int64(3*cent) {
		t.Fatalf("the Budget's window holds %d, want the priced request's %d alone", got, 3*cent)
	}
	if got := h.counter(t, metering.TotalKey(metering.ScopeKeySpend, k.Status.ID)); got != int64(3*cent) {
		t.Fatalf("the Key's spend is %d, want %d", got, 3*cent)
	}
	if got := h.counter(t, metering.TotalKey(metering.ScopeKeyRequests, k.Status.ID)); got != 4 {
		t.Fatalf("the Key counted %d requests, want the priced one and the three zero-priced ones", got)
	}
	if got := h.counter(t, metering.TotalKey(metering.ScopeKeyTokens, k.Status.ID)); got != 3+3*2000 {
		t.Fatalf("the Key counted %d tokens", got)
	}
	if err := RenderBudget(ctx, h.st, wallet, h.clock()); err != nil || wallet.Status.State != v1.BudgetExhausted || wallet.Status.Spent.String() != "0.03" {
		t.Fatalf("the spent Budget renders %+v, %v", wallet.Status, err)
	}
	if err := RenderKey(ctx, h.st, k, h.clock()); err != nil || k.Status.State != v1.KeyExhausted {
		t.Fatalf("the Key renders %s, %v", k.Status.State, err)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted events, want the priced refusal's one", n)
	}
}

// TestZeroPricedModelPastASpendLimit: the Key's own spend window, spent
// past its amount, admits a zero-priced request the same way and refuses
// a priced one spend_exceeded as before.
func TestZeroPricedModelPastASpendLimit(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	l := h.limiter(h.st, nil, manifest.Defaults{})
	p, free := priced("p", "0.01", "0", "USD"), zeroPriced("free")
	k, _ := h.key(t, "capped", spends("0.01", "USD", "1h"))
	overspend(t, l, k, p)
	lease, ref := reserve(t, l, k, free, 1000, 1000)
	if ref != nil {
		t.Fatalf("a zero-priced request past the spend limit = %+v", ref)
	}
	lease.Settle(ctx, gateway.Tokens{Input: 1000, Output: 1000})
	_, ref = reserve(t, l, k, p, 1, 0)
	want := "Key " + k.Status.ID + " has spent 0.03 of 0.01 USD in its 1h window, and this request is estimated at 0.01"
	if ref == nil || ref.Code != gateway.CodeSpendExceeded || ref.Detail != want || ref.RetryAfter != time.Hour {
		t.Fatalf("a priced request past the spend limit = %+v", ref)
	}
	l.Flush(ctx)
	if got := h.counter(t, metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "1h", h.clock(), k.Status.CreatedAt)); got != int64(3*cent) {
		t.Fatalf("the spend window holds %d, want %d", got, 3*cent)
	}
	if got := h.counter(t, metering.CounterKey(metering.ScopeKeyRequests, k.Status.ID, "1h", h.clock(), k.Status.CreatedAt)); got != 2 {
		t.Fatalf("the spend window counted %d requests, want 2", got)
	}
}

// TestOnlyAModelPricedAtZeroPassesASpentBudget: a Model whose input and
// output are free but whose cached input or cache write is priced is a
// priced Model, an unpriced Model is not a free one even under
// allowUnpriced, and a zero-priced Model in another currency than the
// Budget's is still currency_mismatch; each is refused on a spent Budget
// as before.
func TestOnlyAModelPricedAtZeroPassesASpentBudget(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{})
	wallet := h.budget(t, "wallet", "0.01", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "person", draws(wallet))
	overspend(t, l, k, priced("p", "0.01", "0", "USD"))
	lenient, _ := h.key(t, "lenient", edits(draws(wallet), func(k *v1.Key) { k.Spec.AllowUnpriced = true }))
	for _, c := range []struct {
		name  string
		key   *v1.Key
		model *v1.Model
		want  gateway.Code
	}{
		{"a priced cached input", k, pricedAt("cached", "USD", "0", "0", "0.000001", "0"), gateway.CodeBudgetExhausted},
		{"a priced cache write", k, pricedAt("written", "USD", "0", "0", "0", "0.000001"), gateway.CodeBudgetExhausted},
		{"an unpriced Model under allowUnpriced", lenient, unpriced("bare"), gateway.CodeBudgetExhausted},
		{"a zero-priced Model in another currency", k, pricedAt("euro", "EUR", "0", "0", "0", "0"), gateway.CodeCurrencyMismatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ref := reserve(t, l, c.key, c.model, 1000, 1000)
			if ref == nil || ref.Code != c.want {
				t.Fatalf("refusal = %+v, want %s", ref, c.want)
			}
		})
	}
}

// TestRateLimitsHoldForAZeroPricedModel: a zero-priced request on a
// spent Budget still takes its request and its tokens from the Key's
// rate buckets, and is refused rate_limited when either is empty.
func TestRateLimitsHoldForAZeroPricedModel(t *testing.T) {
	h := newHarness(t)
	l := h.limiter(h.st, nil, manifest.Defaults{})
	free := zeroPriced("free")
	wallet := h.budget(t, "wallet", "0.01", "USD", v1.WindowMonth, true)
	spender, _ := h.key(t, "spender", draws(wallet))
	overspend(t, l, spender, priced("p", "0.01", "0", "USD"))
	one, _ := h.key(t, "one-a-minute", edits(rates(1, 0), draws(wallet)))
	if _, ref := reserve(t, l, one, free, 10, 10); ref != nil {
		t.Fatalf("the first zero-priced request = %+v", ref)
	}
	if _, ref := reserve(t, l, one, free, 10, 10); ref == nil || ref.Code != gateway.CodeRateLimited {
		t.Fatalf("the second zero-priced request in the minute = %+v", ref)
	}
	hundred, _ := h.key(t, "hundred-tokens", edits(rates(0, 100), draws(wallet)))
	if _, ref := reserve(t, l, hundred, free, 60, 40); ref != nil {
		t.Fatalf("a hundred tokens of a hundred = %+v", ref)
	}
	if _, ref := reserve(t, l, hundred, free, 1, 0); ref == nil || ref.Code != gateway.CodeRateLimited {
		t.Fatalf("a token past the minute's hundred = %+v", ref)
	}
}
