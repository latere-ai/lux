// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The canned streams.
const (
	anthropicStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":1,\"cache_read_input_tokens\":2}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	geminiArray = "[{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"h\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":1}}\n,\n{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"i\"}]}}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"cachedContentTokenCount\":1}}]"
)

// TestStreamingPassthrough: a streamed passthrough relays events as they
// arrive with the upstream's Content-Type kept, a :streamGenerateContent
// JSON array included, and the record's tokens are the last value of
// each usage member.
func TestStreamingPassthrough(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	// The Model claude names the upstream claude-3, so the frames come
	// back with the Model's name and every other byte as sent.
	rec := w.post("/anthropic/v1/messages", messagesBody("claude", true))
	if want := strings.Replace(anthropicStream, `"model":"claude-3"`, `"model":"claude"`, 1); rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" || rec.Body.String() != want {
		t.Errorf("%d %v\n%s", rec.Code, rec.Header(), rec.Body.String())
	}
	// Equal names relay the bytes as read.
	w.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	if rec := w.post("/anthropic/v1/messages", messagesBody("claude-3", true)); rec.Body.String() != anthropicStream {
		t.Errorf("equal names: %s", rec.Body.String())
	}
	if !rec.Flushed {
		t.Error("the stream was not flushed")
	}
	if got := w.recorder.last(t).Tokens; got != (Tokens{Input: 11, Output: 6, CachedInput: 2}) {
		t.Errorf("tokens %+v", got)
	}
	// The gemini array, without alt=sse, keeps application/json.
	w.gemini.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rc := http.NewResponseController(rw)
		for _, part := range strings.SplitAfter(geminiArray, "\n") {
			_, _ = io.WriteString(rw, part)
			_ = rc.Flush()
		}
	})
	rec = w.post("/gemini/v1beta/models/gemini:streamGenerateContent", `{"contents":[]}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != geminiArray {
		t.Errorf("gemini array: %d %v\n%s", rec.Code, rec.Header(), rec.Body.String())
	}
	if got := w.recorder.last(t); got.Tokens != (Tokens{Input: 4, Output: 2, CachedInput: 1}) || !got.Stream {
		t.Errorf("gemini record %+v", got)
	}
	// The same array as SSE with alt=sse.
	w.gemini.respondSSE("data: {\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":1}}\n\n", "data: {\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":3}}\n\n")
	rec = w.post("/gemini/v1beta/models/gemini:streamGenerateContent?alt=sse", `{"contents":[]}`)
	if rec.Header().Get("Content-Type") != "text/event-stream" || w.recorder.last(t).Tokens != (Tokens{Input: 5, Output: 3}) || w.gemini.last(t).Query != "alt=sse" {
		t.Errorf("gemini sse: %v %+v", rec.Header(), w.recorder.last(t).Tokens)
	}
	// A stream with no usage yields an estimate.
	w.openai.respondSSE("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", "data: [DONE]\n\n")
	w.post("/openai/v1/chat/completions", chatBody("gpt", true))
	if got := w.recorder.last(t).Tokens; !got.Estimated || got.Input == 0 {
		t.Errorf("no usage: %+v", got)
	}
	// A passthrough stream with differing names is relayed frame by frame
	// with the Model's name written back.
	w.openai.respondSSE(
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
		": keep-alive\n\n",
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n",
	)
	rec = w.post("/openai/v1/chat/completions", `{"model":"alias","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	want := "data: {\"id\":\"c1\",\"model\":\"alias\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n: keep-alive\n\ndata: {\"id\":\"c1\",\"model\":\"alias\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
	if rec.Body.String() != want {
		t.Errorf("rewritten stream\n got %q\nwant %q", rec.Body.String(), want)
	}
	if w.recorder.last(t).Tokens != (Tokens{Input: 3, Output: 1}) {
		t.Errorf("rewritten tokens %+v", w.recorder.last(t).Tokens)
	}
	// The same on the anthropic door rewrites message.model.
	w.model("claude-alias", target("ant", "claude-3"))
	w.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	rec = w.post("/anthropic/v1/messages", messagesBody("claude-alias", true))
	if !strings.Contains(rec.Body.String(), `"model":"claude-alias"`) || strings.Contains(rec.Body.String(), "claude-3") {
		t.Errorf("anthropic rewrite: %s", rec.Body.String())
	}
}

// TestStreamingTranslation: a streamed translation re-encodes each event
// in the door's dialect with headers flushed first, and a JSON answer to
// a stream request is re-emitted as events.
func TestStreamingTranslation(t *testing.T) {
	w := newWorld(t)
	w.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	rec := w.post("/openai/v1/chat/completions", chatBody("claude", true))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.Contains(body, `"model":"claude"`) || !strings.Contains(body, `"content":"hi"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("translated stream %s", body)
	}
	if strings.Contains(body, "claude-3") {
		t.Error("the upstream name reached the caller")
	}
	if got := w.recorder.last(t); got.Tokens != (Tokens{Input: 11, Output: 6, CachedInput: 2}) || !got.Translated || !got.Stream {
		t.Errorf("record %+v", got)
	}
	// The outbound request asked for a stream in the target's dialect.
	if !strings.Contains(string(w.anthropic.last(t).Body), `"stream":true`) {
		t.Errorf("outbound %s", w.anthropic.last(t).Body)
	}
	// A JSON answer to a stream request becomes the door's events, on
	// the lux door with a tool use and thinking in the answer.
	w.anthropic.respondJSON(200, `{"id":"m2","type":"message","role":"assistant","model":"claude-3","content":[{"type":"thinking","thinking":"hm","signature":"sig"},{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`)
	rec = w.post("/lux/v1/generate", luxBody("claude", true))
	body = rec.Body.String()
	for _, want := range []string{"event: message_start", `"model":"claude"`, "event: thinking_delta", "event: signature_delta", "event: text_delta", "event: args_delta", `"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("lux events lack %q:\n%s", want, body)
		}
	}
	if w.recorder.last(t).Tokens != (Tokens{Input: 2, Output: 3}) {
		t.Errorf("tokens %+v", w.recorder.last(t).Tokens)
	}
	// Toward an openai target, the anthropic door's stream.
	w.openai.respondSSE(
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1}}\n\n",
		"data: [DONE]\n\n",
	)
	rec = w.post("/anthropic/v1/messages", messagesBody("gpt", true))
	if !strings.HasPrefix(rec.Body.String(), "event: message_start") || !strings.Contains(rec.Body.String(), `"model":"gpt"`) || w.recorder.last(t).Tokens != (Tokens{Input: 9, Output: 1}) {
		t.Errorf("anthropic door stream: %s %+v", rec.Body.String(), w.recorder.last(t).Tokens)
	}
}

// TestStreamUsageIsTheLastValue: input on message_start and output on
// message_delta, and a later chunk's usage over an earlier one's, each
// member's last value winning, on a passthrough and on a translation.
func TestStreamUsageIsTheLastValue(t *testing.T) {
	w := newWorld(t)
	w.openai.respondSSE(
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":50}}}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7}}\n\n",
		"data: [DONE]\n\n",
	)
	w.post("/openai/v1/chat/completions", chatBody("gpt", true))
	if got := w.recorder.last(t).Tokens; got != (Tokens{Input: 50, Output: 7, CachedInput: 50}) {
		t.Errorf("passthrough %+v", got)
	}
	w.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	w.post("/lux/v1/generate", luxBody("claude", true))
	if got := w.recorder.last(t).Tokens; got != (Tokens{Input: 11, Output: 6, CachedInput: 2}) {
		t.Errorf("translation %+v", got)
	}
	// The lux dialect's own stream on a passthrough.
	w.lux.respondSSE(
		"event: message_start\ndata: {\"type\":\"message_start\",\"id\":\"l1\",\"model\":\"luxm\",\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"stop_reason\":\"end_turn\",\"usage\":{\"input_tokens\":4,\"output_tokens\":9,\"reasoning_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)
	w.post("/lux/v1/generate", luxBody("luxm", true))
	if got := w.recorder.last(t).Tokens; got != (Tokens{Input: 4, Output: 9, Reasoning: 2}) {
		t.Errorf("lux passthrough %+v", got)
	}
}

// TestClientDisconnectCancelsUpstream: a caller disconnect cancels the
// upstream request within 100 ms and the record says client_closed with
// the tokens counted so far.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	w := newWorld(t)
	cancelled := make(chan time.Duration, 1)
	w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(200)
		rc := http.NewResponseController(rw)
		_, _ = io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n")
		_ = rc.Flush()
		start := time.Now()
		select {
		case <-r.Context().Done():
			cancelled <- time.Since(start)
		case <-time.After(5 * time.Second):
			cancelled <- 5 * time.Second
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", true)).WithContext(ctx)
	pr, pw := io.Pipe()
	rec := &pipeRecorder{ResponseRecorder: httptest.NewRecorder(), w: pw}
	done := make(chan struct{})
	go func() {
		w.h.ServeHTTP(rec, r)
		close(done)
	}()
	buf := make([]byte, 1024)
	if _, err := pr.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case d := <-cancelled:
		if d > time.Second {
			t.Errorf("the upstream was cancelled after %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream was never cancelled")
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	<-done
	got := w.recorder.last(t)
	if got.Status != StatusFailed || got.Error != ClientClosed || got.Tokens != (Tokens{Input: 3, Output: 1}) {
		t.Errorf("record %+v", got)
	}
	if got.UpstreamStatus != 200 || len(got.Attempts) != 1 || got.Attempts[0].Error != ClientClosed {
		t.Errorf("attempts %+v", got.Attempts)
	}
	// A disconnect before the upstream answers is client_closed too, and
	// nothing is written.
	w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	ctx, cancel = context.WithCancel(context.Background())
	r = w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)).WithContext(ctx)
	plain := httptest.NewRecorder()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	w.h.ServeHTTP(plain, r)
	if got := w.recorder.last(t); got.Error != ClientClosed || plain.Body.Len() != 0 {
		t.Errorf("early disconnect: %+v body %q", got, plain.Body.String())
	}
}

// pipeRecorder streams what the handler writes into a pipe, so a test can
// read the first bytes while the handler is still writing.
type pipeRecorder struct {
	*httptest.ResponseRecorder
	w *io.PipeWriter
}

func (p *pipeRecorder) Write(b []byte) (int, error) {
	n, err := p.ResponseRecorder.Write(b)
	_, _ = p.w.Write(b)
	return n, err
}

// TestNoRetryAfterFirstByte: an upstream failure after the first byte is
// not retried on another target, the record says failed with
// upstream_error, and the tokens counted to the cut are kept.
func TestNoRetryAfterFirstByte(t *testing.T) {
	w := newWorld(t)
	w.openai.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.Header().Set("Content-Length", "1000")
		rw.WriteHeader(200)
		_, _ = io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n")
		http.NewResponseController(rw).Flush()
		panic(http.ErrAbortHandler)
	})
	w.anthropic.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...)
	rec := w.post("/openai/v1/chat/completions", chatBody("dual", true))
	if rec.Code != 200 || w.anthropic.count() != 0 {
		t.Errorf("%d, anthropic dials %d", rec.Code, w.anthropic.count())
	}
	got := w.recorder.last(t)
	if got.Status != StatusFailed || got.Error != CodeUpstreamError || len(got.Attempts) != 1 || got.Tokens != (Tokens{Input: 2, Output: 1}) {
		t.Errorf("record %+v", got)
	}
	if !strings.Contains(rec.Body.String(), "data: {\"error\":") || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("no error frame: %s", rec.Body.String())
	}
}

