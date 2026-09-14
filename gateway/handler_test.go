// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/metrics"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestNewRequiresOptions is the builder's precision: a handler without
// one of the five required options does not start.
func TestNewRequiresOptions(t *testing.T) {
	w := newWorld(t)
	for _, strip := range []func(o *Options){
		func(o *Options) { o.Keys = nil }, func(o *Options) { o.Catalog = nil }, func(o *Options) { o.Credentials = nil },
		func(o *Options) { o.Router = nil }, func(o *Options) { o.Clients = nil },
	} {
		o := w.options()
		strip(&o)
		func() {
			defer func() {
				if recover() == nil {
					t.Error("New accepted a nil required option")
				}
			}()
			New(o)
		}()
	}
	// The optional ones may be nil, and the defaults fill in.
	o := Options{Keys: w.keys, Catalog: w.catalog, Credentials: w.creds, Router: w.router, Clients: w.clients}
	h := New(o)
	if h.maxBody != DefaultMaxBodyBytes || h.ua != "luxd/dev" || h.now == nil || h.newID == nil || h.metrics != nil {
		t.Errorf("defaults: %+v", h)
	}
	id := h.newID()
	if !strings.HasPrefix(id, "req_") || len(id) != 4+v1.IDLength {
		t.Errorf("default id %q", id)
	}
	w.openai.respondJSON(200, openaiChatResponse)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)))
	if rec.Code != 200 {
		t.Errorf("a handler with no limiter, recorder, or health refused: %d %s", rec.Code, rec.Body.String())
	}
}

// TestRouteTable drives every row of the door table through the handler
// and checks the class the record carries and where the model came from;
// every off-table path is not_found in the door's envelope.
func TestRouteTable(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	w.anthropic.respondJSON(200, anthropicResponse)
	w.gemini.respondJSON(200, geminiResponse)
	w.lux.respondJSON(200, luxResponse)
	bodies := map[operation]string{
		opChatCompletions: chatBody("gpt", false), opResponses: `{"model":"gpt","input":"hi"}`,
		opEmbeddings: `{"model":"gpt","input":"hi"}`, opMessages: messagesBody("claude", false),
		opAnthropicCount: messagesBody("claude", false), opGenerate: luxBody("luxm", false),
		opLuxCount: luxBody("luxm", false), opGeminiGenerate: `{"contents":[]}`, opGeminiStream: `{"contents":[]}`,
		opGeminiCount: `{"contents":[]}`, opGeminiEmbed: `{"content":{}}`, opOpaque: `{}`,
	}
	for _, row := range tableRows {
		path := row.path
		switch row.op {
		case opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed:
			path = strings.Replace(path, "gemini-pro", "gemini", 1)
			path = strings.Replace(path, "embed-1", "gemini", 1)
		case opModelsRead:
			path = "/" + string(row.door) + versionPrefix(row.door) + "/models/gpt"
		case opChatCompletions, opResponses, opEmbeddings, opMessages, opAnthropicCount, opGenerate, opLuxCount, opModelsList, opOpaque, opNone:
		}
		r := w.request(row.method, path, bodies[row.op])
		if row.class == ClassOpaque {
			r.Header.Set("Authorization", "Bearer "+passthroughValue)
			r.Header.Set(HeaderProvider, map[v1.Dialect]string{v1.DialectOpenAI: "oai", v1.DialectAnthropic: "ant", v1.DialectGemini: "gem"}[row.door])
		}
		rec := w.do(r)
		record := w.recorder.last(t)
		if rec.Code != 200 {
			t.Errorf("%s %s: %d %s", row.method, path, rec.Code, rec.Body.String())
			continue
		}
		if record.Class != row.class || record.Route != row.template {
			t.Errorf("%s %s: record class %s route %s, want %s %s", row.method, path, record.Class, record.Route, row.class, row.template)
		}
		wantModel := "gpt"
		switch row.op {
		case opMessages, opAnthropicCount:
			wantModel = "claude"
		case opGenerate, opLuxCount:
			wantModel = "luxm"
		case opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed:
			wantModel = "gemini"
		case opModelsList, opOpaque:
			wantModel = ""
		case opChatCompletions, opResponses, opEmbeddings, opModelsRead, opNone:
		}
		if record.Model != wantModel {
			t.Errorf("%s %s: record model %q, want %q", row.method, path, record.Model, wantModel)
		}
	}
	for _, row := range offTable {
		rec := w.do(w.request(row.method, row.path, `{"model":"gpt"}`))
		if code := errorCode(t, rec); code != CodeNotFound {
			t.Errorf("%s %s: %s, want not_found", row.method, row.path, code)
		}
		checkEnvelope(t, row.door, rec, CodeNotFound)
	}
}

