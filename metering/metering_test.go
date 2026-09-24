// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package metering

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

var (
	created = time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	at      = time.Date(2026, 9, 14, 10, 17, 42, 0, time.UTC)
)

// TestWindowEpochs: a duration window's key is the same for every
// instant inside it and differs across the boundary; month resets on
// the first in UTC; none never resets and starts at createdAt; the
// Retry-After of a refusal is the time to the reset and zero for none.
func TestWindowEpochs(t *testing.T) {
	start, resets := Window("1h", at, created)
	if !start.Equal(time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)) || !resets.Equal(start.Add(time.Hour)) {
		t.Fatalf("1h window = %s .. %s", start, resets)
	}
	inside := CounterKey(ScopeKeySpend, "key_1", "1h", at.Add(42*time.Minute), created)
	if inside != CounterKey(ScopeKeySpend, "key_1", "1h", at, created) {
		t.Errorf("two instants inside one window name two keys: %s", inside)
	}
	if next := CounterKey(ScopeKeySpend, "key_1", "1h", resets, created); next == inside {
		t.Errorf("the boundary did not move the key: %s", next)
	}
	mStart, mResets := Window(v1.WindowMonth, at, created)
	if !mStart.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || !mResets.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month = %s .. %s", mStart, mResets)
	}
	nStart, nResets := Window(v1.WindowNone, at, created)
	if !nStart.Equal(created) || !nResets.IsZero() {
		t.Errorf("none = %s .. %s", nStart, nResets)
	}
	if d := RetryAfter(at, resets); d != resets.Sub(at) {
		t.Errorf("RetryAfter to the reset = %s", d)
	}
	if RetryAfter(at, time.Time{}) != 0 || RetryAfter(at, at) != 0 || RetryAfter(at, at.Add(-time.Second)) != 0 {
		t.Error("RetryAfter of none or of a passed reset is not zero")
	}
}

// TestCounterKeyScheme: the key is <kind>:<id>:<counter>:<start unix
// seconds>, one per scope, and the totals' key carries none.
func TestCounterKeyScheme(t *testing.T) {
	for _, c := range []struct {
		scope Scope
		want  string
	}{
		{ScopeKeyRequests, "key:key_1:requests:1789380000"},
		{ScopeKeyTokens, "key:key_1:tokens:1789380000"},
		{ScopeKeySpend, "key:key_1:spend:1789380000"},
		{ScopeKeyExhausted, "key:key_1:exhausted:1789380000"},
		{ScopeBudgetSpend, "budget:key_1:spend:1789380000"},
		{ScopeBudgetExhausted, "budget:key_1:exhausted:1789380000"},
	} {
		if got := CounterKey(c.scope, "key_1", "1h", at, created); got != c.want {
			t.Errorf("%s = %s, want %s", c.scope, got, c.want)
		}
	}
	if got := CounterKey(ScopeKeySpend, "key_1", v1.WindowNone, at, created); got != "key:key_1:spend:none" || got != TotalKey(ScopeKeySpend, "key_1") {
		t.Errorf("the totals key = %s", got)
	}
	// A duration window that begins at the creation instant does not
	// share the totals' key.
	if CounterKey(ScopeKeySpend, "key_1", "1h", created, created) == TotalKey(ScopeKeySpend, "key_1") {
		t.Error("a window aligned to createdAt collides with the totals")
	}
	if got := CounterKey(ScopeKeySpend, "key_1", "1h", created, created); got != "key:key_1:spend:1788249600" {
		t.Errorf("an aligned window's key = %s", got)
	}
}

// TestReservationFigures: the reservation is the estimate plus the
// requested output, the settle is the measured pair, the adjustment is
// their difference, and a window refuses when the projection is over
// the amount and not when it lands on it.
func TestReservationFigures(t *testing.T) {
	if Reserved(100, DefaultOutputTokens) != 1124 || Reserved(0, 0) != 0 || Reserved(-5, 10) != 10 {
		t.Error("Reserved")
	}
	if Settled(120, 380) != 500 || Settled(0, 0) != 0 || Settled(-1, 3) != 3 {
		t.Error("Settled")
	}
	if Adjustment(1124, 500) != 624 || Adjustment(100, 500) != -400 || Adjustment(300, 0) != 300 {
		t.Error("Adjustment")
	}
	if Projected(50, 30, 20) != 100 {
		t.Error("Projected")
	}
	if Exceeds(100, 100) || !Exceeds(101, 100) || Exceeds(0, 0) {
		t.Error("Exceeds")
	}
}