// TestStreamErrorFramePerDoor: a stream that fails after its first byte
// ends with the door's one error frame, and with nothing of the
// gateway's on the /gemini door.
func TestStreamErrorFramePerDoor(t *testing.T) {
	w := newWorld(t)
	cut := func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.Header().Set("Content-Length", "1000")
		rw.WriteHeader(200)
		_, _ = io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		http.NewResponseController(rw).Flush()
		panic(http.ErrAbortHandler)
	}
	for _, s := range w.stubs {
		s.respond(cut)
	}
	cases := []struct {
		door  v1.Dialect
		path  string
		body  string
		frame string
	}{
		{v1.DialectOpenAI, "/openai/v1/chat/completions", chatBody("gpt", true), "\n\ndata: {\"error\":{\"message\":\"" + CodeUpstreamError.Message() + "\",\"type\":\"upstream_error\",\"code\":\"upstream_error\",\"param\":null}}\n\n"},
		{v1.DialectAnthropic, "/anthropic/v1/messages", messagesBody("claude", true), "\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":\"" + CodeUpstreamError.Message() + "\"},\"request_id\":\""},
		{v1.DialectLux, "/lux/v1/generate", luxBody("luxm", true), "\n\nevent: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"" + CodeUpstreamError.Message() + "\",\"details\":{"},
		{v1.DialectGemini, "/gemini/v1beta/models/gemini:streamGenerateContent?alt=sse", `{"contents":[]}`, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"},
	}
	for _, c := range cases {
		rec := w.post(c.path, c.body)
		body := rec.Body.String()
		if c.door == v1.DialectGemini {
			if body != c.frame {
				t.Errorf("gemini: the gateway wrote into the stream: %q", body)
			}
		} else if !strings.Contains(body, c.frame) {
			t.Errorf("%s: body %q lacks %q", c.door, body, c.frame)
		}
		if got := w.recorder.last(t); got.Status != StatusFailed || got.Error != CodeUpstreamError {
			t.Errorf("%s: record %s %s", c.door, got.Status, got.Error)
		}
	}
	// A translated stream that fails mid-way ends the same way.
	w.anthropic.respond(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.Header().Set("Content-Length", "1000")
		rw.WriteHeader(200)
		_, _ = io.WriteString(rw, strings.SplitAfter(anthropicStream, "\n\n")[0])
		http.NewResponseController(rw).Flush()
		panic(http.ErrAbortHandler)
	})
	rec := w.post("/openai/v1/chat/completions", chatBody("claude", true))
	if !strings.Contains(rec.Body.String(), "data: {\"error\":") || strings.Contains(rec.Body.String(), "[DONE]") || w.recorder.last(t).Error != CodeUpstreamError {
		t.Errorf("translated cut: %s %s", rec.Body.String(), w.recorder.last(t).Error)
	}
	// An upstream error frame inside a translated stream is the codec's
	// error and ends the stream the same way.
	w.anthropic.respondSSE("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
	rec = w.post("/openai/v1/chat/completions", chatBody("claude", true))
	if !strings.Contains(rec.Body.String(), "data: {\"error\":") || w.recorder.last(t).Error != CodeUpstreamError {
		t.Errorf("upstream error frame: %s", rec.Body.String())
	}
	// A stream that stalls past the Provider's timeout is
	// upstream_timeout in the record.
	slow := w.provider("slow", v1.DialectOpenAI, w.openai.URL+"/v1", "x")
	slow.Spec.Timeout = "100ms"
	w.model("gpt-slow", target("slow", "gpt-4.1"))
	w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(200)
		_, _ = io.WriteString(rw, "data: {}\n\n")
		http.NewResponseController(rw).Flush()
		<-r.Context().Done()
	})
	w.post("/openai/v1/chat/completions", chatBody("gpt-slow", true))
	if got := w.recorder.last(t); got.Error != CodeUpstreamTimeout {
		t.Errorf("stalled stream: %s", got.Error)
	}
	w.openai.respond(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.Header().Set("Content-Length", "100")
		rw.WriteHeader(200)
		_, _ = io.WriteString(rw, "{")
		http.NewResponseController(rw).Flush()
		<-r.Context().Done()
	})
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt-slow", false)); errorCode(t, rec) != CodeUpstreamTimeout {
		t.Errorf("stalled body: %s", rec.Header().Get(HeaderError))
	}
}

