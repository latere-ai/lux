// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The router alone, over a catalog of four openai Providers, a health
// map the test writes, a clock the test moves, and a draw the test fixes.

type routerFixture struct {
	t       *testing.T
	catalog *fakeCatalog
	reg     *metrics.Registry
	r       *TargetRouter

	mu     sync.Mutex
	health map[string]v1.HealthState // by provider id
	now    time.Time
	draw   float64
}

func newRouterFixture(t *testing.T) *routerFixture {
	t.Helper()
	f := &routerFixture{
		t:       t,
		catalog: &fakeCatalog{models: map[string]*v1.Model{}, providers: map[string]*v1.Provider{}},
		reg:     metrics.NewRegistry(),
		health:  map[string]v1.HealthState{},
		now:     time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		f.provider(name)
	}
	f.r = NewTargetRouter(RouterOptions{Catalog: f.catalog, Health: f.view, Metrics: f.reg, Now: f.clock, Rand: f.rand})
	return f
}

func (f *routerFixture) provider(name string) *v1.Provider {
	p := &v1.Provider{}
	p.Metadata.Name = name
	p.Spec.Dialect, p.Spec.BaseURL = v1.DialectOpenAI, "https://"+name+".example.com/v1"
	p.Status.ID = "prv_" + strings.ToUpper(name) + strings.Repeat("0", 26-len(name))
	f.catalog.addProvider(p)
	return p
}

func (f *routerFixture) view(id string) v1.HealthState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health[id]
}

func (f *routerFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *routerFixture) rand() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.draw
}

func (f *routerFixture) set(provider string, s v1.HealthState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health[f.catalog.providers[provider].Status.ID] = s
}

func (f *routerFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *routerFixture) fix(draw float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.draw = draw
}

// target is the Target of one provider and upstream name.
func (f *routerFixture) target(provider, model string) Target {
	return Target{Provider: f.catalog.providers[provider], Model: model}
}

// open drives a target's circuit to open with the threshold's failures.
func (f *routerFixture) open(provider, model string) {
	for range CircuitThreshold {
		f.r.RecordFailure(f.target(provider, model))
	}
}

// names renders an order as "provider/model" entries.
func names(order []Target) []string {
	out := make([]string, 0, len(order))
	for _, t := range order {
		out = append(out, t.Provider.Metadata.Name+"/"+t.Model)
	}
	return out
}

// tw is a target with an explicit weight and priority.
func tw(provider, model string, weight, priority int) v1.Target {
	return v1.Target{Provider: provider, Model: model, Weight: &weight, Priority: priority}
}

func newModel(name string, targets ...v1.Target) *v1.Model {
	m := &v1.Model{}
	m.Metadata.Name = name
	m.Spec.Targets, m.Spec.Fallback = targets, v1.FallbackOnError
	m.Status.ID = "mdl_" + strings.ToUpper(name) + strings.Repeat("0", 26-len(name))
	return m
}

// failures is the consecutive failure count of one target's circuit,
// and -1 when none was ever built for it.
func (r *TargetRouter) failures(providerID, model string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.circuits[circuitKey{providerID: providerID, model: model}]
	if !ok {
		return -1
	}
	return c.Failures()
}

// TestNewTargetRouterRequiresCatalog: a router without a catalog cannot
// find a target's Provider and refuses to start.
func TestNewTargetRouterRequiresCatalog(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	NewTargetRouter(RouterOptions{})
}

