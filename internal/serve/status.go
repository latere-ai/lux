// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The health state machine of spec 005 and what the published state
// means to a Model, shared by the two jobs.

// unreachableAfter is the consecutive failures that move Degraded to
// Unreachable.
const unreachableAfter = 3

// machine is one Provider's health as one observer counts it: the state
// and the consecutive failures since the last success.
type machine struct {
	state    v1.HealthState
	failures int
}

// step folds one outcome in and reports whether the state changed. One
// failure moves Unknown or Healthy to Degraded, the third consecutive
// failure moves Degraded to Unreachable, and one success returns any
// state to Healthy and resets the count.
func (m *machine) step(failed bool) bool {
	if m.state == "" {
		m.state = v1.HealthUnknown
	}
	prev := m.state
	if failed {
		m.failures++
		switch m.state {
		case v1.HealthUnknown, v1.HealthHealthy:
			m.state = v1.HealthDegraded
		case v1.HealthDegraded:
			if m.failures >= unreachableAfter {
				m.state = v1.HealthUnreachable
			}
		}
	} else {
		m.failures = 0
		m.state = v1.HealthHealthy
	}
	return m.state != prev
}

// force moves the machine to state without counting, the registry's
// verdict on a tunnelled Provider (spec 013), and reports whether the
// state changed. Unreachable carries the count that would have reached
// it, so the next success returns to Healthy as after any run of
// failures.
func (m *machine) force(state v1.HealthState) bool {
	prev := m.state
	m.state = state
	if state == v1.HealthUnreachable {
		m.failures = unreachableAfter
	}
	return m.state != prev
}

// rank orders the states so worse can pick: Unknown carries no
// information and loses to any other state.
func rank(s v1.HealthState) int {
	switch s {
	case v1.HealthHealthy:
		return 1
	case v1.HealthDegraded:
		return 2
	case v1.HealthUnreachable:
		return 3
	default:
		return 0
	}
}

// worse is the state a replica acts on: the worse of the published state
// and its own view, so a replica that is failing against a Provider acts
// on that at once and one that is not never raises the published state.
func worse(a, b v1.HealthState) v1.HealthState {
	if rank(b) > rank(a) {
		return b
	}
	if a == "" {
		return v1.HealthUnknown
	}
	return a
}

// observedModel is the observed half of a Model's status under the
// published states: every target's Provider's state, and available when
// at least one target names a Provider that is not Unreachable.
func observedModel(m *v1.Model, stateOf func(ref string) v1.HealthState) store.ModelObserved {
	targets := make([]v1.TargetStatus, 0, len(m.Spec.Targets))
	available := false
	for _, t := range m.Spec.Targets {
		state := stateOf(t.Provider)
		if state == "" {
			state = v1.HealthUnknown
		}
		if state != v1.HealthUnreachable {
			available = true
		}
		targets = append(targets, v1.TargetStatus{Provider: t.Provider, Model: t.Model, Health: state})
	}
	return store.ModelObserved{Available: &available, Targets: targets}
}

// providers is the set of live Providers as of one read, by id and by
// name, which is how a target refers to one.
type providers map[string]*v1.Provider

// listProviders reads every live Provider.
func listProviders(ctx context.Context, st store.Store) (providers, []*v1.Provider, error) {
	objs, _, err := st.Objects().List(ctx, v1.KindProvider, store.Filter{}, store.Page{})
	if err != nil {
		return nil, nil, fmt.Errorf("listing the Providers: %w", err)
	}
	byRef := make(providers, 2*len(objs))
	list := make([]*v1.Provider, 0, len(objs))
	for _, o := range objs {
		p, ok := o.(*v1.Provider)
		if !ok {
			return nil, nil, fmt.Errorf("listing the Providers: a %T among them", o)
		}
		byRef[p.Status.ID], byRef[p.Metadata.Name] = p, p
		list = append(list, p)
	}
	return byRef, list, nil
}

// refreshModels writes the observed status of every Model with a target
// on the Provider under stateOf, and returns how many of their targets
// name it, which is what the unreachable event reports as targets.
func refreshModels(ctx context.Context, st store.Store, p *v1.Provider, stateOf func(ref string) v1.HealthState) (int, error) {
	models, _, err := st.Objects().List(ctx, v1.KindModel, store.Filter{Provider: p.Status.ID}, store.Page{})
	if err != nil {
		return 0, fmt.Errorf("listing the Models of %s: %w", p.Status.ID, err)
	}
	targets := 0
	for _, o := range models {
		m, ok := o.(*v1.Model)
		if !ok {
			continue
		}
		for _, t := range m.Spec.Targets {
			if t.Provider == p.Status.ID || t.Provider == p.Metadata.Name {
				targets++
			}
		}
		if err := st.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, observedModel(m, stateOf)); err != nil && !errors.Is(err, store.ErrNotFound) {
			return 0, fmt.Errorf("writing the status of Model %s: %w", m.Status.ID, err)
		}
	}
	return targets, nil
}

// defaultHolder is the lease holder's name when none is given: the host,
// which is what an operator reading a lease row recognises, and the
// process, so two replicas on one host differ.
func defaultHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "replica"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
}

// leaseRenew is how often a holder renews: a third of the TTL.
const leaseRenew = store.LeaseTTL / 3

// newEventID mints an evt_ id.
func newEventID(now func() time.Time) func() string {
	return func() string { return v1.NewID(v1.PrefixEvent, now(), nil) }
}