// TestOvershootFormula: three replicas, a one-second flush, ten requests
// a second per replica, and a one-cent request bound the overshoot at
// twenty-one cents; one replica is bound by one request.
func TestOvershootFormula(t *testing.T) {
	cent := v1.Money(10_000)
	if got := Overshoot(3, time.Second, 10, cent); got != 21*cent {
		t.Errorf("three replicas: %s, want 0.21", got)
	}
	if got := Overshoot(1, time.Second, 10, cent); got != cent {
		t.Errorf("one replica: %s, want 0.01", got)
	}
	if got := Overshoot(0, time.Second, 10, cent); got != cent {
		t.Errorf("no replicas: %s", got)
	}
	if got := Overshoot(2, 500*time.Millisecond, 3, cent); got != v1.Money(15_000)+cent {
		t.Errorf("fractional interval: %s", got)
	}
}

// fakeStore is a CounterStore in a map, with one key it can be told to
// refuse and a count of every Add.
type fakeStore struct {
	mu      sync.Mutex
	totals  map[string]int64
	expires map[string]time.Time
	refuse  string
	adds    int
}

var errRefused = errors.New("the store refuses this key")

func newFakeStore() *fakeStore {
	return &fakeStore{totals: map[string]int64{}, expires: map[string]time.Time{}}
}

func (s *fakeStore) Add(_ context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adds++
	if key == s.refuse {
		return 0, errRefused
	}
	if _, ok := s.expires[key]; !ok {
		s.expires[key] = expiresAt
	}
	s.totals[key] += delta
	return s.totals[key], nil
}

func (s *fakeStore) Read(_ context.Context, keys []string) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	for _, k := range keys {
		if v, ok := s.totals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// TestCountersDeltaAndFlush: Add and Total never reach the store, one
// flush writes one Add per dirty key and takes the returned total as
// known, a second replica's spending is seen at the next flush, a
// refused key keeps its delta and is named, and a finished window with
// nothing pending is dropped after the next flush.
func TestCountersDeltaAndFlush(t *testing.T) {
	ctx := t.Context()
	now := at
	clock := func() time.Time { return now }
	st := newFakeStore()
	a := NewCounters(st, 0, WithClock(clock))
	b := NewCounters(st, time.Second, WithClock(clock))
	if a.flush != DefaultFlush {
		t.Fatalf("default flush = %s", a.flush)
	}
	resets := at.Add(time.Hour)
	a.Add("k", 5, resets)
	a.Add("k", 7, resets)
	if a.Total("k") != 12 || a.Known("k") != 0 || a.Pending("k") != 12 || st.adds != 0 || a.Total("other") != 0 || a.Pending("other") != 0 || a.Known("other") != 0 {
		t.Fatalf("before the flush: total %d, pending %d, adds %d", a.Total("k"), a.Pending("k"), st.adds)
	}
	b.Add("k", 100, resets)
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Total("k") != 12 {
		t.Fatalf("a sees b's spending before its own flush: %d", a.Total("k"))
	}
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Total("k") != 112 || a.Known("k") != 112 || a.Pending("k") != 0 || st.adds != 2 || !st.expires["k"].Equal(resets) {
		t.Fatalf("after the flush: total %d, pending %d, adds %d, expires %s", a.Total("k"), a.Pending("k"), st.adds, st.expires["k"])
	}
	// A refused key keeps its delta and the error names it; the other
	// key flushes.
	st.refuse = "bad"
	a.Add("bad", 3, time.Time{})
	a.Add("k", 1, resets)
	err := a.Flush(ctx)
	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "counter bad") {
		t.Fatalf("Flush with a refused key: %v", err)
	}
	if a.Pending("bad") != 3 || a.Total("k") != 113 || a.Pending("k") != 0 {
		t.Fatalf("after the refused flush: bad pending %d, k total %d pending %d", a.Pending("bad"), a.Total("k"), a.Pending("k"))
	}
	st.refuse = ""
	if err := a.Flush(ctx); err != nil || a.Total("bad") != 3 || a.Pending("bad") != 0 {
		t.Fatalf("the kept delta did not flush: %v, total %d", err, a.Total("bad"))
	}
	// Nothing dirty is nothing written.
	before := st.adds
	if err := a.Flush(ctx); err != nil || st.adds != before {
		t.Fatalf("an empty flush wrote: %v, adds %d", err, st.adds-before)
	}
	// The finished window is dropped once it reset and a flush interval
	// passed since it was touched; the none window is kept.
	now = resets.Add(2 * time.Second)
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	_, hasK := a.rows["k"]
	_, hasBad := a.rows["bad"]
	a.mu.Unlock()
	if hasK || !hasBad {
		t.Fatalf("after the window reset: k kept %v, bad kept %v", hasK, hasBad)
	}
}