// TestAttemptOrder: the order is priority ascending, then the weighted
// entries in weighted random order, then the weight-0 entries in
// manifest order; a target whose Provider is Unreachable or whose
// circuit is open leaves it, Degraded and Unknown do not, a Provider the
// catalog lacks is skipped, and a target with no weight weighs 100.
func TestAttemptOrder(t *testing.T) {
	cases := []struct {
		name    string
		targets []v1.Target
		health  map[string]v1.HealthState // by provider name
		open    []string                  // "provider/model"
		draw    float64
		want    []string
	}{
		{
			name:    "one priority, the draw at zero keeps manifest order",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("b", "m", 100, 0), tw("c", "m", 100, 0)},
			want:    []string{"a/m", "b/m", "c/m"},
		},
		{
			name:    "one priority, the draw near one takes the last segment each time",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("b", "m", 100, 0), tw("c", "m", 100, 0)},
			draw:    0.99,
			want:    []string{"c/m", "b/m", "a/m"},
		},
		{
			name:    "a draw of one, outside the half-open interval, takes the last segment",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("b", "m", 100, 0), tw("c", "m", 100, 0)},
			draw:    1,
			want:    []string{"c/m", "b/m", "a/m"},
		},
		{
			name:    "priority ascending across groups",
			targets: []v1.Target{tw("a", "m", 100, 2), tw("b", "m", 100, 0), tw("c", "m", 100, 1)},
			want:    []string{"b/m", "c/m", "a/m"},
		},
		{
			name:    "weight 0 last in manifest order after every weighted peer",
			targets: []v1.Target{tw("a", "m", 0, 0), tw("b", "m", 100, 0), tw("c", "m", 0, 0), tw("d", "m", 100, 0)},
			draw:    0.99,
			want:    []string{"d/m", "b/m", "a/m", "c/m"},
		},
		{
			name:    "the only weighted target of priority 0 Unreachable puts the weight-0 target first",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("b", "m", 0, 0), tw("c", "m", 100, 1)},
			health:  map[string]v1.HealthState{"a": v1.HealthUnreachable},
			want:    []string{"b/m", "c/m"},
		},
		{
			name:    "Unreachable and an open circuit leave; Degraded and Unknown stay",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("b", "m", 100, 0), tw("c", "m", 100, 0), tw("d", "m", 100, 0)},
			health:  map[string]v1.HealthState{"a": v1.HealthUnreachable, "c": v1.HealthDegraded, "d": v1.HealthUnknown},
			open:    []string{"b/m"},
			want:    []string{"c/m", "d/m"},
		},
		{
			name:    "a circuit is per upstream model, not per Provider",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("a", "n", 100, 0)},
			open:    []string{"a/m"},
			want:    []string{"a/n"},
		},
		{
			name:    "nothing admitted is an empty order and no error",
			targets: []v1.Target{tw("a", "m", 100, 0), tw("b", "m", 100, 1)},
			health:  map[string]v1.HealthState{"a": v1.HealthUnreachable, "b": v1.HealthUnreachable},
			want:    []string{},
		},
		{
			name:    "a Provider the catalog lacks is skipped",
			targets: []v1.Target{tw("ghost", "m", 100, 0), tw("a", "m", 100, 0)},
			want:    []string{"a/m"},
		},
		{
			name:    "no weight is 100",
			targets: []v1.Target{{Provider: "a", Model: "m"}, tw("b", "m", 0, 0)},
			want:    []string{"a/m", "b/m"},
		},
		{
			name:    "a Provider by id",
			targets: []v1.Target{tw("prv_A0000000000000000000000000", "m", 100, 0)},
			want:    []string{"a/m"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRouterFixture(t)
			f.fix(c.draw)
			for name, s := range c.health {
				f.set(name, s)
			}
			for _, key := range c.open {
				provider, model, _ := strings.Cut(key, "/")
				f.open(provider, model)
			}
			order, err := f.r.Targets(t.Context(), newModel("m", c.targets...))
			if err != nil {
				t.Fatal(err)
			}
			if got := names(order); strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("order %v, want %v", got, c.want)
			}
		})
	}
}

// TestTargetsReportsTheCatalogFailure: a catalog that cannot answer is
// the router's error, which the door answers store_unavailable.
func TestTargetsReportsTheCatalogFailure(t *testing.T) {
	f := newRouterFixture(t)
	f.catalog.err = errors.New("the store is down")
	order, err := f.r.Targets(t.Context(), newModel("m", tw("a", "m", 100, 0)))
	if err == nil || !strings.Contains(err.Error(), "the store is down") || order != nil {
		t.Fatalf("order %v, err %v", order, err)
	}
}

