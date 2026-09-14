// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	v1 "latere.ai/x/lux/manifest/v1"
)

// errBroken is what a broken operation answers.
var errBroken = errors.New("the store is broken here")

// broken is a Store over another whose named operations fail, so the
// jobs' handling of a store failure is exercised branch by branch. The
// names are Collection.Method, as the instrument counts them.
type broken struct {
	store.Store
	fail map[string]bool
}

func (b *broken) Objects() store.Objects { return brokenObjects{b.Store.Objects(), b.fail} }
func (b *broken) Journal() store.Journal { return brokenJournal{b.Store.Journal(), b.fail} }
func (b *broken) Leases() store.Leases   { return brokenLeases{b.Store.Leases(), b.fail} }
func (b *broken) Transact(ctx context.Context, fn func(tx store.Store) error) error {
	return b.Store.Transact(ctx, func(tx store.Store) error { return fn(&broken{tx, b.fail}) })
}

type brokenObjects struct {
	store.Objects
	fail map[string]bool
}

func (o brokenObjects) Put(ctx context.Context, obj v1.Object, v int64) (int64, error) {
	if o.fail["Objects.Put"] {
		return 0, errBroken
	}
	return o.Objects.Put(ctx, obj, v)
}

func (o brokenObjects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	if o.fail["Objects.Get"] {
		return nil, 0, errBroken
	}
	return o.Objects.Get(ctx, kind, id)
}

func (o brokenObjects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	if o.fail["Objects.ByName"] {
		return nil, 0, errBroken
	}
	return o.Objects.ByName(ctx, kind, name)
}

func (o brokenObjects) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]v1.Object, string, error) {
	if o.fail["Objects.List"] || (kind == v1.KindModel && o.fail["Objects.ListModels"]) {
		return nil, "", errBroken
	}
	return o.Objects.List(ctx, kind, f, p)
}

func (o brokenObjects) Delete(ctx context.Context, kind, id string) error {
	if o.fail["Objects.Delete"] {
		return errBroken
	}
	return o.Objects.Delete(ctx, kind, id)
}

func (o brokenObjects) PutStatus(ctx context.Context, kind, id string, observed any) error {
	if o.fail["Objects.PutStatus"] || (kind == v1.KindModel && o.fail["Objects.PutModelStatus"]) {
		return errBroken
	}
	return o.Objects.PutStatus(ctx, kind, id, observed)
}

type brokenJournal struct {
	store.Journal
	fail map[string]bool
}

func (j brokenJournal) Append(ctx context.Context, e store.Event) (int64, error) {
	if j.fail["Journal.Append"] {
		return 0, errBroken
	}
	return j.Journal.Append(ctx, e)
}

func (j brokenJournal) Since(ctx context.Context, after int64, limit int) ([]store.Event, error) {
	if j.fail["Journal.Since"] {
		return nil, errBroken
	}
	return j.Journal.Since(ctx, after, limit)
}

type brokenLeases struct {
	store.Leases
	fail map[string]bool
}

func (l brokenLeases) Release(ctx context.Context, name, holder string) error {
	if l.fail["Leases.Release"] {
		return errBroken
	}
	return l.Leases.Release(ctx, name, holder)
}

// failingCredentials is a credential source that cannot open anything.
type failingCredentials struct{}

func (failingCredentials) Credential(context.Context, string) ([]byte, error) {
	return nil, errBroken
}

