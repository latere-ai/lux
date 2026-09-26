// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// recordSpans installs an SDK provider with an in-memory recorder as
// the global one, with the W3C propagator, and puts the no-op back when
// the test ends, so the next test starts from tracing off. It runs
// before newWorld, the order the process keeps: Bootstrap, then New.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		tracingOff()
	})
	return sr
}

// tracingOff is the process without an endpoint: the no-op provider and
// no propagator.
func tracingOff() {
	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
}

// attrsOf is a span's attributes by key.
func attrsOf(s sdktrace.ReadOnlySpan) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

// byName splits the recorded spans into this package's two and the
// client spans the instrumented transport opened.
func byName(spans []sdktrace.ReadOnlySpan) (requests, upstreams, clients []sdktrace.ReadOnlySpan) {
	for _, s := range spans {
		switch {
		case s.Name() == SpanRequest:
			requests = append(requests, s)
		case s.Name() == SpanUpstream:
			upstreams = append(upstreams, s)
		case s.SpanKind() == trace.SpanKindClient:
			clients = append(clients, s)
		}
	}
	return requests, upstreams, clients
}

// The attribute sets of spec 019's table for the two spans.
var (
	requestAttrKeys  = []string{AttrDoor, AttrRoute, AttrModel, AttrProvider, AttrStatus, AttrCode, AttrRequestID, AttrStream, AttrTranslated}
	upstreamAttrKeys = []string{AttrProvider, AttrAttempt, AttrMethod, AttrTemplate, AttrHTTPStatus, AttrTTFBMs}
)

func keysOf(attrs map[string]attribute.Value) []string {
	return slices.Sorted(maps.Keys(attrs))
}

// TestRequestSpanNestsUnderTheListenerSpan: when the request's context
// already carries a span this process opened, the listener's server span,
// lux.request is an INTERNAL child of it, the caller's traceparent is not
// read a second time, and the request has one SERVER span.
func TestRequestSpanNestsUnderTheListenerSpan(t *testing.T) {
	sr := recordSpans(t)
	w := newWorld(t)
	ctx, listener := otel.Tracer("listener").Start(context.Background(), "POST /openai/v1/chat/completions", trace.WithSpanKind(trace.SpanKindServer))
	w.anthropic.respondJSON(200, anthropicResponse)
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("dual", false)).WithContext(ctx)
	r.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	w.do(r)
	listener.End()
	requests, _, _ := byName(sr.Ended())
	if len(requests) != 1 {
		t.Fatalf("%d request spans", len(requests))
	}
	req := requests[0]
	if req.SpanKind() != trace.SpanKindInternal || req.Parent().SpanID() != listener.SpanContext().SpanID() || req.SpanContext().TraceID() != listener.SpanContext().TraceID() {
		t.Errorf("lux.request: kind %v parent %s trace %s, want an internal child of the listener's span %s", req.SpanKind(), req.Parent().SpanID(), req.SpanContext().TraceID(), listener.SpanContext().SpanID())
	}
	servers := 0
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			servers++
		}
	}
	if servers != 1 {
		t.Errorf("%d SERVER spans for one request", servers)
	}
}

