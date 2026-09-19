// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The journal events that make the lease holder list a Provider at once,
// and the changed paths of an update that do.
const (
	eventProviderCreated = "provider.created"
	eventProviderUpdated = "provider.updated"
)

// defaultTail is how often the lease holder reads the journal.
const defaultTail = time.Second

// DiscoveryOptions is what the discovery job runs under.
type DiscoveryOptions struct {
	Store       store.Store
	Clients     clientSource
	Credentials credentialSource
	// Interval is LUX_DISCOVERY_INTERVAL, the list period, with up to
	// ten percent of jitter added to each wait.
	Interval time.Duration
	// Tail is how often the holder reads the journal; zero is one second.
	Tail time.Duration
	// Holder names this replica in the lease row; empty is the host and
	// the process id.
	Holder string
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints object and event ids from a prefix; nil mints ULIDs
	// from Now.
	NewID func(prefix string) string
	// Jitter returns a fraction in [0, 1); nil is the default random
	// source.
	Jitter func() float64
}

// Discovery is the discovery job of spec 005 on one replica: under the
// discovery lease it lists every auto-mode Provider on the interval and
// at once when the journal reports a Provider created or re-addressed,
// and resolves each surviving name into a discovered Model.
type Discovery struct {
	o DiscoveryOptions

	mu    sync.Mutex
	held  bool
	after int64 // the journal sequence the tail has read up to
}

// NewDiscovery constructs the job; Run starts it.
func NewDiscovery(o DiscoveryOptions) *Discovery {
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
		o.NewID = func(prefix string) string { return v1.NewID(prefix, o.Now(), nil) }
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64
	}
	if o.Tail <= 0 {
		o.Tail = defaultTail
	}
	return &Discovery{o: o}
}

// Run acquires and renews the lease, lists on the jittered interval,
// tails the journal while holding the lease, and releases it when ctx
// ends.
func (d *Discovery) Run(ctx context.Context) {
	d.acquire(ctx)
	d.Tick(ctx)
	renew := time.NewTicker(leaseRenew)
	defer renew.Stop()
	tail := time.NewTicker(d.o.Tail)
	defer tail.Stop()
	next := time.NewTimer(d.wait())
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			d.release(ctx)
			return
		case <-renew.C:
			d.acquire(ctx)
		case <-tail.C:
			d.Tail(ctx)
		case <-next.C:
			d.Tick(ctx)
			next.Reset(d.wait())
		}
	}
}

// wait is the interval with up to ten percent of jitter, so replicas
// that took the lease in turn do not list in lockstep.
func (d *Discovery) wait() time.Duration {
	return d.o.Interval + time.Duration(float64(d.o.Interval)*0.1*d.o.Jitter())
}

// acquire takes or renews the lease. A replica that just became the
// holder skips the journal to its end first, so the events of the past
// are not replayed as lists.
func (d *Discovery) acquire(ctx context.Context) {
	held, err := d.o.Store.Leases().Acquire(ctx, store.LeaseDiscovery, d.o.Holder, store.LeaseTTL)
	if err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: acquiring the lease", "holder", d.o.Holder, "err", err)
		held = false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if held && !d.held {
		d.skipJournal(ctx)
	}
	d.held = held
}

// release gives the lease back on a clean stop, on a context that
// outlives the stop's cancellation.
func (d *Discovery) release(ctx context.Context) {
	d.mu.Lock()
	held := d.held
	d.held = false
	d.mu.Unlock()
	if !held {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := d.o.Store.Leases().Release(ctx, store.LeaseDiscovery, d.o.Holder); err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: releasing the lease", "holder", d.o.Holder, "err", err)
	}
}

// Held reports whether this replica holds the lease as of its last
// acquire.
func (d *Discovery) Held() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.held
}

// skipJournal moves the tail to the journal's end. The caller holds the
// lock.
func (d *Discovery) skipJournal(ctx context.Context) {
	const batch = 500
	for {
		events, err := d.o.Store.Journal().Since(ctx, d.after, batch)
		if err != nil {
			d.o.Logger.ErrorContext(ctx, "discovery: reading the journal", "err", err)
			return
		}
		if len(events) == 0 {
			return
		}
		d.after = events[len(events)-1].GSeq
		if len(events) < batch {
			return
		}
	}
}