// TestDiscoverySurvivesStoreFailures: every store failure inside a list
// is logged with the catalogue untouched, and a broken journal or lease
// is logged and does not stop the job.
func TestDiscoverySurvivesStoreFailures(t *testing.T) {
	for _, op := range []string{"Objects.Put", "Objects.ByName", "Objects.Delete", "Objects.PutStatus", "Objects.ListModels", "Journal.Append"} {
		t.Run(op, func(t *testing.T) {
			h := newHarness(t)
			up := &stub{pages: map[string]string{"": openaiList("gpt-5", "o3")}}
			h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
			// A first list on the working store, so a delete and an update
			// have something to fail on.
			good := h.discovery("good")
			good.acquire(t.Context())
			good.Tick(t.Context())
			good.release(t.Context())
			up.setPages(map[string]string{"": openaiList("gpt-5", "o4")})
			b := &broken{Store: h.st, fail: map[string]bool{op: true}}
			d := NewDiscovery(DiscoveryOptions{Store: b, Clients: h.clients, Credentials: h.creds, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock})
			d.acquire(t.Context())
			d.Tick(t.Context())
			if got := names(h.models(t, "")); got != "openai/gpt-5 openai/o3" {
				t.Fatalf("the catalogue changed under a broken %s: %q", op, got)
			}
			if !strings.Contains(h.logged(), "discovery: writing the catalogue") || !strings.Contains(h.logged(), errBroken.Error()) {
				t.Fatalf("log:\n%s", h.logged())
			}
		})
	}
	h := newHarness(t)
	b := &broken{Store: h.st, fail: map[string]bool{"Journal.Since": true, "Leases.Release": true}}
	d := NewDiscovery(DiscoveryOptions{Store: b, Clients: h.clients, Credentials: h.creds, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock})
	d.acquire(t.Context())
	d.Tail(t.Context())
	d.release(t.Context())
	for _, want := range []string{"discovery: reading the journal", "discovery: releasing the lease"} {
		if !strings.Contains(h.logged(), want) {
			t.Errorf("log lacks %q:\n%s", want, h.logged())
		}
	}
	// A Provider the journal names and the store cannot read.
	p := h.provider(t, "openai", v1.DialectOpenAI, "http://127.0.0.1:1", nil)
	if _, err := h.st.Journal().Append(t.Context(), store.Event{ID: "evt_1", ObjectID: p.Status.ID, Type: eventProviderCreated}); err != nil {
		t.Fatal(err)
	}
	b = &broken{Store: h.st, fail: map[string]bool{"Objects.Get": true}}
	d = NewDiscovery(DiscoveryOptions{Store: b, Clients: h.clients, Credentials: h.creds, Interval: time.Hour, Holder: "b", Logger: h.logger, Now: h.clock})
	h.advance(store.LeaseTTL + time.Second)
	d.acquire(t.Context())
	d.mu.Lock()
	d.after = 0
	d.mu.Unlock()
	d.Tail(t.Context())
	if !strings.Contains(h.logged(), "discovery: reading a Provider the journal named") {
		t.Fatalf("log:\n%s", h.logged())
	}
	// A credential that cannot be opened, and a baseURL the client
	// refuses, are failed lists.
	d = NewDiscovery(DiscoveryOptions{Store: h.st, Clients: h.clients, Credentials: failingCredentials{}, Interval: time.Hour, Holder: "c", Logger: h.logger, Now: h.clock})
	h.advance(store.LeaseTTL + time.Second)
	d.acquire(t.Context())
	d.List(t.Context(), p)
	if got := h.get(t, p.Status.ID).Status.Health; got == nil || !strings.Contains(got.LastError, errBroken.Error()) {
		t.Fatalf("lastError = %+v", got)
	}
	ftp := h.provider(t, "ftp", v1.DialectOpenAI, "ftp://files.example.com", nil)
	d.List(t.Context(), ftp)
	if got := h.get(t, ftp.Status.ID).Status.Health; got == nil || !strings.Contains(got.LastError, "not an http:// or https:// URL") {
		t.Fatalf("lastError = %+v", got)
	}
	// A body above the cap is a failed list.
	huge := &stub{status: http.StatusOK, body: strings.Repeat("0", maxListBytes+1)}
	hp := h.provider(t, "huge", v1.DialectOpenAI, serveStub(t, huge), nil)
	h.discovery("d").List(t.Context(), hp)
	if got := h.get(t, hp.Status.ID).Status.Health; got == nil || !strings.Contains(got.LastError, "the body is above") {
		t.Fatalf("lastError = %+v", got)
	}
}

// TestDiscoveryRunTicksAndReleases: Run lists on its interval and gives
// the lease back on stop.
func TestDiscoveryRunTicksAndReleases(t *testing.T) {
	h := newHarness(t)
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), nil)
	d := NewDiscovery(DiscoveryOptions{Store: h.st, Clients: h.clients, Credentials: h.creds, Interval: 20 * time.Millisecond, Tail: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock, Jitter: func() float64 { return 0 }})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()
	waitUntil(t, "three lists", func() bool { return up.count() >= 3 })
	cancel()
	<-done
	if held, err := h.st.Leases().Acquire(t.Context(), store.LeaseDiscovery, "b", store.LeaseTTL); err != nil || !held {
		t.Fatalf("the lease was not released: %v, %v", held, err)
	}
}