// TestCountersRunFlushesAtStop: Run flushes on the interval and once
// more when the context ends, so nothing pending is lost at shutdown.
func TestCountersRunFlushesAtStop(t *testing.T) {
	st := newFakeStore()
	c := NewCounters(st, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	c.Add("k", 4, time.Time{})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if m, _ := st.Read(t.Context(), []string{"k"}); m["k"] == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the interval flush did not happen")
		}
		time.Sleep(time.Millisecond)
	}
	c.Add("k", 6, time.Time{})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m, _ := st.Read(t.Context(), []string{"k"}); m["k"] != 10 {
		t.Fatalf("after stop the store holds %d, want 10", m["k"])
	}
	// A run whose last flush fails returns the failure.
	st.refuse = "k"
	c.Add("k", 1, time.Time{})
	ctx2, cancel2 := context.WithCancel(t.Context())
	cancel2()
	if err := c.Run(ctx2); !errors.Is(err, errRefused) {
		t.Fatalf("Run with a refusing store = %v", err)
	}
}

// TestClaimIsFirstOnce: of six replicas claiming one marker at once,
// exactly one is first, and a store failure is an error naming the key.
func TestClaimIsFirstOnce(t *testing.T) {
	st := newFakeStore()
	var firsts sync.Map
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			first, err := Claim(t.Context(), st, "key:key_1:exhausted:60", time.Time{})
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
	if n != 1 {
		t.Fatalf("%d replicas were first, want 1", n)
	}
	st.refuse = "m"
	if _, err := Claim(t.Context(), st, "m", time.Time{}); !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "marker m") {
		t.Fatalf("Claim on a refusing store = %v", err)
	}
}

// gatedStore is a CounterStore whose first Add blocks, once it has been
// entered, until it is released, so a test can hold a flush inside the
// store's round-trip and read the counter while the flush is mid-write.
type gatedStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedStore) Add(_ context.Context, _ string, delta int64, _ time.Time) (int64, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return delta, nil
}

func (g *gatedStore) Read(context.Context, []string) (map[string]int64, error) {
	return map[string]int64{}, nil
}

// TestFlushKeepsOwnSpendVisibleMidRoundTrip: while a flush holds a key's
// captured delta inside the store's Add, a concurrent reader still sees
// that spend, so a hard limit refuses the request its own settled spend
// already exhausts rather than admitting it in the window of the store
// round-trip. Without the fix Total reads zero here, because the flush
// blanked pending before it learned the new known.
func TestFlushKeepsOwnSpendVisibleMidRoundTrip(t *testing.T) {
	g := &gatedStore{entered: make(chan struct{}), release: make(chan struct{})}
	c := NewCounters(g, time.Hour)
	c.Add("k", 100, time.Time{})

	flushed := make(chan error, 1)
	go func() { flushed <- c.Flush(context.Background()) }()

	<-g.entered // the flush now holds the delta inside store.Add
	if got := c.Total("k"); got != 100 {
		t.Errorf("Total mid-flush = %d, want 100: the replica lost its own spend", got)
	}
	if got := c.Known("k") + c.Pending("k"); got != 100 {
		t.Errorf("Known+Pending mid-flush = %d, want 100", got)
	}
	close(g.release)

	if err := <-flushed; err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got, pending := c.Total("k"), c.Pending("k"); got != 100 || pending != 0 {
		t.Fatalf("after the flush: total %d, pending %d, want 100 and 0", got, pending)
	}
}

