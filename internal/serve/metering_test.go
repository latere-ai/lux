// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The rows of spec 009 over the Limiter, named as that spec names them.
// Spec 007's tests of the same behaviour, TestOvershootBound,
// TestRateIsPerReplica, TestSpendWindow, TestSoftBudget, and
// TestExhaustionIsAnnouncedOnce, prove the mechanism in depth; these
// prove the figures and the statements 009's table makes.

// TestTwoReplicaOvershootIsBounded: the bound's numbers, twenty-one
// cents for three replicas at ten requests a second with a one-cent
// request and a one-second flush and one request for one replica, and
// two replicas driven against one hard limit over twenty runs never
// exceed it by more than (R − 1) × F × T × C + C.
func TestTwoReplicaOvershootIsBounded(t *testing.T) {
	if got := metering.Overshoot(3, time.Second, 10, cent); got != 21*cent {
		t.Fatalf("three replicas: %s, want 0.21", got)
	}
	if got := metering.Overshoot(1, time.Second, 10, cent); got != cent {
		t.Fatalf("one replica: %s, want one request", got)
	}
	const replicas, perSecond = 2, 10
	amount := 50 * cent
	bound := metering.Overshoot(replicas, time.Second, perSecond, cent)
	if bound != 11*cent {
		t.Fatalf("two replicas: %s, want 0.11", bound)
	}
	for run := range 20 {
		h := newHarness(t)
		ctx := t.Context()
		p := priced("p", "0.01", "0", "USD")
		k, _ := h.key(t, "half", spends("0.5", "USD", "24h"))
		ls := [replicas]*Limiter{h.limiter(h.st, nil, manifest.Defaults{}), h.limiter(h.st, nil, manifest.Defaults{})}
		rng := rand.New(rand.NewPCG(uint64(run), 3))
		refused := [replicas]bool{}
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
		if spent > amount+bound {
			t.Fatalf("run %d: spent %s against %s, over the bound of %s", run, spent, amount, bound)
		}
		if spent < amount {
			t.Fatalf("run %d: refused at %s, under the amount %s", run, spent, amount)
		}
	}
}

// TestRateWindowIsPerReplica: a rate limit admits replicas times its
// value across replicas and its value on one, because the buckets are
// each replica's own and never the store's.
func TestRateWindowIsPerReplica(t *testing.T) {
	h := newHarness(t)
	k, _ := h.key(t, "thirty", rates(30, 0))
	m := unpriced("m")
	one := h.limiter(h.st, nil, manifest.Defaults{})
	admitted := 0
	for range 40 {
		if _, ref := reserve(t, one, k, m, 1, 1); ref == nil {
			admitted++
		}
	}
	if admitted != 30 {
		t.Fatalf("one replica admitted %d, want 30", admitted)
	}
	ls := []*Limiter{h.limiter(h.st, nil, manifest.Defaults{}), h.limiter(h.st, nil, manifest.Defaults{}), h.limiter(h.st, nil, manifest.Defaults{})}
	admitted = 0
	for _, l := range ls {
		for range 40 {
			if _, ref := reserve(t, l, k, m, 1, 1); ref == nil {
				admitted++
			}
		}
	}
	if admitted != 90 {
		t.Fatalf("three replicas admitted %d, want 90", admitted)
	}
	if n, err := h.st.Counters().Read(t.Context(), []string{"requests:" + k.Status.ID}); err != nil || len(n) != 0 {
		t.Fatalf("a rate bucket reached the store: %v, %v", n, err)
	}
}

