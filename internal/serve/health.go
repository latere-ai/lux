// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// probeBudget is the deadline of one probe, in place of the Provider's
// timeout: a probe measures reachability, not a generation.
const probeBudget = 5 * time.Second

// MetricProviderHealth is the gauge of spec 019 this job owns: one
// series per Provider and state, labeled provider, the Provider's
// name, and state, one of the four states, carrying 1 on the state this
// replica acts on and 0 on the other three, so an expression reads a
// state by selecting it.
const MetricProviderHealth = "lux_provider_health"

// providerHealthHelp is MetricProviderHealth's help text.
const providerHealthHelp = "1 on the state a Provider is in on this replica and 0 on the other three, by provider and state."

// healthStates are the four values of the state label, in the order a
// scrape carries them, which is the order of the state machine.
var healthStates = []v1.HealthState{v1.HealthHealthy, v1.HealthDegraded, v1.HealthUnreachable, v1.HealthUnknown}

// HealthOptions is what the health job runs under.
type HealthOptions struct {
	Store       store.Store
	Clients     clientSource
	Credentials credentialSource
	// Interval is LUX_HEALTH_INTERVAL, the probe period.
	Interval time.Duration
	// Metrics is the registry lux_provider_health is registered in; nil
	// registers none.
	Metrics *metrics.Registry
	// TunnelTTL is LUX_TUNNEL_REGISTRY_TTL, which places a tunneled
	// Provider's last heartbeat at its row's expiry less the window;
	// zero is thirty seconds (spec 013).
	TunnelTTL time.Duration
	// Holder names this replica in the lease row; empty is the host and
	// the process id.
	Holder string
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints event ids; nil mints evt_ ULIDs from Now.
	NewID func() string
}

// Health is the health job of spec 005 on one replica. Under the health
// lease it probes every probe-mode Provider on the interval, folds in
// the data plane's outcomes through Observe, publishes status.health
// and every Model's availability, and raises the two events once per
// transition. On every replica it keeps its own view, which View
// combines with the published state by taking the worse.
type Health struct {
	o HealthOptions

	mu        sync.Mutex
	held      bool
	providers providers                  // as of the last tick, by id and by name
	published map[string]v1.HealthStatus // the stored status, by provider id
	machines  map[string]*machine        // the holder's counters, by provider id
	local     map[string]*machine        // this replica's own view, by provider id
}

// NewHealth constructs the job; Run starts it.
func NewHealth(o HealthOptions) *Health {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Holder == "" {
		o.Holder = defaultHolder()
	}
	if o.NewID == nil {
		o.NewID = newEventID(o.Now)
	}
	if o.TunnelTTL <= 0 {
		o.TunnelTTL = defaultTunnelTTL
	}
	h := &Health{o: o, providers: providers{}, published: map[string]v1.HealthStatus{}, machines: map[string]*machine{}, local: map[string]*machine{}}
	if o.Metrics != nil {
		o.Metrics.Gauge(MetricProviderHealth, providerHealthHelp, h.gauge)
	}
	return h
}

// gauge is the scrape-time collector of lux_provider_health: four
// samples per Provider this replica read on its last tick, 1 on the
// state View gives and 0 on the other three, sorted so an exposition is
// stable. A Provider an operator deleted is gone from the tick's list
// and so from the family, which is the same moment this replica stops
// acting on it; a replica that knows no Provider writes no series, as
// the gauge has nothing to hold at zero.
func (h *Health) gauge() []metrics.LabeledValue {
	h.mu.Lock()
	defer h.mu.Unlock()
	states := make(map[string]v1.HealthState, len(h.providers))
	for ref, p := range h.providers {
		if ref != p.Status.ID {
			continue // the tick's list is by id and by name; one series set per Provider
		}
		states[p.Metadata.Name] = h.viewOf(p.Status.ID)
	}
	out := make([]metrics.LabeledValue, 0, len(states)*len(healthStates))
	for _, name := range slices.Sorted(maps.Keys(states)) {
		for _, state := range healthStates {
			value := 0.0
			if state == states[name] {
				value = 1
			}
			out = append(out, metrics.LabeledValue{Labels: map[string]string{"provider": name, "state": string(state)}, Value: value})
		}
	}
	return out
}

// defaultTunnelTTL is LUX_TUNNEL_REGISTRY_TTL's default (spec 013).
const defaultTunnelTTL = 30 * time.Second

