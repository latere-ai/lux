// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The benchmarks below measure the gateway's own overhead per request:
// one request through Handler.ServeHTTP against the in-process stub
// providers of the test harness, so nothing dials a real provider and no
// network beyond loopback is on the path. Each reuses newWorld, so the
// fakes, the catalog, and the four stubs are the ones the tests trust;
// the body and the answer are fixed, so runs compare. They isolate the
// cost Lux adds on top of a provider call, not the provider's latency,
// which these deliberately exclude (spec 023).

// BenchmarkGatewayPassthrough is the same-dialect hot path: an /openai
// door to an openai target whose upstream name equals the Model's, so the
// body reaches the stub byte for byte and the codec never runs. It bounds
// the routing, key lookup, limit, forward, and record overhead alone.
func BenchmarkGatewayPassthrough(b *testing.B) {
	w := newWorld(b)
	w.openai.respondJSON(http.StatusOK, openaiChatResponse)
	body := chatBody("gpt-4.1", false)
	benchServe(b, w, http.MethodPost, "/openai/v1/chat/completions", body)
}

// BenchmarkGatewayTranslated is the translated hot path: an /openai door
// to an anthropic target, so the request is decoded with the door's codec
// and re-encoded for the target and the answer back again. The delta from
// BenchmarkGatewayPassthrough is the translation overhead through
// llmdialect/bridge.
func BenchmarkGatewayTranslated(b *testing.B) {
	w := newWorld(b)
	w.anthropic.respondJSON(http.StatusOK, anthropicResponse)
	body := chatBody("claude", false)
	benchServe(b, w, http.MethodPost, "/openai/v1/chat/completions", body)
}

// BenchmarkGatewayCount is a token count the gateway answers itself: the
// /anthropic count route toward an openai target is served from the
// bridge's estimator, so no request reaches a provider. It bounds the
// decode-and-estimate overhead of a count.
func BenchmarkGatewayCount(b *testing.B) {
	w := newWorld(b)
	body := messagesBody("gpt", false)
	benchServe(b, w, http.MethodPost, "/anthropic/v1/messages/count_tokens", body)
}

// BenchmarkGatewayStreaming relays a fixed number of SSE events on a
// same-dialect passthrough stream: an /openai chat completion to an openai
// target. It bounds the per-request cost of the streaming relay, the event
// loop and the usage scan included, over a fixed event count so runs
// compare.
func BenchmarkGatewayStreaming(b *testing.B) {
	w := newWorld(b)
	frames := make([]string, 0, benchStreamEvents+2)
	for range benchStreamEvents {
		frames = append(frames, benchStreamDelta)
	}
	frames = append(frames, benchStreamUsage, "data: [DONE]\n\n")
	w.openai.respondSSE(frames...)
	body := chatBody("gpt-4.1", true)
	benchServe(b, w, http.MethodPost, "/openai/v1/chat/completions", body)
}

// benchServe runs one request per iteration through the handler, reporting
// allocations. The request and the recorder are built inside the loop
// because the handler consumes the body and writes the response; the timer
// is reset after the fixed setup so only the served requests are measured.
func benchServe(b *testing.B, w *world, method, path, body string) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+keyValue)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.h.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			b.Fatalf("%s %s: status %d, header %v", method, path, rec.Code, rec.Header())
		}
	}
}

// The streaming benchmark's fixed frames: benchStreamEvents content deltas,
// then one chunk carrying usage and empty choices, then [DONE], the shape
// an openai chat completion stream ends with.
const (
	benchStreamEvents = 16
	benchStreamDelta  = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
	benchStreamUsage  = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4.1\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":16}}\n\n"
)