// checkEnvelope reads a door's error body and holds it to the door's
// shape with the code where the shape has a code member.
func checkEnvelope(t *testing.T, door v1.Dialect, rec *httptest.ResponseRecorder, code Code) {
	t.Helper()
	body := rec.Body.Bytes()
	var got string
	switch door {
	case v1.DialectOpenAI:
		var e openaiError
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		got = e.Error.Code
		if e.Error.Message != code.Message() || e.Error.Type != string(code) {
			t.Errorf("openai envelope %s", body)
		}
	case v1.DialectAnthropic:
		var e anthropicError
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		got = e.Error.Type
		if e.Error.Message != code.Message() || e.RequestID != rec.Header().Get(HeaderRequestID) {
			t.Errorf("anthropic envelope %s", body)
		}
	case v1.DialectGemini:
		var e geminiError
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		got = e.Error.Details[0].Reason
		if e.Error.Message != code.Message() || e.Error.Code != code.Status() || e.Error.Status != googleStatus(code.Status()) {
			t.Errorf("gemini envelope %s", body)
		}
	case v1.DialectLux, "":
		var e struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatal(err)
		}
		got = e.Error.Code
		if e.Error.Message != code.Message() || e.Error.Details["request_id"] != rec.Header().Get(HeaderRequestID) {
			t.Errorf("lux envelope %s", body)
		}
	}
	if got != string(code) {
		t.Errorf("%s envelope carries code %q, want %q: %s", door, got, code, body)
	}
	if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("%s envelope headers %v", door, rec.Header())
	}
}

// TestKeyExtractionOrder is each credential form on each door, and the
// stated precedence when several are present.
func TestKeyExtractionOrder(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	w.anthropic.respondJSON(200, anthropicResponse)
	w.gemini.respondJSON(200, geminiResponse)
	w.lux.respondJSON(200, luxResponse)
	doorRequests := map[v1.Dialect]func() *http.Request{
		v1.DialectOpenAI: func() *http.Request {
			return httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(chatBody("gpt", false)))
		},
		v1.DialectAnthropic: func() *http.Request {
			return httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(messagesBody("claude", false)))
		},
		v1.DialectGemini: func() *http.Request {
			return httptest.NewRequest("POST", "/gemini/v1beta/models/gemini:generateContent", strings.NewReader(`{"contents":[]}`))
		},
		v1.DialectLux: func() *http.Request {
			return httptest.NewRequest("POST", "/lux/v1/generate", strings.NewReader(luxBody("luxm", false)))
		},
	}
	forms := []func(r *http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+keyValue) },
		func(r *http.Request) { r.Header.Set("x-api-key", keyValue) },
		func(r *http.Request) { r.Header.Set("x-goog-api-key", keyValue) },
		func(r *http.Request) { r.URL.RawQuery = "key=" + keyValue },
	}
	for d, mk := range doorRequests {
		for i, form := range forms {
			r := mk()
			form(r)
			if rec := w.do(r); rec.Code != 200 {
				t.Errorf("%s door, form %d: %d %s", d, i, rec.Code, rec.Body.String())
			}
		}
		// The bearer wins over a bad x-api-key, which wins over a bad
		// x-goog-api-key, which wins over a bad query key.
		r := mk()
		r.Header.Set("Authorization", "Bearer "+keyValue)
		r.Header.Set("x-api-key", "wrong")
		r.Header.Set("x-goog-api-key", "wrong")
		r.URL.RawQuery = "key=wrong"
		if rec := w.do(r); rec.Code != 200 {
			t.Errorf("%s door: the bearer did not win: %d", d, rec.Code)
		}
		r = mk()
		r.Header.Set("x-api-key", "wrong")
		r.Header.Set("x-goog-api-key", keyValue)
		if rec := w.do(r); errorCode(t, rec) != CodeUnauthenticated {
			t.Errorf("%s door: x-api-key did not precede x-goog-api-key", d)
		}
		r = mk()
		if rec := w.do(r); errorCode(t, rec) != CodeUnauthenticated {
			t.Errorf("%s door: no credential was not unauthenticated", d)
		}
	}
}

// TestCallerCredentialsNeverForwarded: the query parameter and every
// caller credential header are absent from the outbound request, and
// the Provider's own is present.
func TestCallerCredentialsNeverForwarded(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	r := w.request(http.MethodPost, "/openai/v1/chat/completions?key="+keyValue+"&foo=bar", chatBody("gpt", false))
	r.Header.Set("x-api-key", "caller-copy")
	r.Header.Set("x-goog-api-key", "caller-copy")
	r.Header.Set("Proxy-Authorization", "Basic xyz")
	r.Header.Set("Cookie", "session=1")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("Forwarded", "for=203.0.113.9")
	r.Header.Set(HeaderLabels, "team=a")
	r.Header.Set("Lux-Anything", "x")
	if rec := w.do(r); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := w.openai.last(t)
	for _, name := range []string{"x-api-key", "x-goog-api-key", "Proxy-Authorization", "Cookie", "X-Forwarded-For", "Forwarded", HeaderLabels, "Lux-Anything"} {
		if got.Header.Get(name) != "" {
			t.Errorf("%s reached the provider: %q", name, got.Header.Get(name))
		}
	}
	if got.Header.Get("Authorization") != "Bearer "+openaiCredential {
		t.Errorf("the provider's credential is %q", got.Header.Get("Authorization"))
	}
	if got.Query != "foo=bar" {
		t.Errorf("query %q reached the provider", got.Query)
	}
	if got.Header.Get(HeaderRequestID) == "" || got.Header.Get("User-Agent") != "luxd/test" {
		t.Errorf("outbound headers %v", got.Header)
	}
}