// TestWeightedShareMatchesWeights: over ten thousand requests the share
// each target of one priority receives is within two percent of its
// weight, every order carries every target once, and the draw is
// without replacement.
func TestWeightedShareMatchesWeights(t *testing.T) {
	f := newRouterFixture(t)
	src := rand.New(rand.NewPCG(1, 2))
	f.r = NewTargetRouter(RouterOptions{Catalog: f.catalog, Health: f.view, Now: f.clock, Rand: src.Float64})
	weights := map[string]int{"a": 70, "b": 20, "c": 10}
	m := newModel("m", tw("a", "m", 70, 0), tw("b", "m", 20, 0), tw("c", "m", 10, 0))
	const n = 10000
	first := map[string]int{}
	for range n {
		order, err := f.r.Targets(t.Context(), m)
		if err != nil {
			t.Fatal(err)
		}
		if len(order) != 3 || order[0].Provider == order[1].Provider || order[1].Provider == order[2].Provider || order[0].Provider == order[2].Provider {
			t.Fatalf("order %v is not the three targets once each", names(order))
		}
		first[order[0].Provider.Metadata.Name]++
	}
	for name, w := range weights {
		share := float64(first[name]) / n
		if want := float64(w) / 100; math.Abs(share-want) > 0.02 {
			t.Errorf("%s received %.3f of the requests, want %.2f within 0.02", name, share, want)
		}
	}
}

// TestCircuitStates: five consecutive failures open a circuit and take
// its target out of the order; after the open duration one half-open
// attempt is admitted and every concurrent rival refused; a success
// closes it and a failed probe reopens it for another open duration; a
// success resets the count; a Target naming no target has no circuit.
func TestCircuitStates(t *testing.T) {
	f := newRouterFixture(t)
	target := f.target("a", "m")
	m := newModel("m", tw("a", "m", 100, 0), tw("b", "m", 100, 1))
	for i := range CircuitThreshold - 1 {
		f.r.RecordFailure(target)
		if order, _ := f.r.Targets(t.Context(), m); len(order) != 2 {
			t.Fatalf("after %d failures the order is %v", i+1, names(order))
		}
	}
	f.r.RecordFailure(target)
	if order, _ := f.r.Targets(t.Context(), m); strings.Join(names(order), " ") != "b/m" {
		t.Fatalf("open circuit still in the order: %v", names(order))
	}
	if f.r.Allow(target) {
		t.Fatal("an open circuit allowed an attempt")
	}
	f.advance(CircuitOpen - time.Millisecond)
	if order, _ := f.r.Targets(t.Context(), m); len(order) != 1 {
		t.Fatalf("admitted before the open duration: %v", names(order))
	}
	f.advance(time.Millisecond)
	if order, _ := f.r.Targets(t.Context(), m); strings.Join(names(order), " ") != "a/m b/m" {
		t.Fatalf("not admitted after the open duration: %v", names(order))
	}
	// Ordering took no probe slot: the slot is still there, and exactly
	// one of many concurrent takers gets it.
	const rivals = 64
	var start sync.WaitGroup
	var done sync.WaitGroup
	var admitted sync.Map
	start.Add(1)
	for i := range rivals {
		done.Go(func() {
			start.Wait()
			if f.r.Allow(target) {
				admitted.Store(i, true)
			}
		})
	}
	start.Done()
	done.Wait()
	count := 0
	admitted.Range(func(any, any) bool { count++; return true })
	if count != 1 {
		t.Fatalf("%d rivals took the one probe slot", count)
	}
	if f.r.Allow(target) {
		t.Fatal("a second probe was admitted while the first is unresolved")
	}
	// The probe fails: open again for a whole open duration.
	f.r.RecordFailure(target)
	f.advance(CircuitOpen / 2)
	if f.r.Allow(target) {
		t.Fatal("a failed probe did not reopen the circuit")
	}
	f.advance(CircuitOpen / 2)
	if !f.r.Allow(target) {
		t.Fatal("no probe after the second open duration")
	}
	// The probe succeeds: closed, and every attempt is admitted.
	f.r.RecordSuccess(target)
	for range 3 {
		if !f.r.Allow(target) {
			t.Fatal("a closed circuit refused an attempt")
		}
	}
	// A success resets the count: four failures, a success, four more
	// failures never open it.
	for range CircuitThreshold - 1 {
		f.r.RecordFailure(target)
	}
	f.r.RecordSuccess(target)
	if got := f.r.failures(target.Provider.Status.ID, "m"); got != 0 {
		t.Fatalf("count after a success: %d", got)
	}
	for range CircuitThreshold - 1 {
		f.r.RecordFailure(target)
	}
	if !f.r.Allow(target) {
		t.Fatal("the count survived a success")
	}
	// The opaque route reports a Provider with no upstream model, and a
	// nil Provider is nobody: neither has a circuit.
	for _, none := range []Target{{Provider: target.Provider}, {Model: "m"}, {}} {
		f.r.RecordFailure(none)
		f.r.RecordSuccess(none)
		if !f.r.Allow(none) {
			t.Errorf("a Target naming no target was refused: %+v", none)
		}
	}
	if got := f.r.failures(target.Provider.Status.ID, ""); got != -1 {
		t.Errorf("a circuit was built for a Provider alone: %d", got)
	}
}

