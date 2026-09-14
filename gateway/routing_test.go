// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// Spec 008's rows through the handler with the real router in place of
// the fake: the world of harness_test.go, its Router swapped for a
// TargetRouter over the same catalog, with a health map the test writes
// and a draw fixed at zero so an order over equal weights is manifest
// order and every assertion below is deterministic.

type routed struct {
	*world
	r      *TargetRouter
	slow   *stub // holds every request until the caller's deadline
	hmu    sync.Mutex
	health map[string]v1.HealthState // by provider id
}

// Fixture names beyond the world's.
const (
	deadProvider  = "dead" // a host the transport refuses: a transport failure before any byte
	slowProvider  = "slow" // a 100ms timeout against a stub that never answers
	tieredModel   = "tiered"
	deadFirst     = "deadfirst"
	slowFirst     = "slowfirst"
	deadOnly      = "dead-only"
	slowOnly      = "slow-only"
	threeDead     = "threedead"
	upstreamGPT   = "gpt-4.1"
	upstreamMini  = "gpt-4.1-mini"
	upstreamClaud = "claude-3"
)

func newRouted(t *testing.T) *routed {
	t.Helper()
	w := newWorld(t)
	rt := &routed{world: w, health: map[string]v1.HealthState{}}
	rt.slow = newStub(t)
	w.clients.transport.hosts[rt.slow.host()] = true
	rt.slow.respond(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	w.provider(deadProvider, v1.DialectOpenAI, "http://dead.example.com/v1", "sk-dead")
	w.provider("dead2", v1.DialectOpenAI, "http://dead2.example.com/v1", "sk-dead")
	w.provider("dead3", v1.DialectOpenAI, "http://dead3.example.com/v1", "sk-dead")
	w.provider(slowProvider, v1.DialectOpenAI, rt.slow.URL+"/v1", "sk-slow").Spec.Timeout = "100ms"
	w.model(tieredModel, tw("oai", upstreamGPT, 100, 0), tw("ant", upstreamClaud, 100, 1))
	w.model(deadFirst, target(deadProvider, upstreamGPT), target("ant", upstreamClaud))
	w.model(slowFirst, target(slowProvider, upstreamGPT), target("ant", upstreamClaud))
	w.model(deadOnly, target(deadProvider, upstreamGPT))
	w.model(slowOnly, target(slowProvider, upstreamGPT))
	w.model(threeDead, target(deadProvider, upstreamGPT), target("dead2", upstreamGPT), target("dead3", upstreamGPT))
	rt.r = NewTargetRouter(RouterOptions{Catalog: w.catalog, Health: rt.view, Now: w.clock, Rand: func() float64 { return 0 }})
	o := w.options()
	o.Router = rt.r
	w.h = New(o)
	return rt
}

func (rt *routed) view(id string) v1.HealthState {
	rt.hmu.Lock()
	defer rt.hmu.Unlock()
	return rt.health[id]
}

func (rt *routed) set(provider string, s v1.HealthState) {
	rt.hmu.Lock()
	defer rt.hmu.Unlock()
	rt.health[rt.catalog.providers[provider].Status.ID] = s
}

func (rt *routed) advance(d time.Duration) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.now = rt.now.Add(d)
}

func (rt *routed) target(provider, model string) Target {
	return Target{Provider: rt.catalog.providers[provider], Model: model}
}

func (rt *routed) open(provider, model string) {
	for range CircuitThreshold {
		rt.r.RecordFailure(rt.target(provider, model))
	}
}

func (rt *routed) failures(provider, model string) int {
	return rt.r.failures(rt.catalog.providers[provider].Status.ID, model)
}

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within two seconds", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// modelOf reads the model member of a JSON body.
func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("not a JSON object: %s", body)
	}
	return v.Model
}