// Run acquires and renews the lease, ticks on the interval, and releases
// the lease when ctx ends.
func (h *Health) Run(ctx context.Context) {
	h.acquire(ctx)
	h.Tick(ctx)
	renew := time.NewTicker(leaseRenew)
	defer renew.Stop()
	probe := time.NewTicker(h.o.Interval)
	defer probe.Stop()
	for {
		select {
		case <-ctx.Done():
			h.release(ctx)
			return
		case <-renew.C:
			h.acquire(ctx)
		case <-probe.C:
			h.Tick(ctx)
		}
	}
}

// acquire takes or renews the lease and records whether it is held.
func (h *Health) acquire(ctx context.Context) {
	held, err := h.o.Store.Leases().Acquire(ctx, store.LeaseHealth, h.o.Holder, store.LeaseTTL)
	if err != nil {
		h.o.Logger.ErrorContext(ctx, "health: acquiring the lease", "holder", h.o.Holder, "err", err)
		held = false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if held && !h.held {
		// A new holder starts its counts at zero; the stored state is
		// the one that is authoritative.
		h.machines = map[string]*machine{}
	}
	h.held = held
}

// release gives the lease back on a clean stop, so the next holder does
// not wait out the TTL. The stop has fired, so the call runs on a
// context that keeps the request's values and outlives its cancellation.
func (h *Health) release(ctx context.Context) {
	h.mu.Lock()
	held := h.held
	h.held = false
	h.mu.Unlock()
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := h.o.Store.Leases().Release(ctx, store.LeaseHealth, h.o.Holder); err != nil {
		h.o.Logger.ErrorContext(ctx, "health: releasing the lease", "holder", h.o.Holder, "err", err)
	}
}

// Held reports whether this replica holds the lease as of its last
// acquire.
func (h *Health) Held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

// Tick reads the Providers and their published states on every replica,
// and on the holder probes each probe-mode Provider, publishes the
// outcome, resets a none-mode Provider to Unknown, and fills the status
// of any Model that has none yet.
func (h *Health) Tick(ctx context.Context) {
	byRef, list, err := listProviders(ctx, h.o.Store)
	if err != nil {
		h.o.Logger.ErrorContext(ctx, "health: reading the Providers", "err", err)
		return
	}
	h.mu.Lock()
	h.providers = byRef
	for _, p := range list {
		if p.Status.Health != nil {
			h.published[p.Status.ID] = *p.Status.Health
		} else {
			delete(h.published, p.Status.ID)
		}
	}
	held := h.held
	h.mu.Unlock()
	if !held {
		return
	}
	for _, p := range list {
		if p.Spec.Tunnel {
			// The registry is a tunneled Provider's fourth source of health
			// (spec 013): no live row is Unreachable at once, and a live
			// row is what the mode's own signal applies over. Only the
			// probe has a signal of its own to add on this tick.
			if !h.tunnelTick(ctx, p) || p.Spec.Health.Mode != v1.HealthProbe {
				continue
			}
		}
		switch p.Spec.Health.Mode {
		case v1.HealthProbe:
			failed, lastError := h.Probe(ctx, p)
			h.mu.Lock()
			h.observeLocal(p.Status.ID, failed)
			h.record(ctx, p, failed, lastError, true)
			h.mu.Unlock()
		case v1.HealthNone:
			h.mu.Lock()
			h.resetToUnknown(ctx, p)
			h.mu.Unlock()
		}
	}
	h.fillModels(ctx)
}

// Probe calls the Provider's models route, first page only, within the
// probe budget. A transport failure, a timeout, or a 5xx is a failure;
// any other complete response is a success, and a 401 or 403 is a
// success whose lastError says the credential was refused. It is the
// providers row of luxd check as well as the job's own step, and needs
// no store or lease.
func (h *Health) Probe(ctx context.Context, p *v1.Provider) (failed bool, lastError string) {
	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	rawURL, err := modelsURL(p, "")
	if err != nil {
		return true, err.Error()
	}
	resp, value, err := upstream{clients: h.o.Clients, credentials: h.o.Credentials}.get(ctx, p, rawURL)
	if err != nil {
		return true, "GET " + rawURL + ": " + err.Error()
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode >= 500:
		return true, "GET " + rawURL + ": upstream status " + strconv.Itoa(resp.StatusCode) + ": " + excerpt(redact(body, value))
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return false, "credential refused: " + strconv.Itoa(resp.StatusCode)
	default:
		return false, ""
	}
}

// Observe is the passive half: one data plane outcome toward a Provider,
// failed for a transport error, a timeout, or a 5xx. It feeds this
// replica's own view always and the published state when this replica
// holds the lease and the Provider's mode is probe or passive.
func (h *Health) Observe(providerID string, failed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observeLocal(providerID, failed)
	p := h.providers[providerID]
	if !h.held || p == nil || p.Spec.Health.Mode == v1.HealthNone {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeBudget)
	defer cancel()
	h.record(ctx, p, failed, "", false)
}

// observeLocal steps this replica's own view. The caller holds the lock.
func (h *Health) observeLocal(providerID string, failed bool) {
	m := h.local[providerID]
	if m == nil {
		m = &machine{state: v1.HealthUnknown}
		h.local[providerID] = m
	}
	m.step(failed)
}

// View is the state this replica acts on for a Provider: the worse of
// the published state and its own view.
func (h *Health) View(providerID string) v1.HealthState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.viewOf(providerID)
}