// TestRequestSpans is spec 019's row for the data plane: one lux.request
// span per request, a child of the context the caller propagated, with
// one lux.upstream child per target tried carrying the attempt number,
// each the parent of the transport's client span; the attributes are
// exactly the table's, and a stream's span ends when the stream ends.
func TestRequestSpans(t *testing.T) {
	sr := recordSpans(t)
	w := newWorld(t)
	w.openai.respondJSON(500, `{"error":"down"}`)
	w.anthropic.respondJSON(200, anthropicResponse)
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("dual", false))
	r.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	rec := w.do(r)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	requests, upstreams, clients := byName(sr.Ended())
	if len(requests) != 1 || len(upstreams) != 2 || len(clients) != 2 {
		t.Fatalf("%d request, %d upstream, %d client spans", len(requests), len(upstreams), len(clients))
	}
	req := requests[0]
	if req.SpanKind() != trace.SpanKindServer || req.SpanContext().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || req.Parent().SpanID().String() != "b7ad6b7169203331" {
		t.Errorf("the request span is not the caller's child: kind %v trace %s parent %s", req.SpanKind(), req.SpanContext().TraceID(), req.Parent().SpanID())
	}
	attrs := attrsOf(req)
	if got := keysOf(attrs); !slices.Equal(got, slices.Sorted(slices.Values(requestAttrKeys))) {
		t.Errorf("request attributes %v", got)
	}
	want := map[string]string{AttrDoor: "openai", AttrRoute: "/openai/v1/chat/completions", AttrModel: "dual", AttrProvider: "ant", AttrStatus: "ok", AttrCode: "", AttrRequestID: rec.Header().Get(HeaderRequestID)}
	for k, v := range want {
		if attrs[k].AsString() != v {
			t.Errorf("%s = %q, want %q", k, attrs[k].AsString(), v)
		}
	}
	if attrs[AttrStream].AsBool() || !attrs[AttrTranslated].AsBool() {
		t.Errorf("stream %v translated %v", attrs[AttrStream].AsBool(), attrs[AttrTranslated].AsBool())
	}
	for i, u := range upstreams {
		if u.Parent().SpanID() != req.SpanContext().SpanID() {
			t.Errorf("upstream %d is not the request's child", i)
		}
		ua := attrsOf(u)
		if got := keysOf(ua); !slices.Equal(got, slices.Sorted(slices.Values(upstreamAttrKeys))) {
			t.Errorf("upstream %d attributes %v", i, got)
		}
		if ua[AttrAttempt].AsInt64() != int64(i+1) || ua[AttrMethod].AsString() != http.MethodPost || ua[AttrTTFBMs].AsInt64() <= 0 {
			t.Errorf("upstream %d: attempt %d method %s ttfb %d", i, ua[AttrAttempt].AsInt64(), ua[AttrMethod].AsString(), ua[AttrTTFBMs].AsInt64())
		}
		if !req.EndTime().After(u.EndTime()) && !req.EndTime().Equal(u.EndTime()) {
			t.Errorf("the request span ended before upstream %d", i)
		}
	}
	first, second := attrsOf(upstreams[0]), attrsOf(upstreams[1])
	if first[AttrProvider].AsString() != "oai" || first[AttrHTTPStatus].AsInt64() != 500 || first[AttrTemplate].AsString() != "/chat/completions" {
		t.Errorf("first attempt %v", first)
	}
	if second[AttrProvider].AsString() != "ant" || second[AttrHTTPStatus].AsInt64() != 200 || second[AttrTemplate].AsString() != "/messages" {
		t.Errorf("second attempt %v", second)
	}
	parents := []trace.SpanID{upstreams[0].SpanContext().SpanID(), upstreams[1].SpanContext().SpanID()}
	for _, c := range clients {
		if !slices.Contains(parents, c.Parent().SpanID()) {
			t.Errorf("the client span %s is not under an upstream span", c.Name())
		}
	}

	// A stream: the span ends with the stream, and carries lux.stream.
	sr.Reset()
	w.openai.respondSSE("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n", "data: [DONE]\n\n")
	rec = w.post("/openai/v1/chat/completions", chatBody("gpt", true))
	if rec.Code != 200 {
		t.Fatalf("stream: %d %s", rec.Code, rec.Body.String())
	}
	requests, upstreams, _ = byName(sr.Ended())
	if len(requests) != 1 || len(upstreams) != 1 {
		t.Fatalf("stream: %d request, %d upstream spans", len(requests), len(upstreams))
	}
	if a := attrsOf(requests[0]); !a[AttrStream].AsBool() || a[AttrTranslated].AsBool() || a[AttrProvider].AsString() != "oai" {
		t.Errorf("stream attributes %v", a)
	}
	if requests[0].EndTime().Before(upstreams[0].EndTime()) {
		t.Error("the stream's request span ended before its upstream span")
	}
	if a := attrsOf(upstreams[0]); a[AttrTemplate].AsString() != "/chat/completions" || a[AttrHTTPStatus].AsInt64() != 200 {
		t.Errorf("stream upstream attributes %v", a)
	}
}

// identityKeys are attribute keys no span of this package may carry,
// and the shape a leak of identity would take.
var identityKeys = []string{"lux.subject", "lux.owner", "lux.key_id", "lux.key_prefix", "lux.key", "subject", "owner", "key_id", "key_prefix",
	"client.address", "net.sock.peer.addr", "net.peer.ip", "http.client_ip", "enduser.id", "user_agent.original"}