// TestDoorsTakeKeysOnly: a provider's own key pasted by mistake and an
// issuer token no Key was registered with are unauthenticated; the
// handler compares no shape.
func TestDoorsTakeKeysOnly(t *testing.T) {
	w := newWorld(t)
	for _, value := range []string{providerKeyPasted, unregisteredToken, "lux_" + strings.Repeat("x", 40)} {
		r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(chatBody("gpt", false)))
		r.Header.Set("Authorization", "Bearer "+value)
		rec := w.do(r)
		if errorCode(t, rec) != CodeUnauthenticated {
			t.Errorf("%q opened the door", value)
		}
		if strings.Contains(rec.Header().Get(HeaderErrorDetail), value) || strings.Contains(rec.Body.String(), value) {
			t.Error("the presented value is echoed")
		}
	}
	if w.openai.count() != 0 {
		t.Error("an unauthenticated request reached the provider")
	}
	if got := w.recorder.count(); got != 3 {
		t.Errorf("%d records for 3 refusals", got)
	}
	if rec := w.recorder.last(t); rec.Status != StatusRefused || rec.Error != CodeUnauthenticated || rec.KeyID != "" {
		t.Errorf("record %+v", rec)
	}
}

// TestSuppliedValueOpensTheDoor: an issuer token a platform registered
// as a supplied Key value is served as that Key and as nothing more.
func TestSuppliedValueOpensTheDoor(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(chatBody("gpt", false)))
	r.Header.Set("Authorization", "Bearer "+suppliedValue)
	if rec := w.do(r); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec := w.recorder.last(t)
	if rec.KeyPrefix != "sup_1a2b3c4d" || rec.KeyID != w.keys.byHash[hashValue(suppliedValue)].Status.ID {
		t.Errorf("record %+v", rec)
	}
	if w.openai.last(t).Header.Get("Authorization") != "Bearer "+openaiCredential {
		t.Error("the supplied value was forwarded in place of the provider's credential")
	}
}

// TestInvalidRequestBodies: a body that is not a JSON object, one
// without model, and on translation one the codec refuses are each
// invalid_request with the codec's message in the detail and nothing
// forwarded.
func TestInvalidRequestBodies(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		path, body string
		detail     string
	}{
		{"/openai/v1/chat/completions", `[1,2]`, "not a JSON object"},
		{"/openai/v1/chat/completions", `"text"`, "not a JSON object"},
		{"/openai/v1/chat/completions", `{broken`, "not a JSON object"},
		{"/openai/v1/chat/completions", `{"messages":[]}`, "no string model"},
		{"/openai/v1/chat/completions", `{"model":42}`, "no string model"},
		{"/anthropic/v1/messages", `{"model":"gpt","max_tokens":5}`, "anthropic: messages is required"},
		{"/lux/v1/generate", `{"model":"claude","messages":[{"role":"user","blocks":[{"type":"text","text":"x"}]}],"reasoning":{"effort":"low","budget_tokens":5}}`, "reasoning takes effort or budget_tokens"},
		{"/openai/v1/responses", `{"model":"claude","input":"x","previous_response_id":"r1"}`, "previous_response_id"},
	}
	for _, c := range cases {
		rec := w.post(c.path, c.body)
		if code := errorCode(t, rec); code != CodeInvalidRequest {
			t.Errorf("%s %s: %s, want invalid_request", c.path, c.body, code)
			continue
		}
		if d := rec.Header().Get(HeaderErrorDetail); !strings.Contains(d, c.detail) {
			t.Errorf("%s %s: detail %q lacks %q", c.path, c.body, d, c.detail)
		}
		if strings.Contains(rec.Body.String(), c.detail) && c.path != "/lux/v1/generate" {
			t.Errorf("%s: the detail is in the body", c.path)
		}
	}
	for _, s := range w.stubs {
		if s.count() != 0 {
			t.Error("an invalid body reached a provider")
		}
	}
}

// TestPassthroughForwardsWhatTheCodecCannotRead: the same undecodable
// body on a same-dialect passthrough reaches the stub provider untouched.
func TestPassthroughForwardsWhatTheCodecCannotRead(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondJSON(200, anthropicResponse)
	body := `{"model":"claude-3","max_tokens":5}` // the codec wants messages; the target may not
	if rec := w.post("/anthropic/v1/messages", body); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := string(w.anthropic.last(t).Body); got != body {
		t.Errorf("the provider received %s", got)
	}
}

