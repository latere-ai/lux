// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// errOutage is what every operation of a store that stopped answering
// returns.
var errOutage = errors.New("the store does not answer")

// outage is a Store over another whose reads and writes fail while down
// is set, as a store that stops answering does. AppendRecord, which
// writes the process's own ring, and the leases keep working.
type outage struct {
	store.Store
	down *atomic.Bool
}

func (o *outage) Objects() store.Objects { return outageObjects{o.Store.Objects(), o.down} }
func (o *outage) Keys() store.Keys       { return outageKeys{o.Store.Keys(), o.down} }
func (o *outage) Credentials() store.Credentials {
	return outageCredentials{o.Store.Credentials(), o.down}
}
func (o *outage) Counters() store.Counters { return outageCounters{o.Store.Counters(), o.down} }
func (o *outage) Usage() store.Usage       { return outageUsage{o.Store.Usage(), o.down} }
func (o *outage) Journal() store.Journal   { return outageJournal{o.Store.Journal(), o.down} }
func (o *outage) Transact(ctx context.Context, fn func(tx store.Store) error) error {
	if o.down.Load() {
		return errOutage
	}
	return o.Store.Transact(ctx, func(tx store.Store) error { return fn(&outage{tx, o.down}) })
}

type outageObjects struct {
	store.Objects
	down *atomic.Bool
}

func (o outageObjects) Get(ctx context.Context, kind, id string) (v1.Object, int64, error) {
	if o.down.Load() {
		return nil, 0, errOutage
	}
	return o.Objects.Get(ctx, kind, id)
}

func (o outageObjects) ByName(ctx context.Context, kind, name string) (v1.Object, int64, error) {
	if o.down.Load() {
		return nil, 0, errOutage
	}
	return o.Objects.ByName(ctx, kind, name)
}

func (o outageObjects) List(ctx context.Context, kind string, f store.Filter, p store.Page) ([]v1.Object, string, error) {
	if o.down.Load() {
		return nil, "", errOutage
	}
	return o.Objects.List(ctx, kind, f, p)
}

func (o outageObjects) PutStatus(ctx context.Context, kind, id string, observed any) error {
	if o.down.Load() {
		return errOutage
	}
	return o.Objects.PutStatus(ctx, kind, id, observed)
}

type outageKeys struct {
	store.Keys
	down *atomic.Bool
}

func (k outageKeys) ByHash(ctx context.Context, hash string) (string, error) {
	if k.down.Load() {
		return "", errOutage
	}
	return k.Keys.ByHash(ctx, hash)
}

type outageCredentials struct {
	store.Credentials
	down *atomic.Bool
}

func (c outageCredentials) Get(ctx context.Context, providerID string) (store.Sealed, error) {
	if c.down.Load() {
		return store.Sealed{}, errOutage
	}
	return c.Credentials.Get(ctx, providerID)
}

func (c outageCredentials) List(ctx context.Context) ([]string, error) {
	if c.down.Load() {
		return nil, errOutage
	}
	return c.Credentials.List(ctx)
}

type outageCounters struct {
	store.Counters
	down *atomic.Bool
}

func (c outageCounters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	if c.down.Load() {
		return 0, errOutage
	}
	return c.Counters.Add(ctx, key, delta, expiresAt)
}

type outageUsage struct {
	store.Usage
	down *atomic.Bool
}

func (u outageUsage) AddRows(ctx context.Context, rows []metering.Aggregate) error {
	if u.down.Load() {
		return errOutage
	}
	return u.Usage.AddRows(ctx, rows)
}

type outageJournal struct {
	store.Journal
	down *atomic.Bool
}

func (j outageJournal) Since(ctx context.Context, after int64, limit int) ([]store.Event, error) {
	if j.down.Load() {
		return nil, errOutage
	}
	return j.Journal.Since(ctx, after, limit)
}

func (j outageJournal) Append(ctx context.Context, e store.Event) (int64, error) {
	if j.down.Load() {
		return 0, errOutage
	}
	return j.Journal.Append(ctx, e)
}

// snapshot is a loaded catalog snapshot over st on the harness's clock.
func (h *harness) snapshot(t *testing.T, st store.Store, keys *secrets.Keyring) *CatalogSnapshot {
	t.Helper()
	c := NewCatalogSnapshot(CatalogSnapshotOptions{Store: st, Keys: keys, Retry: 5 * time.Millisecond, Logger: h.logger, Now: h.clock})
	if err := c.Load(t.Context(), triggerStart); err != nil {
		t.Fatal(err)
	}
	return c
}

// replica is one replica's Key cache over st whose tail the snapshot
// follows.
func (h *harness) replica(t *testing.T, st store.Store) (*KeyCache, *CatalogSnapshot) {
	t.Helper()
	snap := h.snapshot(t, st, h.keys)
	cache := NewKeyCache(KeyCacheOptions{Store: st, TTL: DefaultKeyCache, Tail: time.Hour, Follower: snap, Logger: h.logger, Now: h.clock})
	cache.Tail(t.Context())
	return cache, snap
}

