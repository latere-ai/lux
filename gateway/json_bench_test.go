// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/bridge"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The benchmarks below measure the JSON cost of the gateway's payload
// paths with the hot paths' own functions: the request-field merge, the
// bridge legs a translated request, answer, and stream run through, and
// the frame relay that rewrites the model member. Payloads are built
// before the loop from fixed text, so runs compare.

// jsonBenchSizes are the request body sizes the sub-benchmarks target,
// in the number of conversation turns that reach them.
var jsonBenchSizes = []struct {
	name  string
	turns int
	tools int
}{
	{"small", 1, 1},
	{"medium", 24, 12},
	{"large", 480, 40},
}

// jsonBenchSentence is a fixed run of ordinary English prose so message
// contents have a realistic length and escape profile.
const jsonBenchSentence = "The deployment finished without errors, but the readiness probe on the second replica kept flapping, so I rolled it back and attached the \"events\" output for review. "

// jsonBenchText is n bytes of jsonBenchSentence, cut at the byte.
func jsonBenchText(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(jsonBenchSentence)
	}
	return b.String()[:n]
}

// jsonBenchChatRequest is an OpenAI chat completions request with the
// given number of user/assistant turns and tool definitions.
func jsonBenchChatRequest(model string, turns, tools int, stream bool) []byte {
	messages := []map[string]any{{"role": "system", "content": "You are a careful release engineer. Answer with the commands you ran and what they printed."}}
	for i := range turns {
		messages = append(messages,
			map[string]any{"role": "user", "content": fmt.Sprintf("Turn %d. ", i) + jsonBenchText(120+(i%7)*20)},
			map[string]any{"role": "assistant", "content": jsonBenchText(200 + (i%5)*40)},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "Summarize what is left to do."})
	req := map[string]any{
		"model": model, "messages": messages, "temperature": 0.2, "max_tokens": 1024,
		"user": "usr_01JBENCH0000000000000000",
	}
	if stream {
		req["stream"] = true
	}
	if tools > 0 {
		var defs []map[string]any
		for i := range tools {
			defs = append(defs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        fmt.Sprintf("tool_%02d", i),
					"description": "Runs one step of the release pipeline and returns its output as text.",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"target":  map[string]any{"type": "string", "description": "The environment to act on."},
							"timeout": map[string]any{"type": "integer", "description": "Seconds to wait."},
							"flags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
						"required": []string{"target"},
					},
				},
			})
		}
		req["tools"] = defs
	}
	out, err := json.Marshal(req, providerJSON)
	if err != nil {
		panic(err)
	}
	return out
}

// jsonBenchAnthropicResponse is a Messages API answer with n bytes of text.
func jsonBenchAnthropicResponse(n int) []byte {
	out, err := json.Marshal(map[string]any{
		"id": "msg_01JBENCH000000000000000", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5",
		"content":     []map[string]any{{"type": "text", "text": jsonBenchText(n)}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1450, "output_tokens": 312, "cache_read_input_tokens": 1024, "cache_creation_input_tokens": 0},
	}, providerJSON)
	if err != nil {
		panic(err)
	}
	return out
}

// jsonBenchAnthropicStream is a Messages API event stream of n text deltas.
func jsonBenchAnthropicStream(deltas int) []byte {
	var b bytes.Buffer
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_01JBENCH000000000000000","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1450,"output_tokens":1,"cache_read_input_tokens":1024}}}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	ev("ping", `{"type":"ping"}`)
	words := strings.Fields(jsonBenchSentence)
	for i := range deltas {
		text, _ := json.Marshal(" " + words[i%len(words)])
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(text)+`}}`)
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":312}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.Bytes()
}

// jsonBenchOpenAIStream is a chat completions chunk stream of n deltas.
func jsonBenchOpenAIStream(deltas int) []byte {
	var b bytes.Buffer
	chunk := func(delta string, finish string, usage string) {
		fmt.Fprintf(&b, `data: {"id":"chatcmpl-JBENCH","object":"chat.completion.chunk","created":1790000000,"model":"gpt-4.1","system_fingerprint":"fp_bench","choices":[{"index":0,"delta":%s,"logprobs":null,"finish_reason":%s}]%s}`+"\n\n", delta, finish, usage)
	}
	chunk(`{"role":"assistant","content":""}`, "null", "")
	words := strings.Fields(jsonBenchSentence)
	for i := range deltas {
		text, _ := json.Marshal(" " + words[i%len(words)])
		chunk(`{"content":`+string(text)+`}`, "null", "")
	}
	chunk(`{}`, `"stop"`, `,"usage":{"prompt_tokens":1450,"completion_tokens":312,"prompt_tokens_details":{"cached_tokens":1024}}`)
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

// jsonBenchSink is a response writer that keeps nothing and flushes
// for free, standing for the caller's connection.
type jsonBenchSink struct{ h http.Header }

func (s *jsonBenchSink) Header() http.Header         { return s.h }
func (s *jsonBenchSink) Write(p []byte) (int, error) { return len(p), nil }
func (s *jsonBenchSink) WriteHeader(int)             {}
func (s *jsonBenchSink) Flush()                      {}

func jsonBenchWriter() *responseWriter {
	w := &jsonBenchSink{h: http.Header{}}
	return &responseWriter{ResponseWriter: w, rc: http.NewResponseController(w), now: func() time.Time { return time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC) }}
}