// TestRefusalOrder holds two conditions at once and checks the earlier
// stage's code answers, for every refusal code, with nothing forwarded.
func TestRefusalOrder(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	oversized := strings.Repeat("x", int(w.h.maxBody)+1)
	type step struct {
		name string
		code Code
		r    func() *http.Request
	}
	steps := []step{
		{"not_found before body_too_large", CodeNotFound, func() *http.Request {
			r := httptest.NewRequest("POST", "/openai/v2/nothing", strings.NewReader(oversized))
			return r
		}},
		{"body_too_large before unauthenticated", CodeBodyTooLarge, func() *http.Request {
			return httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(oversized))
		}},
		{"unauthenticated before route_not_allowed", CodeUnauthenticated, func() *http.Request {
			return httptest.NewRequest("POST", "/openai/v1/files", strings.NewReader(`{}`))
		}},
		{"key_disabled before model_not_found", CodeKeyDisabled, func() *http.Request {
			r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(chatBody("nope", false)))
			r.Header.Set("Authorization", "Bearer "+disabledKeyValue)
			return r
		}},
		{"key_expired before model_not_found", CodeKeyExpired, func() *http.Request {
			r := httptest.NewRequest("POST", "/openai/v1/chat/completions", strings.NewReader(chatBody("nope", false)))
			r.Header.Set("Authorization", "Bearer "+expiredKeyValue)
			return r
		}},
		{"route_not_allowed before provider_required", CodeRouteNotAllowed, func() *http.Request {
			return w.request("POST", "/openai/v1/files", `{}`)
		}},
		{"invalid_request before model_not_found", CodeInvalidRequest, func() *http.Request {
			return w.request("POST", "/openai/v1/chat/completions", `[]`)
		}},
		{"model_not_found before model_not_allowed", CodeModelNotFound, func() *http.Request {
			r := w.request("POST", "/openai/v1/chat/completions", chatBody("nope", false))
			r.Header.Set("Authorization", "Bearer "+restrictedValue)
			return r
		}},
		{"model_not_allowed before dialect_unsupported", CodeModelNotAllowed, func() *http.Request {
			r := w.request("POST", "/openai/v1/chat/completions", chatBody("gemini", false))
			r.Header.Set("Authorization", "Bearer "+restrictedValue)
			return r
		}},
		{"provider_unavailable before rate_limited", CodeProviderUnavailable, func() *http.Request {
			w.router.exclude["oai/gpt-4.1"] = true
			w.limiter.refusal = &Refusal{Code: CodeRateLimited, RetryAfter: time.Second}
			return w.request("POST", "/openai/v1/chat/completions", chatBody("gpt", false))
		}},
		{"dialect_unsupported before rate_limited", CodeDialectUnsupported, func() *http.Request {
			w.router.exclude = map[string]bool{}
			w.limiter.refusal = &Refusal{Code: CodeRateLimited, RetryAfter: time.Second}
			return w.request("POST", "/openai/v1/chat/completions", chatBody("gemini", false))
		}},
		{"rate_limited before invalid_request at the codec", CodeRateLimited, func() *http.Request {
			w.limiter.refusal = &Refusal{Code: CodeRateLimited, RetryAfter: time.Second, Detail: "bucket empty"}
			return w.request("POST", "/openai/v1/chat/completions", `{"model":"claude","messages":[{"role":"tool","content":"x"}]}`)
		}},
	}
	for _, code := range []Code{CodeModelUnpriced, CodeCurrencyMismatch, CodeSpendExceeded, CodeBudgetExhausted} {
		steps = append(steps, step{string(code) + " from the limiter", code, func() *http.Request {
			w.limiter.refusal = &Refusal{Code: code, RetryAfter: 90 * time.Second}
			return w.request("POST", "/openai/v1/chat/completions", chatBody("gpt", false))
		}})
	}
	steps = append(steps, step{"provider_required with two candidates", CodeProviderRequired, func() *http.Request {
		w.limiter.refusal = nil
		w.provider("oai2", v1.DialectOpenAI, w.openai.URL+"/v1", "x")
		w.model("gpt2", target("oai2", "gpt-4.1"))
		r := w.request("POST", "/openai/v1/files", `{}`)
		r.Header.Set("Authorization", "Bearer "+passthroughValue)
		return r
	}})
	for _, s := range steps {
		rec := w.do(s.r())
		if code := errorCode(t, rec); code != s.code {
			t.Errorf("%s: got %s (%s)", s.name, code, rec.Header().Get(HeaderErrorDetail))
		}
		if s.code == CodeRateLimited && rec.Header().Get("Retry-After") != "1" {
			t.Errorf("%s: Retry-After %q", s.name, rec.Header().Get("Retry-After"))
		}
		if s.code == CodeSpendExceeded && rec.Header().Get("Retry-After") != "90" {
			t.Errorf("%s: Retry-After %q", s.name, rec.Header().Get("Retry-After"))
		}
		if record := w.recorder.last(t); record.Status != StatusRefused || record.Error != s.code {
			t.Errorf("%s: record %s %s", s.name, record.Status, record.Error)
		}
	}
	if w.openai.count() != 0 {
		t.Errorf("%d refused requests reached the provider", w.openai.count())
	}
	// A refusal after the reservation, the codec's at stage 8, settles the
	// lease with zero tokens: the whole reservation is refunded.
	w.limiter.refusal = nil
	before := len(w.limiter.lease.settled)
	rec := w.post("/openai/v1/chat/completions", `{"model":"claude","messages":[]}`)
	if errorCode(t, rec) != CodeInvalidRequest {
		t.Errorf("codec refusal after the reservation: %s", rec.Header().Get(HeaderError))
	}
	settled := w.limiter.lease.settled
	if len(settled) != before+1 || settled[len(settled)-1] != (Tokens{}) {
		t.Errorf("the reservation of a refused request was not refunded: %v", settled)
	}
	if w.openai.count() != 0 || w.anthropic.count() != 0 {
		t.Error("a refused request reached a provider")
	}
}