// Tick lists every auto-mode Provider when this replica holds the lease.
func (d *Discovery) Tick(ctx context.Context) {
	if !d.Held() {
		return
	}
	_, list, err := listProviders(ctx, d.o.Store)
	if err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: reading the Providers", "err", err)
		return
	}
	for _, p := range list {
		if p.Spec.Discovery.Mode == v1.DiscoveryAuto {
			d.List(ctx, p)
		}
	}
}

// Tail reads the journal since the last read and lists at once every
// Provider it reports created, or updated at spec.baseURL or under
// spec.credential, when this replica holds the lease.
func (d *Discovery) Tail(ctx context.Context) {
	if !d.Held() {
		return
	}
	d.mu.Lock()
	after := d.after
	d.mu.Unlock()
	events, err := d.o.Store.Journal().Since(ctx, after, 200)
	if err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: reading the journal", "err", err)
		return
	}
	for _, e := range events {
		d.mu.Lock()
		d.after = e.GSeq
		d.mu.Unlock()
		if !relist(e) {
			continue
		}
		obj, _, err := d.o.Store.Objects().Get(ctx, v1.KindProvider, e.ObjectID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			d.o.Logger.ErrorContext(ctx, "discovery: reading a Provider the journal named", "provider", e.ObjectID, "err", err)
			continue
		}
		if p, ok := obj.(*v1.Provider); ok && p.Spec.Discovery.Mode == v1.DiscoveryAuto {
			d.List(ctx, p)
		}
	}
}

// relist reports whether an event calls for an immediate list: a
// Provider created, or updated at spec.baseURL or under spec.credential.
// The update's changed paths are read from data as a list of paths or
// as an object whose paths member is one.
func relist(e store.Event) bool {
	switch e.Type {
	case eventProviderCreated:
		return true
	case eventProviderUpdated:
	default:
		return false
	}
	var rec struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(e.Payload, &rec); err != nil || len(rec.Data) == 0 {
		return false
	}
	var paths []string
	if err := json.Unmarshal(rec.Data, &paths); err != nil {
		var obj struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal(rec.Data, &obj); err != nil {
			return false
		}
		paths = obj.Paths
	}
	for _, path := range paths {
		if (path == "metadata.labels" || strings.HasPrefix(path, "metadata.labels.")) || path == "spec.baseURL" || strings.HasPrefix(path, "spec.credential") {
			return true
		}
	}
	return false
}

// List reads one Provider's model list and makes it the Provider's
// discovered Models: new names are resolved and created, names the list
// no longer carries are deleted, and a declared Model of a name is never
// touched. A failed or empty list changes no object and is written to
// status.health.lastError.
func (d *Discovery) List(ctx context.Context, p *v1.Provider) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOf(p))
	defer cancel()
	candidates, err := upstream{clients: d.o.Clients, credentials: d.o.Credentials}.listModels(ctx, p)
	if err == nil && len(candidates) == 0 {
		err = errors.New("the model list is empty")
	}
	if err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: the list failed, the catalogue stands", "provider", p.Status.ID, "name", p.Metadata.Name, "err", err)
		d.recordFailure(ctx, p, err)
		return
	}
	result, err := d.apply(ctx, p, filter(p, candidates))
	if err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: writing the catalogue", "provider", p.Status.ID, "name", p.Metadata.Name, "err", err)
		return
	}
	d.o.Logger.InfoContext(ctx, "discovery: listed", "provider", p.Status.ID, "name", p.Metadata.Name, "models", result.count, "created", result.created, "removed", result.removed, "refused", len(result.warnings))
}

