// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The catalog snapshot of spec 036: every live Model and Provider and
// every sealed credential row, held per replica so the doors read no
// store on the data plane. The journal tail keeps it current, a full
// reload every LUX_CATALOG_RELOAD brings in the writes no row names,
// and every change swaps a whole new snapshot, so a lookup never sees
// half of one.
const (
	// DefaultCatalogReload is LUX_CATALOG_RELOAD's default.
	DefaultCatalogReload = 30 * time.Second
	// MetricCatalogAge is the seconds since the snapshot last matched the
	// store.
	MetricCatalogAge = "lux_catalog_age_seconds"
	// MetricCatalogReloads counts reloads by trigger and result.
	MetricCatalogReloads = "lux_catalog_reloads_total"
	// MetricCatalogObjects is the objects the snapshot holds, by kind.
	MetricCatalogObjects = "lux_catalog_objects"
	// targetedLimit is the most objects one tail batch reloads one by
	// one; a batch that names more, such as the first walk of the
	// journal at start or a discovery pass over a large upstream, is one
	// full reload instead.
	targetedLimit = 64
)

// The triggers a reload is counted under.
const (
	triggerStart    = "start"
	triggerTail     = "tail"
	triggerBackstop = "backstop"
	triggerSIGHUP   = "sighup"
)

// JournalFollower takes each batch of journal rows the Key cache's tail
// reads, after the cache has acted on it; an empty batch is a tail read
// that found nothing new.
type JournalFollower interface {
	Follow(ctx context.Context, events []store.Event)
}