// TestTranslatedStreamHeadersPrecedeTheFirstEvent: on a translated
// stream the caller has the status and the headers as soon as the
// upstream answered, before its first event arrives, so the time to
// first byte is the upstream's and does not wait for the first event.
func TestTranslatedStreamHeadersPrecedeTheFirstEvent(t *testing.T) {
	w := newWorld(t)
	gate := make(chan struct{})
	w.anthropic.respond(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(rw)
		_ = rc.Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		for _, f := range strings.SplitAfter(anthropicStream, "\n\n") {
			_, _ = io.WriteString(rw, f)
			_ = rc.Flush()
		}
	})
	srv := httptest.NewServer(w.h)
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/openai/v1/chat/completions", strings.NewReader(chatBody("claude", true)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+keyValue)
	req.Header.Set("Content-Type", "application/json")
	type answer struct {
		resp *http.Response
		err  error
	}
	answered := make(chan answer, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		answered <- answer{resp, err}
	}()
	var resp *http.Response
	select {
	case a := <-answered:
		if a.err != nil {
			t.Fatal(a.err)
		}
		resp = a.resp
	case <-time.After(10 * time.Second):
		t.Fatal("the status waited for the first event")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	close(gate)
	body, err := io.ReadAll(resp.Body)
	if err != nil || !strings.HasSuffix(string(body), "data: [DONE]\n\n") || !strings.Contains(string(body), `"model":"claude"`) {
		t.Fatalf("stream after the gate: %v %s", err, body)
	}
}
