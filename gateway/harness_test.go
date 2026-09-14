// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	v1 "latere.ai/x/lux/manifest/v1"
)

// logBuffer collects the handler's log lines under a lock, because some
// tests drive the handler from a goroutine while they read.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}

// The fakes below satisfy every interface of Options from maps, and count
// every call, so a test can prove what the handler reached and what it
// did not.

type fakeKeys struct {
	mu      sync.Mutex
	byHash  map[string]*v1.Key
	calls   atomic.Int64
	err     error
	lastCtx context.Context // the context of the last lookup, for the span tests
}

func (f *fakeKeys) ByHash(ctx context.Context, hash string) (*v1.Key, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCtx = ctx
	return f.byHash[hash], nil
}

func (f *fakeKeys) context() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCtx
}

func (f *fakeKeys) add(k *v1.Key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[hashValue(value)] = k
}

type fakeCatalog struct {
	mu        sync.Mutex
	models    map[string]*v1.Model
	providers map[string]*v1.Provider // by name and by id
	calls     atomic.Int64
	err       error
}

func (f *fakeCatalog) Model(_ context.Context, name string) (*v1.Model, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models[name], nil
}

func (f *fakeCatalog) Models(context.Context) ([]*v1.Model, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*v1.Model, 0, len(f.models))
	for _, m := range f.models {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeCatalog) Provider(_ context.Context, nameOrID string) (*v1.Provider, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.providers[nameOrID], nil
}

func (f *fakeCatalog) addProvider(p *v1.Provider) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers[p.Metadata.Name] = p
	f.providers[p.Status.ID] = p
}

func (f *fakeCatalog) addModel(m *v1.Model) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models[m.Metadata.Name] = m
}

type fakeCreds struct {
	values map[string]string // provider id to value
	calls  atomic.Int64
	err    error
}

func (f *fakeCreds) Credential(_ context.Context, providerID string) ([]byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.values[providerID]), nil
}

// fakeRouter orders a Model's targets in manifest order, looking each
// Provider up in the catalog, and records the outcomes the circuit
// would.
type fakeRouter struct {
	catalog   *fakeCatalog
	mu        sync.Mutex
	exclude   map[string]bool // "provider/model" left out of the order
	deny      map[string]bool // "provider/model" whose Allow answers false
	successes map[string]int
	failures  map[string]int
	calls     atomic.Int64
	err       error
}

func targetKey(t Target) string { return t.Provider.Metadata.Name + "/" + t.Model }

func (f *fakeRouter) Targets(ctx context.Context, m *v1.Model) ([]Target, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	var out []Target
	for _, t := range m.Spec.Targets {
		p, _ := f.catalog.Provider(ctx, t.Provider)
		if p == nil {
			continue
		}
		target := Target{Provider: p, Model: t.Model}
		f.mu.Lock()
		excluded := f.exclude[targetKey(target)]
		f.mu.Unlock()
		if !excluded {
			out = append(out, target)
		}
	}
	return out, nil
}

func (f *fakeRouter) Allow(t Target) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.deny[targetKey(t)]
}

func (f *fakeRouter) RecordSuccess(t Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes[targetKey(t)]++
}

func (f *fakeRouter) RecordFailure(t Target) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[targetKey(t)]++
}

func (f *fakeRouter) counts(key string) (successes, failures int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.successes[key], f.failures[key]
}

type fakeLease struct {
	mu      sync.Mutex
	settled []Tokens
}

func (l *fakeLease) Settle(_ context.Context, t Tokens) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.settled = append(l.settled, t)
}

type fakeLimiter struct {
	mu           sync.Mutex
	reservations []Reservation
	refusal      *Refusal
	err          error
	lease        *fakeLease
	calls        atomic.Int64
}

func (f *fakeLimiter) Reserve(_ context.Context, r Reservation) (Lease, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.reservations = append(f.reservations, r)
	f.mu.Unlock()
	if f.refusal != nil {
		return nil, f.refusal
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.lease, nil
}

func (f *fakeLimiter) last() Reservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reservations[len(f.reservations)-1]
}

type fakeRecorder struct {
	mu      sync.Mutex
	records []Record
}

func (f *fakeRecorder) Record(r Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, r)
}