// TestLocalDeltaRefusesImmediately: a replica's own delta refuses before
// any flush, while the store still reads zero; the other replica, whose
// view is the store's, is admitted until the first flushes and it
// refreshes at its own, within one interval.
func TestLocalDeltaRefusesImmediately(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "nickel", spends("0.05", "USD", "1h"))
	a, b := h.limiter(h.st, nil, manifest.Defaults{}), h.limiter(h.st, nil, manifest.Defaults{})
	window := metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "1h", h.clock(), k.Status.CreatedAt)
	for i := range 5 {
		if ref := spendOne(t, a, k, p); ref != nil {
			t.Fatalf("a, request %d: %v", i+1, ref)
		}
	}
	if ref := spendOne(t, a, k, p); ref == nil || ref.Code != gateway.CodeSpendExceeded {
		t.Fatalf("a's sixth cent = %+v", ref)
	}
	if h.counter(t, window) != 0 {
		t.Fatal("a's delta reached the store before the flush")
	}
	// b reads the store, which knows nothing yet.
	if ref := spendOne(t, b, k, p); ref != nil {
		t.Fatalf("b before a's flush: %v", ref)
	}
	a.Flush(ctx)
	if got := h.counter(t, window); got != int64(5*cent) {
		t.Fatalf("after a's flush the store holds %d", got)
	}
	// b still reads its own view until its flush refreshes it.
	if ref := spendOne(t, b, k, p); ref != nil {
		t.Fatalf("b before its own flush: %v", ref)
	}
	b.Flush(ctx)
	if ref := spendOne(t, b, k, p); ref == nil || ref.Code != gateway.CodeSpendExceeded {
		t.Fatalf("b after one interval = %+v", ref)
	}
	if got := h.counter(t, window); got != int64(7*cent) {
		t.Fatalf("the store holds %d, want both replicas' cents", got)
	}
}

// TestSoftBudgetContinues: a soft Budget past its amount refuses nothing
// and emits budget.exhausted once per window, at the flush that observes
// it, and once more after the reset.
func TestSoftBudgetContinues(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	l := h.limiter(h.st, nil, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	soft := h.budget(t, "soft", "0.02", "USD", "1h", false)
	k, _ := h.key(t, "softly", draws(soft))
	for i := range 6 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatalf("request %d under a soft Budget: %v", i+1, ref)
		}
		l.Flush(ctx)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted events in one window, want 1", n)
	}
	h.advance(time.Hour)
	for range 3 {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatalf("after the reset: %v", ref)
		}
		l.Flush(ctx)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 2 {
		t.Fatalf("%d budget.exhausted events over two windows, want 2", n)
	}
}

// TestExhaustedMarkerIsClaimedOnce: six replicas that observe one
// window at its amount at once claim its marker, and the store's atomic
// add answers exactly one of them first; six Limiters refusing one Key
// raise one key.exhausted between them.
func TestExhaustedMarkerIsClaimedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	marker := metering.CounterKey(metering.ScopeKeyExhausted, "key_1", "1h", h.clock(), h.clock())
	var firsts sync.Map
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			first, err := metering.Claim(ctx, h.st.Counters(), marker, h.clock().Add(time.Hour))
			if err != nil {
				t.Error(err)
			}
			if first {
				firsts.Store(i, true)
			}
		})
	}
	wg.Wait()
	n := 0
	firsts.Range(func(any, any) bool { n++; return true })
	if n != 1 || h.counter(t, marker) != 6 {
		t.Fatalf("%d replicas were first of 6 claims (%d)", n, h.counter(t, marker))
	}
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "penny", spends("0.01", "USD", "1h"))
	ls := make([]*Limiter, 6)
	for i := range ls {
		ls[i] = h.limiter(h.st, nil, manifest.Defaults{})
	}
	// Each replica's first cent is admitted on its own empty view, which
	// is the one request of the bound's last term; the flush shows every
	// replica the window over its amount.
	for _, l := range ls {
		if ref := spendOne(t, l, k, p); ref != nil {
			t.Fatal(ref)
		}
	}
	for _, l := range ls {
		l.Flush(ctx)
	}
	for _, l := range ls {
		if ref := spendOne(t, l, k, p); ref == nil || ref.Code != gateway.CodeSpendExceeded {
			t.Fatalf("a replica admitted past the amount: %+v", ref)
		}
	}
	if n := len(h.events(t, eventKeyExhausted)); n != 1 {
		t.Fatalf("%d key.exhausted events from six refusing replicas, want 1", n)
	}
}