// filter applies include, then exclude, to the candidates.
func filter(p *v1.Provider, candidates []candidate) []candidate {
	var out []candidate
	for _, c := range candidates {
		if len(p.Spec.Discovery.Include) > 0 && !matchesAny(p.Spec.Discovery.Include, c.name) {
			continue
		}
		if matchesAny(p.Spec.Discovery.Exclude, c.name) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func matchesAny(globs []string, name string) bool {
	for _, g := range globs {
		if manifest.Match(g, name) {
			return true
		}
	}
	return false
}

// listResult is what one successful list did.
type listResult struct {
	count, created, removed int
	warnings                []string
}

// apply writes the surviving candidates as the Provider's discovered
// Models in one transaction: a resolve per name, a create or an update
// per changed Model, a delete per Model the list dropped, the events of
// each, and status.discovered.
func (d *Discovery) apply(ctx context.Context, p *v1.Provider, candidates []candidate) (listResult, error) {
	var result listResult
	now := d.o.Now().UTC()
	err := d.o.Store.Transact(ctx, func(tx store.Store) error {
		result = listResult{warnings: []string{}}
		existing, _, err := tx.Objects().List(ctx, v1.KindModel, store.Filter{Source: string(v1.SourceDiscovered), Provider: p.Status.ID}, store.Page{})
		if err != nil {
			return fmt.Errorf("listing the discovered Models: %w", err)
		}
		byUpstream := map[string]*v1.Model{}
		for _, o := range existing {
			if m, ok := o.(*v1.Model); ok && len(m.Spec.Targets) == 1 {
				byUpstream[m.Spec.Targets[0].Model] = m
			}
		}
		seen := map[string]bool{}
		stateOf := func(string) v1.HealthState {
			if p.Status.Health != nil {
				return p.Status.Health.State
			}
			return v1.HealthUnknown
		}
		for _, c := range candidates {
			if seen[c.name] {
				continue
			}
			seen[c.name] = true
			name := p.Metadata.Name + "/" + c.name
			if shadowed, err := declared(ctx, tx, name); err != nil {
				return err
			} else if shadowed {
				result.count++
				continue
			}
			old := byUpstream[c.name]
			m, err := d.resolve(ctx, p, c, old, now)
			if err != nil {
				result.warnings = append(result.warnings, "upstream model "+c.name+" was not declared: "+err.Error())
				continue
			}
			result.count++
			switch {
			case old == nil:
				m.Status.ID = d.o.NewID(v1.PrefixModel)
				if _, err := tx.Objects().Put(ctx, m, 0); err != nil {
					return fmt.Errorf("creating Model %s: %w", name, err)
				}
				if err := tx.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, observedModel(m, stateOf)); err != nil {
					return fmt.Errorf("writing the status of Model %s: %w", name, err)
				}
				if err := appendEvent(ctx, tx.Journal(), eventModelDiscovered, reasonDiscovery, m, map[string]any{"provider": p.Metadata.Name, "upstreamModel": c.name}, now, func() string { return d.o.NewID(v1.PrefixEvent) }); err != nil {
					return err
				}
				result.created++
			case !sameShape(old, m):
				m.Status.ID = old.Status.ID
				if _, err := tx.Objects().Put(ctx, m, old.Status.Version); err != nil {
					return fmt.Errorf("updating Model %s: %w", name, err)
				}
			}
		}
		for upstreamName, old := range byUpstream {
			if seen[upstreamName] {
				continue
			}
			if err := tx.Objects().Delete(ctx, v1.KindModel, old.Status.ID); err != nil {
				return fmt.Errorf("deleting Model %s: %w", old.Metadata.Name, err)
			}
			if err := appendEvent(ctx, tx.Journal(), eventModelRemoved, reasonDiscovery, old, map[string]any{"provider": p.Metadata.Name, "upstreamModel": upstreamName}, now, func() string { return d.o.NewID(v1.PrefixEvent) }); err != nil {
				return err
			}
			result.removed++
		}
		discovered := &v1.DiscoveredStatus{Count: result.count, At: now, Warnings: result.warnings}
		if err := tx.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Discovered: discovered}); err != nil {
			return fmt.Errorf("writing status.discovered: %w", err)
		}
		return nil
	})
	return result, err
}