// TestBodyLimit: a body one byte over the limit is body_too_large with
// nothing forwarded, with and without a Content-Length.
func TestBodyLimit(t *testing.T) {
	w := newWorld(t)
	over := strings.Repeat("x", int(w.h.maxBody)+1)
	rec := w.post("/openai/v1/chat/completions", over)
	if errorCode(t, rec) != CodeBodyTooLarge || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "Content-Length") {
		t.Errorf("with a length: %s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", over)
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	rec = w.do(r)
	if errorCode(t, rec) != CodeBodyTooLarge || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "crossed") {
		t.Errorf("chunked: %s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	exact := strings.Repeat("x", int(w.h.maxBody))
	if rec := w.post("/openai/v1/chat/completions", exact); errorCode(t, rec) != CodeInvalidRequest {
		t.Errorf("a body at the limit is %s", rec.Header().Get(HeaderError))
	}
	if w.openai.count() != 0 {
		t.Error("an oversized body reached the provider")
	}
	// An opaque route refuses a declared length before a byte is read and
	// a chunked body at the byte that crosses, before the upstream
	// answered.
	r = w.request(http.MethodPost, "/openai/v1/files", over)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	if rec := w.do(r); errorCode(t, rec) != CodeBodyTooLarge {
		t.Errorf("opaque with a length: %s", rec.Header().Get(HeaderError))
	}
	r = w.request(http.MethodPost, "/openai/v1/files", over)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.ContentLength = -1
	if rec := w.do(r); errorCode(t, rec) != CodeBodyTooLarge {
		t.Errorf("opaque chunked: %s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
}

// TestRequestIDOnEveryResponse: every response, refused or served,
// carries Lux-Request-Id matching its record's id; a caller's own is
// replaced and a printable X-Request-Id is echoed.
func TestRequestIDOnEveryResponse(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false))
	r.Header.Set(HeaderRequestID, "req_callers")
	r.Header.Set("X-Request-Id", "trace-42")
	rec := w.do(r)
	id := rec.Header().Get(HeaderRequestID)
	if id == "req_callers" || !strings.HasPrefix(id, "req_") || id != w.recorder.last(t).ID {
		t.Errorf("served: id %q, record %q", id, w.recorder.last(t).ID)
	}
	if rec.Header().Get("X-Request-Id") != "trace-42" {
		t.Error("X-Request-Id was not echoed")
	}
	if got := w.openai.last(t).Header.Get(HeaderRequestID); got != id {
		t.Errorf("the outbound request carries %q", got)
	}
	r = httptest.NewRequest("GET", "/nowhere", nil)
	r.Header.Set("X-Request-Id", "bad\x01byte")
	rec = w.do(r)
	if rec.Header().Get(HeaderRequestID) != w.recorder.last(t).ID || rec.Header().Get("X-Request-Id") != "" {
		t.Errorf("refused: %v", rec.Header())
	}
	r = httptest.NewRequest("GET", "/nowhere", nil)
	r.Header.Set("X-Request-Id", strings.Repeat("a", 129))
	if rec := w.do(r); rec.Header().Get("X-Request-Id") != "" {
		t.Error("a 129-byte X-Request-Id was echoed")
	}
}

// TestErrorEnvelopePerDialect renders every code in each door's envelope
// with the fixed sentence, the code in the shape's code member and in
// Lux-Error, and the detail only in Lux-Error-Detail.
func TestErrorEnvelopePerDialect(t *testing.T) {
	w := newWorld(t)
	for _, d := range doors {
		for _, code := range Codes() {
			rec := httptest.NewRecorder()
			f := &failure{code: code, detail: "developer detail " + string(code)}
			w.h.ServeHTTP(rec, httptest.NewRequest("GET", "/"+string(d)+"/nowhere", nil))
			// The handler answered not_found; render the code under test
			// through the same writer to hold every code to the shape.
			rec = httptest.NewRecorder()
			rec.Header().Set(HeaderRequestID, "req_x")
			rec.Header().Set("X-Content-Type-Options", "nosniff")
			writeFailure(rec, d, "req_x", f)
			checkEnvelope(t, d, rec, code)
			if rec.Header().Get(HeaderError) != string(code) || rec.Header().Get(HeaderErrorDetail) != f.detail {
				t.Errorf("%s %s: headers %v", d, code, rec.Header())
			}
			if d != v1.DialectLux && strings.Contains(rec.Body.String(), "developer detail") {
				t.Errorf("%s %s: the detail is in the body", d, code)
			}
		}
		rec := w.do(httptest.NewRequest("GET", "/"+string(d)+"/nowhere", nil))
		checkEnvelope(t, d, rec, CodeNotFound)
	}
}

// TestStoreFailuresAreStoreUnavailable: a Key lookup, a catalog read, a
// target selection, or a reservation the store could not answer is
// store_unavailable, spec 011's code, never unauthenticated or
// model_not_found.
func TestStoreFailuresAreStoreUnavailable(t *testing.T) {
	w := newWorld(t)
	boom := errors.New("connection refused")
	w.keys.err = boom
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); errorCode(t, rec) != CodeStoreUnavailable {
		t.Errorf("keys: %s", rec.Header().Get(HeaderError))
	}
	w.keys.err = nil
	w.catalog.err = boom
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); errorCode(t, rec) != CodeStoreUnavailable {
		t.Errorf("catalog: %s", rec.Header().Get(HeaderError))
	}
	if rec := w.do(w.request("GET", "/openai/v1/models", "")); errorCode(t, rec) != CodeStoreUnavailable {
		t.Errorf("models list: %s", rec.Header().Get(HeaderError))
	}
	r := w.request("POST", "/openai/v1/files", "{}")
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	if rec := w.do(r); errorCode(t, rec) != CodeStoreUnavailable {
		t.Errorf("opaque candidates: %s", rec.Header().Get(HeaderError))
	}
	w.catalog.err = nil
	w.router.err = boom
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); errorCode(t, rec) != CodeStoreUnavailable {
		t.Errorf("router: %s", rec.Header().Get(HeaderError))
	}
	w.router.err = nil
	w.limiter.err = boom
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); errorCode(t, rec) != CodeStoreUnavailable {
		t.Errorf("limiter: %s", rec.Header().Get(HeaderError))
	}
}