// TestHealthSurvivesStoreFailures: a Model list, a Model status write,
// an event append, and a lease release that fail are each logged and
// leave the Provider's own status written.
func TestHealthSurvivesStoreFailures(t *testing.T) {
	for _, op := range []string{"Objects.ListModels", "Objects.PutModelStatus", "Journal.Append", "Leases.Release"} {
		t.Run(op, func(t *testing.T) {
			h := newHarness(t)
			p := h.provider(t, "openai", v1.DialectOpenAI, "http://127.0.0.1:1", nil)
			h.declare(t, "gpt-5", "openai", "gpt-5")
			b := &broken{Store: h.st, fail: map[string]bool{op: true}}
			job := NewHealth(HealthOptions{Store: b, Clients: h.clients, Credentials: h.creds, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock})
			job.acquire(t.Context())
			for range 3 {
				job.Tick(t.Context())
			}
			job.release(t.Context())
			if got := h.get(t, p.Status.ID).Status.Health; got == nil || got.State != v1.HealthUnreachable {
				t.Fatalf("health = %+v", got)
			}
			if !strings.Contains(h.logged(), errBroken.Error()) {
				t.Fatalf("log:\n%s", h.logged())
			}
		})
	}
	// resetToUnknown over a broken Model list.
	h := newHarness(t)
	quiet := h.provider(t, "quiet", v1.DialectOpenAI, "http://127.0.0.1:1", func(p *v1.Provider) { p.Spec.Health.Mode = v1.HealthNone })
	if err := h.st.Objects().PutStatus(t.Context(), v1.KindProvider, quiet.Status.ID, store.ProviderObserved{Health: &v1.HealthStatus{State: v1.HealthHealthy}}); err != nil {
		t.Fatal(err)
	}
	b := &broken{Store: h.st, fail: map[string]bool{"Objects.ListModels": true}}
	job := NewHealth(HealthOptions{Store: b, Clients: h.clients, Credentials: h.creds, Interval: time.Hour, Holder: "a", Logger: h.logger, Now: h.clock})
	job.acquire(t.Context())
	job.Tick(t.Context())
	if got := h.get(t, quiet.Status.ID).Status.Health; got.State != v1.HealthUnknown || !strings.Contains(h.logged(), "health: refreshing the Models") {
		t.Fatalf("health = %+v\n%s", got, h.logged())
	}
	// A credential that cannot be opened is a failed probe.
	failing := NewHealth(HealthOptions{Store: h.st, Clients: h.clients, Credentials: failingCredentials{}})
	if failed, lastError := failing.probe(t.Context(), h.provider(t, "openai", v1.DialectOpenAI, "http://127.0.0.1:1", nil)); !failed || !strings.Contains(lastError, errBroken.Error()) {
		t.Fatalf("probe = %v, %q", failed, lastError)
	}
	if failing.o.Holder == "" || failing.o.Logger == nil || failing.o.Now == nil || !strings.HasPrefix(failing.o.NewID(), "evt_") {
		t.Fatalf("defaults = %+v", failing.o)
	}
}

// TestHelpers covers the small pure functions' remaining branches.
func TestHelpers(t *testing.T) {
	if labelsOf(&v1.Key{}) != nil {
		t.Fatal("labels of a Key")
	}
	if got := labelsOf(&v1.Model{Metadata: v1.ObjectMeta{Labels: map[string]string{"a": "b"}}}); got["a"] != "b" {
		t.Fatal("labels of a Model")
	}
	if string(redact([]byte("abc"), nil)) != "abc" || string(redact([]byte("a-sk-b"), []byte("sk"))) != "a-[redacted]-b" {
		t.Fatal("redact")
	}
	l := providerLookup{p: &v1.Provider{Metadata: v1.ObjectMeta{Name: "openai"}, Status: v1.ProviderStatus{ID: "prv_1"}}}
	if p, err := l.Provider(t.Context(), "prv_1"); err != nil || p == nil {
		t.Fatal("lookup by id")
	}
	if _, err := l.Provider(t.Context(), "other"); err == nil {
		t.Fatal("lookup of another name")
	}
	if _, err := l.Budget(t.Context(), "b"); err == nil {
		t.Fatal("a budget")
	}
	if refs, err := l.Models(t.Context(), "*"); err != nil || refs != nil {
		t.Fatal("models")
	}
	obs := observedModel(&v1.Model{Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: "x", Model: "m"}}}}, func(string) v1.HealthState { return "" })
	if !*obs.Available || obs.Targets[0].Health != v1.HealthUnknown {
		t.Fatalf("observedModel with no information = %+v", obs)
	}
	if !strings.Contains(defaultHolder(), ":") {
		t.Fatal("defaultHolder")
	}
	// appendEvent over a journal that refuses.
	err := appendEvent(t.Context(), brokenJournal{fail: map[string]bool{"Journal.Append": true}}, "x", "y", &v1.Model{}, nil, time.Now(), func() string { return "evt_1" })
	if !errors.Is(err, errBroken) {
		t.Fatalf("appendEvent = %v", err)
	}
	_ = slog.Default()
}