// TestCircuitIsSharedAcrossModels: two Models naming one upstream model
// on one Provider share the circuit, so the failures of one take the
// target out of the other's order.
func TestCircuitIsSharedAcrossModels(t *testing.T) {
	f := newRouterFixture(t)
	one := newModel("one", tw("a", "m", 100, 0))
	two := newModel("two", tw("a", "m", 100, 0), tw("b", "m", 100, 0))
	order, _ := f.r.Targets(t.Context(), one)
	f.open("a", order[0].Model)
	if got, _ := f.r.Targets(t.Context(), two); strings.Join(names(got), " ") != "b/m" {
		t.Fatalf("the second Model's order is %v", names(got))
	}
	if got, _ := f.r.Targets(t.Context(), one); len(got) != 0 {
		t.Fatalf("the first Model's order is %v", names(got))
	}
}

// TestCircuitOpenGauge: lux_circuit_open is 1 for a target whose circuit
// opened and 0 once traffic closed it, one series per target this
// replica routed to, labelled by the Provider's name and the upstream
// model; a router without a registry registers nothing.
func TestCircuitOpenGauge(t *testing.T) {
	f := newRouterFixture(t)
	m := newModel("m", tw("a", "m", 100, 0), tw("b", "n", 100, 0))
	if _, err := f.r.Targets(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	f.open("a", "m")
	scrape := func() string {
		var buf bytes.Buffer
		f.reg.WritePrometheus(&buf)
		return buf.String()
	}
	if got := scrape(); !strings.Contains(got, MetricCircuitOpen+`{model="m",provider="a"} 1`) || !strings.Contains(got, MetricCircuitOpen+`{model="n",provider="b"} 0`) {
		t.Fatalf("exposition:\n%s", got)
	}
	// Half-open is not closed: the probe in flight reads 1.
	f.advance(CircuitOpen)
	if !f.r.Allow(f.target("a", "m")) {
		t.Fatal("no probe")
	}
	if got := scrape(); !strings.Contains(got, MetricCircuitOpen+`{model="m",provider="a"} 1`) {
		t.Fatalf("a half-open circuit reads 0:\n%s", got)
	}
	f.r.RecordSuccess(f.target("a", "m"))
	if got := scrape(); !strings.Contains(got, MetricCircuitOpen+`{model="m",provider="a"} 0`) {
		t.Fatalf("a closed circuit reads 1:\n%s", got)
	}
	if strings.Count(scrape(), MetricCircuitOpen+"{") != 2 {
		t.Fatalf("series count:\n%s", scrape())
	}
	// No registry, no gauge, no panic.
	r := NewTargetRouter(RouterOptions{Catalog: f.catalog})
	if _, err := r.Targets(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if !r.Allow(f.target("a", "m")) {
		t.Fatal("a fresh circuit refused")
	}
}