// TestModelsListShapes renders GET /v1/models on each door byte-exactly
// against the golden file for the fixed catalog.
func TestModelsListShapes(t *testing.T) {
	w := newWorld(t)
	for _, d := range doors {
		r := w.request("GET", "/"+string(d)+versionPrefix(d)+"/models", "")
		rec := w.do(r)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", d, rec.Code, rec.Body.String())
		}
		golden := filepath.Join("testdata", "models", string(d)+".golden.json")
		if os.Getenv("UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(golden, rec.Body.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.Body.String(); got != string(want) {
			t.Errorf("%s list:\n got %s\nwant %s", d, got, want)
		}
		if w.recorder.last(t).Class != ClassServed {
			t.Errorf("%s: record class %s", d, w.recorder.last(t).Class)
		}
	}
	for _, s := range w.stubs {
		if s.count() != 0 {
			t.Error("a model list reached a provider")
		}
	}
	// The one entry and its refusals.
	rec := w.do(w.request("GET", "/anthropic/v1/models/gpt", ""))
	if rec.Code != 200 || rec.Body.String() != `{"type":"model","id":"gpt","display_name":"gpt","created_at":"1970-01-01T00:00:00Z"}`+"\n" {
		t.Errorf("anthropic read: %d %s", rec.Code, rec.Body.String())
	}
	rec = w.do(w.request("GET", "/gemini/v1beta/models/gpt", ""))
	if rec.Body.String() != `{"name":"models/gpt","displayName":"gpt","supportedGenerationMethods":["generateContent","countTokens"]}`+"\n" {
		t.Errorf("gemini read: %s", rec.Body.String())
	}
	rec = w.do(w.request("GET", "/lux/v1/models/gpt", ""))
	if rec.Body.String() != `{"id":"gpt","object":"model","created":0,"owned_by":"lux"}`+"\n" {
		t.Errorf("lux read: %s", rec.Body.String())
	}
	if rec := w.do(w.request("GET", "/openai/v1/models/nope", "")); errorCode(t, rec) != CodeModelNotFound {
		t.Errorf("unknown: %s", rec.Header().Get(HeaderError))
	}
	r := w.request("GET", "/openai/v1/models/claude", "")
	r.Header.Set("Authorization", "Bearer "+restrictedValue)
	if rec := w.do(r); errorCode(t, rec) != CodeModelNotAllowed {
		t.Errorf("not allowed: %s", rec.Header().Get(HeaderError))
	}
}

// TestModelsListIsTheKeysView: the list is exactly the available Models
// the Key's selectors match, and an empty list renders the shape's
// empty form.
func TestModelsListIsTheKeysView(t *testing.T) {
	w := newWorld(t)
	r := w.request("GET", "/openai/v1/models", "")
	r.Header.Set("Authorization", "Bearer "+restrictedValue)
	rec := w.do(r)
	var list openaiModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 || list.Data[0].ID != "alias" || list.Data[1].ID != "gpt" {
		t.Errorf("restricted list %+v", list.Data)
	}
	r = w.request("GET", "/anthropic/v1/models", "")
	r.Header.Set("Authorization", "Bearer "+restrictedValue)
	w.keys.byHash[hashValue(restrictedValue)].Spec.Models = []string{"nothing-matches"}
	rec = w.do(r)
	if rec.Body.String() != `{"data":[],"has_more":false,"first_id":null,"last_id":null}`+"\n" {
		t.Errorf("empty anthropic list %s", rec.Body.String())
	}
	if rec := w.do(w.request("GET", "/openai/v1/models", "")); strings.Contains(rec.Body.String(), unavailableModel) {
		t.Error("an unavailable Model is listed")
	}
}