const geminiBody = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`

// TestExcludedTargets: a target whose Provider is Unreachable and a
// target whose circuit is open are both out of the order; when neither
// is admitted the request is refused provider_unavailable with no dial
// and a record that is a refusal with no attempt.
func TestExcludedTargets(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(200, openaiChatResponse)
	rt.anthropic.respondJSON(200, anthropicResponse)
	rt.set("oai", v1.HealthUnreachable)
	rec := rt.post("/openai/v1/chat/completions", chatBody("dual", false))
	if rec.Code != 200 || rt.openai.count() != 0 || rt.recorder.last(t).Provider != "ant" {
		t.Fatalf("%d, openai dials %d, provider %s", rec.Code, rt.openai.count(), rt.recorder.last(t).Provider)
	}
	rt.open("ant", upstreamClaud)
	rec = rt.post("/openai/v1/chat/completions", chatBody("dual", false))
	if errorCode(t, rec) != CodeProviderUnavailable || rt.openai.count() != 0 || rt.anthropic.count() != 1 {
		t.Fatalf("%s, openai dials %d, anthropic dials %d", rec.Header().Get(HeaderError), rt.openai.count(), rt.anthropic.count())
	}
	if got := rt.recorder.last(t); got.Status != StatusRefused || len(got.Attempts) != 0 || got.Provider != "" || got.Error != CodeProviderUnavailable {
		t.Errorf("record %+v", got)
	}
}

// TestHalfOpenAdmitsOne: two concurrent requests against one half-open
// target make one attempt at it, the other moves to the next target in
// the order, and the probe's success closes the circuit.
func TestHalfOpenAdmitsOne(t *testing.T) {
	rt := newRouted(t)
	release := make(chan struct{})
	rt.openai.respond(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, openaiChatResponse)
	})
	rt.anthropic.respondJSON(200, anthropicResponse)
	rt.open("oai", upstreamGPT)
	rt.advance(CircuitOpen)
	var first *httptest.ResponseRecorder
	done := make(chan struct{})
	go func() {
		defer close(done)
		first = rt.post("/openai/v1/chat/completions", chatBody(tieredModel, false))
	}()
	waitFor(t, "the probe reaching the provider", func() bool { return rt.openai.count() == 1 })
	second := rt.post("/openai/v1/chat/completions", chatBody(tieredModel, false))
	if second.Code != 200 || rt.recorder.last(t).Provider != "ant" || rt.anthropic.count() != 1 {
		t.Fatalf("the rival: %d from %s, anthropic dials %d", second.Code, rt.recorder.last(t).Provider, rt.anthropic.count())
	}
	if rt.openai.count() != 1 {
		t.Fatalf("openai dials %d while the probe was in flight", rt.openai.count())
	}
	close(release)
	<-done
	if first.Code != 200 || rt.recorder.last(t).Provider != "oai" {
		t.Fatalf("the probe: %d from %s", first.Code, rt.recorder.last(t).Provider)
	}
	if !rt.r.Allow(rt.target("oai", upstreamGPT)) || rt.failures("oai", upstreamGPT) != 0 {
		t.Error("the probe's success did not close the circuit")
	}
}

// TestFallbackWalksTheOrderOnce: fallback: onError tries each target at
// most once, in order, and stops at the first success; an exhausted
// order answers with the last attempt's failure.
func TestFallbackWalksTheOrderOnce(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(503, `{}`)
	rt.anthropic.respondJSON(503, `{}`)
	rec := rt.post("/openai/v1/chat/completions", chatBody("dual", false))
	if errorCode(t, rec) != CodeUpstreamError || len(rt.recorder.last(t).Attempts) != 2 || rt.openai.count() != 1 || rt.anthropic.count() != 1 {
		t.Fatalf("exhausted order: %s, %d attempts, dials %d and %d", rec.Header().Get(HeaderError), len(rt.recorder.last(t).Attempts), rt.openai.count(), rt.anthropic.count())
	}
	rt.anthropic.respondJSON(200, anthropicResponse)
	rec = rt.post("/openai/v1/chat/completions", chatBody("dual", false))
	got := rt.recorder.last(t)
	if rec.Code != 200 || got.Provider != "ant" || len(got.Attempts) != 2 || got.Attempts[0].Provider != "oai" || got.Attempts[1].Provider != "ant" {
		t.Fatalf("second in the order: %d from %s, attempts %+v", rec.Code, got.Provider, got.Attempts)
	}
	rt.openai.respondJSON(200, openaiChatResponse)
	rec = rt.post("/openai/v1/chat/completions", chatBody("dual", false))
	if rec.Code != 200 || rt.recorder.last(t).Provider != "oai" || len(rt.recorder.last(t).Attempts) != 1 || rt.anthropic.count() != 2 {
		t.Fatalf("first success: %d from %s, anthropic dials %d", rec.Code, rt.recorder.last(t).Provider, rt.anthropic.count())
	}
}

// TestFallbackNever: fallback: never fails on the first attempt, and
// the failure still counts on that target's circuit.
func TestFallbackNever(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(503, `{}`)
	rt.anthropic.respondJSON(200, anthropicResponse)
	rec := rt.post("/openai/v1/chat/completions", chatBody("never", false))
	if errorCode(t, rec) != CodeUpstreamError || len(rt.recorder.last(t).Attempts) != 1 || rt.anthropic.count() != 0 {
		t.Fatalf("%s, %d attempts, anthropic dials %d", rec.Header().Get(HeaderError), len(rt.recorder.last(t).Attempts), rt.anthropic.count())
	}
	if rt.failures("oai", upstreamGPT) != 1 {
		t.Errorf("circuit failures %d", rt.failures("oai", upstreamGPT))
	}
}

// failureCase is one row of the failure table: what the first target
// does, whether the next is tried, and the code of the attempt.
type failureCase struct {
	name     string
	status   int    // the first target's answer; 0 is the model's own failure
	model    string // a two-target Model whose first target fails as the row says
	provider string // the first target's Provider
	retried  bool
	code     Code
}

var failureTable = []failureCase{
	{"408", http.StatusRequestTimeout, "dual", "oai", true, CodeUpstreamError},
	{"429", http.StatusTooManyRequests, "dual", "oai", true, CodeUpstreamError},
	{"500", http.StatusInternalServerError, "dual", "oai", true, CodeUpstreamError},
	{"502", http.StatusBadGateway, "dual", "oai", true, CodeUpstreamError},
	{"503", http.StatusServiceUnavailable, "dual", "oai", true, CodeUpstreamError},
	{"529", 529, "dual", "oai", true, CodeUpstreamError},
	{"transport", 0, deadFirst, deadProvider, true, CodeProviderUnavailable},
	{"timeout", 0, slowFirst, slowProvider, true, CodeUpstreamTimeout},
	{"302", http.StatusFound, "dual", "oai", false, CodeUpstreamError},
	{"400", http.StatusBadRequest, "dual", "oai", false, CodeUpstreamRejected},
	{"401", http.StatusUnauthorized, "dual", "oai", false, CodeUpstreamError},
	{"403", http.StatusForbidden, "dual", "oai", false, CodeUpstreamError},
	{"404", http.StatusNotFound, "dual", "oai", false, CodeUpstreamRejected},
	{"422", http.StatusUnprocessableEntity, "dual", "oai", false, CodeUpstreamRejected},
}

// TestRetryableFailures: every retryable row of the failure table moves
// to the next target, 529 among them, and counts on the first target's
// circuit; every non-retryable row does not and counts as the target
// answering.
func TestRetryableFailures(t *testing.T) {
	for _, c := range failureTable {
		t.Run(c.name, func(t *testing.T) {
			rt := newRouted(t)
			if c.status != 0 {
				rt.openai.respondJSON(c.status, `{"error":"x"}`)
			}
			rt.anthropic.respondJSON(200, anthropicResponse)
			rec := rt.post("/openai/v1/chat/completions", chatBody(c.model, false))
			got := rt.recorder.last(t)
			if c.retried {
				if rec.Code != 200 || got.Provider != "ant" || len(got.Attempts) != 2 || got.Attempts[0].Error != c.code || got.Attempts[0].HTTPStatus != c.status || got.Attempts[0].Status != StatusFailed || got.Attempts[1].Status != StatusOK {
					t.Fatalf("%d %s; attempts %+v", rec.Code, rec.Header().Get(HeaderError), got.Attempts)
				}
				if rt.failures(c.provider, upstreamGPT) != 1 {
					t.Errorf("circuit failures %d, want 1", rt.failures(c.provider, upstreamGPT))
				}
				return
			}
			if errorCode(t, rec) != c.code || len(got.Attempts) != 1 || rt.anthropic.count() != 0 || got.Attempts[0].HTTPStatus != c.status {
				t.Fatalf("%s; attempts %+v; anthropic dials %d", rec.Header().Get(HeaderError), got.Attempts, rt.anthropic.count())
			}
			if rt.failures(c.provider, upstreamGPT) != 0 {
				t.Errorf("circuit failures %d, want 0: the target answered", rt.failures(c.provider, upstreamGPT))
			}
		})
	}
}

// TestLastFailureCode: the last attempt's failure maps to upstream_error
// for a retryable status, a 3xx, a 401, or a 403, upstream_rejected for
// any other 4xx, upstream_timeout for a deadline reached against an
// upstream, and provider_unavailable for a transport failure or an
// order with no candidate at all, each with the upstream status in the
// developer detail.
func TestLastFailureCode(t *testing.T) {
	cases := []struct {
		name   string
		status int
		model  string
		setup  func(rt *routed)
		code   Code
		detail string
	}{
		{"503", 503, "gpt", nil, CodeUpstreamError, "upstream status 503"},
		{"529", 529, "gpt", nil, CodeUpstreamError, "upstream status 529"},
		{"302", 302, "gpt", nil, CodeUpstreamError, "upstream status 302"},
		{"401", 401, "gpt", nil, CodeUpstreamError, "upstream status 401"},
		{"400", 400, "gpt", nil, CodeUpstreamRejected, "upstream status 400"},
		{"404", 404, "gpt", nil, CodeUpstreamRejected, "upstream status 404"},
		{"422", 422, "gpt", nil, CodeUpstreamRejected, "upstream status 422"},
		{"transport", 0, deadOnly, nil, CodeProviderUnavailable, "Provider dead"},
		{"timeout", 0, slowOnly, nil, CodeUpstreamTimeout, "no response within 100ms"},
		{"no candidate", 0, "gpt", func(rt *routed) { rt.set("oai", v1.HealthUnreachable) }, CodeProviderUnavailable, "no admitted target"},
		{"the probe slot held by another request", 0, "gpt", func(rt *routed) {
			rt.open("oai", upstreamGPT)
			rt.advance(CircuitOpen)
			rt.r.Allow(rt.target("oai", upstreamGPT)) // half-open with its probe in flight admits nothing
		}, CodeProviderUnavailable, "no admitted target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt := newRouted(t)
			if c.status != 0 {
				rt.openai.respondJSON(c.status, `{"error":"x"}`)
			}
			if c.setup != nil {
				c.setup(rt)
			}
			rec := rt.post("/openai/v1/chat/completions", chatBody(c.model, false))
			if errorCode(t, rec) != c.code || !strings.Contains(rec.Header().Get(HeaderErrorDetail), c.detail) {
				t.Fatalf("%s %q, want %s with %q", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail), c.code, c.detail)
			}
			if c.status == 0 && rt.openai.count() != 0 {
				t.Errorf("openai was dialled %d times", rt.openai.count())
			}
			if got := rt.recorder.last(t); got.Error != c.code {
				t.Errorf("record error %s", got.Error)
			}
		})
	}
}

// TestMidStreamFailureIsNotRetried: a stream that fails after its first
// byte is not retried on the next target, ends with the door's frame,
// is recorded failed with the tokens counted to the cut, and counts as
// the target answering for its circuit.
func TestMidStreamFailureIsNotRetried(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respond(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n")
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	rt.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	rec := rt.post("/openai/v1/chat/completions", chatBody("dual", true))
	if rec.Code != 200 || rt.anthropic.count() != 0 {
		t.Fatalf("%d, anthropic dials %d", rec.Code, rt.anthropic.count())
	}
	got := rt.recorder.last(t)
	if got.Status != StatusFailed || got.Error != CodeUpstreamError || len(got.Attempts) != 1 || got.Tokens != (Tokens{Input: 2, Output: 1}) || !got.Stream {
		t.Errorf("record %+v", got)
	}
	if !strings.Contains(rec.Body.String(), "data: {\"error\":") {
		t.Errorf("no error frame: %s", rec.Body.String())
	}
	if rt.failures("oai", upstreamGPT) != 0 || !rt.r.Allow(rt.target("oai", upstreamGPT)) {
		t.Error("a cut after the first byte counted against the circuit")
	}
}

// TestNoBackoffBetweenAttempts: an order of three targets that each fail
// at transport completes within the failures' own duration, with no
// sleep between attempts.
func TestNoBackoffBetweenAttempts(t *testing.T) {
	rt := newRouted(t)
	start := time.Now()
	rec := rt.post("/openai/v1/chat/completions", chatBody(threeDead, false))
	elapsed := time.Since(start)
	got := rt.recorder.last(t)
	if errorCode(t, rec) != CodeProviderUnavailable || len(got.Attempts) != 3 {
		t.Fatalf("%s, %d attempts", rec.Header().Get(HeaderError), len(got.Attempts))
	}
	for i, a := range got.Attempts {
		if a.Error != CodeProviderUnavailable || a.HTTPStatus != 0 {
			t.Errorf("attempt %d: %+v", i, a)
		}
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("three transport failures took %s", elapsed)
	}
}

// TestCircuitPerTarget: five consecutive retryable failures open a
// target's circuit, which two Models on the target share; one half-open
// attempt is admitted after the open duration and its success closes
// the circuit; a 4xx resets the count and never opens it; an encode
// refusal and a caller cancellation leave the count unchanged.
func TestCircuitPerTarget(t *testing.T) {
	rt := newRouted(t)
	chat := "/openai/v1/chat/completions"
	rt.openai.respondJSON(503, `{}`)
	for i := range CircuitThreshold {
		if rec := rt.post(chat, chatBody("gpt", false)); errorCode(t, rec) != CodeUpstreamError {
			t.Fatalf("failure %d: %s", i+1, rec.Header().Get(HeaderError))
		}
	}
	dials := rt.openai.count()
	for _, name := range []string{upstreamGPT, "gpt"} { // the sibling Model names the same target
		if rec := rt.post(chat, chatBody(name, false)); errorCode(t, rec) != CodeProviderUnavailable || rt.openai.count() != dials {
			t.Fatalf("%s with the circuit open: %s, dials %d", name, rec.Header().Get(HeaderError), rt.openai.count()-dials)
		}
	}
	rt.advance(CircuitOpen)
	rt.openai.respondJSON(200, openaiChatResponse)
	if rec := rt.post(chat, chatBody("gpt", false)); rec.Code != 200 || rt.openai.count() != dials+1 {
		t.Fatalf("the half-open attempt: %d, dials %d", rec.Code, rt.openai.count()-dials)
	}
	if rec := rt.post(chat, chatBody(upstreamGPT, false)); rec.Code != 200 {
		t.Fatalf("after the probe closed the circuit: %d %s", rec.Code, rec.Header().Get(HeaderError))
	}
	// A 4xx resets the count and never opens the circuit.
	rt.openai.respondJSON(503, `{}`)
	for range CircuitThreshold - 1 {
		rt.post(chat, chatBody("gpt", false))
	}
	if rt.failures("oai", upstreamGPT) != CircuitThreshold-1 {
		t.Fatalf("count %d", rt.failures("oai", upstreamGPT))
	}
	rt.openai.respondJSON(400, `{}`)
	if rec := rt.post(chat, chatBody("gpt", false)); errorCode(t, rec) != CodeUpstreamRejected || rt.failures("oai", upstreamGPT) != 0 {
		t.Fatalf("a 400: %s, count %d", rec.Header().Get(HeaderError), rt.failures("oai", upstreamGPT))
	}
	rt.openai.respondJSON(503, `{}`)
	for range CircuitThreshold - 1 {
		rt.post(chat, chatBody("gpt", false))
	}
	if rt.failures("oai", upstreamGPT) != CircuitThreshold-1 || !rt.r.Allow(rt.target("oai", upstreamGPT)) {
		t.Fatalf("count %d after four more failures", rt.failures("oai", upstreamGPT))
	}
	// An encode refusal never reached the target.
	dials = rt.openai.count()
	if rec := rt.post("/anthropic/v1/messages", `{"model":"gpt","max_tokens":5}`); errorCode(t, rec) != CodeInvalidRequest || rt.openai.count() != dials {
		t.Fatalf("encode refusal: %s, dials %d", rec.Header().Get(HeaderError), rt.openai.count()-dials)
	}
	if rt.failures("oai", upstreamGPT) != CircuitThreshold-1 {
		t.Errorf("an encode refusal moved the count to %d", rt.failures("oai", upstreamGPT))
	}
	// A caller cancellation says nothing about the target.
	rt.openai.respond(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(context.Background())
	req := rt.request(http.MethodPost, chat, chatBody("gpt", false)).WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rt.do(req) }()
	waitFor(t, "the held request reaching the provider", func() bool { return rt.openai.count() == dials+1 })
	cancel()
	<-done
	if got := rt.recorder.last(t); got.Error != ClientClosed || got.Status != StatusFailed {
		t.Fatalf("record %+v", got)
	}
	if rt.failures("oai", upstreamGPT) != CircuitThreshold-1 {
		t.Errorf("a cancellation moved the count to %d", rt.failures("oai", upstreamGPT))
	}
}

// doorCase is one door of the dialect matrix: its translated route and
// the body that names a Model on it.
type doorCase struct {
	door v1.Dialect
	path func(model string) string
	body func(model string) string
}

var doorCases = []doorCase{
	{v1.DialectOpenAI, func(string) string { return "/openai/v1/chat/completions" }, func(m string) string { return chatBody(m, false) }},
	{v1.DialectAnthropic, func(string) string { return "/anthropic/v1/messages" }, func(m string) string { return messagesBody(m, false) }},
	{v1.DialectGemini, func(m string) string { return "/gemini/v1beta/models/" + m + ":generateContent" }, func(string) string { return geminiBody }},
	{v1.DialectLux, func(string) string { return "/lux/v1/generate" }, func(m string) string { return luxBody(m, false) }},
}

// The world's one-target Model per target dialect.
var modelOfDialect = map[v1.Dialect]string{v1.DialectOpenAI: "gpt", v1.DialectAnthropic: "claude", v1.DialectGemini: "gemini", v1.DialectLux: "luxm"}

// TestDialectMatrix: every cell of the door and target matrix behaves as
// the table says on a translated route, passthrough on the diagonal,
// translation between the codecs, and dialect_unsupported on the gemini
// row and column; a model route across dialects is dialect_unsupported
// whatever the Key allows.
func TestDialectMatrix(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(200, openaiChatResponse)
	rt.anthropic.respondJSON(200, anthropicResponse)
	rt.gemini.respondJSON(200, geminiResponse)
	rt.lux.respondJSON(200, luxResponse)
	for _, d := range doorCases {
		for td, model := range modelOfDialect {
			t.Run(string(d.door)+"→"+string(td), func(t *testing.T) {
				before := rt.stubs[td].count()
				rec := rt.post(d.path(model), d.body(model))
				reachable := d.door == td || (d.door != v1.DialectGemini && td != v1.DialectGemini)
				switch {
				case reachable && (rec.Code != 200 || rt.stubs[td].count() != before+1):
					t.Errorf("%d %s %s, dials %d", rec.Code, rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail), rt.stubs[td].count()-before)
				case !reachable && (errorCode(t, rec) != CodeDialectUnsupported || rt.stubs[td].count() != before):
					t.Errorf("%s, dials %d", rec.Header().Get(HeaderError), rt.stubs[td].count()-before)
				}
				if got := rt.recorder.last(t); reachable && got.Translated != (d.door != td) {
					t.Errorf("record translated %v", got.Translated)
				}
			})
		}
	}
	embeddings := `{"model":"claude","input":"hello"}`
	dials := rt.anthropic.count()
	for _, key := range []string{keyValue, passthroughValue} {
		r := rt.request(http.MethodPost, "/openai/v1/embeddings", embeddings)
		r.Header.Set("Authorization", "Bearer "+key)
		if rec := rt.do(r); errorCode(t, rec) != CodeDialectUnsupported || rt.anthropic.count() != dials {
			t.Errorf("a model route across dialects: %s, dials %d", rec.Header().Get(HeaderError), rt.anthropic.count()-dials)
		}
	}
}

// TestGeminiIsDoorBound: a gemini door to a non-gemini target and another
// door to a gemini-only Model are both dialect_unsupported and not
// model_not_found; a Model with a gemini target beside another is
// reached through every door on the targets that door can bridge.
func TestGeminiIsDoorBound(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(200, openaiChatResponse)
	rt.gemini.respondJSON(200, geminiResponse)
	if rec := rt.post("/gemini/v1beta/models/gpt:generateContent", geminiBody); errorCode(t, rec) != CodeDialectUnsupported {
		t.Errorf("gemini door to an openai target: %s", rec.Header().Get(HeaderError))
	}
	for _, d := range doorCases[:2] {
		if rec := rt.post(d.path(geminiOnlyModel), d.body(geminiOnlyModel)); errorCode(t, rec) != CodeDialectUnsupported {
			t.Errorf("%s door to a gemini-only Model: %s", d.door, rec.Header().Get(HeaderError))
		}
	}
	if rec := rt.post("/lux/v1/generate", luxBody(geminiOnlyModel, false)); errorCode(t, rec) != CodeDialectUnsupported {
		t.Errorf("lux door to a gemini-only Model: %s", rec.Header().Get(HeaderError))
	}
	if rec := rt.post("/gemini/v1beta/models/"+geminiOnlyModel+":generateContent", geminiBody); rec.Code != 200 {
		t.Errorf("gemini door to the gemini-only Model: %d %s", rec.Code, rec.Header().Get(HeaderError))
	}
	// multi lists the gemini target first; the openai door skips it.
	if rec := rt.post("/openai/v1/chat/completions", chatBody("multi", false)); rec.Code != 200 || rt.recorder.last(t).Provider != "oai" || rt.gemini.count() != 1 {
		t.Errorf("openai door to multi: %d from %s, gemini dials %d", rec.Code, rt.recorder.last(t).Provider, rt.gemini.count())
	}
	if rec := rt.post("/gemini/v1beta/models/multi:generateContent", geminiBody); rec.Code != 200 || rt.recorder.last(t).Provider != "gem" || rt.openai.count() != 1 {
		t.Errorf("gemini door to multi: %d from %s, openai dials %d", rec.Code, rt.recorder.last(t).Provider, rt.openai.count())
	}
}

// TestOpenAIReasoningFamily: the predicate over an openai target's
// upstream name, on the part before any slash, case-insensitively.
func TestOpenAIReasoningFamily(t *testing.T) {
	cases := map[string]bool{
		"gpt-5": true, "o3-mini": true, "GPT-6-turbo": true, "o1": true, "o1-preview": true, "o4-mini": true, "O3": true,
		"gpt-10": true, "gpt-5/2026-01": true, "gpt-5.1": true,
		"gpt-4.1": false, "llama3.1": false, "o-ring": false, "gpt-4o": false, "gpt-": false, "gpt-x": false, "gpt-4": false,
		"gpt5": false, "o": false, "": false, "openai/gpt-5": false, // the part before the slash is the vendor, not a model
	}
	for name, want := range cases {
		if got := OpenAIReasoningFamily(name); got != want {
			t.Errorf("OpenAIReasoningFamily(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestModelNameOnTheWire: the outbound body carries the target's upstream
// name and the response carries the Model's name, on a translated route,
// a passthrough with equal names, and a passthrough with differing
// names, where only the model member is rewritten.
func TestModelNameOnTheWire(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(200, openaiChatResponse)
	rec := rt.post("/anthropic/v1/messages", messagesBody(openaiAliasModel, false))
	if rec.Code != 200 || modelOf(t, rt.openai.last(t).Body) != upstreamMini || modelOf(t, rec.Body.Bytes()) != openaiAliasModel {
		t.Errorf("translated: %d, outbound %s, response %s", rec.Code, rt.openai.last(t).Body, rec.Body.String())
	}
	body := chatBody(upstreamGPT, false)
	rec = rt.post("/openai/v1/chat/completions", body)
	if string(rt.openai.last(t).Body) != body || rec.Body.String() != openaiChatResponse {
		t.Errorf("equal names: outbound %s, response %s", rt.openai.last(t).Body, rec.Body.String())
	}
	body = chatBody(openaiAliasModel, false)
	rec = rt.post("/openai/v1/chat/completions", body)
	wantOut := strings.Replace(body, `"model":"`+openaiAliasModel+`"`, `"model":"`+upstreamMini+`"`, 1)
	wantBack := strings.Replace(openaiChatResponse, `"model":"`+upstreamGPT+`"`, `"model":"`+openaiAliasModel+`"`, 1)
	if string(rt.openai.last(t).Body) != wantOut || rec.Body.String() != wantBack {
		t.Errorf("differing names: outbound %s, response %s", rt.openai.last(t).Body, rec.Body.String())
	}
	if got := rt.recorder.last(t); got.Model != openaiAliasModel || got.UpstreamModel != upstreamMini {
		t.Errorf("record model %s upstream %s", got.Model, got.UpstreamModel)
	}
}

// TestUsageRecordsEveryAttempt: the record of a fallback carries one
// attempts entry per target tried, in order, with the outcome of each,
// and names the target that answered; a request refused before any
// target was chosen has an empty attempts and no provider.
func TestUsageRecordsEveryAttempt(t *testing.T) {
	rt := newRouted(t)
	rt.openai.respondJSON(503, `{}`)
	rt.anthropic.respondJSON(200, anthropicResponse)
	if rec := rt.post("/openai/v1/chat/completions", chatBody("dual", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get(HeaderError))
	}
	got := rt.recorder.last(t)
	oai, ant := rt.catalog.providers["oai"], rt.catalog.providers["ant"]
	want := []Attempt{
		{Provider: "oai", ProviderID: oai.Status.ID, UpstreamModel: upstreamGPT, Status: StatusFailed, HTTPStatus: 503, Error: CodeUpstreamError},
		{Provider: "ant", ProviderID: ant.Status.ID, UpstreamModel: upstreamClaud, Status: StatusOK, HTTPStatus: 200},
	}
	if len(got.Attempts) != len(want) {
		t.Fatalf("attempts %+v", got.Attempts)
	}
	for i, a := range got.Attempts {
		if a.Duration <= 0 {
			t.Errorf("attempt %d has no duration", i)
		}
		a.Duration = 0
		if a != want[i] {
			t.Errorf("attempt %d %+v, want %+v", i, a, want[i])
		}
	}
	if got.Provider != "ant" || got.ProviderID != ant.Status.ID || got.UpstreamModel != upstreamClaud || got.TargetDialect != v1.DialectAnthropic || !got.Translated || got.UpstreamStatus != 200 || got.Status != StatusOK {
		t.Errorf("record %+v", got)
	}
	for name, code := range map[string]Code{"nope": CodeModelNotFound, unavailableModel: CodeProviderUnavailable} {
		if name == unavailableModel {
			rt.set("oai", v1.HealthUnreachable)
		}
		rec := rt.post("/openai/v1/chat/completions", chatBody(name, false))
		if errorCode(t, rec) != code {
			t.Fatalf("%s: %s", name, rec.Header().Get(HeaderError))
		}
		if got := rt.recorder.last(t); len(got.Attempts) != 0 || got.Provider != "" || got.ProviderID != "" || got.Status != StatusRefused || got.UpstreamStatus != 0 {
			t.Errorf("%s: record %+v", name, got)
		}
	}
}
