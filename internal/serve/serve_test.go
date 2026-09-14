// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
)

const (
	subject = "https://login.example.com|alice"
	canary  = "sk-canary-7f3c9a-never-in-any-encoding"
	kek     = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
)

// stub is one upstream: a models route whose answer the test sets, and
// a record of every request it saw.
type stub struct {
	mu       sync.Mutex
	requests []*http.Request
	// pages is the models route's answer per page token, "" for the
	// first page; status answers every request with that code when set.
	pages  map[string]string
	status int
	body   string
	// hold blocks every request until it is closed, when set.
	hold chan struct{}
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(r.Context()))
	status, body, pages, hold := s.status, s.body, s.pages, s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	q := r.URL.Query()
	page := q.Get("after_id")
	if page == "" {
		page = q.Get("pageToken")
	}
	answer, ok := pages[page]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(answer))
}

func (s *stub) set(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *stub) setPages(pages map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.pages = 0, pages
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *stub) all() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.requests...)
}

// openaiList is the openai and lux list shape for the names.
func openaiList(names ...string) string {
	type entry struct {
		ID string `json:"id"`
	}
	var data []entry
	for _, n := range names {
		data = append(data, entry{n})
	}
	b, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	return string(b)
}

// harness is one store, one client source, one credential source, and a
// logger the tests read back.
type harness struct {
	st      *memory.Store
	clients *gateway.Clients
	creds   *StoreCredentials
	keys    *secrets.Keyring
	log     *bytes.Buffer
	logger  *slog.Logger
	now     time.Time
	mu      sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	keys, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{keys: keys, log: &bytes.Buffer{}, now: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)}
	h.st = memory.New(memory.WithClock(h.clock))
	h.clients = gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: true, Version: "test"})
	h.creds = &StoreCredentials{Credentials: h.st.Credentials(), Keys: keys}
	h.logger = slog.New(slog.NewTextHandler(&syncWriter{w: h.log, mu: &h.mu}, nil))
	return h
}

// syncWriter serialises the logger's writes with the test's reads.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (h *harness) logged() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.log.String()
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// provider stores one resolved Provider toward the stub with a sealed
// canary credential and returns it as the store renders it.
func (h *harness) provider(t *testing.T, name string, dialect v1.Dialect, baseURL string, edit func(p *v1.Provider)) *v1.Provider {
	t.Helper()
	ctx := t.Context()
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: name},
		Spec: v1.ProviderSpec{
			Dialect:    dialect,
			BaseURL:    baseURL,
			Credential: &v1.Credential{Header: dialect.CredentialHeader(), Scheme: dialect.CredentialScheme()},
			Discovery:  v1.Discovery{Mode: v1.DiscoveryAuto},
			Health:     v1.Health{Mode: v1.HealthProbe},
			Timeout:    "10s",
		},
		Status: v1.ProviderStatus{
			ID: v1.NewID(v1.PrefixProvider, h.clock(), nil), Owner: subject,
			Credential: &v1.CredentialStatus{Set: true, Version: 1, UpdatedAt: h.clock()},
			Warnings:   []string{},
		},
	}
	if edit != nil {
		edit(p)
	}
	if _, err := h.st.Objects().Put(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	row, err := h.keys.Seal(p.Status.ID, 1, []byte(canary))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Credentials().Put(ctx, p.Status.ID, row); err != nil {
		t.Fatal(err)
	}
	return h.get(t, p.Status.ID)
}

func (h *harness) get(t *testing.T, id string) *v1.Provider {
	t.Helper()
	obj, _, err := h.st.Objects().Get(t.Context(), v1.KindProvider, id)
	if err != nil {
		t.Fatal(err)
	}
	return obj.(*v1.Provider)
}

// models lists the Models of one source, or every Model when source is
// "", by name.
func (h *harness) models(t *testing.T, source string) map[string]*v1.Model {
	t.Helper()
	objs, _, err := h.st.Objects().List(t.Context(), v1.KindModel, store.Filter{Source: source}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*v1.Model{}
	for _, o := range objs {
		out[o.Name()] = o.(*v1.Model)
	}
	return out
}

// names is the sorted list of a map's keys, for one-line comparisons.
func names(m map[string]*v1.Model) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return strings.Join(out, " ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// events is every journal row of one type, in order.
func (h *harness) events(t *testing.T, typ string) []store.Event {
	t.Helper()
	all, err := h.st.Journal().Since(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Event
	for _, e := range all {
		if typ == "" || e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// declare stores a declared Model with one target on the Provider.
func (h *harness) declare(t *testing.T, name, provider, upstream string) *v1.Model {
	t.Helper()
	w := 100
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ModelSpec{Targets: []v1.Target{{Provider: provider, Model: upstream, Weight: &w}}, Fallback: v1.FallbackOnError},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, h.clock(), nil), Owner: subject, Source: v1.SourceDeclared, Warnings: []string{}},
	}
	if _, err := h.st.Objects().Put(t.Context(), m, 0); err != nil {
		t.Fatal(err)
	}
	return m
}

func (h *harness) discovery(holder string) *Discovery {
	return NewDiscovery(DiscoveryOptions{
		Store: h.st, Clients: h.clients, Credentials: h.creds, Interval: time.Hour, Tail: 10 * time.Millisecond,
		Holder: holder, Logger: h.logger, Now: h.clock, Jitter: func() float64 { return 0 },
	})
}

func (h *harness) health(holder string) *Health {
	return NewHealth(HealthOptions{
		Store: h.st, Clients: h.clients, Credentials: h.creds, Interval: time.Hour,
		Holder: holder, Logger: h.logger, Now: h.clock,
	})
}

// serve starts a stub and returns it with its URL.
func serveStub(t *testing.T, s *stub) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv.URL
}

// waitUntil polls cond for up to a second.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within a second", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// run starts fn under a context the test cancels at its end and waits
// for it to return.
func run(t *testing.T, fn func(ctx context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the job did not stop")
		}
	})
}

func mustKeys(t *testing.T, raw string) *secrets.Keyring {
	t.Helper()
	k, err := secrets.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
