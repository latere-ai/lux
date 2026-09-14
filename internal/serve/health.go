// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// probeBudget is the deadline of one probe, in place of the Provider's
// timeout: a probe measures reachability, not a generation.
const probeBudget = 5 * time.Second

// HealthOptions is what the health job runs under.
type HealthOptions struct {
	Store       store.Store
	Clients     clientSource
	Credentials credentialSource
	// Interval is LUX_HEALTH_INTERVAL, the probe period.
	Interval time.Duration
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
	return &Health{o: o, providers: providers{}, published: map[string]v1.HealthStatus{}, machines: map[string]*machine{}, local: map[string]*machine{}}
}

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
			continue // the tunnel registry publishes a tunnelled Provider's state (spec 013)
		}
		switch p.Spec.Health.Mode {
		case v1.HealthProbe:
			failed, lastError := h.probe(ctx, p)
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

// probe calls the Provider's models route, first page only, within the
// probe budget. A transport failure, a timeout, or a 5xx is a failure;
// any other complete response is a success, and a 401 or 403 is a
// success whose lastError says the credential was refused.
func (h *Health) probe(ctx context.Context, p *v1.Provider) (failed bool, lastError string) {
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
	if !h.held || p == nil || p.Spec.Health.Mode == v1.HealthNone || p.Spec.Tunnel {
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
	id := p.Status.ID
	prev := h.published[id]
	m := h.machines[id]
	if m == nil {
		m = &machine{state: prev.State}
		h.machines[id] = m
	}
	changed := m.step(failed)
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
