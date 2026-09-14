// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestSameDialectSameBytes: a request through the /openai door to an
// openai target with equal names arrives byte-identical, with only the
// credential, Host, User-Agent, Lux-Request-Id, and the hop-by-hop and
// Accept-Encoding headers changed.
func TestSameDialectSameBytes(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	body := `{ "model": "gpt-4.1", "messages": [{"role":"user","content":"hi"}], "temperature": 0.5, "unknown_member": {"a": [1, 2]} }`
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", body)
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("Connection", "keep-alive, X-Hop")
	r.Header.Set("X-Hop", "dropped")
	r.Header.Set("Keep-Alive", "timeout=5")
	r.Header.Set("OpenAI-Beta", "assistants=v2")
	r.Header.Set("X-Custom", "kept")
	r.Header.Set("User-Agent", "sdk/1.0")
	rec := w.do(r)
	if rec.Code != 200 || rec.Body.String() != openaiChatResponse {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := w.openai.last(t)
	if string(got.Body) != body {
		t.Errorf("the provider received\n%s\nwant\n%s", got.Body, body)
	}
	if got.Path != "/v1/chat/completions" || got.Method != http.MethodPost {
		t.Errorf("%s %s", got.Method, got.Path)
	}
	for name, want := range map[string]string{
		"Authorization": "Bearer " + openaiCredential, "User-Agent": "luxd/test", "X-Custom": "kept", "OpenAI-Beta": "assistants=v2",
		"Content-Type": "application/json", "Accept-Encoding": "", "Connection": "", "X-Hop": "", "Keep-Alive": "",
	} {
		if got.Header.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, got.Header.Get(name), want)
		}
	}
	if got.Header.Get(HeaderRequestID) != rec.Header().Get(HeaderRequestID) {
		t.Error("Lux-Request-Id differs between the outbound request and the response")
	}
	if rec.Header().Get(HeaderLoss) != "" || rec.Header().Get("Content-Length") != "" && rec.Header().Get("Content-Length") != itoa(int64(len(openaiChatResponse))) {
		t.Errorf("response headers %v", rec.Header())
	}
	if w.limiter.last().InputTokens != int64(len(body))/4 || w.limiter.last().OutputTokens != defaultOutputTokens {
		t.Errorf("passthrough reservation %+v", w.limiter.last())
	}
}

