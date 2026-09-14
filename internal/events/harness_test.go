// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

var secret = []byte("s3cr3t-for-the-tests")

// clock is the fake clock the store, the sink, and the worker share.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// delivery is one POST the test sink saw.
type delivery struct {
	header string
	body   []byte
}

func (d delivery) id() string {
	var rec Record
	_ = json.Unmarshal(d.body, &rec)
	return rec.ID
}

// sink is the test sink over httptest: it records every POST and
// answers what plan says for the n-th request, counting from 1; hang
// holds the request open until the client gives up.
type sink struct {
	srv *httptest.Server

	mu   sync.Mutex
	got  []delivery
	plan func(n int, body []byte) (status int, hang bool)
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{plan: func(int, []byte) (int, bool) { return http.StatusOK, false }}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.got = append(s.got, delivery{header: r.Header.Get(Header), body: body})
	status, hang := s.plan(len(s.got), body)
	s.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	w.WriteHeader(status)
}

// setPlan replaces the plan under the lock.
func (s *sink) setPlan(plan func(n int, body []byte) (status int, hang bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plan = plan
}

func (s *sink) deliveries() []delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]delivery(nil), s.got...)
}

// ids is every delivered event id in receipt order.
func (s *sink) ids() []string {
	var out []string
	for _, d := range s.deliveries() {
		out = append(out, d.id())
	}
	return out
}

// verified asserts every delivery's signature verifies under the secret
// and returns the t of each.
func (s *sink) verified(t *testing.T) []time.Time {
	t.Helper()
	var out []time.Time
	for i, d := range s.deliveries() {
		if err := Verify(secret, d.header, d.body, time.Time{}, 0); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
		at, _, _ := Parse(d.header)
		out = append(out, at)
	}
	return out
}

// failFirst answers status to the first n deliveries whose body names
// id, or to every delivery when id is empty, and 200 after.
func (s *sink) failFirst(n, status int, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	failed := 0
	s.plan = func(_ int, body []byte) (int, bool) {
		if (id == "" || delivery{body: body}.id() == id) && failed < n {
			failed++
			return status, false
		}
		return http.StatusOK, false
	}
}

// logs is a logger over a buffer the test reads back.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// harness is one store, clock, sink, and registry.
type harness struct {
	c    *clock
	st   *memory.Store
	sink *sink
	log  *logs
	reg  *metrics.Registry
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	c := newClock()
	return &harness{c: c, st: memory.New(memory.WithClock(c.Now)), sink: newSink(t), log: &logs{}, reg: metrics.NewRegistry()}
}

// scrape is the registry in the exposition format.
func (h *harness) scrape() string {
	var buf bytes.Buffer
	h.reg.WritePrometheus(&buf)
	return buf.String()
}

// worker builds a worker on the harness's store, or on st when given,
// with a short delivery deadline so a hanging sink is abandoned at once.
func (h *harness) worker(holder string, st store.Store, edit func(*WorkerOptions)) *Worker {
	if st == nil {
		st = h.st
	}
	o := WorkerOptions{
		Store: st, Holder: holder, Metrics: h.reg, Now: h.c.Now,
		Sink:   NewSink(SinkOptions{URL: h.sink.srv.URL, Secret: secret, Client: h.sink.srv.Client(), Deadline: 200 * time.Millisecond, Now: h.c.Now}),
		Logger: slog.New(slog.NewTextHandler(h.log, nil)),
	}
	if edit != nil {
		edit(&o)
	}
	return NewWorker(o)
}

// key is a Key object named by its id.
func key(id string) *v1.Key {
	return &v1.Key{Metadata: v1.ObjectMeta{Name: "k-" + id, Labels: map[string]string{"run": "r_42"}}, Status: v1.KeyStatus{ID: id, Owner: "https://login.example.com|alice"}}
}

// append journals one key.updated event about the object at the clock.
func (h *harness) append(t *testing.T, id, object string) {
	t.Helper()
	h.appendAt(t, id, object, h.c.Now())
}

func (h *harness) appendAt(t *testing.T, id, object string, at time.Time) {
	t.Helper()
	if err := Append(t.Context(), h.st.Journal(), Event{ID: id, Type: KeyUpdated, At: at, Subject: "https://login.example.com|alice", Reason: ReasonRequest, RequestID: "req_1", Object: key(object), Data: map[string]any{"paths": []string{"spec.models"}}}); err != nil {
		t.Fatal(err)
	}
}

// rows is one object's journal rows.
func (h *harness) rows(t *testing.T, object string) []store.Event {
	t.Helper()
	rows, _, err := h.st.Journal().ByObject(t.Context(), object, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// tick acquires and ticks once.
func tick(ctx context.Context, w *Worker) {
	w.acquire(ctx)
	w.Tick(ctx)
}

// errBroken is what a broken operation answers.
var errBroken = errors.New("the store is broken here")

// broken is a Store over another whose named operations fail, so the
// worker's handling of each store failure is exercised branch by branch.
type broken struct {
	store.Store
	fail map[string]bool
}

func (b *broken) Journal() store.Journal   { return brokenJournal{b.Store.Journal(), b.fail} }
func (b *broken) Leases() store.Leases     { return brokenLeases{b.Store.Leases(), b.fail} }
func (b *broken) Counters() store.Counters { return brokenCounters{b.Store.Counters(), b.fail} }

type brokenJournal struct {
	store.Journal
	fail map[string]bool
}

func (j brokenJournal) Pending(ctx context.Context, limit int) ([]store.Event, error) {
	if j.fail["Journal.Pending"] {
		return nil, errBroken
	}
	return j.Journal.Pending(ctx, limit)
}

func (j brokenJournal) Acknowledge(ctx context.Context, id string) error {
	if j.fail["Journal.Acknowledge"] {
		return errBroken
	}
	return j.Journal.Acknowledge(ctx, id)
}

func (j brokenJournal) Defer(ctx context.Context, id string, attempts int, next time.Time) error {
	if j.fail["Journal.Defer"] {
		return errBroken
	}
	return j.Journal.Defer(ctx, id, attempts, next)
}

func (j brokenJournal) Drop(ctx context.Context, id string) error {
	if j.fail["Journal.Drop"] {
		return errBroken
	}
	return j.Journal.Drop(ctx, id)
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

func (l brokenLeases) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	if l.fail["Leases.Acquire"] {
		return false, errBroken
	}
	return l.Leases.Acquire(ctx, name, holder, ttl)
}

func (l brokenLeases) Release(ctx context.Context, name, holder string) error {
	if l.fail["Leases.Release"] {
		return errBroken
	}
	return l.Leases.Release(ctx, name, holder)
}

type brokenCounters struct {
	store.Counters
	fail map[string]bool
}

func (c brokenCounters) Read(ctx context.Context, keys []string) (map[string]int64, error) {
	if c.fail["Counters.Read"] {
		return nil, errBroken
	}
	if c.fail["Counters.ReadBlind"] {
		return map[string]int64{}, nil
	}
	return c.Counters.Read(ctx, keys)
}

func (c brokenCounters) Add(ctx context.Context, key string, delta int64, expiresAt time.Time) (int64, error) {
	if c.fail["Counters.Add"] || (delta < 0 && c.fail["Counters.AddNegative"]) {
		return 0, errBroken
	}
	return c.Counters.Add(ctx, key, delta, expiresAt)
}
