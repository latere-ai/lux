// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/metering"
)

// The canaries of spec 009's no-content row: a prompt, a completion, a
// header value, and, from serve_test.go, the Provider credential. The
// Key value is minted per test.
const (
	canaryPrompt     = "canary-prompt-4f1e9c-never-in-a-record"
	canaryCompletion = "canary-completion-8b2d7a-never-in-a-record"
	canaryHeader     = "canary-header-c3a5e1-never-in-a-record"
)

// Upstream answers per dialect, each with the dialect's usage members
// and the completion canary as the text.
var (
	openaiChatResponse = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"` + canaryCompletion + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}}`
	openaiNoUsage      = `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"` + canaryCompletion + `"},"finish_reason":"stop"}]}`
	anthropicResponse  = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-3","content":[{"type":"text","text":"` + canaryCompletion + `"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":1,"cache_creation_input_tokens":5}}`
	geminiResponse     = `{"candidates":[{"content":{"parts":[{"text":"` + canaryCompletion + `"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"cachedContentTokenCount":3},"modelVersion":"gemini-pro"}`
	luxResponse        = `{"id":"lux-1","model":"luxm","blocks":[{"type":"text","text":"` + canaryCompletion + `"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1,"reasoning_tokens":1}}`
)

// card is the pricing every priced Model of these tests carries: per
// token, so the figures are read off the usage members.
func card(t *testing.T, currency string) *v1.Pricing {
	t.Helper()
	p := &v1.Pricing{Currency: currency, Per: 1}
	for _, m := range []struct {
		dst **v1.Money
		s   string
	}{{&p.Input, "0.01"}, {&p.Output, "0.02"}, {&p.CachedInput, "0.001"}, {&p.CacheWrite, "0.0125"}} {
		v, err := v1.ParseMoney(m.s)
		if err != nil {
			t.Fatal(err)
		}
		*m.dst = &v
	}
	return p
}

// plane is one data plane on the harness: the store, the real Key
// cache, catalog, credentials, router, Limiter, and Recorder of this
// package under one gateway handler, with the registry the metrics land
// in.
type plane struct {
	h        *harness
	reg      *metrics.Registry
	cache    *KeyCache
	limiter  *Limiter
	recorder *Recorder
	handler  *gateway.Handler
	ids      atomic.Int64
}

func (h *harness) plane(t *testing.T, st store.Store) *plane {
	t.Helper()
	if st == nil {
		st = h.st
	}
	reg := metrics.NewRegistry()
	catalog := &Catalog{Objects: st.Objects()}
	cache := NewKeyCache(KeyCacheOptions{Store: st, TTL: time.Hour, Logger: h.logger, Now: h.clock})
	limiter := NewLimiter(LimiterOptions{Store: st, Budgets: cache, Defaults: manifest.Defaults{}, Flush: time.Second, Logger: h.logger, Now: h.clock})
	recorder := NewRecorder(RecorderOptions{Store: st, Catalog: catalog, Limiter: limiter, Metrics: reg, Flush: time.Second, Logger: h.logger, Now: h.clock})
	p := &plane{h: h, reg: reg, cache: cache, limiter: limiter, recorder: recorder}
	// The clock is fixed, so the ids order the records: the newest is
	// the last minted.
	p.handler = gateway.New(gateway.Options{
		Keys: cache, Catalog: catalog, Credentials: h.creds,
		Router:   gateway.NewTargetRouter(gateway.RouterOptions{Catalog: catalog, Now: h.clock}),
		Limiter:  limiter,
		Recorder: recorder,
		Clients:  h.clients,
		Version:  "test",
		Now:      h.clock,
		NewID:    func() string { return "req_" + strings.Repeat("0", 20) + fmt.Sprintf("%06d", p.ids.Add(1)) },
	})
	return p
}

// upstream starts a stub answering body to every request and stores a
// Provider of dialect toward it.
func (p *plane) upstream(t *testing.T, name string, d v1.Dialect, body string) (*v1.Provider, *stub) {
	t.Helper()
	s := &stub{}
	s.set(http.StatusOK, body)
	base := serveStub(t, s)
	if d == v1.DialectGemini {
		base += "/v1beta"
	} else {
		base += "/v1"
	}
	return p.h.provider(t, name, d, base, nil), s
}

// model stores a declared Model with one target and the pricing given,
// nil for an unpriced one.
func (p *plane) model(t *testing.T, name, provider, upstream string, pricing *v1.Pricing) *v1.Model {
	t.Helper()
	w := 100
	available := true
	m := &v1.Model{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ModelSpec{Targets: []v1.Target{{Provider: provider, Model: upstream, Weight: &w}}, Fallback: v1.FallbackOnError, Pricing: pricing},
		Status:   v1.ModelStatus{ID: v1.NewID(v1.PrefixModel, p.h.clock(), nil), Owner: subject, Source: v1.SourceDeclared, Warnings: []string{}, Available: &available},
	}
	if _, err := p.h.st.Objects().Put(t.Context(), m, 0); err != nil {
		t.Fatal(err)
	}
	return m
}

// do runs one request through the handler with value as the bearer.
func (p *plane) do(method, path, value, body string, headers map[string]string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rd)
	if value != "" {
		r.Header.Set("Authorization", "Bearer "+value)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.handler.ServeHTTP(rec, r)
	return rec
}

// records is every record in the store's rings, newest first.
func (p *plane) records(t *testing.T) []metering.Record {
	t.Helper()
	now := p.h.clock()
	recs, _, err := p.h.st.Usage().Records(t.Context(), metering.RecordQuery{Query: metering.Query{From: now.Add(-24 * time.Hour), To: now.Add(24 * time.Hour)}}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// chat is a chat completion body naming model with the prompt canary.
func chat(model string) string {
	b, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": canaryPrompt}}})
	return string(b)
}

// messages is a Messages API body naming model.
func messages(model string) string {
	b, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 64, "messages": []map[string]any{{"role": "user", "content": canaryPrompt}}})
	return string(b)
}

// generate is a lux generate body naming model.
func generate(model string) string {
	b, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "blocks": []map[string]any{{"type": "text", "text": canaryPrompt}}}}})
	return string(b)
}

// gemini is a generateContent body.
func geminiBody() string {
	b, _ := json.Marshal(map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": canaryPrompt}}}}})
	return string(b)
}

// TestEveryRequestHasOneUsageRecord is the package-level half of the
// row, through gateway.New with this package's Recorder and the real
// Key cache, catalog, Limiter, and router: every request, refused
// before the Key was known, refused at the gate, failed against the
// upstream, or served, has exactly one record, with its status and its
// code, and the records reach the store's ring. The e2e tier's half is
// spec 015's.
func TestEveryRequestHasOneUsageRecord(t *testing.T) {
	h := newHarness(t)
	p := h.plane(t, nil)
	oai, ok := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	down := &stub{}
	down.set(http.StatusInternalServerError, `{"error":{"message":"down"}}`)
	broken := h.provider(t, "broken", v1.DialectOpenAI, serveStub(t, down)+"/v1", nil)
	p.model(t, "flaky", broken.Metadata.Name, "gpt-4.1", card(t, "USD"))
	k, value := h.key(t, "all", rates(3, 0))

	want := []struct {
		status metering.Status
		code   string
		do     func() *httptest.ResponseRecorder
	}{
		{metering.StatusOK, "", func() *httptest.ResponseRecorder {
			return p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil)
		}},
		{metering.StatusOK, "", func() *httptest.ResponseRecorder {
			return p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil)
		}},
		{metering.StatusFailed, "upstream_error", func() *httptest.ResponseRecorder {
			return p.do("POST", "/openai/v1/chat/completions", value, chat("flaky"), nil)
		}},
		{metering.StatusRefused, "rate_limited", func() *httptest.ResponseRecorder {
			return p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil)
		}},
		{metering.StatusRefused, "model_not_found", func() *httptest.ResponseRecorder {
			return p.do("POST", "/openai/v1/chat/completions", value, chat("nope"), nil)
		}},
		{metering.StatusRefused, "unauthenticated", func() *httptest.ResponseRecorder {
			return p.do("POST", "/openai/v1/chat/completions", "lux_nobody", chat("gpt"), nil)
		}},
		{metering.StatusRefused, "route_not_allowed", func() *httptest.ResponseRecorder { return p.do("GET", "/openai/v1/nothing/here", value, "", nil) }},
	}
	for i, w := range want {
		rec := w.do()
		if got := gateway.Code(rec.Header().Get(gateway.HeaderError)); got != gateway.Code(w.code) {
			t.Fatalf("request %d: code %q, want %q (%d %s)", i, got, w.code, rec.Code, rec.Body.String())
		}
	}
	recs := p.records(t)
	if len(recs) != len(want) {
		t.Fatalf("%d records for %d requests", len(recs), len(want))
	}
	ids := map[string]bool{}
	byStatus := map[metering.Status]int{}
	byCode := map[string]int{}
	for _, r := range recs {
		if ids[r.ID] || !strings.HasPrefix(r.ID, "req_") {
			t.Fatalf("record id %q repeats or is not a request id", r.ID)
		}
		ids[r.ID] = true
		byStatus[r.Status]++
		byCode[r.Error]++
		if r.Status == metering.StatusRefused && (len(r.Attempts) != 0 || r.Tokens != (metering.Tokens{}) || r.Cost.Amount != 0) {
			t.Fatalf("a refusal carries attempts, tokens, or cost: %+v", r)
		}
		if r.Status == metering.StatusOK && (r.Key.ID != k.Status.ID || !r.Cost.Priced || r.Cost.Amount == 0 || len(r.Attempts) != 1 || r.Provider.Name != "oai") {
			t.Fatalf("a served record = %+v", r)
		}
		if r.Status == metering.StatusFailed && (r.UpstreamStatus != 500 || len(r.Attempts) == 0 || r.Provider.Name != "broken") {
			t.Fatalf("a failed record = %+v", r)
		}
		if r.Error == "unauthenticated" && (r.Key.ID != "" || r.Owner != "") {
			t.Fatalf("an unauthenticated record names a Key: %+v", r)
		}
	}
	if byStatus[metering.StatusOK] != 2 || byStatus[metering.StatusFailed] != 1 || byStatus[metering.StatusRefused] != 4 {
		t.Fatalf("statuses = %v", byStatus)
	}
	for _, w := range want {
		byCode[w.code]--
	}
	for code, n := range byCode {
		if n != 0 {
			t.Fatalf("code %q is off by %d", code, n)
		}
	}
	if ok.count() != 2 {
		t.Fatalf("the upstream saw %d requests, want 2", ok.count())
	}
}

// TestRecordCarriesNoContent is the run half of the row: a canary
// prompt, completion, header value, Provider credential, and Key value
// appear in no record and in no aggregate row of a run over every door,
// served, failed, and refused.
func TestRecordCarriesNoContent(t *testing.T) {
	h := newHarness(t)
	p := h.plane(t, nil)
	oai, _ := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	ant, _ := p.upstream(t, "ant", v1.DialectAnthropic, anthropicResponse)
	gem, _ := p.upstream(t, "gem", v1.DialectGemini, geminiResponse)
	lx, _ := p.upstream(t, "lx", v1.DialectLux, luxResponse)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	p.model(t, "claude", ant.Metadata.Name, "claude-3", card(t, "USD"))
	p.model(t, "gemini-pro", gem.Metadata.Name, "gemini-pro", card(t, "USD"))
	p.model(t, "luxm", lx.Metadata.Name, "luxm", card(t, "USD"))
	_, value := h.key(t, "all", func(k *v1.Key) { k.Metadata.Labels = map[string]string{"team": "red"} })
	headers := map[string]string{"X-Canary": canaryHeader, gateway.HeaderLabels: "run=r1"}
	for _, c := range []struct{ path, body string }{
		{"/openai/v1/chat/completions", chat("gpt")},
		{"/openai/v1/chat/completions", chat("claude")},
		{"/anthropic/v1/messages", messages("claude")},
		{"/anthropic/v1/messages", messages("gpt")},
		{"/gemini/v1beta/models/gemini-pro:generateContent", geminiBody()},
		{"/lux/v1/generate", generate("luxm")},
		{"/lux/v1/generate", generate("nope")},
		{"/openai/v1/chat/completions", `{"model":"gpt","messages":"` + canaryPrompt + `"}`},
	} {
		if rec := p.do("POST", c.path, value, c.body, headers); rec.Code == 0 {
			t.Fatalf("%s: no response", c.path)
		}
	}
	p.do("POST", "/openai/v1/chat/completions", "lux_"+strings.Repeat("z", 40), chat("gpt"), headers)
	if err := p.recorder.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	recs := p.records(t)
	if len(recs) != 9 {
		t.Fatalf("%d records, want 9", len(recs))
	}
	rows, err := h.st.Usage().QueryRows(t.Context(), metering.Query{From: h.clock().Add(-time.Hour), To: h.clock().Add(time.Hour), By: []metering.Dimension{metering.DimensionKey, "label:team"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no aggregate rows")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	text := out.String()
	for name, c := range map[string]string{"prompt": canaryPrompt, "completion": canaryCompletion, "header": canaryHeader, "credential": canary, "key value": value} {
		if strings.Contains(text, c) {
			t.Errorf("the %s canary appears in a record or a row", name)
		}
	}
	// The record's attribution is there, so the check is not vacuous.
	if !strings.Contains(text, `"prefix":"`+value[:12]+`"`) || !strings.Contains(text, `"team":"red"`) || !strings.Contains(text, `"run":"r1"`) {
		t.Fatalf("the records lack the Key's prefix, labels, or request labels:\n%s", text)
	}
}

// TestCachedInputIsNotBilledTwice is the dialect half of the row: on
// each of the four dialects, as a passthrough and as a translation, the
// record's input excludes the cached count, the cached count is billed
// once at the cached price, and the cost is metering.Cost over the
// record's tokens.
func TestCachedInputIsNotBilledTwice(t *testing.T) {
	h := newHarness(t)
	p := h.plane(t, nil)
	pricing := card(t, "USD")
	oai, _ := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	ant, _ := p.upstream(t, "ant", v1.DialectAnthropic, anthropicResponse)
	gem, _ := p.upstream(t, "gem", v1.DialectGemini, geminiResponse)
	lx, _ := p.upstream(t, "lx", v1.DialectLux, luxResponse)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", pricing)
	p.model(t, "claude", ant.Metadata.Name, "claude-3", pricing)
	p.model(t, "gemini-pro", gem.Metadata.Name, "gemini-pro", pricing)
	p.model(t, "luxm", lx.Metadata.Name, "luxm", pricing)
	_, value := h.key(t, "all", nil)
	// What each upstream reports: the prompt total, the cached part, and
	// the rest, as the dialect table of spec 009 reads them.
	upstreams := map[string]metering.Tokens{
		"gpt":        {Input: 10, Output: 3, CachedInput: 2},
		"claude":     {Input: 10, Output: 4, CachedInput: 1, CacheWrite: 5},
		"gemini-pro": {Input: 6, Output: 2, CachedInput: 3},
		"luxm":       {Input: 7, Output: 1, Reasoning: 1},
	}
	for _, c := range []struct {
		name       string
		path, body string
		model      string
		translated bool
	}{
		{"openai passthrough", "/openai/v1/chat/completions", chat("gpt"), "gpt", false},
		{"anthropic passthrough", "/anthropic/v1/messages", messages("claude"), "claude", false},
		{"gemini passthrough", "/gemini/v1beta/models/gemini-pro:generateContent", geminiBody(), "gemini-pro", false},
		{"lux passthrough", "/lux/v1/generate", generate("luxm"), "luxm", false},
		{"openai door to anthropic", "/openai/v1/chat/completions", chat("claude"), "claude", true},
		{"anthropic door to openai", "/anthropic/v1/messages", messages("gpt"), "gpt", true},
		{"lux door to anthropic", "/lux/v1/generate", generate("claude"), "claude", true},
		{"openai door to lux", "/openai/v1/chat/completions", chat("luxm"), "luxm", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := p.do("POST", c.path, value, c.body, nil)
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			r := p.records(t)[0]
			want := upstreams[c.model]
			if r.Model.Name != c.model || r.Translated != c.translated || r.Status != metering.StatusOK {
				t.Fatalf("record = %+v", r)
			}
			if r.Tokens.Input != want.Input || r.Tokens.CachedInput != want.CachedInput || r.Tokens.CacheWrite != want.CacheWrite || r.Tokens.Output != want.Output || r.Tokens.Estimated {
				t.Fatalf("tokens = %+v, want %+v", r.Tokens, want)
			}
			cost, _ := metering.Cost(r.Tokens, pricing)
			if !r.Cost.Priced || r.Cost.Currency != "USD" || r.Cost.Amount != int64(cost) {
				t.Fatalf("cost = %+v, want %s", r.Cost, cost)
			}
			// The cached count at the input price would be more.
			twice, _ := metering.Cost(metering.Tokens{Input: r.Tokens.Input + r.Tokens.CachedInput, Output: r.Tokens.Output, CachedInput: r.Tokens.CachedInput, CacheWrite: r.Tokens.CacheWrite}, pricing)
			if want.CachedInput > 0 && int64(twice) <= r.Cost.Amount {
				t.Fatalf("the cached count was billed at the input price: %d", r.Cost.Amount)
			}
		})
	}
	// Reasoning is recorded and not billed: the lux upstream's answer.
	for _, r := range p.records(t) {
		if r.Model.Name == "luxm" {
			if r.Tokens.Reasoning != 1 {
				t.Fatalf("reasoning = %d", r.Tokens.Reasoning)
			}
			if r.Cost.Amount != 7*10_000+1*20_000 {
				t.Fatalf("reasoning was billed: %d", r.Cost.Amount)
			}
		}
	}
}

// TestEstimatedTokensAreMarked: an upstream that reports no usage
// yields estimated true, an input count from the estimator, and an
// output count of zero; a token count no upstream answers is a record
// with zero tokens and no price.
func TestEstimatedTokensAreMarked(t *testing.T) {
	h := newHarness(t)
	p := h.plane(t, nil)
	oai, _ := p.upstream(t, "oai", v1.DialectOpenAI, openaiNoUsage)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	_, value := h.key(t, "all", nil)
	if rec := p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	r := p.records(t)[0]
	if !r.Tokens.Estimated || r.Tokens.Input <= 0 || r.Tokens.Output != 0 || r.Tokens.CachedInput != 0 {
		t.Fatalf("tokens = %+v", r.Tokens)
	}
	if !r.Cost.Priced || r.Cost.Amount != r.Tokens.Input*10_000 {
		t.Fatalf("an estimated record's cost = %+v", r.Cost)
	}
	// A count route toward an openai Provider is answered from the
	// estimate and priced by nothing.
	rec := p.do("POST", "/lux/v1/count_tokens", value, generate("gpt"), nil)
	if rec.Code != 200 || rec.Header().Get(gateway.HeaderEstimated) != "true" {
		t.Fatalf("count: %d %s", rec.Code, rec.Body.String())
	}
	r = p.records(t)[0]
	if r.Route != "/lux/v1/count_tokens" || r.Status != metering.StatusOK || r.Cost.Priced || r.Cost.Amount != 0 || r.Cost.Currency != "" {
		t.Fatalf("a count route's record = %+v", r)
	}
}

// TestOpaqueRouteIsUnpriced: an opaque route names no Model and yields
// zero tokens, estimated true, priced false, with the Provider that
// answered.
func TestOpaqueRouteIsUnpriced(t *testing.T) {
	h := newHarness(t)
	p := h.plane(t, nil)
	oai, s := p.upstream(t, "oai", v1.DialectOpenAI, `{"object":"file","id":"file-1"}`)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	_, value := h.key(t, "pass", func(k *v1.Key) { k.Spec.Passthrough = true })
	rec := p.do("POST", "/openai/v1/files", value, `{"purpose":"batch"}`, map[string]string{gateway.HeaderProvider: "oai"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if s.count() != 1 {
		t.Fatalf("the upstream saw %d requests", s.count())
	}
	r := p.records(t)[0]
	if r.Status != metering.StatusOK || r.Model != (metering.Ref{}) || r.Provider.Name != "oai" || r.Route != "/openai/v1/*" {
		t.Fatalf("record = %+v", r)
	}
	if r.Tokens != (metering.Tokens{Estimated: true}) || r.Cost != (metering.Charge{}) {
		t.Fatalf("tokens %+v, cost %+v", r.Tokens, r.Cost)
	}
}

// TestUnpricedModelRefusedUnderABudget: a Key under a hard Budget naming
// an unpriced Model is refused model_unpriced before any bytes reach
// the Provider, and the refusal has a record with the code, no
// attempts, zero tokens, and no price.
func TestUnpricedModelRefusedUnderABudget(t *testing.T) {
	h := newHarness(t)
	p := h.plane(t, nil)
	oai, s := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	p.model(t, "free", oai.Metadata.Name, "gpt-4.1", nil)
	hard := h.budget(t, "hard", "1", "USD", v1.WindowMonth, true)
	_, value := h.key(t, "budgeted", draws(hard))
	rec := p.do("POST", "/openai/v1/chat/completions", value, chat("free"), nil)
	if rec.Header().Get(gateway.HeaderError) != string(gateway.CodeModelUnpriced) {
		t.Fatalf("%d %s %s", rec.Code, rec.Header().Get(gateway.HeaderError), rec.Body.String())
	}
	if s.count() != 0 {
		t.Fatalf("%d bytes' worth of requests reached the Provider", s.count())
	}
	recs := p.records(t)
	if len(recs) != 1 {
		t.Fatalf("%d records", len(recs))
	}
	r := recs[0]
	if r.Status != metering.StatusRefused || r.Error != "model_unpriced" || len(r.Attempts) != 0 || r.Tokens != (metering.Tokens{}) || r.Cost != (metering.Charge{}) || r.Model.Name != "free" {
		t.Fatalf("record = %+v", r)
	}
	// With allowUnpriced the request is served, priced false.
	_, allowed := h.key(t, "allowed", edits(draws(hard), func(k *v1.Key) { k.Spec.AllowUnpriced = true }))
	if rec := p.do("POST", "/openai/v1/chat/completions", allowed, chat("free"), nil); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if r := p.records(t)[0]; r.Status != metering.StatusOK || r.Cost.Priced || r.Tokens.Input != 10 {
		t.Fatalf("the allowed record = %+v", r)
	}
}

// TestKeyCountersFollowTheRecords: the three counters under a Key's
// spend window and under none carry the requests, tokens, and spend the
// Key's admitted records sum to, the Recorder adding to none of them;
// and no window row exists for a Key without a spend limit.
func TestKeyCountersFollowTheRecords(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.plane(t, nil)
	oai, _ := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	down := &stub{}
	down.set(http.StatusBadGateway, `{"error":"down"}`)
	broken := h.provider(t, "broken", v1.DialectOpenAI, serveStub(t, down)+"/v1", nil)
	p.model(t, "flaky", broken.Metadata.Name, "gpt-4.1", card(t, "USD"))
	limited, value := h.key(t, "limited", spends("1000", "USD", "1h"))
	free, freeValue := h.key(t, "free", nil)
	for range 5 {
		if rec := p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	p.do("POST", "/openai/v1/chat/completions", value, chat("flaky"), nil) // admitted, then failed
	p.do("POST", "/openai/v1/chat/completions", value, chat("nope"), nil)  // refused before the gate
	for range 2 {
		p.do("POST", "/openai/v1/chat/completions", freeValue, chat("gpt"), nil)
	}
	p.limiter.Flush(ctx)
	if err := p.recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var requests, tokens, spend int64
	for _, r := range p.records(t) {
		if r.Key.ID != limited.Status.ID || r.Status == metering.StatusRefused {
			continue
		}
		requests++
		tokens += r.Tokens.Input + r.Tokens.Output
		spend += r.Cost.Amount
	}
	if requests != 6 || tokens != 5*13 || spend != 5*(10*10_000+3*20_000+2*1_000) {
		t.Fatalf("the records sum to %d requests, %d tokens, %d micro-units", requests, tokens, spend)
	}
	now, created := h.clock(), limited.Status.CreatedAt
	for _, c := range []struct {
		key  string
		want int64
	}{
		{metering.TotalKey(metering.ScopeKeyRequests, limited.Status.ID), requests},
		{metering.TotalKey(metering.ScopeKeyTokens, limited.Status.ID), tokens},
		{metering.TotalKey(metering.ScopeKeySpend, limited.Status.ID), spend},
		{metering.CounterKey(metering.ScopeKeyRequests, limited.Status.ID, "1h", now, created), requests},
		{metering.CounterKey(metering.ScopeKeyTokens, limited.Status.ID, "1h", now, created), tokens},
		{metering.CounterKey(metering.ScopeKeySpend, limited.Status.ID, "1h", now, created), spend},
		{metering.TotalKey(metering.ScopeKeyRequests, free.Status.ID), 2},
	} {
		if got := h.counter(t, c.key); got != c.want {
			t.Errorf("%s = %d, want %d", c.key, got, c.want)
		}
	}
	m, err := h.st.Counters().Read(ctx, []string{metering.CounterKey(metering.ScopeKeyRequests, free.Status.ID, "1h", now, free.Status.CreatedAt)})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Fatalf("a Key without a spend limit has a window row: %v", m)
	}
	// status.usage renders the same figures.
	if err := RenderKey(ctx, h.st, limited, now); err != nil {
		t.Fatal(err)
	}
	if u := limited.Status.Usage; u.Total.Requests != requests || u.Total.Tokens != tokens || int64(u.Total.Spend) != spend || u.Window.Requests != requests {
		t.Fatalf("status.usage = %+v", u)
	}
}

// TestAggregatesMatchTheRecords is the Recorder's half of the row: the
// rows the store answers after a flush equal the fold of the records
// the run produced, for every grouping, and nothing is written before
// the flush.
func TestAggregatesMatchTheRecords(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	p := h.plane(t, nil)
	oai, _ := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	ant, _ := p.upstream(t, "ant", v1.DialectAnthropic, anthropicResponse)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	p.model(t, "claude", ant.Metadata.Name, "claude-3", card(t, "EUR"))
	p.model(t, "free", ant.Metadata.Name, "claude-3", nil)
	_, a := h.key(t, "a", func(k *v1.Key) { k.Metadata.Labels = map[string]string{"team": "red"} })
	_, b := h.key(t, "b", func(k *v1.Key) { k.Metadata.Labels = map[string]string{"team": "blue"} })
	for i := range 12 {
		value := a
		if i%3 == 0 {
			value = b
		}
		switch i % 4 {
		case 0:
			p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil)
		case 1:
			p.do("POST", "/anthropic/v1/messages", value, messages("claude"), nil)
		case 2:
			p.do("POST", "/anthropic/v1/messages", value, messages("free"), nil)
		default:
			p.do("POST", "/openai/v1/chat/completions", value, chat("nope"), nil)
		}
	}
	q := metering.Query{From: h.clock().Add(-time.Hour), To: h.clock().Add(time.Hour)}
	rows, err := h.st.Usage().QueryRows(ctx, q)
	if err != nil || len(rows) != 0 {
		t.Fatalf("before the flush: %d rows, %v", len(rows), err)
	}
	if p.recorder.Pending() == 0 {
		t.Fatal("no rows pending")
	}
	if err := p.recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if p.recorder.Pending() != 0 {
		t.Fatal("rows pending after the flush")
	}
	recs := p.records(t)
	if len(recs) != 12 {
		t.Fatalf("%d records", len(recs))
	}
	for _, by := range [][]metering.Dimension{nil, {metering.DimensionKey}, {metering.DimensionModel, metering.DimensionStatus}, {"label:team", metering.DimensionDoor}} {
		for _, in := range []metering.Interval{metering.IntervalNone, metering.IntervalHour, metering.IntervalDay} {
			q.By, q.Interval = by, in
			got, err := Usage(ctx, h.st, q, h.clock())
			if err != nil {
				t.Fatal(err)
			}
			want := metering.Fold(recs, by, in)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("by %v, %s:\n got %s\nwant %s", by, in, gotJSON, wantJSON)
			}
		}
	}
	// Three currencies' worth of rows in the total: USD, EUR, and the
	// unpriced and refused rows with none.
	rows, err = Usage(ctx, h.st, metering.Query{From: q.From, To: q.To}, h.clock())
	if err != nil || len(rows) != 3 {
		t.Fatalf("total rows = %d, %v", len(rows), err)
	}
	// A second flush with nothing pending writes nothing and keeps the
	// rows; a second run adds to them.
	if err := p.recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	p.do("POST", "/openai/v1/chat/completions", a, chat("gpt"), nil)
	if err := p.recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = Usage(ctx, h.st, metering.Query{From: q.From, To: q.To, By: []metering.Dimension{metering.DimensionStatus}}, h.clock())
	var requests int64
	for _, r := range rows {
		requests += r.Requests
	}
	if requests != 13 {
		t.Fatalf("after a second run the rows hold %d requests", requests)
	}
}

// TestUsageDefaultsAndValidation: Usage fills an open range with the
// last day, refuses a query outside the table with a QueryError at its
// parameter, never answers nil, and reports the store's failure.
func TestUsageDefaultsAndValidation(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	now := h.clock()
	rows, err := Usage(ctx, h.st, metering.Query{}, now)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("an open query on an empty store = %v, %v", rows, err)
	}
	_, err = Usage(ctx, h.st, metering.Query{From: now.Add(-91 * 24 * time.Hour)}, now)
	var qe *metering.QueryError
	if !errors.As(err, &qe) || qe.Field != "to" {
		t.Fatalf("ninety-one days = %v", err)
	}
	_, err = Usage(ctx, h.st, metering.Query{By: []metering.Dimension{"currency"}}, now)
	if !errors.As(err, &qe) || qe.Field != "by" {
		t.Fatalf("an unknown dimension = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Usage(cancelled, h.st, metering.Query{}, now); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "reading the usage rows from") {
		t.Fatalf("a store failure = %v", err)
	}
}

// failingUsage is a store whose Usage().AddRows fails while told to,
// and whose AppendRecord fails while failAppend is set.
type failingUsage struct {
	store.Store
	fail       bool
	failAppend bool
}

func (f failingRows) AppendRecord(ctx context.Context, r metering.Record) error {
	if f.s.failAppend {
		return errStoreDown
	}
	return f.Usage.AppendRecord(ctx, r)
}

type failingRows struct {
	store.Usage
	s *failingUsage
}

var errStoreDown = errors.New("the store is down")

func (f *failingUsage) Usage() store.Usage { return failingRows{f.Store.Usage(), f} }

func (f failingRows) AddRows(ctx context.Context, rows []metering.Aggregate) error {
	if f.s.fail {
		return errStoreDown
	}
	return f.Usage.AddRows(ctx, rows)
}

// TestMeteringMetrics: lux_tokens_total by direction and
// lux_spend_microunits_total by currency follow a run's records, and
// lux_metering_flush_lag_seconds grows while the store refuses the
// flush, over the Recorder's rows and the Limiter's counters alike, and
// falls to zero when a flush succeeds, the kept rows written with it.
func TestMeteringMetrics(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	failing := &failingUsage{Store: h.st}
	p := h.plane(t, failing)
	oai, _ := p.upstream(t, "oai", v1.DialectOpenAI, openaiChatResponse)
	ant, _ := p.upstream(t, "ant", v1.DialectAnthropic, anthropicResponse)
	p.model(t, "gpt", oai.Metadata.Name, "gpt-4.1", card(t, "USD"))
	p.model(t, "claude", ant.Metadata.Name, "claude-3", card(t, "EUR"))
	_, value := h.key(t, "all", nil)
	for range 3 {
		p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil)
	}
	p.do("POST", "/anthropic/v1/messages", value, messages("claude"), nil)
	p.do("POST", "/openai/v1/chat/completions", value, chat("nope"), nil)
	var tokens metering.Tokens
	spend := map[string]int64{}
	for _, r := range p.records(t) {
		tokens.Input += r.Tokens.Input
		tokens.Output += r.Tokens.Output
		tokens.CachedInput += r.Tokens.CachedInput
		tokens.CacheWrite += r.Tokens.CacheWrite
		if r.Cost.Priced {
			spend[r.Cost.Currency] += r.Cost.Amount
		}
	}
	counter := p.reg.Counter(MetricTokens, "")
	for direction, want := range map[string]int64{"input": tokens.Input, "output": tokens.Output, "cached_input": tokens.CachedInput, "cache_write": tokens.CacheWrite} {
		if got := counter.Value(map[string]string{"direction": direction}); int64(got) != want || want == 0 {
			t.Errorf("lux_tokens_total{direction=%s} = %d, want %d", direction, got, want)
		}
	}
	for currency, want := range spend {
		if got := p.reg.Counter(MetricSpend, "").Value(map[string]string{"currency": currency}); int64(got) != want || want == 0 {
			t.Errorf("lux_spend_microunits_total{currency=%s} = %d, want %d", currency, got, want)
		}
	}
	if got := p.reg.Counter(MetricSpend, "").Value(map[string]string{"currency": ""}); got != 0 {
		t.Errorf("an unpriced record added %d to the spend", got)
	}
	// The lag: zero after a flush, the clock's advance without one, and
	// growing while the store refuses; the rows wait and land later.
	if lag := flushLag(t, p.reg); lag != 0 {
		t.Fatalf("lag at start = %s", lag)
	}
	h.advance(30 * time.Second)
	if lag := flushLag(t, p.reg); lag != 30*time.Second {
		t.Fatalf("lag without a flush = %s", lag)
	}
	failing.fail = true
	if err := p.recorder.Flush(ctx); !errors.Is(err, errStoreDown) {
		t.Fatalf("Flush against a refusing store = %v", err)
	}
	if lag := flushLag(t, p.reg); lag != 30*time.Second {
		t.Fatalf("lag after a refused flush = %s", lag)
	}
	pending := p.recorder.Pending()
	if pending == 0 {
		t.Fatal("the refused rows were dropped")
	}
	h.advance(15 * time.Second)
	p.do("POST", "/openai/v1/chat/completions", value, chat("gpt"), nil)
	if p.recorder.Pending() != pending {
		t.Fatalf("a record after the refusal opened %d rows, want the same hour's", p.recorder.Pending()-pending)
	}
	failing.fail = false
	if err := p.recorder.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// The Limiter's counters have not flushed since, so the gauge still
	// reads their lag; once they do it falls to zero.
	if lag := flushLag(t, p.reg); lag != 45*time.Second {
		t.Fatalf("lag with the Recorder flushed and the Limiter not = %s", lag)
	}
	p.limiter.Flush(ctx)
	if lag := flushLag(t, p.reg); lag != 0 {
		t.Fatalf("lag after both flushed = %s", lag)
	}
	rows, err := Usage(ctx, h.st, metering.Query{From: h.clock().Add(-time.Hour), To: h.clock().Add(time.Hour)}, h.clock())
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, r := range rows {
		requests += r.Requests
	}
	if requests != 6 {
		t.Fatalf("the rows hold %d requests after the kept flush, want 6", requests)
	}
	// The three are what the registry exposes for this spec.
	var out bytes.Buffer
	p.reg.WritePrometheus(&out)
	for _, name := range []string{MetricTokens, MetricSpend, MetricFlushLag} {
		if !strings.Contains(out.String(), "\n"+name+"{") && !strings.Contains(out.String(), "\n"+name+" ") {
			t.Errorf("%s is not exposed:\n%s", name, out.String())
		}
	}
}

// flushLag reads the gauge off the registry's exposition.
func flushLag(t *testing.T, reg *metrics.Registry) time.Duration {
	t.Helper()
	var out bytes.Buffer
	reg.WritePrometheus(&out)
	for line := range strings.SplitSeq(out.String(), "\n") {
		if !strings.HasPrefix(line, MetricFlushLag) {
			continue
		}
		_, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("gauge line %q: %v", line, err)
		}
		return time.Duration(f * float64(time.Second))
	}
	t.Fatalf("%s is not exposed:\n%s", MetricFlushLag, out.String())
	return 0
}

// TestRecorderRunFlushesAtStop: Run flushes on the interval and once
// more when the context ends, and a Recorder with no registry or
// catalog records unpriced and counts nothing.
func TestRecorderRunFlushesAtStop(t *testing.T) {
	h := newHarness(t)
	r := NewRecorder(RecorderOptions{Store: h.st, Flush: 10 * time.Millisecond})
	if r.o.Now == nil || r.o.Logger == nil || r.tokens != nil {
		t.Fatal("defaults")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	at := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	r.Record(gateway.Record{ID: "req_1", At: at, EndedAt: at.Add(time.Second), KeyID: "key_1", Model: "gpt", Status: gateway.StatusOK, Tokens: gateway.Tokens{Input: 5}, Latency: time.Second})
	waitUntil(t, "the interval flush", func() bool {
		rows, _ := h.st.Usage().QueryRows(t.Context(), metering.Query{From: at.Add(-time.Hour), To: at.Add(time.Hour)})
		return len(rows) == 1
	})
	r.Record(gateway.Record{ID: "req_2", At: at, EndedAt: at.Add(time.Second), KeyID: "key_1", Status: gateway.StatusRefused, Error: gateway.CodeRateLimited})
	cancel()
	<-done
	rows, err := h.st.Usage().QueryRows(t.Context(), metering.Query{From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil || len(rows) != 1 || rows[0].Requests != 2 || rows[0].Refused != 1 || rows[0].UnpricedRequests != 2 {
		t.Fatalf("after stop: %+v, %v", rows, err)
	}
	recs, _, _ := h.st.Usage().Records(t.Context(), metering.RecordQuery{Query: metering.Query{From: at.Add(-time.Hour), To: at.Add(time.Hour)}}, store.Page{})
	if len(recs) != 2 || recs[0].Cost.Priced || recs[0].Loss == nil || recs[0].Attempts == nil || recs[0].Labels == nil || recs[0].RequestLabels == nil {
		t.Fatalf("records = %+v", recs)
	}
	// A run whose final flush fails logs it and stops.
	var log bytes.Buffer
	failing := &failingUsage{Store: h.st, fail: true}
	r2 := NewRecorder(RecorderOptions{Store: failing, Flush: time.Hour, Logger: slog.New(slog.NewTextHandler(&log, nil))})
	r2.Record(gateway.Record{ID: "req_3", At: at, Status: gateway.StatusOK})
	ctx2, cancel2 := context.WithCancel(t.Context())
	cancel2()
	r2.Run(ctx2)
	if !strings.Contains(log.String(), "the final flush of the aggregates") || r2.Pending() != 1 {
		t.Fatalf("log = %s, pending %d", log.String(), r2.Pending())
	}
}

// brokenCatalog is a catalog that fails, or knows no Model.
type brokenCatalog struct {
	gateway.Catalog
	err error
}

func (c brokenCatalog) Model(context.Context, string) (*v1.Model, error) { return nil, c.err }

// TestRecorderToleratesTheStore: a ring that refuses the record and a
// catalog that fails or knows no Model are logged and the record is
// kept, unpriced where the price could not be read; a refused flush
// keeps its rows and folds a later record of the same hour into them.
func TestRecorderToleratesTheStore(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	failing := &failingUsage{Store: h.st, failAppend: true}
	at := h.clock()
	rec := gateway.Record{ID: "req_1", At: at, EndedAt: at, KeyID: "key_1", Model: "gpt", Status: gateway.StatusOK, Tokens: gateway.Tokens{Input: 5}}
	r := NewRecorder(RecorderOptions{Store: failing, Catalog: brokenCatalog{err: errStoreDown}, Logger: h.logger, Now: h.clock})
	r.Record(rec)
	if !strings.Contains(h.logged(), "appending the record to the ring") || !strings.Contains(h.logged(), "reading the Model's pricing") {
		t.Fatalf("log = %s", h.logged())
	}
	if r.Pending() != 1 {
		t.Fatalf("pending = %d", r.Pending())
	}
	// A catalog that knows no Model of that name prices nothing and
	// logs nothing.
	before := len(h.logged())
	r2 := NewRecorder(RecorderOptions{Store: h.st, Catalog: brokenCatalog{}, Logger: h.logger, Now: h.clock})
	r2.Record(rec)
	if len(h.logged()) != before {
		t.Fatalf("an unknown Model was logged: %s", h.logged()[before:])
	}
	if recs, _, _ := h.st.Usage().Records(ctx, metering.RecordQuery{Query: metering.Query{From: at.Add(-time.Hour), To: at.Add(time.Hour)}}, store.Page{}); len(recs) != 1 || recs[0].Cost.Priced {
		t.Fatalf("records = %+v", recs)
	}
	// A refused flush keeps the row; a second record of the same hour
	// merges into the kept row; the next flush writes both.
	failing.fail, failing.failAppend = true, false
	if err := r.Flush(ctx); !errors.Is(err, errStoreDown) || r.Pending() != 1 {
		t.Fatalf("Flush = %v, pending %d", err, r.Pending())
	}
	r.Record(rec)
	if r.Pending() != 1 {
		t.Fatalf("a second record of the hour opened a row: %d", r.Pending())
	}
	if err := r.Flush(ctx); !errors.Is(err, errStoreDown) || r.Pending() != 1 {
		t.Fatalf("second refused Flush = %v, pending %d", err, r.Pending())
	}
	failing.fail = false
	if err := r.Flush(ctx); err != nil || r.Pending() != 0 {
		t.Fatalf("Flush = %v, pending %d", err, r.Pending())
	}
	rows, err := h.st.Usage().QueryRows(ctx, metering.Query{From: at.Add(-time.Hour), To: at.Add(time.Hour), Keys: []string{"key_1"}})
	if err != nil || len(rows) != 1 || rows[0].Requests != 2 || rows[0].InputTokens != 10 || rows[0].UnpricedRequests != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	// Run logs an interval flush that fails and keeps going.
	failing.fail = true
	r3 := NewRecorder(RecorderOptions{Store: failing, Flush: 5 * time.Millisecond, Logger: h.logger, Now: h.clock})
	r3.Record(rec)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r3.Run(runCtx)
	}()
	waitUntil(t, "the logged interval flush", func() bool { return strings.Contains(h.logged(), "flushing the aggregates") })
	cancel()
	<-done
	if r3.Pending() != 1 {
		t.Fatalf("the failed run dropped its row")
	}
}