// declared reports whether a live declared Model has the name, which
// shadows the discovered one: discovery never writes or deletes it.
func declared(ctx context.Context, tx store.Store, name string) (bool, error) {
	obj, _, err := tx.Objects().ByName(ctx, v1.KindModel, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading Model %s: %w", name, err)
	}
	m, ok := obj.(*v1.Model)
	return ok && m.Status.Source == v1.SourceDeclared, nil
}

// resolve runs manifest.Resolve over the discovered Model of one
// candidate under the Provider's owner: one target, weight 100, priority
// 0, fallback never, no pricing, the default modalities unless the
// upstream said the model embeds and nothing else, and source
// discovered.
func (d *Discovery) resolve(ctx context.Context, p *v1.Provider, c candidate, old *v1.Model, now time.Time) (*v1.Model, error) {
	weight := 100
	in := &v1.Model{
		Metadata: v1.ObjectMeta{Name: p.Metadata.Name + "/" + c.name, Labels: maps.Clone(p.Metadata.Labels)},
		Spec: v1.ModelSpec{
			Targets:  []v1.Target{{Provider: p.Metadata.Name, Model: c.name, Weight: &weight, Priority: 0}},
			Fallback: v1.FallbackNever,
		},
	}
	if c.embedding {
		in.Spec.Modalities.Output = []v1.Modality{v1.ModalityEmbedding}
	}
	var existing v1.Object
	if old != nil {
		existing = old
	}
	resolved, err := manifest.Resolve(ctx, in, manifest.Options{
		Actor:    manifest.Actor{Subject: p.Status.Owner},
		Lookup:   providerLookup{p: p},
		Existing: existing,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		return nil, err
	}
	m, ok := resolved.Object.(*v1.Model)
	if !ok {
		return nil, fmt.Errorf("manifest.Resolve returned a %T", resolved.Object)
	}
	m.Status.Owner = p.Status.Owner
	m.Status.Source = v1.SourceDiscovered
	return m, nil
}

// providerLookup answers the one Provider a discovered Model names.
type providerLookup struct{ p *v1.Provider }

func (l providerLookup) Provider(_ context.Context, nameOrID string) (*v1.Provider, error) {
	if nameOrID == l.p.Metadata.Name || nameOrID == l.p.Status.ID {
		return l.p, nil
	}
	return nil, manifest.ErrNotFound
}

func (providerLookup) Budget(context.Context, string) (*v1.Budget, error) {
	return nil, manifest.ErrNotFound
}

func (providerLookup) Models(context.Context, string) ([]v1.ModelRef, error) { return nil, nil }

// sameShape reports whether two Models have one metadata and one spec in
// their JSON form, so an unchanged discovered Model is left at its
// version.
func sameShape(a, b *v1.Model) bool {
	return shapeOf(a) == shapeOf(b)
}

func shapeOf(m *v1.Model) string {
	data, err := json.Marshal(struct {
		Metadata v1.ObjectMeta `json:"metadata"`
		Spec     v1.ModelSpec  `json:"spec"`
	}{m.Metadata, m.Spec})
	if err != nil {
		return ""
	}
	return string(data)
}

// recordFailure writes the list's failure to status.health.lastError,
// leaving the rest of the health block as the health job wrote it.
func (d *Discovery) recordFailure(ctx context.Context, p *v1.Provider, cause error) {
	obj, _, err := d.o.Store.Objects().Get(ctx, v1.KindProvider, p.Status.ID)
	if err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: reading the Provider", "provider", p.Status.ID, "err", err)
		return
	}
	current, ok := obj.(*v1.Provider)
	if !ok {
		return
	}
	next := v1.HealthStatus{State: v1.HealthUnknown}
	if current.Status.Health != nil {
		next = *current.Status.Health
	}
	next.LastError = "model list: " + cause.Error()
	if err := d.o.Store.Objects().PutStatus(ctx, v1.KindProvider, p.Status.ID, store.ProviderObserved{Health: &next}); err != nil {
		d.o.Logger.ErrorContext(ctx, "discovery: writing status.health.lastError", "provider", p.Status.ID, "err", err)
	}
}
