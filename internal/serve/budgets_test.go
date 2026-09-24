// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// drawsAll is the edit that makes a Key list every one of bs (spec 037).
func drawsAll(bs ...*v1.Budget) func(k *v1.Key) {
	return func(k *v1.Key) {
		for _, b := range bs {
			k.Spec.Budgets = append(k.Spec.Budgets, b.Metadata.Name)
			k.Status.Budgets = append(k.Status.Budgets, v1.BudgetRef{Name: b.Metadata.Name, ID: b.Status.ID})
		}
	}
}

// update writes b's new spec and the budget.updated row, and lets the
// cache read it.
func (h *harness) update(t *testing.T, cache *KeyCache, b *v1.Budget) {
	t.Helper()
	obj, version, err := h.st.Objects().Get(t.Context(), v1.KindBudget, b.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur := obj.(*v1.Budget)
	cur.Spec = b.Spec
	if _, err := h.st.Objects().Put(t.Context(), cur, version); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventBudgetUpdated, cur)
	cache.Tail(t.Context())
}

// TestSeveralBudgets is spec 037's stage 7: a Key listing a weekly and a
// monthly Budget is refused budget_exhausted when either would pass its
// amount, the detail naming only the refusing one; an admitted request
// adds its estimate and its settled cost to both; and the Key renders
// Exhausted while any hard Budget it lists is at its amount.
func TestSeveralBudgets(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	cache := h.cache(h.st, nil, time.Hour)
	l := h.limiter(h.st, cache, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	week := h.budget(t, "week", "0.02", "USD", "168h", true)
	month := h.budget(t, "month", "0.05", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "member", drawsAll(month, week))
	for range 2 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatal(ref)
		}
	}
	ref := spendOne(t, l, k, p)
	if ref == nil || ref.Code != gateway.CodeBudgetExhausted || !strings.Contains(ref.Detail, "Budget week") || strings.Contains(ref.Detail, "Budget month") {
		t.Fatalf("the third cent = %+v", ref)
	}
	_, weekReset := metering.BudgetWindow(week, h.clock())
	if ref.RetryAfter != weekReset.Sub(h.clock()) {
		t.Fatalf("Retry-After = %s, want the week's reset in %s", ref.RetryAfter, weekReset.Sub(h.clock()))
	}
	l.Flush(ctx)
	for _, b := range []*v1.Budget{week, month} {
		if got := h.counter(t, metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, h.clock())); got != int64(2*cent) {
			t.Fatalf("Budget %s holds %d, want the two cents admitted", b.Metadata.Name, got)
		}
	}
	if err := RenderKey(ctx, h.st, k, h.clock()); err != nil || k.Status.State != v1.KeyExhausted {
		t.Fatalf("the Key under an exhausted week = %s, %v", k.Status.State, err)
	}
	if err := RenderBudget(ctx, h.st, week, h.clock()); err != nil || *week.Status.Keys != 1 {
		t.Fatalf("the week's keys = %v, %v", week.Status.Keys, err)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted rows, want the week's one", n)
	}
	// Two weeks on, still in September, the month holds four cents and
	// the week one: the next cent fits the week and not the month, which
	// refuses alone.
	h.advance(7 * 24 * time.Hour)
	for range 2 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatalf("the second week: %+v", ref)
		}
	}
	h.advance(7 * 24 * time.Hour)
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("the third week: %+v", ref)
	}
	ref = spendOne(t, l, k, p)
	if ref == nil || !strings.Contains(ref.Detail, "Budget month") || strings.Contains(ref.Detail, "Budget week") {
		t.Fatalf("the month's refusal = %+v", ref)
	}
}