// TestCatalogSnapshotServesWithoutStoreReads is spec 036's first
// criterion: once the snapshot is loaded and the Key is cached, a chat
// to a Model with two priced targets, a model list, and a passthrough
// request read no Model, Provider, or credential from the store.
func TestCatalogSnapshotServesWithoutStoreReads(t *testing.T) {
	h := newHarness(t)
	up := &stub{}
	up.set(http.StatusOK, openaiChatResponse)
	base := serveStub(t, up) + "/v1"
	a := h.provider(t, "oai-a", v1.DialectOpenAI, base, nil)
	b := h.provider(t, "oai-b", v1.DialectOpenAI, base, nil)
	w := 50
	available := true
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: "gpt"},
		Spec: v1.ModelSpec{Targets: []v1.Target{{Provider: a.Metadata.Name, Model: "gpt-4.1", Weight: &w}, {Provider: b.Metadata.Name, Model: "gpt-4.1", Weight: &w}},
			Fallback: v1.FallbackOnError, Pricing: card(t, "USD")},
		Status: v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, h.clock(), nil), Owner: subject, Source: v1.SourceDeclared, Warnings: []string{}, Available: &available},
	}
	if _, err := h.st.Objects().Put(t.Context(), m, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Objects().PutStatus(t.Context(), v1.KindModel, m.Status.ID, store.ModelObserved{Available: &available}); err != nil {
		t.Fatal(err)
	}
	_, value := h.key(t, "all", func(k *v1.Key) { k.Spec.Passthrough = true })
	st, reg := h.instrumented()
	snap := h.snapshot(t, st, h.keys)
	cache := NewKeyCache(KeyCacheOptions{Store: st, TTL: time.Hour, Logger: h.logger, Now: h.clock})
	limiter := NewLimiter(LimiterOptions{Store: st, Budgets: cache, Flush: time.Second, Logger: h.logger, Now: h.clock})
	recorder := NewRecorder(RecorderOptions{Store: st, Catalog: snap, Limiter: limiter, Flush: time.Second, Logger: h.logger, Now: h.clock})
	doors := gateway.New(gateway.Options{
		Keys: cache, Catalog: snap, Credentials: snap,
		Router:  gateway.NewTargetRouter(gateway.RouterOptions{Catalog: snap, Now: h.clock}),
		Limiter: limiter, Recorder: recorder, Clients: h.clients, Version: "test", Now: h.clock,
	})
	do := func(method, path, body string, provider ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+value)
		for _, p := range provider {
			r.Header.Set("Lux-Provider", p)
		}
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		doors.ServeHTTP(rec, r)
		return rec
	}
	if rec := do("POST", "/openai/v1/chat/completions", chat("gpt")); rec.Code != http.StatusOK {
		t.Fatalf("warming chat = %d %s", rec.Code, rec.Body)
	}
	catalogReads := func() uint64 {
		return ops(reg, "Objects.Get") + ops(reg, "Objects.ByName") + ops(reg, "Objects.List") + ops(reg, "Credentials.Get") + ops(reg, "Credentials.List")
	}
	before := catalogReads()
	for range 20 {
		if rec := do("POST", "/openai/v1/chat/completions", chat("gpt")); rec.Code != http.StatusOK {
			t.Fatalf("chat = %d %s", rec.Code, rec.Body)
		}
	}
	if rec := do("GET", "/openai/v1/models", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"gpt"`) {
		t.Fatalf("model list = %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/openai/v1/files", "", a.Metadata.Name); rec.Code != http.StatusOK {
		t.Fatalf("passthrough = %d %s", rec.Code, rec.Body)
	}
	if got := catalogReads(); got != before {
		t.Fatalf("the catalog was read from the store %d times after the snapshot loaded", got-before)
	}
	if up.lastHeader("Authorization") != "Bearer "+canary {
		t.Fatalf("the upstream saw %q", up.lastHeader("Authorization"))
	}
	now := h.clock()
	recs, _, err := h.st.Usage().Records(t.Context(), metering.RecordQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	priced := 0
	for _, r := range recs {
		if r.Model.Name == "gpt" && r.Cost.Priced {
			priced++
		}
	}
	if priced != 21 {
		t.Fatalf("%d priced records of the Model, want 21: the Recorder priced from the snapshot", priced)
	}
}

// TestCatalogSnapshotFollowsTheJournal: a Model written on one replica
// is served by a second replica sharing the store once its tail reads
// the row, and a delete is model_not_found there in the same bound.
func TestCatalogSnapshotFollowsTheJournal(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	cacheB, snapB := h.replica(t, h.st)
	m := h.declare(t, "fresh", p.Metadata.Name, "gpt-4.1")
	h.event(t, "model.created", m)
	if got, err := snapB.Model(ctx, "fresh"); err != nil || got != nil {
		t.Fatalf("the second replica served the Model before its tail: %v, %v", got, err)
	}
	cacheB.Tail(ctx)
	if got, err := snapB.Model(ctx, "fresh"); err != nil || got == nil || got.Status.ID != m.Status.ID {
		t.Fatalf("after the tail = %v, %v", got, err)
	}
	if err := h.st.Objects().Delete(ctx, v1.KindModel, m.Status.ID); err != nil {
		t.Fatal(err)
	}
	h.event(t, "model.deleted", m)
	cacheB.Tail(ctx)
	if got, err := snapB.Model(ctx, "fresh"); err != nil || got != nil {
		t.Fatalf("after the delete = %v, %v", got, err)
	}
	for _, name := range []string{"", "mdl_0000"} {
		if got, err := snapB.Model(ctx, name); err != nil || got != nil {
			t.Fatalf("Model(%q) = %v, %v", name, got, err)
		}
	}
	if got, _ := snapB.Provider(ctx, p.Status.ID); got == nil || got.Metadata.Name != "oai" {
		t.Fatalf("Provider by id = %v", got)
	}
	if got, _ := snapB.Provider(ctx, ""); got != nil {
		t.Fatalf("Provider(\"\") = %v", got)
	}
	// A Provider deleted drops its credential row and re-reads the Models.
	h.event(t, "provider.deleted", p)
	if err := h.st.Objects().Delete(ctx, v1.KindProvider, p.Status.ID); err != nil {
		t.Fatal(err)
	}
	cacheB.Tail(ctx)
	if got, _ := snapB.Provider(ctx, "oai"); got != nil {
		t.Fatalf("a deleted Provider is served: %v", got)
	}
	if _, _, creds := snapB.counts(); creds != 0 {
		t.Fatalf("%d credential rows held after the delete", creds)
	}
}

// TestCatalogSnapshotRotatedCredential: a Provider credential replaced
// through a write that journals provider.updated is the one opened on
// the second replica once its tail reads the row, and not before.
func TestCatalogSnapshotRotatedCredential(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	cacheB, snapB := h.replica(t, h.st)
	if got, err := snapB.Credential(ctx, p.Status.ID); err != nil || string(got) != canary {
		t.Fatalf("the loaded credential = %q, %v", got, err)
	}
	row, err := h.keys.Seal(p.Status.ID, 2, []byte("sk-rotated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Credentials().Put(ctx, p.Status.ID, row); err != nil {
		t.Fatal(err)
	}
	h.event(t, "provider.updated", p)
	if got, _ := snapB.Credential(ctx, p.Status.ID); string(got) != canary {
		t.Fatalf("the rotated value was served before the tail: %q", got)
	}
	cacheB.Tail(ctx)
	if got, err := snapB.Credential(ctx, p.Status.ID); err != nil || string(got) != "sk-rotated" {
		t.Fatalf("after the tail = %q, %v", got, err)
	}
	// A Provider the snapshot has not seen yet is read from the store.
	q := h.provider(t, "late", v1.DialectOpenAI, "https://late.example.com/v1", nil)
	if got, err := snapB.Credential(ctx, q.Status.ID); err != nil || string(got) != canary {
		t.Fatalf("an unseen Provider = %q, %v", got, err)
	}
	// A Provider that stores no credential answers none without a read.
	st, reg := h.instrumented()
	bare := h.provider(t, "bare", v1.DialectOpenAI, "https://bare.example.com/v1", nil)
	if err := h.st.Credentials().Delete(ctx, bare.Status.ID); err != nil {
		t.Fatal(err)
	}
	snap := h.snapshot(t, st, h.keys)
	before := ops(reg, "Credentials.Get")
	if got, err := snap.Credential(ctx, bare.Status.ID); err != nil || got != nil {
		t.Fatalf("no credential = %q, %v", got, err)
	}
	if ops(reg, "Credentials.Get") != before {
		t.Fatal("a Provider known to store no credential was read from the store")
	}
	// In the file mode the snapshot holds no credential at all.
	files := NewCatalogSnapshot(CatalogSnapshotOptions{Store: h.st, Logger: h.logger})
	if _, err := files.Credential(ctx, p.Status.ID); err == nil {
		t.Fatal("a snapshot without keys opened a credential")
	}
}

// TestCatalogSnapshotBackstop: a status written by PutStatus alone, with
// no journal row, reaches the model list at the backstop's full reload
// and not before, and Run performs that reload on its interval.
func TestCatalogSnapshotBackstop(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	m := h.declare(t, "quiet", p.Metadata.Name, "gpt-4.1")
	cache, snap := h.replica(t, h.st)
	available := func() bool {
		ms, err := snap.Models(ctx)
		if err != nil || len(ms) != 1 {
			t.Fatalf("Models = %v, %v", ms, err)
		}
		return ms[0].Status.Available != nil && *ms[0].Status.Available
	}
	if available() {
		t.Fatal("the Model is available before any status")
	}
	yes := true
	if err := h.st.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Available: &yes}); err != nil {
		t.Fatal(err)
	}
	cache.Tail(ctx)
	if available() {
		t.Fatal("a status with no journal row was seen before the backstop")
	}
	if err := snap.Load(ctx, triggerBackstop); err != nil {
		t.Fatal(err)
	}
	if !available() {
		t.Fatal("the backstop did not bring the status in")
	}
	// Run reloads on its interval, and first loads a snapshot that has
	// none.
	no := false
	if err := h.st.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Available: &no}); err != nil {
		t.Fatal(err)
	}
	fresh := NewCatalogSnapshot(CatalogSnapshotOptions{Store: h.st, Keys: h.keys, Reload: 10 * time.Millisecond, Retry: 5 * time.Millisecond, Logger: h.logger})
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); fresh.Run(run) }()
	waitFor(t, func() bool { return fresh.Loaded() })
	if err := h.st.Objects().PutStatus(ctx, v1.KindModel, m.Status.ID, store.ModelObserved{Available: &yes}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		got, _ := fresh.Model(ctx, "quiet")
		return got != nil && got.Status.Available != nil && *got.Status.Available
	})
	stop()
	<-done
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("the condition did not hold within five seconds")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDiscoveryShapeUpdateIsJournaled: a discovered Model whose shape
// changed is journaled model.updated with reason discovery and its
// changed paths, in the update's transaction, and a replica following
// the journal serves the new shape after one tail.
func TestDiscoveryShapeUpdateIsJournaled(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	up := &stub{pages: map[string]string{"": openaiList("gpt-5")}}
	p := h.provider(t, "openai", v1.DialectOpenAI, serveStub(t, up), func(p *v1.Provider) { p.Metadata.Labels = map[string]string{"tenant": "a"} })
	d := h.discovery("a")
	d.acquire(ctx)
	d.Tick(ctx)
	cache, snap := h.replica(t, h.st)
	if n := len(h.events(t, eventModelUpdated)); n != 0 {
		t.Fatalf("%d model.updated rows before any change", n)
	}
	p = h.get(t, p.Status.ID)
	p.Metadata.Labels["tenant"] = "b"
	if _, err := h.st.Objects().Put(ctx, p, p.Status.Version); err != nil {
		t.Fatal(err)
	}
	d.Tick(ctx)
	rows := h.events(t, eventModelUpdated)
	if len(rows) != 1 {
		t.Fatalf("%d model.updated rows, want 1", len(rows))
	}
	var rec eventRecord
	if err := json.Unmarshal(rows[0].Payload, &rec); err != nil {
		t.Fatal(err)
	}
	data, _ := rec.Data.(map[string]any)
	paths, _ := data["paths"].([]any)
	if rec.Reason != reasonDiscovery || rec.Subject != "" || rec.Object.Kind != v1.KindModel || len(paths) != 1 || paths[0] != "metadata.labels.tenant" {
		t.Fatalf("event = %+v", rec)
	}
	cache.Tail(ctx)
	got, err := snap.Model(ctx, "openai/gpt-5")
	if err != nil || got == nil || got.Metadata.Labels["tenant"] != "b" {
		t.Fatalf("the replica serves %v, %v", got, err)
	}
	// An unchanged list journals nothing more.
	d.Tick(ctx)
	if n := len(h.events(t, eventModelUpdated)); n != 1 {
		t.Fatalf("%d model.updated rows after an unchanged list", n)
	}
}

// TestCatalogReadiness: the snapshot is not ready until its first load
// succeeds, lookups read the store until then, a failed first load is
// retried by Run, and once loaded it stays ready whatever its age.
func TestCatalogReadiness(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	h.declare(t, "m", p.Metadata.Name, "gpt-4.1")
	down := &atomic.Bool{}
	down.Store(true)
	st := &outage{Store: h.st, down: down}
	reg := metrics.NewRegistry()
	snap := NewCatalogSnapshot(CatalogSnapshotOptions{Store: st, Keys: h.keys, Retry: 5 * time.Millisecond, Metrics: reg, Logger: h.logger, Now: h.clock})
	if err := snap.Load(ctx, triggerStart); err == nil {
		t.Fatal("a load over a store that does not answer succeeded")
	}
	if snap.Ready(ctx) == nil || snap.Age() != 0 {
		t.Fatal("ready before a load succeeded")
	}
	if _, err := snap.Model(ctx, "m"); !errors.Is(err, errOutage) {
		t.Fatalf("before the load a lookup reads the store: %v", err)
	}
	if _, err := snap.Models(ctx); !errors.Is(err, errOutage) {
		t.Fatalf("Models before the load: %v", err)
	}
	if _, err := snap.Provider(ctx, "oai"); !errors.Is(err, errOutage) {
		t.Fatalf("Provider before the load: %v", err)
	}
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); snap.Run(run) }()
	time.Sleep(20 * time.Millisecond)
	if snap.Loaded() {
		t.Fatal("loaded while the store does not answer")
	}
	down.Store(false)
	waitFor(t, snap.Loaded)
	stop()
	<-done
	if err := snap.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Hour)
	if err := snap.Ready(ctx); err != nil || snap.Age() < time.Hour {
		t.Fatalf("an hour-old snapshot: ready %v, age %s", err, snap.Age())
	}
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	exposition := buf.String()
	for _, want := range []string{`lux_catalog_reloads_total{result="error",trigger="start"}`, `lux_catalog_reloads_total{result="ok",trigger="start"}`, `lux_catalog_objects{kind="Model"} 1`, `lux_catalog_objects{kind="credential"} 1`, "lux_catalog_age_seconds 3600"} {
		if !strings.Contains(exposition, want) {
			t.Errorf("the exposition lacks %s:\n%s", want, exposition)
		}
	}
}

// TestCatalogSnapshotStoreDown: with the store failing after the load,
// the snapshot serves the Model, the Provider, and the credential, its
// age grows, a tail batch that cannot be reloaded marks a full reload
// that Run performs once the store answers, and the replica stays ready.
func TestCatalogSnapshotStoreDown(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	m := h.declare(t, "m", p.Metadata.Name, "gpt-4.1")
	down := &atomic.Bool{}
	st := &outage{Store: h.st, down: down}
	snap := h.snapshot(t, st, h.keys)
	down.Store(true)
	h.advance(10 * time.Minute)
	if got, err := snap.Model(ctx, "m"); err != nil || got == nil {
		t.Fatalf("Model during the outage = %v, %v", got, err)
	}
	if got, err := snap.Provider(ctx, "oai"); err != nil || got == nil {
		t.Fatalf("Provider during the outage = %v, %v", got, err)
	}
	if got, err := snap.Credential(ctx, p.Status.ID); err != nil || string(got) != canary {
		t.Fatalf("Credential during the outage = %q, %v", got, err)
	}
	if snap.Age() < 10*time.Minute || snap.Ready(ctx) != nil {
		t.Fatalf("age %s, ready %v", snap.Age(), snap.Ready(ctx))
	}
	snap.Follow(ctx, []store.Event{{Type: "model.updated", ObjectID: m.Status.ID}})
	if !snap.full.Load() {
		t.Fatal("a failed targeted reload did not mark a full one")
	}
	snap.Follow(ctx, nil)
	if snap.Age() < 10*time.Minute {
		t.Fatal("an empty batch with a full reload pending counted as a match")
	}
	if err := snap.Load(ctx, triggerBackstop); err == nil {
		t.Fatal("a full reload over a store that does not answer succeeded")
	}
	down.Store(false)
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); snap.Run(run) }()
	waitFor(t, func() bool { return !snap.full.Load() })
	stop()
	<-done
	if snap.Age() != 0 {
		t.Fatalf("after the retried reload the age is %s", snap.Age())
	}
	// A batch naming more than the targeted limit is one full reload.
	many := make([]store.Event, 0, targetedLimit+1)
	for i := range targetedLimit + 1 {
		many = append(many, store.Event{Type: "model.updated", ObjectID: fmt.Sprintf("mdl_%026d", i)})
	}
	down.Store(true)
	snap.Follow(ctx, many)
	down.Store(false)
	if !snap.full.Load() {
		t.Fatal("a failed full reload from a large batch did not stay pending")
	}
	snap.Follow(ctx, many)
	if snap.full.Load() {
		t.Fatal("a large batch did not reload in full")
	}
}

// TestCatalogSnapshotReopensOnce: a sealed row the snapshot cannot open,
// because the row was re-wrapped under a key this replica lacks, is read
// from the store once and opened; a second failure is the error; and
// the snapshot never holds the plaintext.
func TestCatalogSnapshotReopensOnce(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	const next = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	both, err := secrets.Parse(next + "," + kek)
	if err != nil {
		t.Fatal(err)
	}
	only, err := secrets.Parse(next)
	if err != nil {
		t.Fatal(err)
	}
	st, reg := h.instrumented()
	snap := h.snapshot(t, st, only)
	row, err := h.st.Credentials().Get(ctx, p.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	rewrapped, changed, err := both.Rewrap(p.Status.ID, row)
	if err != nil || !changed {
		t.Fatalf("rewrap: %v, %v", changed, err)
	}
	if err := h.st.Credentials().Rewrap(ctx, p.Status.ID, row.Version, rewrapped.WrappedKey, rewrapped.WrappedNonce); err != nil {
		t.Fatal(err)
	}
	before := ops(reg, "Credentials.Get")
	if got, err := snap.Credential(ctx, p.Status.ID); err != nil || string(got) != canary {
		t.Fatalf("after the re-read = %q, %v", got, err)
	}
	if n := ops(reg, "Credentials.Get") - before; n != 1 {
		t.Fatalf("%d store reads, want one", n)
	}
	broken := rewrapped
	broken.Ciphertext = bytes.Repeat([]byte{7}, len(rewrapped.Ciphertext))
	if err := h.st.Credentials().Put(ctx, p.Status.ID, broken); err != nil {
		t.Fatal(err)
	}
	if err := snap.Load(ctx, triggerBackstop); err != nil {
		t.Fatal(err)
	}
	if got, err := snap.Credential(ctx, p.Status.ID); err == nil || got != nil {
		t.Fatalf("a row that opens under no key = %q, %v", got, err)
	}
	if holds(reflect.ValueOf(snap.cur.Load()), []byte(canary)) {
		t.Fatal("the snapshot holds the plaintext credential")
	}
}

// holds walks v and reports whether any string or byte slice in it
// contains needle.
func holds(v reflect.Value, needle []byte) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && holds(v.Elem(), needle)
	case reflect.Struct:
		for _, field := range v.Fields() {
			if holds(field, needle) {
				return true
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if holds(k, needle) || holds(v.MapIndex(k), needle) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return bytes.Contains(v.Bytes(), needle)
		}
		for i := range v.Len() {
			if holds(v.Index(i), needle) {
				return true
			}
		}
	case reflect.String:
		return strings.Contains(v.String(), string(needle))
	}
	return false
}

// TestCatalogSnapshotSwapIsAtomic: lookups racing a stream of reloads
// each see one whole snapshot, the two Models written together always at
// the same generation, under the race detector.
func TestCatalogSnapshotSwapIsAtomic(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	a := h.declare(t, "a", p.Metadata.Name, "x")
	b := h.declare(t, "b", p.Metadata.Name, "y")
	snap := h.snapshot(t, h.st, h.keys)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				ms, err := snap.Models(ctx)
				if err != nil || len(ms) != 2 {
					t.Errorf("Models = %d, %v", len(ms), err)
					return
				}
				if ms[0].Metadata.Labels["gen"] != ms[1].Metadata.Labels["gen"] {
					t.Errorf("a lookup saw two generations: %q and %q", ms[0].Metadata.Labels["gen"], ms[1].Metadata.Labels["gen"])
					return
				}
			}
		})
	}
	for gen := range 50 {
		err := h.st.Transact(ctx, func(tx store.Store) error {
			for _, m := range []*v1.Model{a, b} {
				obj, version, err := tx.Objects().Get(ctx, v1.KindModel, m.Status.ID)
				if err != nil {
					return err
				}
				cur := obj.(*v1.Model)
				cur.Metadata.Labels = map[string]string{"gen": string(rune('a' + gen%26))}
				if _, err := tx.Objects().Put(ctx, cur, version); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := snap.Load(ctx, triggerBackstop); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestKeyCacheStaleGrace is spec 036's grace: with the store failing
// every read after a Key and its Budget were cached, both are served
// past LUX_KEY_CACHE until the window plus the grace, each Key lookup
// counted stale, and refused with the store's error after it; with no
// grace the refusal comes at the window.
func TestKeyCacheStaleGrace(t *testing.T) {
	for _, grace := range []time.Duration{5 * time.Minute, 0} {
		h := newHarness(t)
		ctx := t.Context()
		b := h.budget(t, "team", "10", "USD", v1.WindowMonth, true)
		_, value := h.key(t, "k", draws(b))
		down := &atomic.Bool{}
		st := &outage{Store: h.st, down: down}
		reg := metrics.NewRegistry()
		c := NewKeyCache(KeyCacheOptions{Store: st, TTL: 10 * time.Second, Grace: grace, Metrics: reg, Logger: h.logger, Now: h.clock})
		hash := HashKeyValue(value)
		if k, err := c.ByHash(ctx, hash); err != nil || k == nil {
			t.Fatalf("warming: %v, %v", k, err)
		}
		if got, err := c.Budget(ctx, b.Status.ID); err != nil || got == nil {
			t.Fatalf("warming the Budget: %v, %v", got, err)
		}
		down.Store(true)
		h.advance(11 * time.Second)
		k, err := c.ByHash(ctx, hash)
		if grace == 0 {
			if err == nil || k != nil {
				t.Fatalf("no grace: past the window = %v, %v", k, err)
			}
			if _, err := c.Budget(ctx, b.Status.ID); err == nil {
				t.Fatal("no grace: the Budget served past its window")
			}
			continue
		}
		if err != nil || k == nil {
			t.Fatalf("inside the grace = %v, %v", k, err)
		}
		if got, err := c.Budget(ctx, b.Status.ID); err != nil || got == nil {
			t.Fatalf("the Budget inside the grace = %v, %v", got, err)
		}
		if n := hits(reg, "stale"); n != 1 {
			t.Fatalf("%d stale lookups counted, want 1", n)
		}
		h.advance(grace)
		if k, err := c.ByHash(ctx, hash); err == nil || k != nil {
			t.Fatalf("past the grace = %v, %v", k, err)
		}
		if _, err := c.Budget(ctx, b.Status.ID); err == nil {
			t.Fatal("the Budget served past the grace")
		}
	}
}

// TestKeyCacheStaleGraceLimits: the grace serves no negative entry and no
// Key never cached, and the door still refuses a disabled cached Key and
// answers key_expired for a cached Key past its expiresAt, whatever the
// grace.
func TestKeyCacheStaleGraceLimits(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	h.declare(t, "m", p.Metadata.Name, "gpt-4.1")
	down := &atomic.Bool{}
	st := &outage{Store: h.st, down: down}
	c := NewKeyCache(KeyCacheOptions{Store: st, TTL: 10 * time.Second, Grace: time.Hour, Logger: h.logger, Now: h.clock})
	snap := h.snapshot(t, st, h.keys)
	limiter := NewLimiter(LimiterOptions{Store: st, Budgets: c, Flush: time.Second, Logger: h.logger, Now: h.clock})
	doors := gateway.New(gateway.Options{
		Keys: c, Catalog: snap, Credentials: snap,
		Router:  gateway.NewTargetRouter(gateway.RouterOptions{Catalog: snap, Now: h.clock}),
		Limiter: limiter, Recorder: NewRecorder(RecorderOptions{Store: st, Catalog: snap, Limiter: limiter, Logger: h.logger, Now: h.clock}),
		Clients: h.clients, Version: "test", Now: h.clock,
	})
	code := func(value string) string {
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(chat("m")))
		r.Header.Set("Authorization", "Bearer "+value)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		doors.ServeHTTP(rec, r)
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Error.Code
	}
	const unknown = "lux_never-minted-value-000000000000000000000"
	if k, err := c.ByHash(ctx, HashKeyValue(unknown)); err != nil || k != nil {
		t.Fatalf("an unknown value = %v, %v", k, err)
	}
	_, disabled := h.key(t, "off", func(k *v1.Key) { k.Spec.Disabled = true })
	_, expiring := h.key(t, "soon", func(k *v1.Key) { k.Status.ExpiresAt = h.clock().Add(30 * time.Second) })
	for _, v := range []string{disabled, expiring} {
		if k, err := c.ByHash(ctx, HashKeyValue(v)); err != nil || k == nil {
			t.Fatalf("warming: %v, %v", k, err)
		}
	}
	_, never := h.key(t, "never-cached", nil)
	down.Store(true)
	h.advance(time.Minute)
	if got := code(unknown); got != string(gateway.CodeStoreUnavailable) {
		t.Fatalf("a negative entry past its window = %q", got)
	}
	if got := code(never); got != string(gateway.CodeStoreUnavailable) {
		t.Fatalf("a Key never cached = %q", got)
	}
	if got := code(disabled); got != string(gateway.CodeKeyDisabled) {
		t.Fatalf("a stale disabled Key = %q", got)
	}
	if got := code(expiring); got != string(gateway.CodeKeyExpired) {
		t.Fatalf("a stale Key past its expiresAt = %q", got)
	}
}

// TestKeyCacheStaleGraceRecovers: past its window a lookup reads the
// store first, so a store that answers again replaces the stale entry
// at once, and a Key the journal names is evicted whatever the grace.
func TestKeyCacheStaleGraceRecovers(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	down := &atomic.Bool{}
	st, reg := h.instrumented()
	c := NewKeyCache(KeyCacheOptions{Store: &outage{Store: st, down: down}, TTL: 10 * time.Second, Grace: time.Hour, Metrics: reg, Logger: h.logger, Now: h.clock})
	k, value := h.key(t, "k", nil)
	hash := HashKeyValue(value)
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil {
		t.Fatalf("warming: %v, %v", got, err)
	}
	down.Store(true)
	h.advance(time.Minute)
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil {
		t.Fatalf("stale: %v, %v", got, err)
	}
	down.Store(false)
	reads := ops(reg, "Keys.ByHash")
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil {
		t.Fatalf("after the store answers: %v, %v", got, err)
	}
	if ops(reg, "Keys.ByHash") != reads+1 || hits(reg, "miss") < 2 {
		t.Fatal("the lookup after the outage did not read the store")
	}
	if got, err := c.ByHash(ctx, hash); err != nil || got == nil || hits(reg, "hit") != 1 {
		t.Fatalf("the replaced entry is not fresh: %v, %v, %d hits", got, err, hits(reg, "hit"))
	}
	if err := h.st.Objects().Delete(ctx, v1.KindKey, k.Status.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Keys().Delete(ctx, k.Status.ID); err != nil {
		t.Fatal(err)
	}
	h.event(t, eventKeyDeleted, k)
	c.Tail(ctx)
	down.Store(true)
	h.advance(time.Minute)
	if got, err := c.ByHash(ctx, hash); err == nil || got != nil {
		t.Fatalf("a deleted Key served stale = %v, %v", got, err)
	}
}

// TestLateCorrectionAfterOutage: spend counter deltas and hourly usage
// rows made on two replicas while the store fails every write land in
// the store at the first flush after it answers, summed exactly; each
// replica admits only against its own spend during the outage; and a
// hard Budget the combined spend has passed refuses the next call on
// both replicas and raises budget.exhausted once.
func TestLateCorrectionAfterOutage(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.provider(t, "oai", v1.DialectOpenAI, "https://api.example.com/v1", nil)
	m := priced("p", "0.01", "0", "USD")
	m.Spec.Targets = []v1.Target{{Provider: p.Metadata.Name, Model: "x"}}
	if _, err := h.st.Objects().Put(ctx, m, 0); err != nil {
		t.Fatal(err)
	}
	b := h.budget(t, "team", "0.05", "USD", v1.WindowMonth, true)
	k, _ := h.key(t, "k", draws(b))
	down := &atomic.Bool{}
	st := &outage{Store: h.st, down: down}
	type replica struct {
		l *Limiter
		r *Recorder
	}
	var replicas []replica
	for range 2 {
		cache := NewKeyCache(KeyCacheOptions{Store: st, TTL: 10 * time.Second, Grace: 5 * time.Minute, Logger: h.logger, Now: h.clock})
		snap := h.snapshot(t, st, h.keys)
		l := NewLimiter(LimiterOptions{Store: st, Budgets: cache, Defaults: manifest.Defaults{}, Flush: time.Second, Logger: h.logger, Now: h.clock})
		r := NewRecorder(RecorderOptions{Store: st, Catalog: snap, Limiter: l, Flush: time.Second, Logger: h.logger, Now: h.clock})
		if ref := spendOne(t, l, k, m); ref != nil {
			t.Fatalf("warming: %v", ref)
		}
		l.Flush(ctx)
		replicas = append(replicas, replica{l, r})
	}
	down.Store(true)
	h.advance(time.Minute)
	for i, rep := range replicas {
		for range 2 {
			if ref := spendOne(t, rep.l, k, m); ref != nil {
				t.Fatalf("replica %d during the outage: %v", i, ref)
			}
			rep.r.Record(gateway.Record{ID: "req_" + strings.Repeat("0", 26), At: h.clock(), EndedAt: h.clock(), KeyID: k.Status.ID, Owner: subject,
				Model: m.Metadata.Name, ModelID: m.Status.ID, Door: v1.DialectOpenAI, Status: gateway.StatusOK, Tokens: gateway.Tokens{Input: 1}})
		}
		rep.l.Flush(ctx)
		if err := rep.r.Flush(ctx); err == nil {
			t.Fatalf("replica %d flushed its rows to a store that does not answer", i)
		}
	}
	budgetKey := metering.CounterKey(metering.ScopeBudgetSpend, b.Status.ID, b.Spec.Window, h.clock(), b.Status.CreatedAt)
	if got := h.counter(t, budgetKey); got != int64(2*cent) {
		t.Fatalf("during the outage the store holds %d", got)
	}
	down.Store(false)
	for _, rep := range replicas {
		rep.l.Flush(ctx)
		if err := rep.r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.counter(t, budgetKey); got != int64(6*cent) {
		t.Fatalf("after the outage the store holds %d, want the six cents spent", got)
	}
	rows, err := Usage(ctx, h.st, metering.Query{From: h.clock().Add(-time.Hour), To: h.clock().Add(time.Hour)}, h.clock())
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	var cost int64
	for _, row := range rows {
		requests += row.Requests
		cost += row.Cost
	}
	if requests != 4 || cost != int64(4*cent) {
		t.Fatalf("usage after the outage: %d requests, %d cost, want the four recorded during it", requests, cost)
	}
	// The replica that flushed last read the combined total and refuses.
	// The other read the total as of its own flush, and learns the rest
	// at its next flush of a delta: at most one more request, the
	// one-request term of spec 009's bound, and then it refuses too.
	if ref := spendOne(t, replicas[1].l, k, m); ref == nil || ref.Code != gateway.CodeBudgetExhausted {
		t.Fatalf("the replica that flushed last = %+v", ref)
	}
	if ref := spendOne(t, replicas[0].l, k, m); ref == nil {
		replicas[0].l.Flush(ctx)
		if ref := spendOne(t, replicas[0].l, k, m); ref == nil || ref.Code != gateway.CodeBudgetExhausted {
			t.Fatalf("the other replica after its next flush = %+v", ref)
		}
	} else if ref.Code != gateway.CodeBudgetExhausted {
		t.Fatalf("the other replica = %+v", ref)
	}
	if n := len(h.events(t, eventBudgetExhausted)); n != 1 {
		t.Fatalf("%d budget.exhausted rows, want 1", n)
	}
}
