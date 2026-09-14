// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// TestKeyStates: each state is decided from the facts, Disabled from the
// spec, Expired from the clock on every replica at once, Exhausted from
// the current window of the Key or of its hard Budget and cleared by
// the reset without a write, and a soft Budget exhausts no Key; the
// Limiter refuses each exhausted window with its code.
func TestKeyStates(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	p := priced("p", "0.01", "0", "USD")
	hard := h.budget(t, "hard", "0.01", "USD", "1h", true)
	soft := h.budget(t, "soft", "0.01", "USD", "1h", false)
	expiry := h.clock().Add(time.Hour)
	for _, c := range []struct {
		name   string
		edit   func(k *v1.Key)
		spend  int          // one-cent requests admitted before the read
		want   v1.KeyState  // at the read
		refuse gateway.Code // the Limiter's answer to one more request, "" for admitted
		later  v1.KeyState  // an hour later, after the window reset or the expiry
	}{
		{"active", nil, 0, v1.KeyActive, "", v1.KeyActive},
		{"disabled", func(k *v1.Key) { k.Spec.Disabled = true }, 0, v1.KeyDisabled, "", v1.KeyDisabled},
		{"expires later", func(k *v1.Key) { k.Status.ExpiresAt = expiry }, 0, v1.KeyActive, "", v1.KeyExpired},
		{"expired", func(k *v1.Key) { k.Status.ExpiresAt = h.clock() }, 0, v1.KeyExpired, "", v1.KeyExpired},
		{"exhausted window", spends("0.01", "USD", "1h"), 1, v1.KeyExhausted, gateway.CodeSpendExceeded, v1.KeyActive},
		{"exhausted hard budget", draws(hard), 1, v1.KeyExhausted, gateway.CodeBudgetExhausted, v1.KeyActive},
		{"soft budget reached", draws(soft), 1, v1.KeyActive, "", v1.KeyActive},
		// The door refuses key_disabled and key_expired at stage 3; a
		// request that reached stage 7 anyway meets the full window.
		{"disabled before exhausted", edits(func(k *v1.Key) { k.Spec.Disabled = true }, spends("0.01", "USD", "1h")), 1, v1.KeyDisabled, gateway.CodeSpendExceeded, v1.KeyDisabled},
		{"expired before exhausted", edits(func(k *v1.Key) { k.Status.ExpiresAt = h.clock() }, spends("0.01", "USD", "1h")), 1, v1.KeyExpired, gateway.CodeSpendExceeded, v1.KeyExpired},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.now = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
			l := h.limiter(st, nil, manifest.Defaults{})
			k, _ := h.key(t, strings.ReplaceAll(c.name, " ", "-"), c.edit)
			for range c.spend {
				if ref := spendOne(t, l, k, p); ref != nil {
					t.Fatal(ref)
				}
			}
			l.Flush(ctx)
			// Two replicas render the same answer from the store and the clock.
			for range 2 {
				if err := RenderKey(ctx, st, k, h.clock()); err != nil || k.Status.State != c.want {
					t.Fatalf("state = %s, %v; want %s", k.Status.State, err, c.want)
				}
			}
			_, ref := reserve(t, l, k, p, 1, 0)
			switch {
			case c.refuse == "" && ref != nil:
				t.Fatalf("refused %s", ref.Code)
			case c.refuse != "" && (ref == nil || ref.Code != c.refuse):
				t.Fatalf("refusal = %+v, want %s", ref, c.refuse)
			}
			puts := ops(reg, "Objects.Put")
			h.advance(time.Hour)
			if err := RenderKey(ctx, st, k, h.clock()); err != nil || k.Status.State != c.later {
				t.Fatalf("an hour later: %s, %v; want %s", k.Status.State, err, c.later)
			}
			if ops(reg, "Objects.Put") != puts {
				t.Fatal("the reset or the expiry wrote the object")
			}
		})
	}
}