// CatalogSnapshotOptions is what the snapshot runs under.
type CatalogSnapshotOptions struct {
	// Store is where every reload reads from, and the fallback of every
	// lookup until the first load has succeeded.
	Store store.Store
	// Keys opens a sealed credential row. Nil holds no credential rows,
	// which is the file mode, whose values are read from the environment
	// and never sealed; Credential is then not to be called.
	Keys *secrets.Keyring
	// Reload is LUX_CATALOG_RELOAD, the backstop's interval; zero is
	// DefaultCatalogReload.
	Reload time.Duration
	// Retry is how often a failed load or a failed targeted reload is
	// retried as a full reload; zero is one second, the tail's interval.
	Retry time.Duration
	// Metrics receives the three metrics; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// CatalogSnapshot is spec 004's Catalog and CredentialSource over a
// per-replica copy of the catalog.
type CatalogSnapshot struct {
	o        CatalogSnapshotOptions
	fallback *Catalog
	reloads  *metrics.Counter

	cur     atomic.Pointer[catalogState]
	mu      sync.Mutex // serializes every build and swap
	full    atomic.Bool
	matched atomic.Int64 // unix nanoseconds of the last match with the store
}

// catalogState is one whole snapshot, never changed once published.
type catalogState struct {
	models    map[string]*v1.Model // by name
	modelIDs  map[string]string    // id to name
	ordered   []*v1.Model          // by name ascending
	providers map[string]*v1.Provider
	byID      map[string]*v1.Provider
	// sealed is every Provider's credential row by Provider id; a nil
	// value is a Provider that stores none.
	sealed map[string]*store.Sealed
}

// NewCatalogSnapshot constructs the snapshot; it holds nothing until
// Load or Run succeeds, and until then every lookup reads the store.
func NewCatalogSnapshot(o CatalogSnapshotOptions) *CatalogSnapshot {
	if o.Reload <= 0 {
		o.Reload = DefaultCatalogReload
	}
	if o.Retry <= 0 {
		o.Retry = defaultTail
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &CatalogSnapshot{o: o, fallback: &Catalog{Objects: o.Store.Objects()}}
	if o.Metrics != nil {
		c.reloads = o.Metrics.Counter(MetricCatalogReloads, "Catalog snapshot reloads by trigger and result.")
		o.Metrics.Gauge(MetricCatalogAge, "Seconds since this replica's catalog snapshot last matched the store.", func() []metrics.LabeledValue {
			return []metrics.LabeledValue{{Labels: map[string]string{}, Value: c.Age().Seconds()}}
		})
		o.Metrics.Gauge(MetricCatalogObjects, "Objects this replica's catalog snapshot holds, by kind.", func() []metrics.LabeledValue {
			models, providers, credentials := c.counts()
			return []metrics.LabeledValue{
				{Labels: map[string]string{"kind": "Model"}, Value: float64(models)},
				{Labels: map[string]string{"kind": "Provider"}, Value: float64(providers)},
				{Labels: map[string]string{"kind": "credential"}, Value: float64(credentials)},
			}
		})
	}
	return c
}

// Loaded reports whether a first full load has succeeded.
func (c *CatalogSnapshot) Loaded() bool { return c.cur.Load() != nil }

// Ready is the readiness check of spec 036: nil once the first full
// load has succeeded, and it stays nil whatever the snapshot's age.
func (c *CatalogSnapshot) Ready(context.Context) error {
	if !c.Loaded() {
		return errors.New("the catalog snapshot has not loaded yet")
	}
	return nil
}

// Age is the time since the snapshot last matched the store: the later
// of the last successful full reload and the last tail read that left
// nothing to reload. It is zero before the first load.
func (c *CatalogSnapshot) Age() time.Duration {
	at := c.matched.Load()
	if at == 0 {
		return 0
	}
	return c.o.Now().Sub(time.Unix(0, at))
}

// counts is the number of Models, Providers, and credential rows held.
func (c *CatalogSnapshot) counts() (models, providers, credentials int) {
	s := c.cur.Load()
	if s == nil {
		return 0, 0, 0
	}
	for _, row := range s.sealed {
		if row != nil {
			credentials++
		}
	}
	return len(s.ordered), len(s.byID), credentials
}

// Model implements gateway.Catalog by exact name, as Catalog.Model does.
func (c *CatalogSnapshot) Model(ctx context.Context, name string) (*v1.Model, error) {
	s := c.cur.Load()
	if s == nil {
		return c.fallback.Model(ctx, name)
	}
	if name == "" || isID(name) {
		return nil, nil
	}
	return s.models[name], nil
}

// Models implements gateway.Catalog: every live Model by name ascending.
// The slice is the caller's; the Models are the snapshot's and are read,
// never changed.
func (c *CatalogSnapshot) Models(ctx context.Context) ([]*v1.Model, error) {
	s := c.cur.Load()
	if s == nil {
		return c.fallback.Models(ctx)
	}
	return slices.Clone(s.ordered), nil
}

// Provider implements gateway.Catalog, by prv_ id or by name.
func (c *CatalogSnapshot) Provider(ctx context.Context, nameOrID string) (*v1.Provider, error) {
	s := c.cur.Load()
	if s == nil {
		return c.fallback.Provider(ctx, nameOrID)
	}
	if nameOrID == "" {
		return nil, nil
	}
	if strings.HasPrefix(nameOrID, v1.PrefixProvider) {
		return s.byID[nameOrID], nil
	}
	return s.providers[nameOrID], nil
}

// Credential implements gateway.CredentialSource: the plaintext of the
// Provider's credential, opened from the sealed row the snapshot holds,
// so the plaintext lives only as long as the caller's request. A row
// that fails to open is read from the store once and opened again, which
// covers a row re-wrapped since the snapshot loaded it; a Provider the
// snapshot does not know yet is read from the store.
func (c *CatalogSnapshot) Credential(ctx context.Context, providerID string) ([]byte, error) {
	if c.o.Keys == nil {
		return nil, errors.New("the catalog snapshot holds no credentials in this mode")
	}
	if s := c.cur.Load(); s != nil {
		if row, known := s.sealed[providerID]; known {
			if row == nil {
				return nil, nil
			}
			if value, err := c.o.Keys.Open(providerID, *row); err == nil {
				return value, nil
			}
		}
	}
	return c.readCredential(ctx, providerID)
}

// readCredential opens the Provider's row as the store holds it now.
func (c *CatalogSnapshot) readCredential(ctx context.Context, providerID string) ([]byte, error) {
	row, err := c.o.Store.Credentials().Get(ctx, providerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credential of %s: %w", providerID, err)
	}
	return c.o.Keys.Open(providerID, row)
}

// Load reads the whole catalog and swaps it in, the first load and the
// backstop alike; a failure leaves the snapshot as it was.
func (c *CatalogSnapshot) Load(ctx context.Context, trigger string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadLocked(ctx, trigger)
}

func (c *CatalogSnapshot) loadLocked(ctx context.Context, trigger string) error {
	started := c.o.Now()
	next, err := c.readAll(ctx)
	if err != nil {
		c.count(trigger, "error")
		c.full.Store(true)
		return err
	}
	c.cur.Store(next)
	c.full.Store(false)
	c.matched.Store(started.UnixNano())
	c.count(trigger, "ok")
	return nil
}

// readAll reads every live Provider and Model and every credential row.
func (c *CatalogSnapshot) readAll(ctx context.Context) (*catalogState, error) {
	s := &catalogState{
		models: map[string]*v1.Model{}, modelIDs: map[string]string{},
		providers: map[string]*v1.Provider{}, byID: map[string]*v1.Provider{},
		sealed: map[string]*store.Sealed{},
	}
	providers, _, err := c.o.Store.Objects().List(ctx, v1.KindProvider, store.Filter{}, store.Page{})
	if err != nil {
		return nil, fmt.Errorf("listing the Providers: %w", err)
	}
	for _, o := range providers {
		p, ok := o.(*v1.Provider)
		if !ok {
			return nil, fmt.Errorf("listing the Providers: a %T among them", o)
		}
		s.putProvider(p)
	}
	if c.o.Keys != nil {
		ids, err := c.o.Store.Credentials().List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing the credentials: %w", err)
		}
		for _, id := range ids {
			if err := c.readSealed(ctx, s, id); err != nil {
				return nil, err
			}
		}
	}
	for id := range s.byID {
		if _, ok := s.sealed[id]; !ok {
			s.sealed[id] = nil
		}
	}
	models, err := c.listModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		s.putModel(m)
	}
	s.order()
	return s, nil
}