// TestSpansCarryNoIdentity is spec 019's row over every span this
// package produces for a served, a refused, and a failed request: no
// attribute names or carries a subject, owner, Key id, Key prefix, Key
// value, or the caller's address, on the two spans of the table or on
// the transport's client spans beside them.
func TestSpansCarryNoIdentity(t *testing.T) {
	sr := recordSpans(t)
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	w.anthropic.respondJSON(503, `{"error":"down"}`)
	for _, body := range []string{chatBody("gpt", false), chatBody("nope", false), messagesBody("never", false)} {
		door := "/openai/v1/chat/completions"
		if strings.Contains(body, "never") {
			door = "/anthropic/v1/messages"
		}
		w.do(w.request(http.MethodPost, door, body))
	}
	rec := w.recorder.last(t)
	forbidden := []string{keyValue, rec.Owner, "key_ALL", keyValue[:12], "192.0.2.1", "alice"}
	spans := sr.Ended()
	if len(spans) < 5 {
		t.Fatalf("%d spans", len(spans))
	}
	for _, s := range spans {
		for _, kv := range s.Attributes() {
			key, value := string(kv.Key), kv.Value.String()
			if slices.Contains(identityKeys, key) {
				t.Errorf("span %s carries %s", s.Name(), key)
			}
			for _, f := range forbidden {
				if strings.Contains(value, f) {
					t.Errorf("span %s attribute %s carries %q", s.Name(), key, f)
				}
			}
		}
	}
}

// TestTracingOffByDefault is spec 019's row: with no endpoint the
// provider is the no-op, so the request path starts no recording span
// and nothing downstream sees a span context, while the request is
// served as before; with a provider installed, the same request runs
// under a valid span.
func TestTracingOffByDefault(t *testing.T) {
	tracingOff()
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if sc := trace.SpanContextFromContext(w.keys.context()); sc.IsValid() {
		t.Errorf("a span context %s reached the Key lookup with tracing off", sc.SpanID())
	}
	if trace.SpanFromContext(w.keys.context()).IsRecording() {
		t.Error("a recording span with tracing off")
	}

	sr := recordSpans(t)
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !trace.SpanContextFromContext(w.keys.context()).IsValid() {
		t.Error("no span context reached the Key lookup with a provider installed")
	}
	if requests, _, _ := byName(sr.Ended()); len(requests) != 1 {
		t.Errorf("%d request spans with a provider installed", len(requests))
	}
}

// dataLogFields is spec 019's field row for the data plane.
var dataLogFields = []string{"request_id", "door", "route", "model", "provider", "status", "code", "key_prefix", "owner", "duration_ms", "ttfb_ms", "input_tokens", "output_tokens", "stream"}

// logLines parses the handler's log as one JSON object per line.
func logLines(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON line: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// TestRequestLineFields is the data plane's half of spec 019's log row
// at the package: one INFO line per request named after the span, with
// exactly the row's fields beside the encoder's three, the values of
// the record, the Key's prefix and never its value, and a model string
// of newlines and terminal escapes producing one line whose model is
// empty because nothing resolved.
func TestRequestLineFields(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	lines := logLines(t, w.log.String())
	if len(lines) != 1 {
		t.Fatalf("%d lines:\n%s", len(lines), w.log.String())
	}
	line := lines[0]
	if line["msg"] != LogRequest || line["level"] != "INFO" {
		t.Errorf("msg %v level %v", line["msg"], line["level"])
	}
	fields := slices.Sorted(maps.Keys(line))
	fields = slices.DeleteFunc(fields, func(k string) bool { return k == "time" || k == "level" || k == "msg" })
	if !slices.Equal(fields, slices.Sorted(slices.Values(dataLogFields))) {
		t.Errorf("fields %v", fields)
	}
	want := map[string]any{"request_id": rec.Header().Get(HeaderRequestID), "door": "openai", "route": "/openai/v1/chat/completions", "model": "gpt", "provider": "oai",
		"status": "ok", "code": "", "key_prefix": keyValue[:12], "owner": "https://login.example.com|alice", "input_tokens": float64(10), "output_tokens": float64(3), "stream": false}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if line["duration_ms"].(float64) <= 0 || line["ttfb_ms"].(float64) <= 0 {
		t.Errorf("timings %v %v", line["duration_ms"], line["ttfb_ms"])
	}
	if strings.Contains(w.log.String(), keyValue) {
		t.Error("the Key value reached the log")
	}

	w.log.Reset()
	hostile := "gpt\n\x1b[31mred\x1b[0m\r\n{\"level\":\"ERROR\"}"
	rec = w.post("/openai/v1/chat/completions", chatBody(hostile, false))
	if errorCode(t, rec) != CodeModelNotFound {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	lines = logLines(t, w.log.String())
	if len(lines) != 1 || lines[0]["model"] != "" || lines[0]["status"] != "refused" || lines[0]["code"] != "model_not_found" {
		t.Errorf("a hostile model string produced %d lines: %v", len(lines), lines)
	}
	if strings.Contains(w.log.String(), "\x1b") || strings.Contains(w.log.String(), "red") {
		t.Errorf("the caller's string reached the log: %q", w.log.String())
	}
}