// BenchmarkJSONRequestFieldsRoundTrip is a Provider's requestFields
// merged into a caller's chat request on every attempt: the body read
// as an object, the fields merged, the object written back.
func BenchmarkJSONRequestFieldsRoundTrip(b *testing.B) {
	fields := map[string]any{
		"temperature": 0.7,
		"provider":    map[string]any{"order": []any{"anthropic", "openai"}, "allow_fallbacks": true},
		"extra_body":  map[string]any{"metadata": map[string]any{"team": "platform", "tier": "paid"}},
	}
	for _, size := range jsonBenchSizes {
		body := jsonBenchChatRequest("gpt-4.1", size.turns, size.tools, false)
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, err := mergeRequestFields(body, fields); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONTranslateRequestRoundTrip is the request leg of an
// openai door to an anthropic target: decoded with the door's codec and
// written with the target's, as the forwarder does per attempt.
func BenchmarkJSONTranslateRequestRoundTrip(b *testing.B) {
	for _, size := range jsonBenchSizes {
		body := jsonBenchChatRequest("claude", size.turns, size.tools, false)
		b.Run(size.name, func(b *testing.B) {
			br, err := bridge.Open(doorDialect(opChatCompletions), targetDialect(v1.DialectAnthropic, "claude-sonnet-4-5"), bridge.Options{DefaultMaxTokens: defaultMaxTokens})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, _, err := br.Request(body, bridge.RequestOptions{Model: "claude-sonnet-4-5"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONTranslateResponseRoundTrip is the answer leg of the same
// route: the anthropic body decoded and written as a chat completion.
func BenchmarkJSONTranslateResponseRoundTrip(b *testing.B) {
	for _, size := range []struct {
		name string
		n    int
	}{{"small", 300}, {"medium", 8 << 10}, {"large", 150 << 10}} {
		body := jsonBenchAnthropicResponse(size.n)
		b.Run(size.name, func(b *testing.B) {
			br, err := bridge.Open(doorDialect(opChatCompletions), targetDialect(v1.DialectAnthropic, "claude-sonnet-4-5"), bridge.Options{DefaultMaxTokens: defaultMaxTokens})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				if _, _, _, err := br.Response(body, bridge.ResponseOptions{Model: "claude"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONTranslateStreamRoundTrip is a translated stream: every
// anthropic event decoded and written as a chat completion chunk.
func BenchmarkJSONTranslateStreamRoundTrip(b *testing.B) {
	for _, size := range []struct {
		name   string
		deltas int
	}{{"small", 4}, {"medium", 200}, {"large", 2000}} {
		stream := jsonBenchAnthropicStream(size.deltas)
		b.Run(size.name, func(b *testing.B) {
			br, err := bridge.Open(doorDialect(opChatCompletions), targetDialect(v1.DialectAnthropic, "claude-sonnet-4-5"), bridge.Options{DefaultMaxTokens: defaultMaxTokens})
			if err != nil {
				b.Fatal(err)
			}
			w := jsonBenchWriter()
			b.ReportAllocs()
			b.SetBytes(int64(len(stream)))
			for b.Loop() {
				if _, err := br.Stream(w, bytes.NewReader(stream), bridge.StreamOptions{Model: "claude", Flush: w.Flush}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONStreamFramesRelayRoundTrip is a passthrough stream whose
// Model's name differs from the upstream's: every frame's model member
// rewritten and its usage scanned, flushed per frame.
func BenchmarkJSONStreamFramesRelayRoundTrip(b *testing.B) {
	for _, size := range []struct {
		name   string
		deltas int
	}{{"small", 4}, {"medium", 200}, {"large", 2000}} {
		stream := jsonBenchOpenAIStream(size.deltas)
		b.Run(size.name, func(b *testing.B) {
			w := jsonBenchWriter()
			b.ReportAllocs()
			b.SetBytes(int64(len(stream)))
			for b.Loop() {
				sn := bridge.NewUsageScanner(bridge.WireOpenAI, bridge.FramingSSE)
				if err := relayFrames(w, bytes.NewReader(stream), sn, "gpt"); err != nil {
					b.Fatal(err)
				}
				sn.Close()
				if _, ok := sn.Usage(); !ok {
					b.Fatal("the stream carried no usage")
				}
			}
		})
	}
}

// BenchmarkJSONStreamChunksRelayDecode is a same-name passthrough
// stream: bytes relayed as read, with the usage scanner decoding every
// frame for the record's tokens.
func BenchmarkJSONStreamChunksRelayDecode(b *testing.B) {
	for _, size := range []struct {
		name   string
		deltas int
	}{{"small", 4}, {"medium", 200}, {"large", 2000}} {
		stream := jsonBenchOpenAIStream(size.deltas)
		b.Run(size.name, func(b *testing.B) {
			w := jsonBenchWriter()
			b.ReportAllocs()
			b.SetBytes(int64(len(stream)))
			for b.Loop() {
				sn := bridge.NewUsageScanner(bridge.WireOpenAI, bridge.FramingSSE)
				if err := relayChunks(w, bytes.NewReader(stream), sn); err != nil {
					b.Fatal(err)
				}
				sn.Close()
				if _, ok := sn.Usage(); !ok {
					b.Fatal("the stream carried no usage")
				}
			}
		})
	}
}
