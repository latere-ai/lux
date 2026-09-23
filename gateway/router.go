// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"latere.ai/x/pkg/circuitbreaker"
	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
)

// This file is the Router of spec 008: the attempt order over a Model's
// targets and one circuit per target. The handler of spec 004 drives it
// through the Router interface; internal/serve and a platform that
// mounts the handler construct it here, so there is one implementation
// in the tree.

// The circuit per target. The two numbers are this package's constants
// and not configuration, because a value an operator would tune per
// upstream belongs on the Provider and no field for it exists yet.
const (
	// CircuitThreshold is the consecutive retryable failures that open a
	// target's circuit.
	CircuitThreshold = 5
	// CircuitOpen is how long an open circuit refuses work before it
	// admits one half-open attempt.
	CircuitOpen = 30 * time.Second
)

// MetricCircuitOpen is the gauge of spec 019 this file owns: 1 while a
// target's circuit is not closed, 0 otherwise, one series per target
// this replica has routed to, labeled provider, the Provider's name,
// and model, the target's upstream name, which are the two halves of the
// circuit's key.
const MetricCircuitOpen = "lux_circuit_open"

// defaultWeight is a target's weight when it carries none, spec 003's
// default, so a Model built without Resolve orders as a resolved one.
const defaultWeight = 100

// HealthView is the health this replica acts on for a Provider, by id:
// spec 005's serve.Health.View, the worse of the published state and the
// replica's own. A target whose Provider is Unreachable is not a
// candidate; Degraded and Unknown take nothing out.
type HealthView func(providerID string) v1.HealthState

// RouterOptions is what NewTargetRouter builds the Router from.
type RouterOptions struct {
	// Catalog resolves the Provider a target names, by name or prv_ id,
	// without its credential value. Required.
	Catalog Catalog
	// Health is the health view; nil is Unknown for every Provider, so
	// health takes nothing out of selection.
	Health HealthView
	// Metrics is the registry lux_circuit_open is registered in; nil
	// registers none.
	Metrics *metrics.Registry
	// Now is the circuits' clock; nil is time.Now.
	Now func() time.Time
	// Rand draws the weighted order, a number in [0, 1), safe for
	// concurrent calls; nil is math/rand/v2's shared source.
	Rand func() float64
}

// TargetRouter is the Router of spec 008: one attempt order per request
// from a Model's targets, each Provider's health, and one
// circuitbreaker.Breaker per (provider id, upstream model), so two
// Models naming one upstream model share the circuit that failure
// belongs to. The circuits are this replica's alone and are written to
// no object's status.
type TargetRouter struct {
	o        RouterOptions
	mu       sync.Mutex
	circuits map[circuitKey]*circuit
}

// circuitKey is the circuit's key: the Provider's id and the upstream's
// own name for the model.
type circuitKey struct {
	providerID string
	model      string
}

// circuit is one breaker and the Provider's name its series carries.
type circuit struct {
	*circuitbreaker.Breaker
	provider string
}