// listModels reads every live Model.
func (c *CatalogSnapshot) listModels(ctx context.Context) ([]*v1.Model, error) {
	objs, _, err := c.o.Store.Objects().List(ctx, v1.KindModel, store.Filter{}, store.Page{})
	if err != nil {
		return nil, fmt.Errorf("listing the Models: %w", err)
	}
	out := make([]*v1.Model, 0, len(objs))
	for _, o := range objs {
		m, ok := o.(*v1.Model)
		if !ok {
			return nil, fmt.Errorf("listing the Models: a %T among them", o)
		}
		out = append(out, m)
	}
	return out, nil
}

// readSealed reads one Provider's row into s, nil for none.
func (c *CatalogSnapshot) readSealed(ctx context.Context, s *catalogState, providerID string) error {
	row, err := c.o.Store.Credentials().Get(ctx, providerID)
	if errors.Is(err, store.ErrNotFound) {
		s.sealed[providerID] = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the credential of %s: %w", providerID, err)
	}
	s.sealed[providerID] = &row
	return nil
}

// Follow implements JournalFollower: the Providers and Models a batch
// names are read again and swapped in, a batch that names more than
// targetedLimit objects is a full reload, and a failure leaves the
// snapshot as it was and marks it for a full reload at the next retry.
// A batch that names nothing, or whose reload succeeds with no full
// reload pending, is a match with the store.
func (c *CatalogSnapshot) Follow(ctx context.Context, rows []store.Event) {
	if !c.Loaded() {
		return
	}
	started := c.o.Now()
	models, providers := map[string]bool{}, map[string]bool{}
	for _, e := range rows {
		switch e.Type {
		case events.ModelCreated, events.ModelUpdated, events.ModelDeleted, events.ModelDiscovered, events.ModelRemoved:
			models[e.ObjectID] = true
		case events.ProviderCreated, events.ProviderUpdated, events.ProviderDeleted, events.ProviderUnreachable, events.ProviderHealthy:
			providers[e.ObjectID] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(models)+len(providers) > targetedLimit {
		if err := c.loadLocked(ctx, triggerTail); err != nil {
			c.o.Logger.ErrorContext(ctx, "catalog snapshot: reloading after the journal", "err", err)
		}
		return
	}
	if len(models)+len(providers) > 0 {
		if err := c.apply(ctx, models, providers); err != nil {
			c.count(triggerTail, "error")
			c.full.Store(true)
			c.o.Logger.ErrorContext(ctx, "catalog snapshot: reloading what the journal names", "err", err)
			return
		}
		c.count(triggerTail, "ok")
	}
	if !c.full.Load() {
		c.matched.Store(started.UnixNano())
	}
}

// apply builds the next snapshot from the current one with the named
// objects read again, and swaps it. A Provider's change reads the
// Provider, its credential row, and every Model, since a Model's
// availability and its targets' resolution follow the Provider. The
// caller holds the lock.
func (c *CatalogSnapshot) apply(ctx context.Context, models, providers map[string]bool) error {
	next := c.cur.Load().clone()
	for id := range providers {
		obj, _, err := c.o.Store.Objects().Get(ctx, v1.KindProvider, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			next.dropProvider(id)
			continue
		case err != nil:
			return fmt.Errorf("reading Provider %s: %w", id, err)
		}
		p, ok := obj.(*v1.Provider)
		if !ok {
			return fmt.Errorf("reading Provider %s: the store returned a %T", id, obj)
		}
		next.dropProvider(id)
		next.putProvider(p)
		if c.o.Keys != nil {
			if err := c.readSealed(ctx, next, id); err != nil {
				return err
			}
		}
	}
	if len(providers) > 0 {
		all, err := c.listModels(ctx)
		if err != nil {
			return err
		}
		next.models, next.modelIDs = map[string]*v1.Model{}, map[string]string{}
		for _, m := range all {
			next.putModel(m)
		}
	} else {
		for id := range models {
			obj, _, err := c.o.Store.Objects().Get(ctx, v1.KindModel, id)
			switch {
			case errors.Is(err, store.ErrNotFound):
				next.dropModel(id)
				continue
			case err != nil:
				return fmt.Errorf("reading Model %s: %w", id, err)
			}
			m, ok := obj.(*v1.Model)
			if !ok {
				return fmt.Errorf("reading Model %s: the store returned a %T", id, obj)
			}
			next.dropModel(id)
			next.putModel(m)
		}
	}
	next.order()
	c.cur.Store(next)
	return nil
}

// Run loads the snapshot until a load succeeds, retrying every Retry,
// then reloads the whole catalog every Reload, and retries a full reload
// every Retry while one is pending, until ctx ends.
func (c *CatalogSnapshot) Run(ctx context.Context) {
	retry := time.NewTicker(c.o.Retry)
	defer retry.Stop()
	backstop := time.NewTicker(c.o.Reload)
	defer backstop.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
			if !c.Loaded() || c.full.Load() {
				trigger := triggerTail
				if !c.Loaded() {
					trigger = triggerStart
				}
				if err := c.Load(ctx, trigger); err != nil {
					c.o.Logger.ErrorContext(ctx, "catalog snapshot: loading", "trigger", trigger, "err", err)
				}
			}
		case <-backstop.C:
			if err := c.Load(ctx, triggerBackstop); err != nil {
				c.o.Logger.ErrorContext(ctx, "catalog snapshot: the backstop reload", "err", err)
			}
		}
	}
}