// TestBudgetWindow is spec 037's Budget window: without an anchor or a
// restart the keys are CounterKey's, so counters keep their rows across
// the upgrade; a restart inside the window and not after now moves the
// start and keeps the reset; one before the window, or in the future,
// has no effect; under none a restart renders the lifetime key with the
// instant; and the marker carries the amount.
func TestBudgetWindow(t *testing.T) {
	amount := v1.Money(10_000_000)
	b := func(w v1.Window, anchor, restart time.Time) *v1.Budget {
		return &v1.Budget{Spec: v1.BudgetSpec{Amount: &amount, Window: w, Anchor: anchor, RestartedAt: restart},
			Status: v1.BudgetStatus{ID: "bud_1", CreatedAt: created}}
	}
	for _, w := range []v1.Window{"1h", "168h", v1.WindowMonth, v1.WindowNone} {
		if got, want := BudgetCounterKey(ScopeBudgetSpend, b(w, time.Time{}, time.Time{}), at), CounterKey(ScopeBudgetSpend, "bud_1", w, at, created); got != want {
			t.Errorf("%s: BudgetCounterKey = %s, CounterKey = %s", w, got, want)
		}
	}
	natural, resets := v1.WindowMonth.Bounds(at, created)
	restart := at.Add(-time.Hour)
	start, r := BudgetWindow(b(v1.WindowMonth, time.Time{}, restart), at)
	if !start.Equal(restart) || !r.Equal(resets) {
		t.Fatalf("a restart inside the window = %s, %s", start, r)
	}
	if BudgetCounterKey(ScopeBudgetSpend, b(v1.WindowMonth, time.Time{}, restart), at) == CounterKey(ScopeBudgetSpend, "bud_1", v1.WindowMonth, at, created) {
		t.Fatal("a restart kept the natural window's counter")
	}
	for name, instant := range map[string]time.Time{"before the window": natural.Add(-time.Hour), "in the future": at.Add(time.Minute)} {
		if start, _ := BudgetWindow(b(v1.WindowMonth, time.Time{}, instant), at); !start.Equal(natural) {
			t.Errorf("a restart %s moved the start to %s", name, start)
		}
	}
	if start, _ := BudgetWindow(b(v1.WindowMonth, time.Time{}, at.Add(time.Minute)), at.Add(2*time.Minute)); !start.Equal(at.Add(time.Minute)) {
		t.Errorf("a future restart did not take effect once the clock passed it: %s", start)
	}
	if got := BudgetCounterKey(ScopeBudgetSpend, b(v1.WindowNone, time.Time{}, restart), at); got != "budget:bud_1:spend:none@"+strconv.FormatInt(restart.Unix(), 10) {
		t.Errorf("a restart under none = %s", got)
	}
	if got := BudgetCounterKey(ScopeBudgetSpend, b(v1.WindowNone, time.Time{}, created), at); got != TotalKey(ScopeBudgetSpend, "bud_1") {
		t.Errorf("a restart at createdAt under none = %s", got)
	}
	monday := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	if start, r := BudgetWindow(b("168h", monday, time.Time{}), at); !start.Equal(monday) || !r.Equal(monday.Add(168*time.Hour)) {
		t.Errorf("an anchored week = %s, %s", start, r)
	}
	if got := MarkerKey("budget:bud_1:exhausted:1", amount); got != "budget:bud_1:exhausted:1:10000000" {
		t.Errorf("MarkerKey = %s", got)
	}
}