// viewOf is View with the lock held.
func (h *Health) viewOf(providerID string) v1.HealthState {
	published := v1.HealthUnknown
	if s, ok := h.published[providerID]; ok && s.State != "" {
		published = s.State
	}
	local := v1.HealthUnknown
	if m, ok := h.local[providerID]; ok {
		local = m.state
	}
	return worse(published, local)
}

// record is the holder's step: fold the outcome into the Provider's
// counter, write status.health, and on a change of state refresh the
// Models and raise the event. probed says a probe produced the outcome,
// which stamps lastProbeAt and writes lastError; a passive outcome moves
// the state and leaves the probe's detail alone. The caller holds the
// lock.
func (h *Health) record(ctx context.Context, p *v1.Provider, failed bool, lastError string, probed bool) {
	changed := h.machineFor(p.Status.ID).step(failed)
	h.publish(ctx, p, changed, lastError, probed)
}

// recordUnreachable is the registry's verdict on a tunneled Provider
// with no live session: Unreachable at once, without the probe counter,
// published exactly as a counted transition is.
func (h *Health) recordUnreachable(ctx context.Context, p *v1.Provider, lastError string) {
	changed := h.machineFor(p.Status.ID).force(v1.HealthUnreachable)
	h.publish(ctx, p, changed, lastError, true)
}

// machineFor is the holder's counter for a Provider, started at the
// published state. The caller holds the lock.
func (h *Health) machineFor(id string) *machine {
	m := h.machines[id]
	if m == nil {
		m = &machine{state: h.published[id].State}
		h.machines[id] = m
	}
	return m
}

// publish writes status.health after the machine moved, and on a change
// of state refreshes the Models and raises the event. The caller holds
// the lock.
func (h *Health) publish(ctx context.Context, p *v1.Provider, changed bool, lastError string, probed bool) {
	id := p.Status.ID
	prev := h.published[id]
	m := h.machines[id]
	now := h.o.Now().UTC()
	next := prev
	next.State = m.state
	if probed {
		next.LastProbeAt = now
		next.LastError = lastError
	}
	if changed {
		next.Since = now
	}
	if err := h.o.Store.Objects().PutStatus(ctx, v1.KindProvider, id, store.ProviderObserved{Health: &next}); err != nil {
		h.o.Logger.ErrorContext(ctx, "health: writing status.health", "provider", id, "state", next.State, "err", err)
		return
	}
	h.published[id] = next
	if !changed {
		return
	}
	h.o.Logger.InfoContext(ctx, "health: state changed", "provider", id, "name", p.Metadata.Name, "from", prev.State, "to", next.State, "lastError", next.LastError)
	targets, err := refreshModels(ctx, h.o.Store, p, h.stateOf)
	if err != nil {
		h.o.Logger.ErrorContext(ctx, "health: refreshing the Models", "provider", id, "err", err)
	}
	switch next.State {
	case v1.HealthUnreachable:
		data := map[string]any{"since": next.Since, "lastError": next.LastError, "targets": targets}
		if err := appendEvent(ctx, h.o.Store.Journal(), eventProviderUnreachable, reasonProbe, p, data, now, h.o.NewID); err != nil {
			h.o.Logger.ErrorContext(ctx, "health: raising the event", "provider", id, "err", err)
		}
	case v1.HealthHealthy:
		data := map[string]any{"since": next.Since}
		if prev.State == v1.HealthUnreachable && !prev.Since.IsZero() {
			data["wasUnreachableFor"] = now.Sub(prev.Since).String()
		}
		if err := appendEvent(ctx, h.o.Store.Journal(), eventProviderHealthy, reasonProbe, p, data, now, h.o.NewID); err != nil {
			h.o.Logger.ErrorContext(ctx, "health: raising the event", "provider", id, "err", err)
		}
	}
}