// Reload is the file mode's SIGHUP: the directory's new snapshot is read
// whole into this one.
func (c *CatalogSnapshot) Reload(ctx context.Context) error {
	return c.Load(ctx, triggerSIGHUP)
}

// count records one reload.
func (c *CatalogSnapshot) count(trigger, result string) {
	if c.reloads != nil {
		c.reloads.Inc(map[string]string{"trigger": trigger, "result": result})
	}
}

// clone copies the maps, never the objects, which are shared and never
// changed.
func (s *catalogState) clone() *catalogState {
	return &catalogState{
		models: maps.Clone(s.models), modelIDs: maps.Clone(s.modelIDs),
		providers: maps.Clone(s.providers), byID: maps.Clone(s.byID),
		sealed: maps.Clone(s.sealed),
	}
}

func (s *catalogState) putProvider(p *v1.Provider) {
	s.providers[p.Metadata.Name] = p
	s.byID[p.Status.ID] = p
}

func (s *catalogState) dropProvider(id string) {
	if p, ok := s.byID[id]; ok {
		delete(s.providers, p.Metadata.Name)
		delete(s.byID, id)
	}
	delete(s.sealed, id)
}

func (s *catalogState) putModel(m *v1.Model) {
	s.models[m.Metadata.Name] = m
	s.modelIDs[m.Status.ID] = m.Metadata.Name
}

func (s *catalogState) dropModel(id string) {
	if name, ok := s.modelIDs[id]; ok {
		if m := s.models[name]; m != nil && m.Status.ID == id {
			delete(s.models, name)
		}
		delete(s.modelIDs, id)
	}
}

// order rebuilds the name-ordered list from the map.
func (s *catalogState) order() {
	s.ordered = make([]*v1.Model, 0, len(s.models))
	for _, m := range s.models {
		s.ordered = append(s.ordered, m)
	}
	slices.SortFunc(s.ordered, func(a, b *v1.Model) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
}

// The seams: the doors take the snapshot as their Catalog and their
// credential source, and the Key cache's tail passes it every batch.
var (
	_ gateway.Catalog          = (*CatalogSnapshot)(nil)
	_ gateway.CredentialSource = (*CatalogSnapshot)(nil)
	_ JournalFollower          = (*CatalogSnapshot)(nil)
)
