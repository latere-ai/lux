// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/lux/internal/lifecycle"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

// healthRecord is one Provider under test with the job that writes its
// health, as the lease holder holds them.
type healthRecord struct {
	h *Health
	p *v1.Provider
}

// healthRecords gives each Enter a store, a Provider and a job that holds the
// lease, and gives Move and State the record by key.
type healthRecords struct {
	mu   sync.Mutex
	seq  int
	byID map[string]*healthRecord
}

func (r *healthRecords) get(key string) *healthRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[key]
}

func (r *healthRecords) enter(t *testing.T, mk func(rec *healthRecord, st store.Store)) string {
	t.Helper()
	st := memory.New()
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "lifecycle"},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: "https://api.example.com/v1", Discovery: v1.Discovery{Mode: v1.DiscoveryAuto}, Health: v1.Health{Mode: v1.HealthProbe}},
		Status:   v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, time.Now(), nil), Warnings: []string{}},
	}
	if _, err := st.Objects().Put(t.Context(), p, 0); err != nil {
		t.Fatalf("create the Provider: %v", err)
	}
	h := NewHealth(HealthOptions{Store: st, Interval: time.Hour, Holder: "a", Logger: slog.New(slog.DiscardHandler)})
	h.held = true
	h.providers = providers{p.Status.ID: p, p.Metadata.Name: p}
	rec := &healthRecord{h: h, p: p}
	mk(rec, st)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	key := fmt.Sprintf("%s-%d", p.Status.ID, r.seq)
	if r.byID == nil {
		r.byID = map[string]*healthRecord{}
	}
	r.byID[key] = rec
	return key
}

// settle is the answer of a writer that reports whether it changed the
// record: no change is the refusal.
func settle(what string, changed bool) error {
	if changed {
		return nil
	}
	return fmt.Errorf("%s: the record was not in a state before it: %w", what, statemachine.ErrIllegalTransition)
}

// TestHealthWriters drives the health job's writers through HealthTable: a
// probe or data-plane success (healthy), a failure (degraded), the registry's
// verdict (unreachable), and the reset of a Provider whose mode became none
// (unknown). The state read back is the one the job published to the store.
func TestHealthWriters(t *testing.T) {
	recs := &healthRecords{}
	d := statemachinetest.Driver[v1.HealthState]{
		Enter: func(t *testing.T, phase v1.HealthState) string {
			return recs.enter(t, func(rec *healthRecord, st store.Store) {
				if phase == v1.HealthUnknown {
					return
				}
				health := v1.HealthStatus{State: phase, Since: time.Now().UTC()}
				if err := st.Objects().PutStatus(t.Context(), v1.KindProvider, rec.p.Status.ID, store.ProviderObserved{Health: &health}); err != nil {
					t.Fatalf("publish %s: %v", phase, err)
				}
				rec.h.published[rec.p.Status.ID] = health
			})
		},
		Move: func(key string, to v1.HealthState) error {
			rec := recs.get(key)
			ctx := context.Background()
			rec.h.mu.Lock()
			defer rec.h.mu.Unlock()
			switch to {
			case v1.HealthHealthy:
				return settle("success", rec.h.record(ctx, rec.p, false, "", true))
			case v1.HealthDegraded:
				return settle("failure", rec.h.record(ctx, rec.p, true, "refused", true))
			case v1.HealthUnreachable:
				return settle("registry verdict", rec.h.recordUnreachable(ctx, rec.p, "no live tunnel session"))
			case v1.HealthUnknown:
				return settle("reset", rec.h.resetToUnknown(ctx, rec.p))
			}
			return fmt.Errorf("health %s: no writer: %w", to, statemachine.ErrIllegalTransition)
		},
		State: func(t *testing.T, key string) v1.HealthState {
			rec := recs.get(key)
			obj, _, err := rec.h.o.Store.Objects().Get(t.Context(), v1.KindProvider, rec.p.Status.ID)
			if err != nil {
				t.Fatalf("read the Provider: %v", err)
			}
			p, ok := obj.(*v1.Provider)
			if !ok {
				t.Fatalf("read %T, want a Provider", obj)
			}
			if p.Status.Health == nil {
				return v1.HealthUnknown
			}
			return p.Status.Health.State
		},
	}
	statemachinetest.Exhaustive(t, lifecycle.HealthTable, d)
	statemachinetest.Race(t, lifecycle.HealthTable, d)
}

// TestHealthFailuresReachUnreachable: the third consecutive failure takes
// Degraded to Unreachable through the same table, and a failure against an
// Unreachable Provider changes nothing.
func TestHealthFailuresReachUnreachable(t *testing.T) {
	var m machine
	for i, want := range []v1.HealthState{v1.HealthDegraded, v1.HealthDegraded, v1.HealthUnreachable, v1.HealthUnreachable} {
		if _, err := m.step(true); err != nil || m.state != want {
			t.Fatalf("failure %d: state %s, err %v; want %s", i+1, m.state, err, want)
		}
	}
	if changed, err := m.force(v1.HealthDegraded); err == nil || changed || m.state != v1.HealthUnreachable {
		t.Fatalf("force Degraded on Unreachable = %v, %v, state %s; want the table's refusal", changed, err, m.state)
	}
}

// TestTunnelWriters drives the writer of status.tunnel through TunnelTable:
// the registry's live row publishes connected, and no row publishes
// disconnected.
func TestTunnelWriters(t *testing.T) {
	recs := &healthRecords{}
	// The same row every time, so a second writer of one state finds the
	// status already as it would write it.
	connectedAt := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	d := statemachinetest.Driver[v1.TunnelState]{
		Enter: func(t *testing.T, phase v1.TunnelState) string {
			return recs.enter(t, func(rec *healthRecord, _ store.Store) {
				rec.h.mu.Lock()
				defer rec.h.mu.Unlock()
				row := &store.Tunnel{ProviderID: rec.p.Status.ID, Session: "tun_1", ConnectedAt: connectedAt, ExpiresAt: connectedAt.Add(time.Minute)}
				if phase == v1.TunnelDisconnected {
					row = nil
				}
				if !rec.h.publishTunnel(t.Context(), rec.p, row) {
					t.Fatalf("publish the first state %s", phase)
				}
			})
		},
		Move: func(key string, to v1.TunnelState) error {
			rec := recs.get(key)
			rec.h.mu.Lock()
			defer rec.h.mu.Unlock()
			var row *store.Tunnel
			if to == v1.TunnelConnected {
				row = &store.Tunnel{ProviderID: rec.p.Status.ID, Session: "tun_2", ConnectedAt: connectedAt, ExpiresAt: connectedAt.Add(time.Minute)}
			}
			return settle("publish "+string(to), rec.h.publishTunnel(context.Background(), rec.p, row))
		},
		State: func(t *testing.T, key string) v1.TunnelState {
			rec := recs.get(key)
			obj, _, err := rec.h.o.Store.Objects().Get(t.Context(), v1.KindProvider, rec.p.Status.ID)
			if err != nil {
				t.Fatalf("read the Provider: %v", err)
			}
			p, ok := obj.(*v1.Provider)
			if !ok || p.Status.Tunnel == nil {
				t.Fatalf("read %T with no tunnel status", obj)
			}
			return p.Status.Tunnel.State
		},
	}
	statemachinetest.Exhaustive(t, lifecycle.TunnelTable, d)
	statemachinetest.Race(t, lifecycle.TunnelTable, d)
}