// TestCountTokensEmulation: a count toward an anthropic target is
// forwarded and relayed; toward an openai target and on the /lux door it
// is answered from the estimate with Lux-Estimated: true, the stub sees
// nothing, and the record says ok with zero tokens.
func TestCountTokensEmulation(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondJSON(200, `{"input_tokens":123}`)
	rec := w.post("/anthropic/v1/messages/count_tokens", messagesBody("claude", false))
	if rec.Code != 200 || rec.Body.String() != `{"input_tokens":123}` || rec.Header().Get(HeaderEstimated) != "" {
		t.Errorf("forwarded count: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if got := w.anthropic.last(t); got.Path != "/v1/messages/count_tokens" {
		t.Errorf("the count reached %s", got.Path)
	}
	if record := w.recorder.last(t); record.Status != StatusOK || record.Tokens != (Tokens{}) {
		t.Errorf("record %+v", record)
	}
	res := w.limiter.last()
	if res.InputTokens != 0 || res.OutputTokens != 0 {
		t.Errorf("a count reserved tokens: %+v", res)
	}

	before := w.openai.count()
	rec = w.post("/anthropic/v1/messages/count_tokens", messagesBody("gpt", false))
	if rec.Code != 200 || rec.Header().Get(HeaderEstimated) != "true" {
		t.Fatalf("estimated count: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	var count struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil || count.InputTokens <= 0 {
		t.Errorf("estimate body %s: %v", rec.Body.String(), err)
	}
	if w.openai.count() != before {
		t.Error("an estimated count reached the provider")
	}
	if record := w.recorder.last(t); record.Status != StatusOK || record.Tokens != (Tokens{}) || record.Model != "gpt" {
		t.Errorf("record %+v", record)
	}

	// The lux door: the estimate toward openai, a re-encoded forward
	// toward anthropic, the estimate toward a lux target, and the
	// codec's refusal of an undecodable body.
	rec = w.post("/lux/v1/count_tokens", luxBody("gpt", false))
	if rec.Code != 200 || rec.Header().Get(HeaderEstimated) != "true" {
		t.Errorf("lux estimate: %d %v", rec.Code, rec.Header())
	}
	rec = w.post("/lux/v1/count_tokens", luxBody("claude", false))
	if rec.Code != 200 || rec.Body.String() != `{"input_tokens":123}` || rec.Header().Get(HeaderEstimated) != "" {
		t.Errorf("lux forwarded count: %d %s", rec.Code, rec.Body.String())
	}
	got := w.anthropic.last(t)
	if got.Path != "/v1/messages/count_tokens" || strings.Contains(string(got.Body), "max_tokens") || got.Header.Get("anthropic-version") != AnthropicVersion {
		t.Errorf("lux count reached %s with %s %v", got.Path, got.Body, got.Header)
	}
	rec = w.post("/lux/v1/count_tokens", luxBody("luxm", false))
	if rec.Code != 200 || rec.Header().Get(HeaderEstimated) != "true" {
		t.Errorf("lux toward lux: %d %v", rec.Code, rec.Header())
	}
	rec = w.post("/anthropic/v1/messages/count_tokens", `{"model":"gpt"}`)
	if errorCode(t, rec) != CodeInvalidRequest {
		t.Errorf("an undecodable count is %s", rec.Header().Get(HeaderError))
	}
	// The gemini count is a model route passthrough.
	w.gemini.respondJSON(200, `{"totalTokens":31}`)
	rec = w.post("/gemini/v1beta/models/gemini:countTokens", `{"contents":[]}`)
	if rec.Code != 200 || rec.Body.String() != `{"totalTokens":31}` || w.gemini.last(t).Path != "/v1beta/models/gemini-pro:countTokens" {
		t.Errorf("gemini count: %d %s %s", rec.Code, rec.Body.String(), w.gemini.last(t).Path)
	}
}

// TestOpaqueRoutes: route_not_allowed without passthrough, the named
// Lux-Provider with it, the sole candidate without the header, and
// provider_required with two candidates; the body and the answer stream
// through with Accept-Encoding kept.
func TestOpaqueRoutes(t *testing.T) {
	w := newWorld(t)
	w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain")
		rw.Header().Set("Set-Cookie", "a=b")
		rw.WriteHeader(201)
		_, _ = rw.Write([]byte("created " + r.URL.Path + "?" + r.URL.RawQuery))
	})
	if rec := w.post("/openai/v1/files", `{"purpose":"x"}`); errorCode(t, rec) != CodeRouteNotAllowed {
		t.Errorf("without passthrough: %s", rec.Header().Get(HeaderError))
	}
	r := w.request("POST", "/openai/v1/files?purpose=fine-tune", `{"purpose":"x"}`)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set("Accept-Encoding", "gzip")
	r.ContentLength = -1
	rec := w.do(r)
	if rec.Code != 201 || rec.Body.String() != "created /v1/files?purpose=fine-tune" || rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("sole candidate: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	got := w.openai.last(t)
	if got.Header.Get("Accept-Encoding") != "gzip" || string(got.Body) != `{"purpose":"x"}` || got.Header.Get("Authorization") != "Bearer "+openaiCredential {
		t.Errorf("opaque outbound %v %s", got.Header, got.Body)
	}
	record := w.recorder.last(t)
	if record.Class != ClassOpaque || record.Status != StatusOK || record.Provider != "oai" || record.UpstreamStatus != 201 || !record.Tokens.Estimated || record.Tokens.Input != 0 {
		t.Errorf("record %+v", record)
	}
	if res := w.limiter.last(); !res.Opaque || res.Model != nil || res.InputTokens != 0 {
		t.Errorf("reservation %+v", res)
	}

	w.provider("oai2", v1.DialectOpenAI, w.openai.URL+"/v1", "x")
	w.model("gpt2", target("oai2", "gpt-4.1"))
	r = w.request("POST", "/openai/v1/files", `{}`)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	if rec := w.do(r); errorCode(t, rec) != CodeProviderRequired {
		t.Errorf("two candidates: %s", rec.Header().Get(HeaderError))
	}
	r.Header.Set(HeaderProvider, "oai2")
	r = w.request("POST", "/openai/v1/files", `{}`)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set(HeaderProvider, "oai2")
	if rec := w.do(r); rec.Code != 201 || w.recorder.last(t).Provider != "oai2" {
		t.Errorf("named provider: %d %s", rec.Code, w.recorder.last(t).Provider)
	}
	if w.openai.last(t).Header.Get(HeaderProvider) != "" {
		t.Error("Lux-Provider reached the provider")
	}
	r = w.request("POST", "/openai/v1/files", `{}`)
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set(HeaderProvider, "ant")
	if rec := w.do(r); errorCode(t, rec) != CodeProviderRequired {
		t.Errorf("a provider of another dialect: %s", rec.Header().Get(HeaderError))
	}
	// An upstream error status on an opaque route is the caller's to read.
	w.openai.respondJSON(404, `{"error":{"message":"no such file"}}`)
	r = w.request("GET", "/openai/v1/files/f1", "")
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set(HeaderProvider, "oai")
	if rec := w.do(r); rec.Code != 404 || !strings.Contains(rec.Body.String(), "no such file") {
		t.Errorf("opaque error relay: %d %s", rec.Code, rec.Body.String())
	}
	// A refused connection on an opaque route.
	w.provider("dead", v1.DialectOpenAI, "http://127.0.0.1:1/v1", "x")
	w.model("gpt-dead", target("dead", "gpt-4.1"))
	r = w.request("GET", "/openai/v1/files/f1", "")
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set(HeaderProvider, "dead")
	if rec := w.do(r); errorCode(t, rec) != CodeProviderUnavailable {
		t.Errorf("dead provider: %s", rec.Header().Get(HeaderError))
	}
}

// TestRecordFields: the record carries the Key, the Model, the Provider,
// the dialects, the labels, the tokens, and the timings, and the metrics
// follow it.
func TestRecordFields(t *testing.T) {
	w := newWorld(t)
	reg := metrics.NewRegistry()
	o := w.options()
	o.Metrics = reg
	w.h = New(o)
	w.openai.respondJSON(200, openaiChatResponse)
	r := w.request("POST", "/openai/v1/chat/completions", chatBody("gpt", false))
	r.Header.Set(HeaderLabels, "run=r1,env=test")
	if rec := w.do(r); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec := w.recorder.last(t)
	want := Record{
		ID: rec.ID, At: w.now, EndedAt: w.now,
		KeyID: "key_ALL" + strings.Repeat("0", 23), KeyPrefix: keyValue[:12], Owner: "https://login.example.com|alice",
		Labels: map[string]string{"team": "all"}, Model: "gpt", ModelID: "mdl_GPT" + strings.Repeat("0", 23),
		Provider: "oai", ProviderID: "prv_OAI" + strings.Repeat("0", 23), UpstreamModel: "gpt-4.1",
		Door: v1.DialectOpenAI, TargetDialect: v1.DialectOpenAI, Route: "/openai/v1/chat/completions", Class: ClassTranslated,
		Attempts: []Attempt{{Provider: "oai", ProviderID: "prv_OAI" + strings.Repeat("0", 23), UpstreamModel: "gpt-4.1", Status: StatusOK, HTTPStatus: 200}},
		Status:   StatusOK, UpstreamStatus: 200,
		Tokens:        Tokens{Input: 10, Output: 3, CachedInput: 2},
		RequestLabels: map[string]string{"run": "r1", "env": "test"},
	}
	if rec.TTFB <= 0 || rec.Latency < rec.TTFB || rec.EndedAt.Before(rec.At) || rec.Attempts[0].Duration <= 0 {
		t.Errorf("timings %s %s %s", rec.TTFB, rec.Latency, rec.Attempts[0].Duration)
	}
	rec.At, rec.EndedAt, rec.Latency, rec.TTFB, rec.Attempts[0].Duration = w.now, w.now, 0, 0, 0
	want.At, want.EndedAt = w.now, w.now
	got, _ := json.Marshal(rec)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("record\n got %s\nwant %s", got, exp)
	}
	if w.limiter.lease.settled[len(w.limiter.lease.settled)-1] != want.Tokens {
		t.Errorf("settled %+v", w.limiter.lease.settled)
	}
	if reg.Counter(MetricRequests, "").Value(map[string]string{"door": "openai", "model": "gpt", "provider": "oai", "status": "ok", "code": ""}) != 1 {
		t.Error("lux_requests_total did not count the request")
	}
	if reg.Histogram(MetricRequestDuration, "", nil).Count(map[string]string{"door": "openai", "model": "gpt", "status": "ok"}) != 1 {
		t.Error("lux_request_duration_seconds did not observe the request")
	}
	// A refused request counts with its code and no ttfb.
	w.post("/openai/v1/chat/completions", chatBody("nope", false))
	if reg.Counter(MetricRequests, "").Value(map[string]string{"door": "openai", "model": "", "provider": "", "status": "refused", "code": "model_not_found"}) != 1 {
		t.Error("lux_requests_total did not count the refusal without a model label")
	}
	if reg.Histogram(MetricTimeToFirstByte, "", nil).Count(map[string]string{"door": "openai", "model": ""}) != 0 {
		t.Error("a refusal observed a time to first byte")
	}
	w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if reg.Histogram(MetricTimeToFirstByte, "", nil).Count(map[string]string{"door": "openai", "model": "gpt"}) != 2 {
		t.Error("lux_time_to_first_byte_seconds did not observe the served requests")
	}
}

// TestResponsesEstimateUsesItsOwnCodec: a /openai/v1/responses request
// whose upstream reports no usage is estimated by the Responses codec,
// not refused by the Chat codec and left to the bytes fallback.
func TestResponsesEstimateUsesItsOwnCodec(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, `{"id":"r1","object":"response","model":"gpt-4.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
	body := `{"model":"gpt","input":"hello there, a prompt long enough that the estimator and a guess of the bytes over four disagree on the count"}`
	rec := w.post("/openai/v1/responses", body)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	want, _, err := bridge.CountTokensFor(ir.DialectOpenAIResponses, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if want == int64(len(body))/4 {
		t.Fatal("the fixture cannot tell the estimator from the bytes fallback")
	}
	if got := w.recorder.last(t).Tokens; !got.Estimated || got.Input != want {
		t.Fatalf("tokens %+v, want an estimated input of %d", got, want)
	}
}
