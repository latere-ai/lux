// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package events_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/api"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/events"
	"latere.ai/x/lux/internal/reqlog"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/manifest"
	"latere.ai/x/lux/metering"
)

// The canaries: each is a value that must reach the sink nowhere.
const (
	canaryKey        = "platform-token-canary-7f3c9a-never-in-any-encoding"
	canaryCredential = "sk-canary-credential-1d2e3f-never-in-any-encoding"
	canaryPrompt     = "canary-prompt-4f1e9c-never-in-an-event"
	canaryCompletion = "canary-completion-8b2d7a-never-in-an-event"
	clientAddress    = "203.0.113.9"
	kek              = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
)

var sinkSecret = []byte("the-operator's-secret")

// upstream is one OpenAI-dialect stub: a models route listing the names
// set, a chat completion answering the canary completion, and a status
// that, when set, answers every request, which is how the health job
// sees it fail.
type upstream struct {
	mu     sync.Mutex
	names  []string
	status atomic.Int32
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if code := int(u.status.Load()); code != 0 {
		w.WriteHeader(code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/models"):
		u.mu.Lock()
		names := slices.Clone(u.names)
		u.mu.Unlock()
		data := make([]map[string]any, 0, len(names))
		for _, n := range names {
			data = append(data, map[string]any{"id": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	default:
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"`+canaryCompletion+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	}
}

func (u *upstream) list(names ...string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.names = names
}

// collector is the operator's sink: it verifies every signature and
// keeps every body.
type collector struct {
	t   *testing.T
	mu  sync.Mutex
	got [][]byte
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := events.Verify(sinkSecret, r.Header.Get(events.Header), body, time.Time{}, 0); err != nil {
		c.t.Errorf("sink: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.got = append(c.got, body)
	c.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (c *collector) bodies() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.got)
}

// plane is the whole surface over one memory store: the API of spec
// 011, the doors of spec 004 with the seams of specs 005 to 009, the two
// jobs of spec 005, and the sink and the archive of this spec.
type plane struct {
	t       *testing.T
	st      *memory.Store
	api     *api.Handler
	doors   *gateway.Handler
	up      *upstream
	upURL   string
	sink    *collector
	sinkURL string
	bucket  *s3test.Server
	archive *reqlog.Exporter
	clients *gateway.Clients
	creds   *serve.StoreCredentials
	logger  *slog.Logger
	bearer  string
	subject string
	now     time.Time
	mu      sync.Mutex
}

func (p *plane) clock() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.now
}

func newPlane(t *testing.T) *plane {
	t.Helper()
	p := &plane{t: t, now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), logger: slog.New(slog.DiscardHandler)}
	p.st = memory.New(memory.WithClock(p.clock))
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	p.bearer = iss.Mint(issuertest.Claims{Sub: "alice"})
	p.subject = iss.URL() + "|alice"
	a, err := auth.New(t.Context(), auth.Options{Issuers: []string{iss.URL()}, Audiences: []string{"lux"}, AdminSubjects: []string{p.subject}, Now: p.clock})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	p.up = &upstream{}
	up := httptest.NewServer(p.up)
	t.Cleanup(up.Close)
	p.upURL = up.URL + "/v1"
	p.sink = &collector{t: t}
	sink := httptest.NewServer(p.sink)
	t.Cleanup(sink.Close)
	p.sinkURL = sink.URL
	base, _ := url.Parse("https://lux.example.com")
	p.clients = gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: true, Version: "test"})
	p.creds = &serve.StoreCredentials{Credentials: p.st.Credentials(), Keys: keys}
	p.api = api.New(api.Options{
		Store: p.st, Auth: a, Authorizer: a.Authorizer(&serve.ObjectOwners{Objects: p.st.Objects()}),
		PublicURL: base, Version: "test", MaxManifestBytes: 4096, AllowPrivateUpstreams: true,
		Defaults: manifest.Defaults{RequestsPerMinute: 60, TokensPerMinute: 1000, Timeout: time.Minute},
		Keys:     keys, Clients: p.clients, Logger: p.logger, Now: p.clock,
	})
	catalog := &serve.Catalog{Objects: p.st.Objects()}
	cache := serve.NewKeyCache(serve.KeyCacheOptions{Store: p.st, TTL: time.Hour, Logger: p.logger, Now: p.clock})
	limiter := serve.NewLimiter(serve.LimiterOptions{Store: p.st, Budgets: cache, Flush: time.Second, Logger: p.logger, Now: p.clock})
	p.bucket = s3test.New(t, "archive")
	p.archive = reqlog.NewExporter(reqlog.ExporterOptions{Bucket: p.bucket.Client(true), Replica: "replica-a", Logger: p.logger, Now: p.clock})
	recorder := serve.NewRecorder(serve.RecorderOptions{Store: p.st, Catalog: catalog, Limiter: limiter, Archive: p.archive, Flush: time.Second, Logger: p.logger, Now: p.clock})
	p.doors = gateway.New(gateway.Options{
		Keys: cache, Catalog: catalog, Credentials: p.creds,
		Router:   gateway.NewTargetRouter(gateway.RouterOptions{Catalog: catalog, Now: p.clock}),
		Limiter:  limiter,
		Recorder: recorder,
		Clients:  p.clients,
		Version:  "test",
		Now:      p.clock,
	})
	return p
}

// control sends one request to the API as alice and asserts the status.
func (p *plane) control(method, path, body string, want int) *httptest.ResponseRecorder {
	p.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = clientAddress + ":4444"
	r.Header.Set("Authorization", "Bearer "+p.bearer)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	p.api.ServeHTTP(rec, r)
	if rec.Code != want {
		p.t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
	}
	return rec
}

// door sends one chat completion through the openai door with value as
// the Key and asserts the code the door answers.
func (p *plane) door(value, model string, wantCode gateway.Code) {
	p.t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": canaryPrompt}}})
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	r.RemoteAddr = clientAddress + ":4444"
	r.Header.Set("Authorization", "Bearer "+value)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.doors.ServeHTTP(rec, r)
	if got := gateway.Code(rec.Header().Get(gateway.HeaderError)); got != wantCode {
		p.t.Fatalf("door with %s: code %q, want %q (%d %s)", model, got, wantCode, rec.Code, rec.Body.String())
	}
}

// journalled counts the journal rows by type.
func (p *plane) journalled() map[string]int {
	p.t.Helper()
	rows, err := p.st.Journal().Since(p.t.Context(), 0, 0)
	if err != nil {
		p.t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range rows {
		out[r.Type]++
	}
	return out
}

// await polls until cond holds or five seconds pass.
func (p *plane) await(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			p.t.Fatalf("waited five seconds for %s; journal %v", what, p.journalled())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// exercise drives every row of the type table once: the four kinds
// through the API, an update and a delete of each, a rotate, the two
// exhaustions through a door, discovery and health through their jobs,
// and check.ping through the sink. It returns the count of each type
// the run should have journalled.
func (p *plane) exercise() map[string]int {
	p.t.Helper()
	ctx := p.t.Context()
	p.up.list("gpt-4.1", "extra")
	provider := `{"spec": {"dialect": "openai", "baseURL": "` + p.upURL + `", "credential": {"value": "` + canaryCredential + `"}, "discovery": {"mode": "auto"}, "health": {"mode": "probe"}}}`
	p.control(http.MethodPut, "/v1/providers/up", provider, http.StatusCreated)
	p.control(http.MethodPut, "/v1/providers/up", `{"metadata": {"labels": {"tier": "a"}}, `+provider[1:], http.StatusOK)
	model := `{"spec": {"targets": [{"provider": "up", "model": "gpt-4.1"}], "pricing": {"currency": "USD", "per": 1, "input": "1", "output": "1"}}}`
	p.control(http.MethodPut, "/v1/models/m", model, http.StatusCreated)
	p.control(http.MethodPut, "/v1/models/m", `{"metadata": {"labels": {"tier": "a"}}, `+model[1:], http.StatusOK)
	budget := `{"spec": {"amount": "0.5", "currency": "USD", "window": "1h"}}`
	p.control(http.MethodPut, "/v1/budgets/b", budget, http.StatusCreated)
	p.control(http.MethodPut, "/v1/budgets/b", `{"metadata": {"labels": {"tier": "a"}}, `+budget[1:], http.StatusOK)
	limited := `{"spec": {"models": ["m"], "value": "` + canaryKey + `", "limits": {"spend": {"amount": "0.5", "currency": "USD", "window": "1h"}}}}`
	p.control(http.MethodPut, "/v1/keys/limited", limited, http.StatusCreated)
	p.control(http.MethodPut, "/v1/keys/limited", `{"metadata": {"labels": {"tier": "a"}}, "spec": {"models": ["m"], "limits": {"spend": {"amount": "0.5", "currency": "USD", "window": "1h"}}}}`, http.StatusOK)
	p.control(http.MethodPut, "/v1/keys/drawing", `{"spec": {"models": ["m"], "budget": "b"}}`, http.StatusCreated)
	open := p.control(http.MethodPut, "/v1/keys/open", `{"spec": {"models": ["m"]}}`, http.StatusCreated)
	var created struct {
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
	}
	if err := json.Unmarshal(open.Body.Bytes(), &created); err != nil || created.Status.Value == "" {
		p.t.Fatalf("the open Key's value: %v %s", err, open.Body.String())
	}
	p.control(http.MethodPut, "/v1/keys/drawing", `{"metadata": {"labels": {"tier": "a"}}, "spec": {"models": ["m"], "budget": "b"}}`, http.StatusOK)

	// The doors: a served request with the canary prompt and completion,
	// then a Key's spend window and a Budget's window at their amounts.
	p.door(created.Status.Value, "m", "")
	p.door(canaryKey, "m", gateway.CodeSpendExceeded)
	rotated := p.control(http.MethodPost, "/v1/keys/drawing/rotate", "", http.StatusOK)
	var fresh struct {
		Status struct {
			Value string `json:"value"`
		} `json:"status"`
	}
	if err := json.Unmarshal(rotated.Body.Bytes(), &fresh); err != nil || fresh.Status.Value == "" {
		p.t.Fatalf("the rotated Key's value: %v %s", err, rotated.Body.String())
	}
	p.door(fresh.Status.Value, "m", gateway.CodeBudgetExhausted)

	// Discovery declares the two upstream names and removes one dropped.
	jobs, stopJobs := context.WithCancel(ctx)
	var running sync.WaitGroup
	discovery := serve.NewDiscovery(serve.DiscoveryOptions{Store: p.st, Clients: p.clients, Credentials: p.creds, Interval: 20 * time.Millisecond, Tail: time.Hour, Holder: "d", Logger: p.logger, Now: p.clock})
	running.Go(func() { discovery.Run(jobs) })
	p.await("two discovered Models", func() bool { return p.journalled()[events.ModelDiscovered] == 2 })
	p.up.list("gpt-4.1")
	p.await("one removed Model", func() bool { return p.journalled()[events.ModelRemoved] == 1 })
	stopJobs()
	running.Wait()

	// Health sees the upstream fail three probes, then answer again.
	jobs, stopJobs = context.WithCancel(ctx)
	p.up.status.Store(http.StatusInternalServerError)
	health := serve.NewHealth(serve.HealthOptions{Store: p.st, Clients: p.clients, Credentials: p.creds, Interval: 20 * time.Millisecond, Holder: "h", Logger: p.logger, Now: p.clock})
	running.Go(func() { health.Run(jobs) })
	p.await("provider.unreachable", func() bool { return p.journalled()[events.ProviderUnreachable] == 1 })
	p.up.status.Store(0)
	p.await("provider.healthy", func() bool { return p.journalled()[events.ProviderHealthy] == 1 })
	stopJobs()
	running.Wait()

	for _, path := range []string{"/v1/keys/limited", "/v1/keys/drawing", "/v1/keys/open", "/v1/budgets/b", "/v1/models/m", "/v1/providers/up"} {
		p.control(http.MethodDelete, path, "", http.StatusNoContent)
	}
	fenceBody, err := json.Marshal(map[string]any{"owner": p.subject, "labels": map[string]string{"run": "audit"}})
	if err != nil {
		p.t.Fatal(err)
	}
	p.control(http.MethodPost, "/v1/keys/reserved/fence", string(fenceBody), http.StatusOK)
	p.control(http.MethodPost, "/v1/keys/reserved/fence", string(fenceBody), http.StatusOK)
	if err := events.NewSink(events.SinkOptions{URL: p.sinkURL, Secret: sinkSecret, Now: p.clock}).Ping(ctx); err != nil {
		p.t.Fatal(err)
	}
	return map[string]int{
		events.ProviderCreated: 1, events.ProviderUpdated: 1, events.ProviderDeleted: 1,
		events.ProviderUnreachable: 1, events.ProviderHealthy: 1,
		events.ModelCreated: 1, events.ModelUpdated: 1, events.ModelDeleted: 1,
		events.ModelDiscovered: 2, events.ModelRemoved: 1,
		events.KeyFenced: 1, events.KeyCreated: 3, events.KeyUpdated: 2, events.KeyRotated: 1, events.KeyDeleted: 3, events.KeyExhausted: 1,
		events.BudgetCreated: 1, events.BudgetUpdated: 1, events.BudgetDeleted: 1, events.BudgetExhausted: 1,
	}
}

// deliver runs the worker until every journal row is acknowledged.
func (p *plane) deliver() {
	p.t.Helper()
	ctx, cancel := context.WithCancel(p.t.Context())
	defer cancel()
	w := events.NewWorker(events.WorkerOptions{
		Store: p.st, Holder: "w", Poll: 5 * time.Millisecond, Logger: p.logger, Now: p.clock,
		Sink: events.NewSink(events.SinkOptions{URL: p.sinkURL, Secret: sinkSecret, Now: p.clock}),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	total := 0
	for _, n := range p.journalled() {
		total += n
	}
	p.await("every row delivered", func() bool { return len(p.sink.bodies()) >= total+1 && w.Pending() == 0 })
	cancel()
	<-done
}

// TestEventTable drives every type of the table by the action in its
// row and reads what the sink received: each type the number of times
// its action ran, with the reason of its source, the subject and the
// request id set for a request and empty otherwise, and data carrying
// the row's members and nothing outside the row.
func TestEventTable(t *testing.T) {
	p := newPlane(t)
	want := p.exercise()
	if got := p.journalled(); !mapsEqual(got, want) {
		t.Fatalf("journalled %v, want %v", got, want)
	}
	p.deliver()
	got := map[string]int{}
	for _, body := range p.sink.bodies() {
		var rec events.Record
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Fatal(err)
		}
		got[rec.Type]++
		row, ok := events.Table[rec.Type]
		if !ok {
			t.Errorf("%s is not in the table", rec.Type)
			continue
		}
		if rec.Reason != row.Reason || !strings.HasPrefix(rec.ID, "evt_") || rec.Time.IsZero() {
			t.Errorf("%s: reason %q, want %q; id %s; time %s", rec.Type, rec.Reason, row.Reason, rec.ID, rec.Time)
		}
		if isRequest := rec.Reason == events.ReasonRequest; (rec.Subject == p.subject) != isRequest || (strings.HasPrefix(rec.RequestID, "req_")) != isRequest {
			t.Errorf("%s: subject %q, request_id %q for reason %s", rec.Type, rec.Subject, rec.RequestID, rec.Reason)
		}
		if rec.Type == events.CheckPing {
			if rec.Object.Kind != "" || rec.Object.ID != "" {
				t.Errorf("check.ping names an object: %+v", rec.Object)
			}
		} else if rec.Object.Kind == "" || rec.Object.ID == "" || rec.Object.Name == "" || rec.Object.Owner != p.subject || rec.Object.Labels == nil {
			t.Errorf("%s: object %+v", rec.Type, rec.Object)
		}
		data, ok := rec.Data.(map[string]any)
		if !ok {
			t.Errorf("%s: data is a %T", rec.Type, rec.Data)
			continue
		}
		for _, m := range row.Members {
			if _, ok := data[m]; !ok {
				t.Errorf("%s: data lacks %s: %v", rec.Type, m, data)
			}
		}
		for m := range data {
			if !slices.Contains(row.Members, m) && !slices.Contains(row.Optional, m) {
				t.Errorf("%s: data carries %s, which its row does not name: %v", rec.Type, m, data)
			}
		}
	}
	want[events.CheckPing] = 1
	if !mapsEqual(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
}

// TestEventsCarryNoSecrets runs the canaries through every mutation and
// every event source and asserts none reaches the sink: the supplied
// Key value and its hash, the Provider credential and its ciphertext in
// any encoding, the prompt, the completion, the caller's address, and
// the caller's bearer.
func TestEventsCarryNoSecrets(t *testing.T) {
	p := newPlane(t)
	p.exercise()
	rows, err := p.st.Journal().Since(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	p.deliver()
	bodies := p.sink.bodies()
	for _, r := range rows {
		bodies = append(bodies, r.Payload)
	}
	forbidden := map[string]string{
		"the Key value": canaryKey, "the Key hash": serve.HashKeyValue(canaryKey),
		"the credential": canaryCredential, "the prompt": canaryPrompt, "the completion": canaryCompletion,
		"the caller's address": clientAddress, "the bearer": p.bearer,
	}
	for _, enc := range []struct {
		name string
		fn   func([]byte) string
	}{{"hex", hex.EncodeToString}, {"base64", base64.StdEncoding.EncodeToString}, {"base64url", base64.RawURLEncoding.EncodeToString}} {
		for name, v := range map[string]string{"the Key value": canaryKey, "the credential": canaryCredential} {
			forbidden[name+" in "+enc.name] = enc.fn([]byte(v))
		}
	}
	if len(bodies) < 20 {
		t.Fatalf("%d bodies", len(bodies))
	}
	for _, body := range bodies {
		for name, v := range forbidden {
			if bytes.Contains(body, []byte(v)) {
				t.Errorf("%s reached the sink in %s", name, body)
			}
		}
	}
}

// TestArchiveCarriesNoContent is the archive's half of the canary row,
// over the same run: the records of the door requests reach the bucket
// as NDJSON lines that each parse as one metering.Record, and no
// archived byte carries the canary Key value, credential, prompt,
// completion, the caller's address, or the bearer. The type half is the
// same walk metering runs over Record: no member that could hold a body.
func TestArchiveCarriesNoContent(t *testing.T) {
	var leaks []string
	walk(reflect.TypeFor[metering.Record](), "Record", &leaks)
	if len(leaks) != 0 {
		t.Fatalf("Record can carry content: %v", leaks)
	}
	p := newPlane(t)
	p.exercise()
	if err := p.archive.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keys := p.bucket.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "lux/2026/09/14/12/replica-a-") {
		t.Fatalf("keys %v", keys)
	}
	data, _ := p.bucket.Get(keys[0])
	if ct, _ := p.bucket.ContentType(keys[0]); ct != reqlog.ContentType {
		t.Fatalf("Content-Type %q", ct)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d archived records for three door requests:\n%s", len(lines), data)
	}
	statuses := map[metering.Status]int{}
	for _, line := range lines {
		var rec metering.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if !strings.HasPrefix(rec.ID, "req_") || rec.Key.ID == "" || rec.Door != "openai" {
			t.Errorf("record %+v", rec)
		}
		statuses[rec.Status]++
	}
	if statuses[metering.StatusOK] != 1 || statuses[metering.StatusRefused] != 2 {
		t.Fatalf("statuses %v", statuses)
	}
	for name, v := range map[string]string{
		"the Key value": canaryKey, "the Key hash": serve.HashKeyValue(canaryKey), "the credential": canaryCredential,
		"the prompt": canaryPrompt, "the completion": canaryCompletion, "the caller's address": clientAddress, "the bearer": p.bearer,
	} {
		if bytes.Contains(data, []byte(v)) {
			t.Errorf("%s reached the archive", name)
		}
	}
}

// walk reports every field of ty whose kind could hold content: an
// interface, a byte slice, a pointer to one, or a function; the walk
// metering's TestRecordCarriesNoContent runs.
func walk(ty reflect.Type, path string, out *[]string) {
	switch ty.Kind() {
	case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		*out = append(*out, path+" is a "+ty.Kind().String())
	case reflect.Slice, reflect.Array:
		if ty.Elem().Kind() == reflect.Uint8 {
			*out = append(*out, path+" is bytes")
			return
		}
		walk(ty.Elem(), path+"[]", out)
	case reflect.Map:
		walk(ty.Key(), path+"{key}", out)
		walk(ty.Elem(), path+"{value}", out)
	case reflect.Pointer:
		walk(ty.Elem(), "*"+path, out)
	case reflect.Struct:
		if ty == reflect.TypeFor[time.Time]() {
			return
		}
		for f := range ty.Fields() {
			walk(f.Type, path+"."+f.Name, out)
		}
	default:
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