// TestKeyStatusRendersFromCounters: status.usage.window and .total
// render the counters' requests, tokens, and spend with spend as a money
// string, window is absent for a Key without a spend limit and carries
// resetsAt with one, and the Limiter's reserve and settle are what the
// counters sum to.
func TestKeyStatusRendersFromCounters(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	k, _ := h.key(t, "windowed", spends("1", "USD", "1h"))
	add := func(scope metering.Scope, w v1.Window, n int64) {
		var expires time.Time
		if w != v1.WindowNone {
			_, expires = metering.Window(w, h.clock(), k.Status.CreatedAt)
		}
		if _, err := h.st.Counters().Add(ctx, metering.CounterKey(scope, k.Status.ID, w, h.clock(), k.Status.CreatedAt), n, expires); err != nil {
			t.Fatal(err)
		}
	}
	add(metering.ScopeKeyRequests, v1.WindowNone, 7)
	add(metering.ScopeKeyTokens, v1.WindowNone, 1234)
	add(metering.ScopeKeySpend, v1.WindowNone, 123_456)
	add(metering.ScopeKeyRequests, "1h", 2)
	add(metering.ScopeKeyTokens, "1h", 300)
	add(metering.ScopeKeySpend, "1h", 20_000)
	if err := RenderKey(ctx, h.st, k, h.clock()); err != nil {
		t.Fatal(err)
	}
	u := k.Status.Usage
	if u == nil || u.Total.Requests != 7 || u.Total.Tokens != 1234 || u.Total.Spend.String() != "0.123456" {
		t.Fatalf("total = %+v", u)
	}
	if u.Window == nil || u.Window.Requests != 2 || u.Window.Tokens != 300 || u.Window.Spend.String() != "0.02" || !u.Window.ResetsAt.Equal(h.clock().Add(time.Hour)) {
		t.Fatalf("window = %+v", u.Window)
	}
	body, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"spend":"0.02"`) || !strings.Contains(string(body), `"spend":"0.123456"`) {
		t.Fatalf("usage JSON = %s", body)
	}
	// Without a spend limit: totals only, no window member.
	plain, _ := h.key(t, "plain", nil)
	if err := RenderKey(ctx, h.st, plain, h.clock()); err != nil || plain.Status.Usage == nil || plain.Status.Usage.Window != nil {
		t.Fatalf("usage without a spend limit = %+v, %v", plain.Status.Usage, err)
	}
	if body, _ := json.Marshal(plain.Status.Usage); strings.Contains(string(body), "window") || !strings.Contains(string(body), `"total":{"requests":0,"tokens":0,"spend":"0"}`) {
		t.Fatalf("usage JSON without a spend limit = %s", body)
	}
	// Through the Limiter: one request of 40 tokens at a cent each on
	// input, settled at 30 in and 10 out, is one request, 40 tokens, and
	// thirty cents in the window and the totals alike, after the flush.
	l := h.limiter(h.st, nil, manifest.Defaults{})
	p := priced("p", "0.01", "0", "USD")
	lease, ref := reserve(t, l, k, p, 40, 0)
	if ref != nil {
		t.Fatal(ref)
	}
	lease.Settle(ctx, gateway.Tokens{Input: 30, Output: 10})
	l.Flush(ctx)
	if err := RenderKey(ctx, h.st, k, h.clock()); err != nil {
		t.Fatal(err)
	}
	u = k.Status.Usage
	if u.Window.Requests != 3 || u.Window.Tokens != 340 || u.Window.Spend.String() != "0.32" || u.Total.Requests != 8 || u.Total.Tokens != 1274 || u.Total.Spend.String() != "0.423456" {
		t.Fatalf("after one measured request: window %+v, total %+v", u.Window, u.Total)
	}
	// A none spend window is the totals themselves, counted once.
	none, _ := h.key(t, "none", spends("1", "USD", v1.WindowNone))
	lease, _ = reserve(t, l, none, p, 5, 0)
	lease.Settle(ctx, gateway.Tokens{Input: 5})
	l.Flush(ctx)
	if err := RenderKey(ctx, h.st, none, h.clock()); err != nil {
		t.Fatal(err)
	}
	if u := none.Status.Usage; u.Window == nil || u.Window.Requests != 1 || u.Window.Spend.String() != "0.05" || u.Total.Requests != 1 || u.Total.Spend.String() != "0.05" || !u.Window.ResetsAt.IsZero() {
		t.Fatalf("a none window = %+v %+v", u.Window, u.Total)
	}
}

// TestLastUsedIsCoalesced: status.lastUsedAt is written at most once per
// minute per used Key, rounded down to the minute, by the flush.
func TestLastUsedIsCoalesced(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	st, reg := h.instrumented()
	l := h.limiter(st, nil, manifest.Defaults{})
	m := unpriced("m")
	h.now = time.Date(2026, 9, 14, 10, 0, 5, 0, time.UTC)
	k, _ := h.key(t, "busy", nil)
	other, _ := h.key(t, "other", nil)
	read := func(k *v1.Key) time.Time {
		obj, _, err := st.Objects().Get(ctx, v1.KindKey, k.Status.ID)
		if err != nil {
			t.Fatal(err)
		}
		return obj.(*v1.Key).Status.LastUsedAt
	}
	for range 50 {
		if _, ref := reserve(t, l, k, m, 1, 1); ref != nil {
			t.Fatal(ref)
		}
		h.advance(500 * time.Millisecond) // fifty requests over twenty-five seconds
	}
	if !read(k).IsZero() || ops(reg, "Objects.PutStatus") != 0 {
		t.Fatal("lastUsedAt was written before the flush")
	}
	for range 3 {
		l.Flush(ctx)
	}
	if got := read(k); !got.Equal(time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("lastUsedAt = %s", got)
	}
	if ops(reg, "Objects.PutStatus") != 1 {
		t.Fatalf("fifty requests in one minute cost %d status writes", ops(reg, "Objects.PutStatus"))
	}
	// Another request in the same minute: no write.
	h.now = time.Date(2026, 9, 14, 10, 0, 59, 0, time.UTC)
	reserve(t, l, k, m, 1, 1)
	l.Flush(ctx)
	if ops(reg, "Objects.PutStatus") != 1 {
		t.Fatal("a request in the same minute wrote again")
	}
	// The next minute, and another Key: one write each.
	h.now = time.Date(2026, 9, 14, 10, 1, 2, 0, time.UTC)
	reserve(t, l, k, m, 1, 1)
	reserve(t, l, other, m, 1, 1)
	l.Flush(ctx)
	if ops(reg, "Objects.PutStatus") != 3 || !read(k).Equal(time.Date(2026, 9, 14, 10, 1, 0, 0, time.UTC)) || !read(other).Equal(time.Date(2026, 9, 14, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("after the next minute: %d writes, %s, %s", ops(reg, "Objects.PutStatus"), read(k), read(other))
	}
	// A Key deleted between its use and the flush is no error.
	gone, _ := h.key(t, "gone", nil)
	reserve(t, l, gone, m, 1, 1)
	if err := h.st.Objects().Delete(ctx, v1.KindKey, gone.Status.ID); err != nil {
		t.Fatal(err)
	}
	l.Flush(ctx)
	if strings.Contains(h.logged(), "level=ERROR") {
		t.Fatalf("a deleted Key's lastUsedAt was an error:\n%s", h.logged())
	}
	// An hour on, the record of what was written is forgotten and the
	// Key is stamped again at its next use.
	h.advance(2 * time.Hour)
	l.Flush(ctx)
	reserve(t, l, k, m, 1, 1)
	l.Flush(ctx)
	if !read(k).Equal(h.clock().Truncate(time.Minute)) {
		t.Fatalf("after an hour: %s", read(k))
	}
}

// TestLimiterRunAndFailures: Run flushes on its interval and once at
// stop; every store failure is logged and leaves the arithmetic sound: a
// failed flush keeps the deltas, a failed marker still refuses, a failed
// event append is logged, a failed status write is logged, and a Budget
// the store cannot answer is the store's error and not a refusal.
func TestLimiterRunAndFailures(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := priced("p", "0.01", "0", "USD")
	k, _ := h.key(t, "run", spends("1", "USD", "1h"))
	spendKey := metering.CounterKey(metering.ScopeKeySpend, k.Status.ID, "1h", h.clock(), k.Status.CreatedAt)
	l := NewLimiter(LimiterOptions{Store: h.st, Flush: 10 * time.Millisecond, Logger: h.logger, Now: h.clock})
	run(t, l.Run)
	if ref := spendOne(t, l, k, p); ref != nil {
		t.Fatal(ref)
	}
	waitUntil(t, "the interval flush", func() bool { return h.counter(t, spendKey) == int64(cent) })

	if bare := NewLimiter(LimiterOptions{Store: h.st}); bare.o.Flush != metering.DefaultFlush || bare.o.NewID == nil || bare.o.Logger == nil || bare.o.Now == nil {
		t.Fatalf("defaults = %+v", bare.o)
	}
	fail := map[string]bool{}
	st := &broken{Store: h.st, fail: fail}
	l = h.limiter(st, nil, manifest.Defaults{})
	// A flush the store refuses keeps the delta for the next one.
	spendOne(t, l, k, p)
	fail["Counters.Add"] = true
	l.Flush(ctx)
	if !strings.Contains(h.logged(), "limits: flushing the spend counters") || l.counters.Pending(spendKey) != int64(cent) {
		t.Fatalf("a refused flush: pending %d, log:\n%s", l.counters.Pending(spendKey), h.logged())
	}
	fail["Counters.Add"] = false
	l.Flush(ctx)
	if h.counter(t, spendKey) != int64(2*cent) || l.counters.Pending(spendKey) != 0 {
		t.Fatalf("the kept delta did not flush: store %d, pending %d", h.counter(t, spendKey), l.counters.Pending(spendKey))
	}
	// A marker the store refuses is logged, and the refusal stands.
	tiny, _ := h.key(t, "tiny", spends("0.01", "USD", "1h"))
	spendOne(t, l, tiny, p)
	fail["Counters.AddMarker"] = true
	if ref := spendOne(t, l, tiny, p); ref == nil || ref.Code != gateway.CodeSpendExceeded {
		t.Fatalf("with a broken marker = %+v", ref)
	}
	if !strings.Contains(h.logged(), "limits: claiming the exhaustion marker") || len(h.events(t, eventKeyExhausted)) != 0 {
		t.Fatalf("a broken marker: log:\n%s", h.logged())
	}
	fail["Counters.AddMarker"] = false
	// An event the journal refuses is logged; the marker is claimed, so
	// the event of this window is lost, which spec 012 says.
	fail["Journal.Append"] = true
	spendOne(t, l, tiny, p)
	if !strings.Contains(h.logged(), "limits: raising the event") || len(h.events(t, eventKeyExhausted)) != 0 {
		t.Fatalf("a broken journal: log:\n%s", h.logged())
	}
	fail["Journal.Append"] = false
	// The soft Budget's announcement fails the same way and is logged.
	soft := h.budget(t, "soft", "0.01", "USD", "1h", false)
	softly, _ := h.key(t, "softly", draws(soft))
	spendOne(t, l, softly, p)
	fail["Counters.AddMarker"] = true
	l.Flush(ctx)
	fail["Counters.AddMarker"] = false
	if strings.Count(h.logged(), "limits: claiming the exhaustion marker") != 2 {
		t.Fatalf("the soft marker failure was not logged:\n%s", h.logged())
	}
	// A status write the store refuses is logged.
	h.advance(time.Minute)
	fail["Objects.PutStatus"] = true
	spendOne(t, l, k, p)
	l.Flush(ctx)
	if !strings.Contains(h.logged(), "limits: writing lastUsedAt") {
		t.Fatalf("a broken status write:\n%s", h.logged())
	}
	fail["Objects.PutStatus"] = false
	// A Budget the store cannot answer is its error, not a refusal; a
	// Budget that is gone is no Budget.
	hard := h.budget(t, "hard", "1", "USD", "1h", true)
	drawing, _ := h.key(t, "drawing", draws(hard))
	fail["Objects.Get"] = true
	_, err := l.Reserve(ctx, gateway.Reservation{Key: drawing, Model: p, InputTokens: 1})
	var ref *gateway.Refusal
	if !errors.Is(err, errBroken) || errors.As(err, &ref) || !strings.Contains(err.Error(), "Budget of Key "+drawing.Status.ID) {
		t.Fatalf("a broken Budget read = %v", err)
	}
	fail["Objects.Get"] = false
	if err := h.st.Objects().Delete(ctx, v1.KindBudget, hard.Status.ID); err != nil {
		t.Fatal(err)
	}
	if _, ref := reserve(t, l, drawing, p, 1, 0); ref != nil {
		t.Fatalf("a Key whose Budget is gone was refused: %v", ref)
	}
	// Through the cache, the same answers.
	cache := NewKeyCache(KeyCacheOptions{Store: st, TTL: time.Hour, Now: h.clock})
	l = h.limiter(st, cache, manifest.Defaults{})
	fail["Objects.Get"] = true
	if _, err := l.Reserve(ctx, gateway.Reservation{Key: drawing, Model: p, InputTokens: 1}); !errors.Is(err, errBroken) {
		t.Fatalf("a broken Budget read through the cache = %v", err)
	}
	fail["Objects.Get"] = false
	if _, err := l.Reserve(ctx, gateway.Reservation{}); err == nil || !strings.Contains(err.Error(), "without a Key") {
		t.Fatalf("a reservation without a Key = %v", err)
	}
	// The renderers report the store's failures.
	fail["Counters.Read"] = true
	if err := RenderKey(ctx, st, k, h.clock()); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "counters of Key") {
		t.Fatalf("RenderKey with broken counters = %v", err)
	}
	if err := RenderBudget(ctx, st, soft, h.clock()); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "counters of Budget") {
		t.Fatalf("RenderBudget with broken counters = %v", err)
	}
	fail["Counters.Read"] = false
	fail["Objects.List"] = true
	if err := RenderBudget(ctx, st, soft, h.clock()); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "Keys of Budget") {
		t.Fatalf("RenderBudget with a broken list = %v", err)
	}
	fail["Objects.List"] = false
	fail["Objects.Get"] = true
	if err := RenderKey(ctx, st, softly, h.clock()); !errors.Is(err, errBroken) || !strings.Contains(err.Error(), "Budget of Key") {
		t.Fatalf("RenderKey with a broken Budget read = %v", err)
	}
	fail["Objects.Get"] = false
	// A Key whose Budget is gone renders from its own counters.
	if err := RenderKey(ctx, st, drawing, h.clock()); err != nil || drawing.Status.State != v1.KeyActive {
		t.Fatalf("a Key whose Budget is gone = %s, %v", drawing.Status.State, err)
	}
	// A Budget without an amount renders nothing remaining.
	bare := &v1.Budget{Metadata: v1.ObjectMeta{Name: "bare"}, Status: v1.BudgetStatus{ID: v1.NewID(v1.PrefixBudget, h.clock(), nil), Owner: subject, Warnings: []string{}}}
	if err := RenderBudget(ctx, h.st, bare, h.clock()); err != nil || bare.Status.Remaining.String() != "0" || bare.Status.State != v1.BudgetExhausted {
		t.Fatalf("a Budget without an amount = %+v, %v", bare.Status, err)
	}
	if strings.Contains(h.logged(), "level=ERROR") && strings.Contains(h.logged(), "lux_") {
		t.Fatal("a Key value reached the log")
	}
}

// TestCostOf: the cost arithmetic spec 009 states, as this package holds
// it until metering.Cost lands: money per Per tokens, one rounding half
// up over the whole sum, cached and cache-write tokens at their prices.
func TestCostOf(t *testing.T) {
	money := func(s string) *v1.Money {
		m, err := v1.ParseMoney(s)
		if err != nil {
			t.Fatal(err)
		}
		return &m
	}
	per := &v1.Pricing{Currency: "USD", Per: 1_000_000, Input: money("2"), Output: money("8"), CachedInput: money("0.5"), CacheWrite: money("2.5")}
	for _, c := range []struct {
		tokens gateway.Tokens
		p      *v1.Pricing
		want   string
	}{
		{gateway.Tokens{Input: 1000, Output: 500}, per, "0.006"},
		{gateway.Tokens{Input: 1000, CachedInput: 1000, CacheWrite: 1000}, per, "0.005"},
		{gateway.Tokens{Input: 1}, per, "0.000002"},
		{gateway.Tokens{Input: 1, Output: 1}, &v1.Pricing{Per: 1000, Input: money("0.0005"), Output: money("0.0005")}, "0.000001"}, // half up over the sum
		{gateway.Tokens{Input: 3}, &v1.Pricing{Input: money("1")}, "3"},                                                            // Per unset is per token
		{gateway.Tokens{Input: 3}, &v1.Pricing{Per: 1}, "0"},                                                                       // no prices set
	} {
		if got := costOf(c.tokens, c.p); got.String() != c.want {
			t.Errorf("costOf(%+v) = %s, want %s", c.tokens, got, c.want)
		}
	}
}

var _ store.Store = (*broken)(nil)