// NewTargetRouter returns the Router. A nil Catalog is a panic, because
// a router that cannot find a target's Provider selects nothing and
// should not start.
func NewTargetRouter(o RouterOptions) *TargetRouter {
	if o.Catalog == nil {
		panic("gateway.NewTargetRouter: RouterOptions.Catalog is nil")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	r := &TargetRouter{o: o, circuits: map[circuitKey]*circuit{}}
	if o.Metrics != nil {
		o.Metrics.Gauge(MetricCircuitOpen, "1 while a target's circuit is open, by provider and upstream model.", r.gauge)
	}
	return r
}

// candidate is one target that step 1 kept, with what the order reads.
type candidate struct {
	Target
	weight   int
	priority int
}

// Targets implements Router: the attempt order of spec 008, computed
// per request. A target is a candidate unless its Provider is gone from
// the catalog, is Unreachable on this replica, or has a circuit that
// does not admit work, read with the side-effect-free Admits so ordering
// takes no probe slot. Candidates are grouped by priority ascending;
// inside a group the targets with weight above 0 come first in weighted
// random order, one drawn with probability proportional to its weight
// and removed until none is left, and the targets with weight 0 come
// last in manifest order. Empty is provider_unavailable at the door; an
// error is the catalog's own failure.
func (r *TargetRouter) Targets(ctx context.Context, m *v1.Model) ([]Target, error) {
	var cands []candidate
	for i, t := range m.Spec.Targets {
		p, err := r.o.Catalog.Provider(ctx, t.Provider)
		if err != nil {
			return nil, fmt.Errorf("Provider %q of target %d: %w", t.Provider, i, err)
		}
		if p == nil {
			continue // the Provider is gone; nothing to dial
		}
		if r.health(p.Status.ID) == v1.HealthUnreachable {
			continue
		}
		if !r.circuit(p, t.Model).Admits() {
			continue
		}
		weight := defaultWeight
		if t.Weight != nil {
			weight = *t.Weight
		}
		cands = append(cands, candidate{Provider: p, Model: t.Model, weight: weight, priority: t.Priority})
	}
	return r.order(cands), nil
}

// order concatenates the priority groups, lowest first. The sort is
// stable so a group keeps its manifest order for the weight-0 tail.
func (r *TargetRouter) order(cands []candidate) []Target {
	slices.SortStableFunc(cands, func(a, b candidate) int { return cmp.Compare(a.priority, b.priority) })
	out := make([]Target, 0, len(cands))
	for start := 0; start < len(cands); {
		end := start
		for end < len(cands) && cands[end].priority == cands[start].priority {
			end++
		}
		out = r.group(out, cands[start:end])
		start = end
	}
	return out
}

// group appends one priority group: the weighted targets in weighted
// random order without replacement, then the weight-0 targets as the
// manifest lists them.
func (r *TargetRouter) group(out []Target, g []candidate) []Target {
	var weighted, tail []candidate
	for _, c := range g {
		if c.weight > 0 {
			weighted = append(weighted, c)
		} else {
			tail = append(tail, c)
		}
	}
	for len(weighted) > 0 {
		i := r.draw(weighted)
		out = append(out, weighted[i].Target)
		weighted = slices.Delete(weighted, i, i+1)
	}
	for _, c := range tail {
		out = append(out, c.Target)
	}
	return out
}

// draw picks one index of cands with probability proportional to its
// weight: a point in [0, total) falls in the segment of the target it
// selects.
func (r *TargetRouter) draw(cands []candidate) int {
	total := 0
	for _, c := range cands {
		total += c.weight
	}
	x := r.o.Rand() * float64(total)
	acc := 0.0
	for i, c := range cands {
		acc += float64(c.weight)
		if x < acc {
			return i
		}
	}
	return len(cands) - 1 // a draw at or past total, which [0, 1) never gives
}

// Allow implements Router: it takes the half-open probe slot immediately
// before an attempt and answers false when another request took it, in
// which case the door skips the target.
func (r *TargetRouter) Allow(t Target) bool {
	c := r.circuitOf(t)
	return c == nil || c.Allow()
}

// RecordSuccess implements Router: every complete response the retry
// table does not retry closes the circuit and resets its count.
func (r *TargetRouter) RecordSuccess(t Target) {
	if c := r.circuitOf(t); c != nil {
		c.RecordSuccess()
	}
}

// RecordFailure implements Router: every retryable failure counts toward
// the threshold, and a failed probe reopens the circuit.
func (r *TargetRouter) RecordFailure(t Target) {
	if c := r.circuitOf(t); c != nil {
		c.RecordFailure()
	}
}

// circuitOf is the circuit of a Target the door hands back, and nil for
// a Target that names no target of a Model: the opaque route reports its
// Provider with no upstream model, and a Provider alone has no circuit.
func (r *TargetRouter) circuitOf(t Target) *circuit {
	if t.Provider == nil || t.Model == "" {
		return nil
	}
	return r.circuit(t.Provider, t.Model)
}

// circuit is the breaker of one target, built on first sight with the
// two constants and the router's clock.
func (r *TargetRouter) circuit(p *v1.Provider, model string) *circuit {
	key := circuitKey{providerID: p.Status.ID, model: model}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.circuits[key]
	if !ok {
		c = &circuit{
			Breaker:  circuitbreaker.New(CircuitThreshold, CircuitOpen, circuitbreaker.WithClock(r.o.Now)),
			provider: p.Metadata.Name,
		}
		r.circuits[key] = c
	}
	return c
}

// health is the view's answer, Unknown without a view.
func (r *TargetRouter) health(providerID string) v1.HealthState {
	if r.o.Health == nil {
		return v1.HealthUnknown
	}
	return r.o.Health(providerID)
}

// gauge is the scrape-time collector of lux_circuit_open: one sample per
// circuit, 1 unless the breaker is closed, so an open circuit is visible
// without a request and stays visible until traffic closes it. The
// samples are sorted so an exposition is stable.
func (r *TargetRouter) gauge() []metrics.LabeledValue {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]metrics.LabeledValue, 0, len(r.circuits))
	for key, c := range r.circuits {
		v := 0.0
		if c.State() != circuitbreaker.Closed {
			v = 1
		}
		out = append(out, metrics.LabeledValue{Labels: map[string]string{"provider": c.provider, "model": key.model}, Value: v})
	}
	slices.SortFunc(out, func(a, b metrics.LabeledValue) int {
		return cmp.Or(cmp.Compare(a.Labels["provider"], b.Labels["provider"]), cmp.Compare(a.Labels["model"], b.Labels["model"]))
	})
	return out
}