// TestSeveralBudgetsRetryAfter: when several Budgets refuse at once each
// announces its own exhaustion, Retry-After is the latest reset among
// them, and it is absent when one of them never resets.
func TestSeveralBudgetsRetryAfter(t *testing.T) {
	h := newHarness(t)
	cache := h.cache(h.st, nil, time.Hour)
	l := h.limiter(h.st, cache, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	hour := h.budget(t, "hour", "0.01", "USD", "1h", true)
	week := h.budget(t, "week", "0.01", "USD", "168h", true)
	life := h.budget(t, "life", "0.01", "USD", v1.WindowNone, true)
	both, _ := h.key(t, "both", drawsAll(hour, week))
	if ref := spendOne(t, l, both, p); ref != nil {
		t.Fatal(ref)
	}
	ref := spendOne(t, l, both, p)
	_, weekReset := metering.BudgetWindow(week, h.clock())
	if ref == nil || ref.RetryAfter != weekReset.Sub(h.clock()) || !strings.Contains(ref.Detail, "Budget hour") || !strings.Contains(ref.Detail, "Budget week") {
		t.Fatalf("two refusing Budgets = %+v", ref)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 2 {
		t.Fatalf("%d budget.exhausted rows, want one per refusing Budget", n)
	}
	lifelong, _ := h.key(t, "lifelong", drawsAll(hour, life))
	h.advance(time.Hour)
	if ref := spendOne(t, l, lifelong, p); ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, lifelong, p); ref == nil || ref.RetryAfter != 0 {
		t.Fatalf("with a lifetime Budget refusing, Retry-After = %+v", ref)
	}
}

// TestBudgetAmountRaisedRearms: a Budget exhausted, raised, and exhausted
// again inside one window announces twice, once per amount.
func TestBudgetAmountRaisedRearms(t *testing.T) {
	h := newHarness(t)
	cache := h.cache(h.st, nil, time.Hour)
	l := h.limiter(h.st, cache, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	b := h.budget(t, "team", "0.01", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "k", drawsAll(b))
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, k, p); ref == nil {
		t.Fatal("the second cent was admitted")
	}
	raised := 2 * cent
	b.Spec.Amount = &raised
	h.update(t, cache, b)
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("after the raise: %+v", ref)
	}
	if ref := spendOne(t, l, k, p); ref == nil {
		t.Fatal("the raised amount was passed")
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 2 {
		t.Fatalf("%d budget.exhausted rows, want one per amount", n)
	}
}

// TestBudgetRestart is spec 037's restartedAt: inside the current window
// and not in the future it restarts the spend at zero with the reset
// unchanged, reopens an exhausted Budget, and re-arms its marker; a
// request admitted before the restart settles into the old window; a
// restart before the window has no effect; under none the lifetime
// restarts.
func TestBudgetRestart(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	cache := h.cache(h.st, nil, time.Hour)
	l := h.limiter(h.st, cache, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	b := h.budget(t, "team", "0.02", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "k", drawsAll(b))
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	inFlight, ref := reserve(t, l, k, p, 1, 0)
	if ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, k, p); ref == nil || ref.Code != gateway.CodeBudgetExhausted {
		t.Fatalf("the third cent = %+v", ref)
	}
	_, reset := metering.BudgetWindow(b, h.clock())
	oldKey := metering.BudgetCounterKey(metering.ScopeBudgetSpend, b, h.clock())
	h.advance(time.Minute)
	b.Spec.RestartedAt = h.clock()
	h.update(t, cache, b)
	inFlight.Settle(ctx, gateway.Tokens{Input: 1})
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("after the restart: %+v", ref)
	}
	l.Flush(ctx)
	if got := h.counter(t, oldKey); got != int64(2*cent) {
		t.Fatalf("the old window holds %d, want the two cents admitted before the restart", got)
	}
	if err := RenderBudget(ctx, h.st, b, h.clock()); err != nil || b.Status.Spent.String() != "0.01" || !b.Status.ResetsAt.Equal(reset) || b.Status.State != v1.BudgetOpen {
		t.Fatalf("after the restart = %+v, %v", b.Status, err)
	}
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, k, p); ref == nil {
		t.Fatal("the restarted window was passed")
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 2 {
		t.Fatalf("%d budget.exhausted rows, want one before and one after the restart", n)
	}
	// A restart before the current window changes nothing.
	early := h.budget(t, "early", "0.05", "USD", "1h", true)
	early.Spec.RestartedAt = h.clock().Add(-2 * time.Hour)
	if got, want := metering.BudgetCounterKey(metering.ScopeBudgetSpend, early, h.clock()), metering.CounterKey(metering.ScopeBudgetSpend, early.Status.ID, "1h", h.clock(), early.Status.CreatedAt); got != want {
		t.Fatalf("a restart before the window moved the key to %s", got)
	}
	// Under none the lifetime counts from the restart.
	life := h.budget(t, "life", "0.01", "USD", v1.WindowNone, true)
	lk, _ := h.key(t, "life", drawsAll(life))
	if ref := spendOne(t, l, lk, p); ref != nil {
		t.Fatal(ref)
	}
	if ref := spendOne(t, l, lk, p); ref == nil {
		t.Fatal("the lifetime Budget was passed")
	}
	h.advance(time.Minute)
	life.Spec.RestartedAt = h.clock()
	h.update(t, cache, life)
	if ref := spendOne(t, l, lk, p); ref != nil {
		t.Fatalf("after the lifetime restart: %+v", ref)
	}
}

// TestAnchoredBudget: a weekly Budget anchored to a Monday resets on the
// next Monday, not on the epoch's Thursday, and renders that reset.
func TestAnchoredBudget(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	cache := h.cache(h.st, nil, time.Hour)
	l := h.limiter(h.st, cache, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	b := h.budget(t, "week", "0.01", "USD", "168h", true)
	monday := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	b.Spec.Anchor = monday
	h.update(t, cache, b)
	k, _ := h.key(t, "k", drawsAll(b))
	h.advance(24 * time.Hour)
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	ref := spendOne(t, l, k, p)
	next := monday.Add(7 * 24 * time.Hour)
	if ref == nil || ref.RetryAfter != next.Sub(h.clock()) {
		t.Fatalf("the anchored refusal = %+v, want Retry-After %s", ref, next.Sub(h.clock()))
	}
	if err := RenderBudget(ctx, h.st, b, h.clock()); err != nil || !b.Status.ResetsAt.Equal(next) {
		t.Fatalf("resetsAt = %s, %v", b.Status.ResetsAt, err)
	}
	h.advance(next.Sub(h.clock()))
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatalf("on the next Monday: %+v", ref)
	}
}