// TestTranslationReportsLoss: a request through the /anthropic door to
// an openai target is translated, and a field the target cannot
// represent appears in Lux-Loss and in the record.
func TestTranslationReportsLoss(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	body := `{"model":"gpt","max_tokens":32,"top_k":5,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u1"}}`
	r := w.request(http.MethodPost, "/anthropic/v1/messages", body)
	r.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	rec := w.do(r)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	loss := rec.Header().Get(HeaderLoss)
	if !strings.Contains(loss, "top_k") || !strings.Contains(loss, "header.anthropic-beta") {
		t.Errorf("Lux-Loss %q", loss)
	}
	record := w.recorder.last(t)
	if strings.Join(record.Loss, ",") != loss || !record.Translated || record.TargetDialect != v1.DialectOpenAI {
		t.Errorf("record %+v", record)
	}
	got := w.openai.last(t)
	var sent map[string]any
	if err := json.Unmarshal(got.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "gpt-4.1" || sent["max_tokens"] != float64(32) || sent["top_k"] != nil || got.Header.Get("anthropic-beta") != "" || got.Path != "/v1/chat/completions" {
		t.Errorf("the provider received %s at %s with %v", got.Body, got.Path, got.Header)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "message" || out["model"] != "gpt" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("the caller received %s", rec.Body.String())
	}
	if record.Tokens != (Tokens{Input: 10, Output: 3, CachedInput: 2}) {
		t.Errorf("tokens %+v", record.Tokens)
	}
	if res := w.limiter.last(); res.InputTokens <= 0 || res.OutputTokens != 32 {
		t.Errorf("translated reservation %+v", res)
	}
}

// TestNoLossNoHeader: a translation that loses nothing carries no
// Lux-Loss, and neither does a passthrough.
func TestNoLossNoHeader(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondJSON(200, anthropicResponse)
	rec := w.post("/openai/v1/chat/completions", chatBody("claude", false))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, present := rec.Header()[HeaderLoss]; present {
		t.Errorf("Lux-Loss %q on a lossless translation", rec.Header().Get(HeaderLoss))
	}
	if len(w.recorder.last(t).Loss) != 0 {
		t.Error("the record carries a loss")
	}
}

// TestAnthropicVersionInjected: a translated request toward an anthropic
// target carries anthropic-version unless the Provider's headers name
// it; a passthrough forwards the caller's.
func TestAnthropicVersionInjected(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondJSON(200, anthropicResponse)
	if rec := w.post("/openai/v1/chat/completions", chatBody("claude", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := w.anthropic.last(t)
	if got.Header.Get("anthropic-version") != AnthropicVersion || got.Header.Get("x-api-key") != anthropicCred || got.Path != "/v1/messages" {
		t.Errorf("outbound %v at %s", got.Header, got.Path)
	}
	w.catalog.providers["ant"].Spec.Headers = map[string]string{"Anthropic-Version": "2024-01-01"}
	w.post("/openai/v1/chat/completions", chatBody("claude", false))
	if got := w.anthropic.last(t); got.Header.Get("anthropic-version") != "2024-01-01" {
		t.Errorf("the Provider's header did not win: %q", got.Header.Get("anthropic-version"))
	}
	w.catalog.providers["ant"].Spec.Headers = nil
	r := w.request(http.MethodPost, "/anthropic/v1/messages", messagesBody("claude", false))
	r.Header.Set("anthropic-version", "2023-01-01")
	w.do(r)
	if got := w.anthropic.last(t); got.Header.Get("anthropic-version") != "2023-01-01" {
		t.Errorf("passthrough did not forward the caller's header: %q", got.Header.Get("anthropic-version"))
	}
}

// TestDialectBridging is the door and target matrix: equal dialects pass
// through, a translated route bridges any two codecs, the gemini door
// and a gemini target are never bridged, a model route across dialects
// is dialect_unsupported, and a lux door reaches every dialect.
func TestDialectBridging(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	w.anthropic.respondJSON(200, anthropicResponse)
	w.gemini.respondJSON(200, geminiResponse)
	w.lux.respondJSON(200, luxResponse)
	models := map[v1.Dialect]string{v1.DialectOpenAI: "gpt", v1.DialectAnthropic: "claude", v1.DialectGemini: "gemini", v1.DialectLux: "luxm"}
	bodies := map[v1.Dialect]func(string) string{
		v1.DialectOpenAI:    func(m string) string { return chatBody(m, false) },
		v1.DialectAnthropic: func(m string) string { return messagesBody(m, false) },
		v1.DialectLux:       func(m string) string { return luxBody(m, false) },
	}
	paths := map[v1.Dialect]string{v1.DialectOpenAI: "/openai/v1/chat/completions", v1.DialectAnthropic: "/anthropic/v1/messages", v1.DialectLux: "/lux/v1/generate"}
	for door, path := range paths {
		for target, model := range models {
			rec := w.post(path, bodies[door](model))
			record := w.recorder.last(t)
			switch {
			case target == v1.DialectGemini:
				if errorCode(t, rec) != CodeDialectUnsupported {
					t.Errorf("%s -> %s: %s", door, target, rec.Header().Get(HeaderError))
				}
			case rec.Code != 200:
				t.Errorf("%s -> %s: %d %s", door, target, rec.Code, rec.Body.String())
			case record.Translated != (door != target):
				t.Errorf("%s -> %s: translated %v", door, target, record.Translated)
			}
		}
	}
	// The gemini door reaches gemini alone, and a Model with a gemini
	// target beside an openai one is served by the openai target from
	// another door.
	for target, model := range models {
		rec := w.post("/gemini/v1beta/models/"+model+":generateContent", `{"contents":[]}`)
		if target == v1.DialectGemini {
			if rec.Code != 200 {
				t.Errorf("gemini -> gemini: %d %s", rec.Code, rec.Body.String())
			}
		} else if errorCode(t, rec) != CodeDialectUnsupported {
			t.Errorf("gemini -> %s: %s", target, rec.Header().Get(HeaderError))
		}
	}
	if rec := w.post("/openai/v1/chat/completions", chatBody("multi", false)); rec.Code != 200 || w.recorder.last(t).Provider != "oai" {
		t.Errorf("a mixed Model was not served by its openai target: %d %s", rec.Code, rec.Header().Get(HeaderError))
	}
	// A model route across dialects has no codec.
	rec := w.post("/openai/v1/embeddings", `{"model":"claude","input":"x"}`)
	if errorCode(t, rec) != CodeDialectUnsupported {
		t.Errorf("embeddings across dialects: %s", rec.Header().Get(HeaderError))
	}
	w.openai.respondJSON(200, `{"object":"list","data":[],"usage":{"prompt_tokens":4,"total_tokens":4}}`)
	rec = w.post("/openai/v1/embeddings", `{"model":"gpt","input":"x"}`)
	if rec.Code != 200 || w.openai.last(t).Path != "/v1/embeddings" || w.recorder.last(t).Tokens != (Tokens{Input: 4}) {
		t.Errorf("embeddings passthrough: %d %s %+v", rec.Code, w.openai.last(t).Path, w.recorder.last(t).Tokens)
	}
	// The responses route is a passthrough toward openai and a
	// translation toward anthropic on /responses' own shape.
	w.openai.respondJSON(200, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-4.1","output":[],"usage":{"input_tokens":3,"output_tokens":1}}`)
	rec = w.post("/openai/v1/responses", `{"model":"gpt","input":"hi"}`)
	if rec.Code != 200 || w.openai.last(t).Path != "/v1/responses" {
		t.Errorf("responses passthrough: %d %s", rec.Code, w.openai.last(t).Path)
	}
	rec = w.post("/openai/v1/responses", `{"model":"claude","input":"hi"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Errorf("responses translated: %d %s", rec.Code, rec.Body.String())
	}
}

// TestOpenAITargetRoute: a translated request toward an openai target in
// the reasoning family arrives on /responses with max_completion_tokens,
// and one toward any other name on /chat/completions with max_tokens; a
// passthrough arrives on the route it was sent to whatever the name.
func TestOpenAITargetRoute(t *testing.T) {
	for name, want := range map[string]bool{"gpt-5": true, "o3-mini": true, "GPT-6-turbo": true, "gpt-10/x": true, "o1": true, "gpt-4.1": false, "llama3.1": false, "o-ring": false, "gpt-4o": false, "gpt-": false, "gpt-x": false} {
		if OpenAIReasoningFamily(name) != want {
			t.Errorf("OpenAIReasoningFamily(%q) = %v", name, !want)
		}
	}
	w := newWorld(t)
	w.openai.respondJSON(200, `{"id":"resp_1","object":"response","status":"completed","model":"o3-mini","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`)
	rec := w.post("/anthropic/v1/messages", messagesBody(openaiReasonerName, false))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := w.openai.last(t)
	if got.Path != "/v1/responses" || !strings.Contains(string(got.Body), `"max_output_tokens":64`) {
		t.Errorf("reasoning family reached %s with %s", got.Path, got.Body)
	}
	w.openai.respondJSON(200, openaiChatResponse)
	w.post("/anthropic/v1/messages", messagesBody("gpt", false))
	if got := w.openai.last(t); got.Path != "/v1/chat/completions" || !strings.Contains(string(got.Body), `"max_tokens":64`) {
		t.Errorf("gpt-4.1 reached %s with %s", got.Path, got.Body)
	}
	w.post("/openai/v1/chat/completions", chatBody(openaiReasonerName, false))
	if got := w.openai.last(t); got.Path != "/v1/chat/completions" {
		t.Errorf("a passthrough moved to %s", got.Path)
	}
}

// TestUpstreamStatusMapping: the last attempt's 400, 404, and 422 are
// upstream_rejected; its 401, 403, 302, and 529 are upstream_error; a
// refused connection is provider_unavailable; a timeout is
// upstream_timeout; each carries the upstream status and body excerpt in
// Lux-Error-Detail and never in the body.
func TestUpstreamStatusMapping(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		status int
		code   Code
	}{
		{400, CodeUpstreamRejected}, {404, CodeUpstreamRejected}, {422, CodeUpstreamRejected},
		{401, CodeUpstreamError}, {403, CodeUpstreamError}, {302, CodeUpstreamError}, {529, CodeUpstreamError},
		{500, CodeUpstreamError}, {429, CodeUpstreamError}, {408, CodeUpstreamError},
	}
	for _, c := range cases {
		w.openai.respond(func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Content-Type", "application/json")
			if c.status == 302 {
				rw.Header().Set("Location", w.openai.URL+"/elsewhere")
			}
			rw.WriteHeader(c.status)
			_, _ = io.WriteString(rw, `{"error":{"message":"upstream said no to gpt-4.1"}}`)
		})
		rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false))
		if code := errorCode(t, rec); code != c.code {
			t.Errorf("%d: %s, want %s", c.status, code, c.code)
		}
		detail := rec.Header().Get(HeaderErrorDetail)
		if !strings.Contains(detail, "upstream status "+itoa(int64(c.status))) || !strings.Contains(detail, "upstream said no") {
			t.Errorf("%d: detail %q", c.status, detail)
		}
		if strings.Contains(rec.Body.String(), "upstream said no") || strings.Contains(rec.Body.String(), "gpt-4.1") {
			t.Errorf("%d: the upstream body reached the caller: %s", c.status, rec.Body.String())
		}
		record := w.recorder.last(t)
		if record.Status != StatusFailed || record.Error != c.code || record.UpstreamStatus != c.status || len(record.Attempts) != 1 || record.Attempts[0].HTTPStatus != c.status {
			t.Errorf("%d: record %+v", c.status, record)
		}
	}
	// A refused connection and a timeout.
	w.provider("dead", v1.DialectOpenAI, "http://127.0.0.1:1/v1", "x")
	w.model("gpt-dead", target("dead", "gpt-4.1"))
	rec := w.post("/openai/v1/chat/completions", chatBody("gpt-dead", false))
	if errorCode(t, rec) != CodeProviderUnavailable || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "dead") {
		t.Errorf("refused connection: %s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	slow := w.provider("slow", v1.DialectOpenAI, w.openai.URL+"/v1", "x")
	slow.Spec.Timeout = "50ms"
	w.model("gpt-slow", target("slow", "gpt-4.1"))
	w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt-slow", false))
	if errorCode(t, rec) != CodeUpstreamTimeout {
		t.Errorf("timeout: %s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	observed := w.health.observed[slow.Status.ID]
	if len(observed) != 1 || !observed[0] {
		t.Errorf("health observed %v for a timeout", observed)
	}
	if _, failures := w.router.counts("slow/gpt-4.1"); failures != 1 {
		t.Errorf("the circuit saw %d failures for a timeout", failures)
	}
}

// TestUpstreamBodyIsDetailOnly holds an upstream error body of every
// shape out of the caller's body on every door, and bounds the detail.
func TestUpstreamBodyIsDetailOnly(t *testing.T) {
	w := newWorld(t)
	long := strings.Repeat("secret-upstream-text ", 200)
	w.openai.respondJSON(500, long)
	for _, c := range []struct{ path, body string }{
		{"/openai/v1/chat/completions", chatBody("gpt", false)},
		{"/anthropic/v1/messages", messagesBody("gpt", false)},
		{"/lux/v1/generate", luxBody("gpt", false)},
	} {
		rec := w.post(c.path, c.body)
		// The lux door carries the developer detail in details.detail by
		// design; the other doors' bodies never carry it.
		if errorCode(t, rec) != CodeUpstreamError || (c.path != "/lux/v1/generate" && strings.Contains(rec.Body.String(), "secret-upstream")) {
			t.Errorf("%s: %s %s", c.path, rec.Header().Get(HeaderError), rec.Body.String())
		}
		if d := rec.Header().Get(HeaderErrorDetail); !strings.Contains(d, "secret-upstream") || len(d) > maxDetailBytes {
			t.Errorf("%s: detail %d bytes", c.path, len(d))
		}
	}
}

// TestFallbackWalksTheOrder: a retryable failure before any response
// byte hands the request to the next target, every attempt is recorded,
// the circuit and the health observer see each outcome, fallback: never
// stops at the first attempt, and a circuit that refuses the probe is
// skipped.
func TestFallbackWalksTheOrder(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(503, `{"error":"down"}`)
	w.anthropic.respondJSON(200, anthropicResponse)
	rec := w.post("/openai/v1/chat/completions", chatBody("dual", false))
	if rec.Code != 200 {
		t.Fatalf("%d %s %s", rec.Code, rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	record := w.recorder.last(t)
	if len(record.Attempts) != 2 || record.Attempts[0].Status != StatusFailed || record.Attempts[0].Error != CodeUpstreamError || record.Attempts[0].HTTPStatus != 503 || record.Attempts[1].Status != StatusOK || record.Provider != "ant" || !record.Translated {
		t.Errorf("attempts %+v provider %s", record.Attempts, record.Provider)
	}
	if _, f := w.router.counts("oai/gpt-4.1"); f != 1 {
		t.Errorf("circuit failures for the 503: %d", f)
	}
	if s, _ := w.router.counts("ant/claude-3"); s != 1 {
		t.Errorf("circuit successes for the 200: %d", s)
	}
	oai := w.catalog.providers["oai"].Status.ID
	if got := w.health.observed[oai]; len(got) != 1 || !got[0] {
		t.Errorf("health observed %v for a 503", got)
	}
	// A 429 is retried but is a complete answer for health.
	w.openai.respondJSON(429, `{}`)
	w.post("/openai/v1/chat/completions", chatBody("dual", false))
	if got := w.health.observed[oai]; got[len(got)-1] {
		t.Error("a 429 counted as a health failure")
	}
	// fallback: never fails on the first attempt.
	w.openai.respondJSON(503, `{}`)
	rec = w.post("/openai/v1/chat/completions", chatBody("never", false))
	if errorCode(t, rec) != CodeUpstreamError || len(w.recorder.last(t).Attempts) != 1 {
		t.Errorf("fallback never: %s, %d attempts", rec.Header().Get(HeaderError), len(w.recorder.last(t).Attempts))
	}
	// A non-retryable 4xx is final whatever follows.
	w.openai.respondJSON(400, `{}`)
	rec = w.post("/openai/v1/chat/completions", chatBody("dual", false))
	if errorCode(t, rec) != CodeUpstreamRejected || len(w.recorder.last(t).Attempts) != 1 {
		t.Errorf("a 400 was retried: %s", rec.Header().Get(HeaderError))
	}
	// The probe slot taken skips the target; every target skipped is
	// provider_unavailable without a dial.
	w.openai.respondJSON(200, openaiChatResponse)
	w.router.deny["oai/gpt-4.1"] = true
	rec = w.post("/openai/v1/chat/completions", chatBody("dual", false))
	if rec.Code != 200 || w.recorder.last(t).Provider != "ant" {
		t.Errorf("the denied target was not skipped: %d %s", rec.Code, w.recorder.last(t).Provider)
	}
	before := w.openai.count()
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if errorCode(t, rec) != CodeProviderUnavailable || w.openai.count() != before {
		t.Errorf("all denied: %s, dials %d", rec.Header().Get(HeaderError), w.openai.count()-before)
	}
	// An excluded order is provider_unavailable before any dial, and the
	// record is a refusal.
	w.router.deny = map[string]bool{}
	w.router.exclude["oai/gpt-4.1"] = true
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if errorCode(t, rec) != CodeProviderUnavailable || w.recorder.last(t).Status != StatusRefused {
		t.Errorf("no target: %s %s", rec.Header().Get(HeaderError), w.recorder.last(t).Status)
	}
	// A client or a credential the importer cannot supply moves on to
	// the next target and is provider_unavailable on the last.
	w.router.exclude = map[string]bool{}
	w.clients.err = errors.New("no client")
	rec = w.post("/openai/v1/chat/completions", chatBody("dual", false))
	if errorCode(t, rec) != CodeProviderUnavailable || len(w.recorder.last(t).Attempts) != 2 {
		t.Errorf("client failure: %s %d", rec.Header().Get(HeaderError), len(w.recorder.last(t).Attempts))
	}
	w.clients.err = nil
	w.creds.err = errors.New("kek missing")
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if errorCode(t, rec) != CodeProviderUnavailable || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "credential") {
		t.Errorf("credential failure: %s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
}

// TestModelNameRewrite: the body's model name is rewritten to the
// upstream name on the way out and the Model's name comes back, on a
// passthrough with differing names, on a translation, and on the gemini
// path.
func TestModelNameRewrite(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, strings.Replace(openaiChatResponse, `"model":"gpt-4.1"`, `"model":"gpt-4.1-mini"`, 1))
	body := `{"model":"alias","messages":[{"role":"user","content":"hi"}]}`
	rec := w.post("/openai/v1/chat/completions", body)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := string(w.openai.last(t).Body); got != `{"model":"gpt-4.1-mini","messages":[{"role":"user","content":"hi"}]}` {
		t.Errorf("outbound %s", got)
	}
	if !strings.Contains(rec.Body.String(), `"model":"alias"`) || strings.Contains(rec.Body.String(), "gpt-4.1-mini") {
		t.Errorf("response %s", rec.Body.String())
	}
	// Translated: the encode writes the upstream name and the Model's
	// name comes back.
	w.anthropic.respondJSON(200, anthropicResponse)
	rec = w.post("/openai/v1/chat/completions", chatBody("claude", false))
	if !strings.Contains(string(w.anthropic.last(t).Body), `"model":"claude-3"`) || !strings.Contains(rec.Body.String(), `"model":"claude"`) {
		t.Errorf("translated: out %s back %s", w.anthropic.last(t).Body, rec.Body.String())
	}
	// Gemini: the upstream name is in the path and the response is not
	// touched.
	w.gemini.respondJSON(200, geminiResponse)
	rec = w.post("/gemini/v1beta/models/gemini:generateContent?alt=json", `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	if rec.Code != 200 || rec.Body.String() != geminiResponse {
		t.Errorf("gemini: %d %s", rec.Code, rec.Body.String())
	}
	if got := w.gemini.last(t); got.Path != "/v1beta/models/gemini-pro:generateContent" || got.Query != "alt=json" || got.Header.Get("x-goog-api-key") != geminiCredential {
		t.Errorf("gemini outbound %s?%s %v", got.Path, got.Query, got.Header)
	}
	if w.recorder.last(t).Tokens != (Tokens{Input: 6, Output: 2, CachedInput: 3}) {
		t.Errorf("gemini tokens %+v", w.recorder.last(t).Tokens)
	}
}

// TestIncludeUsageInjected: a streamed /openai chat completion toward an
// openai target carries stream_options.include_usage: true upstream and
// the usage chunk reaches the caller; a non-streamed one is
// byte-identical; the record's tokens are the chunk's.
func TestIncludeUsageInjected(t *testing.T) {
	w := newWorld(t)
	w.openai.respondSSE(
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4.1\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\n",
		"data: [DONE]\n\n",
	)
	body := `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := w.post("/openai/v1/chat/completions", body)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var sent map[string]any
	if err := json.Unmarshal(w.openai.last(t).Body, &sent); err != nil {
		t.Fatal(err)
	}
	if so, _ := sent["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("outbound %s", w.openai.last(t).Body)
	}
	if !strings.Contains(rec.Body.String(), `"usage":{"prompt_tokens":8`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("the usage chunk did not reach the caller: %s", rec.Body.String())
	}
	if w.recorder.last(t).Tokens != (Tokens{Input: 8, Output: 2}) || !w.recorder.last(t).Stream {
		t.Errorf("record %+v", w.recorder.last(t))
	}
	w.openai.respondJSON(200, openaiChatResponse)
	plain := `{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`
	w.post("/openai/v1/chat/completions", plain)
	if got := string(w.openai.last(t).Body); got != plain {
		t.Errorf("a non-streamed body was edited: %s", got)
	}
	// A stream toward an anthropic target is translated, and the codec
	// asks for the usage itself; no edit of the caller's body applies.
	w.anthropic.respondJSON(200, anthropicResponse)
	w.post("/openai/v1/chat/completions", `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(string(w.anthropic.last(t).Body), "stream_options") {
		t.Error("stream_options reached an anthropic target")
	}
}

// TestNoHTMLIsEverServed: an upstream text/html response reaches the
// caller as application/octet-stream, and every door response carries
// X-Content-Type-Options: nosniff.
func TestNoHTMLIsEverServed(t *testing.T) {
	w := newWorld(t)
	w.openai.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Header().Set("Set-Cookie", "session=x")
		rw.Header().Set("Content-Encoding", "identity")
		rw.Header().Set("X-Upstream", "kept")
		_, _ = io.WriteString(rw, "<html>login</html>")
	})
	rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("X-Upstream") != "kept" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
	if rec.Body.String() != "<html>login</html>" {
		t.Errorf("body %s", rec.Body.String())
	}
	if !w.recorder.last(t).Tokens.Estimated {
		t.Error("an unreadable body did not yield an estimate")
	}
	for _, path := range []string{"/openai/v1/chat/completions", "/anthropic/nowhere", "/gemini/v1beta/models", "/lux/v1/models", "/elsewhere"} {
		rec := w.do(w.request("GET", path, ""))
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
	// An HTML stream, too.
	w.openai.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(rw, "<html>stream</html>")
	})
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt", true))
	if rec.Header().Get("Content-Type") != "application/octet-stream" {
		t.Errorf("stream %v", rec.Header())
	}
}

// TestHotPathDialsNoWebhook: during one thousand requests the handler
// reaches nothing but the stub provider. The stub authorizer and issuer
// below count zero calls because the handler has no option that could
// name them; every importer-supplied interface counts its calls, and the
// client's transport refuses any host that is not a provider's.
func TestHotPathDialsNoWebhook(t *testing.T) {
	w := newWorld(t)
	authorizer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the authorizer was called") }))
	defer authorizer.Close()
	issuer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the issuer was called") }))
	defer issuer.Close()
	w.openai.respondJSON(200, openaiChatResponse)
	const n = 1000
	for range n {
		if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	if got := w.clients.transport.calls.Load(); got != n {
		t.Errorf("%d outbound requests for %d calls", got, n)
	}
	if w.clients.transport.other.Load() != 0 {
		t.Error("the handler dialed a host that is no provider")
	}
	if w.openai.count() != n || w.keys.calls.Load() != n || w.recorder.count() != n {
		t.Errorf("provider %d, key lookups %d, records %d", w.openai.count(), w.keys.calls.Load(), w.recorder.count())
	}
}

// TestDataPlaneServesWhileAuthorizerIsDown is the same proof as
// TestHotPathDialsNoWebhook from the other side: with the authorizer's
// address unreachable, a request with a valid Key is served, because
// the handler asks no authorizer and has no field that could name one.
func TestDataPlaneServesWhileAuthorizerIsDown(t *testing.T) {
	w := newWorld(t)
	authorizer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	authorizer.Close() // down before the first request
	w.openai.respondJSON(200, openaiChatResponse)
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != 200 {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	if w.clients.transport.other.Load() != 0 {
		t.Error("the handler dialed something that is no provider")
	}
}

// TestUpstreamBodyCap: a non-streaming upstream body one byte over the
// cap is upstream_error naming the cap; a stream of twice that size is
// relayed whole.
func TestUpstreamBodyCap(t *testing.T) {
	w := newWorld(t)
	over := strings.Repeat("x", int(w.h.maxBody)+1)
	w.openai.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, over)
	})
	rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if errorCode(t, rec) != CodeUpstreamError || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "cap") {
		t.Errorf("%s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	big := strings.Repeat("y", int(w.h.maxBody)*2)
	w.openai.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(rw, "data: "+big+"\n\n")
	})
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt", true))
	if rec.Code != 200 || rec.Body.Len() != len(big)+8 {
		t.Errorf("stream relay: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

// TestTranslationResponseFailures: an upstream 2xx body the target's
// codec cannot decode, and a decoded response the door's codec cannot
// encode, are upstream_error with the codec's message in the detail.
func TestTranslationResponseFailures(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondJSON(200, `not json`)
	rec := w.post("/openai/v1/chat/completions", chatBody("claude", false))
	if errorCode(t, rec) != CodeUpstreamError || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "decoding") {
		t.Errorf("%s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	if w.recorder.last(t).Status != StatusFailed {
		t.Error("a failed decode is not a failed record")
	}
	// An image block in a response is nothing the openai door can encode.
	w.anthropic.respondJSON(200, `{"id":"m","type":"message","role":"assistant","model":"claude-3","content":[{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	rec = w.post("/openai/v1/chat/completions", chatBody("claude", false))
	if errorCode(t, rec) != CodeUpstreamError || !strings.Contains(rec.Header().Get(HeaderErrorDetail), "encoding") {
		t.Errorf("%s %s", rec.Header().Get(HeaderError), rec.Header().Get(HeaderErrorDetail))
	}
	// An encode refusal on the request leg is invalid_request and is not
	// retried on the second target.
	w.openai.respondJSON(200, openaiChatResponse)
	rec = w.post("/anthropic/v1/messages", `{"model":"dual","max_tokens":5,"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}]}`)
	if errorCode(t, rec) != CodeInvalidRequest || len(w.recorder.last(t).Attempts) != 1 {
		t.Errorf("encode refusal: %s, %d attempts", rec.Header().Get(HeaderError), len(w.recorder.last(t).Attempts))
	}
	if s, f := w.router.counts("oai/gpt-4.1"); s != 0 || f != 0 {
		t.Error("an encode refusal reached the circuit")
	}
}

// TestProviderHeadersAndCredentialSchemes: the Provider's static headers
// are added, the credential header wins over a static header and over
// the caller's copy, a raw scheme on a custom header carries the bare
// value, and a Provider without a credential gets no header.
func TestProviderHeadersAndCredentialSchemes(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	p := w.catalog.providers["oai"]
	p.Spec.Headers = map[string]string{"X-Static": "yes", "Authorization": "Bearer static-loses"}
	r := w.request("POST", "/openai/v1/chat/completions", chatBody("gpt", false))
	r.Header.Set("Authorization", "Bearer "+keyValue)
	w.do(r)
	got := w.openai.last(t)
	if got.Header.Get("X-Static") != "yes" || got.Header.Get("Authorization") != "Bearer "+openaiCredential {
		t.Errorf("%v", got.Header)
	}
	p.Spec.Headers = nil
	p.Spec.Credential = &v1.Credential{Header: "api-key", Scheme: v1.SchemeRaw}
	w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if got := w.openai.last(t); got.Header.Get("api-key") != openaiCredential || got.Header.Get("Authorization") != "" {
		t.Errorf("raw scheme: %v", got.Header)
	}
	p.Spec.Credential = &v1.Credential{}
	w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if got := w.openai.last(t); got.Header.Get("Authorization") != "Bearer "+openaiCredential {
		t.Errorf("dialect defaults: %v", got.Header)
	}
	p.Spec.Credential = nil
	w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if got := w.openai.last(t); got.Header.Get("Authorization") != "" {
		t.Errorf("no credential: %v", got.Header)
	}
	p.Spec.Credential = &v1.Credential{}
	w.creds.values[p.Status.ID] = ""
	w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if got := w.openai.last(t); got.Header.Get("Authorization") != "" {
		t.Errorf("empty credential: %v", got.Header)
	}
	// A Provider whose base URL cannot be parsed is provider_unavailable.
	p.Spec.BaseURL = "http://[::1"
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); errorCode(t, rec) != CodeProviderUnavailable {
		t.Errorf("bad base URL: %s", rec.Header().Get(HeaderError))
	}
	r = w.request("POST", "/openai/v1/files", "{}")
	r.Header.Set("Authorization", "Bearer "+passthroughValue)
	r.Header.Set(HeaderProvider, "oai")
	if rec := w.do(r); errorCode(t, rec) != CodeProviderUnavailable {
		t.Errorf("bad base URL on an opaque route: %s", rec.Header().Get(HeaderError))
	}
}

// TestHopByHopHeadersAreRemovedBothWays: the fixed hop-by-hop set and
// every header a Connection header names leave the request toward the
// provider and the response toward the caller, and a header named by
// neither is kept in both directions.
func TestHopByHopHeadersAreRemovedBothWays(t *testing.T) {
	src := func() http.Header {
		h := http.Header{}
		h.Set("Connection", "X-Hop, x-other")
		h.Set("X-Hop", "1")
		h.Set("X-Other", "2")
		h.Set("Keep-Alive", "timeout=5")
		h.Set("Transfer-Encoding", "chunked")
		h.Set("Proxy-Connection", "keep-alive")
		h.Set("X-Kept", "yes")
		return h
	}
	request := src()
	removeHopByHop(request)
	response := http.Header{}
	relayHeaders(response, src(), false)
	for _, tc := range []struct {
		direction string
		h         http.Header
	}{{"request", request}, {"response", response}} {
		for _, gone := range []string{"Connection", "X-Hop", "X-Other", "Keep-Alive", "Transfer-Encoding", "Proxy-Connection"} {
			if tc.h.Get(gone) != "" {
				t.Errorf("%s: %s survived", tc.direction, gone)
			}
		}
		if tc.h.Get("X-Kept") != "yes" {
			t.Errorf("%s: X-Kept was removed", tc.direction)
		}
	}
}