// tunnelTick is the registry's reading of a tunneled Provider on the
// holder: status.tunnel is written from the row, or Disconnected when
// there is none; no live row is Unreachable at once, without the probe
// counter; and a live row lets the mode's own signal apply, folding one
// success first when the Provider was Unreachable or Unknown, because a
// passive Provider that left selection would otherwise never observe
// the success that brings it back, and a none-mode Provider has no
// other signal than the row. live reports whether a row is live.
func (h *Health) tunnelTick(ctx context.Context, p *v1.Provider) (live bool) {
	row, err := h.o.Store.Tunnels().Get(ctx, p.Status.ID)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.publishTunnel(ctx, p, nil)
		h.recordUnreachable(ctx, p, "no live tunnel session")
		return false
	case err != nil:
		h.o.Logger.ErrorContext(ctx, "health: reading the tunnel registry", "provider", p.Status.ID, "err", err)
		return false
	}
	h.publishTunnel(ctx, p, &row)
	if p.Spec.Health.Mode != v1.HealthProbe {
		switch h.machineFor(p.Status.ID).state {
		case v1.HealthUnknown, v1.HealthUnreachable, "":
			h.record(ctx, p, false, "", true)
		}
	}
	return true
}

// publishTunnel writes status.tunnel when it differs from what is
// stored: Connected with the row's session, subject, agent, connect
// time, and last heartbeat, or Disconnected since this tick. The caller
// holds the lock.
func (h *Health) publishTunnel(ctx context.Context, p *v1.Provider, row *store.Tunnel) {
	now := h.o.Now().UTC()
	next := v1.TunnelStatus{State: v1.TunnelDisconnected, Since: now}
	if row != nil {
		next = v1.TunnelStatus{
			State: v1.TunnelConnected, Session: row.Session, Subject: row.Subject, Agent: row.Agent,
			Since: row.ConnectedAt.UTC(), LastHeartbeatAt: row.ExpiresAt.Add(-h.o.TunnelTTL).UTC(),
		}
	}
	if prev := p.Status.Tunnel; prev != nil {
		if prev.State == v1.TunnelDisconnected && next.State == v1.TunnelDisconnected {
			return
		}
		if prev.State == v1.TunnelConnected && next.State == v1.TunnelConnected && prev.Session == next.Session && prev.LastHeartbeatAt.Equal(next.LastHeartbeatAt) {
			return
		}
	}
	if err := h.o.Store.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Tunnel: &next}); err != nil {
		h.o.Logger.ErrorContext(ctx, "health: writing status.tunnel", "provider", p.Status.ID, "state", next.State, "err", err)
		return
	}
	p.Status.Tunnel = &next
}

// resetToUnknown writes Unknown once for a Provider whose mode is none
// and whose stored state says otherwise, which is what turning the probe
// off leaves behind. The caller holds the lock.
func (h *Health) resetToUnknown(ctx context.Context, p *v1.Provider) {
	id := p.Status.ID
	prev, stored := h.published[id]
	if !stored || prev.State == v1.HealthUnknown {
		return
	}
	next := v1.HealthStatus{State: v1.HealthUnknown, Since: h.o.Now().UTC(), LastProbeAt: prev.LastProbeAt}
	if err := h.o.Store.Objects().PutStatus(ctx, v1.KindProvider, id, store.ProviderObserved{Health: &next}); err != nil {
		h.o.Logger.ErrorContext(ctx, "health: writing status.health", "provider", id, "state", next.State, "err", err)
		return
	}
	h.published[id] = next
	delete(h.machines, id)
	if _, err := refreshModels(ctx, h.o.Store, p, h.stateOf); err != nil {
		h.o.Logger.ErrorContext(ctx, "health: refreshing the Models", "provider", id, "err", err)
	}
}

// stateOf is the published state of the Provider a target names, by id
// or by name, and Unknown for one this replica has not read. The caller
// holds the lock.
func (h *Health) stateOf(ref string) v1.HealthState {
	p := h.providers[ref]
	if p == nil {
		return v1.HealthUnknown
	}
	if s, ok := h.published[p.Status.ID]; ok && s.State != "" {
		return s.State
	}
	return v1.HealthUnknown
}

// fillModels writes the status of every Model that has none yet, a
// Model declared or discovered since the last tick, so a door's model
// list carries it before any Provider changes state.
func (h *Health) fillModels(ctx context.Context) {
	models, _, err := h.o.Store.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	if err != nil {
		h.o.Logger.ErrorContext(ctx, "health: reading the Models", "err", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, o := range models {
		m, ok := o.(*v1.Model)
		if !ok || m.Status.Available != nil {
			continue
		}
		if err := h.o.Store.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, observedModel(m, h.stateOf)); err != nil && !errors.Is(err, store.ErrNotFound) {
			h.o.Logger.ErrorContext(ctx, "health: writing the status of a Model", "model", m.Status.ID, "err", err)
		}
	}
}