func (f *fakeRecorder) last(t *testing.T) Record {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.records) == 0 {
		t.Fatal("no record was written")
	}
	return f.records[len(f.records)-1]
}

func (f *fakeRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// countingTransport counts every outbound request and refuses any host
// that is not a stub provider's, so the hot-path proof is a count and
// not a promise.
type countingTransport struct {
	next  http.RoundTripper
	hosts map[string]bool
	calls atomic.Int64
	other atomic.Int64
}

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	if !t.hosts[r.URL.Host] {
		t.other.Add(1)
		return nil, errors.New("dial to a host that is no provider: " + r.URL.Host)
	}
	return t.next.RoundTrip(r)
}

type fakeClients struct {
	transport *countingTransport
	calls     atomic.Int64
	err       error
}

func (f *fakeClients) Client(context.Context, *v1.Provider) (*http.Client, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &http.Client{Transport: f.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type fakeHealth struct {
	mu       sync.Mutex
	observed map[string][]bool
}

func (f *fakeHealth) Observe(providerID string, failed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed[providerID] = append(f.observed[providerID], failed)
}

// stub is one stub provider: it records what it received and answers
// what the test configured.
type stub struct {
	*httptest.Server
	mu       sync.Mutex
	received []*received
	handle   func(w http.ResponseWriter, r *http.Request)
	hits     atomic.Int64
}

// received is one request as the stub saw it.
type received struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.received = append(s.received, &received{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		handle := s.handle
		s.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if handle != nil {
			handle(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stub) respond(h func(w http.ResponseWriter, r *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle = h
}

// respondJSON answers every request with one status and body.
func (s *stub) respondJSON(status int, body string) {
	s.respond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

// respondSSE answers every request with the frames, flushed one by one.
func (s *stub) respondSSE(frames ...string) {
	s.respond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			_ = rc.Flush()
		}
	})
}

func (s *stub) last(t *testing.T) *received {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.received) == 0 {
		t.Fatal("the stub provider received nothing")
	}
	return s.received[len(s.received)-1]
}

func (s *stub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

func (s *stub) host() string { return strings.TrimPrefix(s.URL, "http://") }

// world is one handler with its fakes and its four stub providers, one
// per dialect, and a fixed catalog.
type world struct {
	t        *testing.T
	h        *Handler
	keys     *fakeKeys
	catalog  *fakeCatalog
	creds    *fakeCreds
	router   *fakeRouter
	limiter  *fakeLimiter
	recorder *fakeRecorder
	clients  *fakeClients
	health   *fakeHealth
	log      *logBuffer
	mu       sync.Mutex
	now      time.Time
	ids      atomic.Int64

	openai, anthropic, gemini, lux *stub
	stubs                          map[v1.Dialect]*stub
}

// The fixture's credentials: Key values and Provider credentials.
const (
	keyValue           = "lux_0123456789abcdefghijklmnopqrstuvwxyzABCD"
	passthroughValue   = "lux_passthrough0123456789abcdefghijklmnopqr"
	restrictedValue    = "lux_restricted0123456789abcdefghijklmnopqrs"
	suppliedValue      = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2xvZ2luLmV4YW1wbGUuY29tIiwic3ViIjoiYWxpY2UifQ.c2lnbmF0dXJl"
	unregisteredToken  = "eyJhbGciOiJSUzI1NiJ9.eyJpc3MiOiJodHRwczovL2xvZ2luLmV4YW1wbGUuY29tIiwic3ViIjoiYm9iIn0.b3RoZXJzaWc"
	providerKeyPasted  = "sk-openai-pasted-by-mistake-0123456789"
	openaiCredential   = "sk-oai-secret"
	anthropicCred      = "sk-ant-secret"
	geminiCredential   = "AIza-gem-secret"
	luxCredential      = "lux_upstream-secret"
	disabledKeyValue   = "lux_disabled0123456789abcdefghijklmnopqrstu"
	expiredKeyValue    = "lux_expired0123456789abcdefghijklmnopqrstuv"
	unavailableModel   = "down"
	geminiOnlyModel    = "gemini-only"
	openaiAliasModel   = "alias"
	openaiReasonerName = "reasoner"
)

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		t:        t,
		keys:     &fakeKeys{byHash: map[string]*v1.Key{}},
		catalog:  &fakeCatalog{models: map[string]*v1.Model{}, providers: map[string]*v1.Provider{}},
		creds:    &fakeCreds{values: map[string]string{}},
		limiter:  &fakeLimiter{lease: &fakeLease{}},
		recorder: &fakeRecorder{},
		health:   &fakeHealth{observed: map[string][]bool{}},
		log:      &logBuffer{},
		now:      time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
	}
	w.router = &fakeRouter{catalog: w.catalog, exclude: map[string]bool{}, deny: map[string]bool{}, successes: map[string]int{}, failures: map[string]int{}}
	w.openai, w.anthropic, w.gemini, w.lux = newStub(t), newStub(t), newStub(t), newStub(t)
	w.stubs = map[v1.Dialect]*stub{v1.DialectOpenAI: w.openai, v1.DialectAnthropic: w.anthropic, v1.DialectGemini: w.gemini, v1.DialectLux: w.lux}
	// The transport is spec 005's in the one rule the door tests read:
	// it adds no Accept-Encoding of its own, so the header's absence on
	// the outbound request is the gateway's doing and not the client's.
	inner := http.DefaultTransport.(*http.Transport).Clone()
	inner.DisableCompression = true
	// The instrumented transport of spec 005's client, so the span tests
	// see the client span each attempt opens under lux.upstream.
	transport := &countingTransport{next: otelhttp.NewTransport(inner), hosts: map[string]bool{}}
	for _, s := range w.stubs {
		transport.hosts[s.host()] = true
	}
	w.clients = &fakeClients{transport: transport}

	w.provider("oai", v1.DialectOpenAI, w.openai.URL+"/v1", openaiCredential)
	w.provider("ant", v1.DialectAnthropic, w.anthropic.URL+"/v1", anthropicCred)
	w.provider("gem", v1.DialectGemini, w.gemini.URL+"/v1beta", geminiCredential)
	w.provider("lx", v1.DialectLux, w.lux.URL+"/v1", luxCredential)

	w.model("gpt", target("oai", "gpt-4.1"))
	w.model("gpt-4.1", target("oai", "gpt-4.1")) // equal names: the byte-identical passthrough
	w.model("claude", target("ant", "claude-3"))
	w.model("claude-3", target("ant", "claude-3"))
	w.model("gemini", target("gem", "gemini-pro"))
	w.model("luxm", target("lx", "luxm"))
	w.model(openaiAliasModel, target("oai", "gpt-4.1-mini"))
	w.model(openaiReasonerName, target("oai", "o3-mini"))
	w.model("dual", target("oai", "gpt-4.1"), target("ant", "claude-3"))
	w.model("never", target("oai", "gpt-4.1"), target("ant", "claude-3")).Spec.Fallback = v1.FallbackNever
	w.model(geminiOnlyModel, target("gem", "gemini-pro"))
	w.model("multi", target("gem", "gemini-pro"), target("oai", "gpt-4.1"))
	w.model("priced", target("oai", "gpt-4.1")).Spec.MaxOutputTokens = 512
	down := w.model(unavailableModel, target("oai", "gpt-4.1"))
	down.Status.Available = new(bool)

	w.key("all", keyValue, "*")
	pass := w.key("pass", passthroughValue, "*")
	pass.Spec.Passthrough = true
	w.key("restricted", restrictedValue, "gpt", "alias")
	w.key("supplied", suppliedValue, "*").Status.Prefix = "sup_1a2b3c4d"
	disabled := w.key("disabled", disabledKeyValue, "*")
	disabled.Spec.Disabled = true
	expired := w.key("expired", expiredKeyValue, "*")
	expired.Status.ExpiresAt = w.now.Add(-time.Hour)

	w.h = New(w.options())
	return w
}

func (w *world) options() Options {
	return Options{
		Keys: w.keys, Catalog: w.catalog, Credentials: w.creds, Router: w.router, Limiter: w.limiter,
		Recorder: w.recorder, Clients: w.clients, Health: w.health, Version: "test", MaxBodyBytes: 1 << 20,
		Logger: slog.New(slog.NewJSONHandler(w.log, nil)),
		Now:    w.clock,
		NewID:  func() string { return "req_" + strings.Repeat("0", 20) + padID(w.ids.Add(1)) },
	}
}

// clock is the fixture's time: it advances one millisecond per reading,
// so every duration the handler measures is positive and every record's
// timestamps are ordered, while the tests still name the instant a Key
// expired against.
func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(time.Millisecond)
	return w.now
}

func padID(n int64) string {
	s := "000000" + itoa(n)
	return s[len(s)-6:]
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (w *world) provider(name string, d v1.Dialect, baseURL, credential string) *v1.Provider {
	p := &v1.Provider{}
	p.Metadata.Name = name
	p.Spec.Dialect, p.Spec.BaseURL, p.Spec.Timeout = d, baseURL, "5s"
	p.Spec.Credential = &v1.Credential{Header: d.CredentialHeader(), Scheme: d.CredentialScheme()}
	p.Status.ID = "prv_" + strings.ToUpper(name) + strings.Repeat("0", 26-len(name))
	w.catalog.addProvider(p)
	w.creds.values[p.Status.ID] = credential
	return p
}

func target(provider, model string) v1.Target {
	weight := 100
	return v1.Target{Provider: provider, Model: model, Weight: &weight}
}

func (w *world) model(name string, targets ...v1.Target) *v1.Model {
	m := &v1.Model{}
	m.Metadata.Name = name
	m.Spec.Targets, m.Spec.Fallback = targets, v1.FallbackOnError
	m.Status.ID = "mdl_" + strings.ToUpper(strings.ReplaceAll(name, "-", "")) + strings.Repeat("0", 26-len(strings.ReplaceAll(name, "-", "")))
	available := true
	m.Status.Available = &available
	w.catalog.addModel(m)
	return m
}

func (w *world) key(name, value string, selectors ...string) *v1.Key {
	k := &v1.Key{}
	k.Metadata.Name = name
	k.Metadata.Labels = map[string]string{"team": name}
	k.Spec.Models = selectors
	k.Status.ID = "key_" + strings.ToUpper(name) + strings.Repeat("0", 26-len(name))
	k.Status.Prefix = value[:12]
	k.Status.Owner = "https://login.example.com|alice"
	w.keys.add(k, value)
	return k
}

// request builds a door request with the fixture's Key as a bearer.
func (w *world) request(method, path, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Header.Set("Authorization", "Bearer "+keyValue)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

// do runs one request through the handler and returns the recorder.
func (w *world) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

// post is a POST with the fixture's Key.
func (w *world) post(path, body string) *httptest.ResponseRecorder {
	return w.do(w.request(http.MethodPost, path, body))
}

// errorCode reads the code off a door's error response through the
// header, and checks that the body's shape carries the same code.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) Code {
	t.Helper()
	code := Code(rec.Header().Get(HeaderError))
	if code == "" {
		t.Fatalf("no %s header on a %d response: %s", HeaderError, rec.Code, rec.Body.String())
	}
	if rec.Code != code.Status() {
		t.Errorf("status %d for %s, want %d", rec.Code, code, code.Status())
	}
	return code
}

// chatBody is a chat completion request naming model.
func chatBody(model string, stream bool) string {
	b := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	if stream {
		b["stream"] = true
	}
	out, _ := json.Marshal(b)
	return string(out)
}

// messagesBody is a Messages API request naming model.
func messagesBody(model string, stream bool) string {
	b := map[string]any{"model": model, "max_tokens": 64, "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	if stream {
		b["stream"] = true
	}
	out, _ := json.Marshal(b)
	return string(out)
}

// luxBody is a lux generate request naming model.
func luxBody(model string, stream bool) string {
	b := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "blocks": []map[string]any{{"type": "text", "text": "hello"}}}}}
	if stream {
		b["stream"] = true
	}
	out, _ := json.Marshal(b)
	return string(out)
}

// Canned upstream answers per dialect.
const (
	openaiChatResponse = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`
	anthropicResponse  = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":1,"cache_creation_input_tokens":5}}`
	geminiResponse     = `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"cachedContentTokenCount":3},"modelVersion":"gemini-pro"}`
	luxResponse        = `{"id":"lux-1","model":"luxm","blocks":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1,"reasoning_tokens":1}}`
)
