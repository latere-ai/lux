// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	v1 "latere.ai/x/lux/manifest/v1"
)

// The shapes below are the dialects' own, written as maps so the encoder
// sorts the keys and one request twice yields identical bytes. The usage
// members are the route's, as spec 015's table has them, and the
// cached-input and cache-write members are 0.

// The fixed identifiers of an answer.
const (
	chatID     = "chatcmpl-stub"
	responseID = "resp_stub"
	messageID  = "msg_stub"
	luxID      = "lux-stub"
)

// ModelName is the one name each stub lists on its models route:
// stub-<dialect>, so a discovered Model names the dialect it came from.
func ModelName(d v1.Dialect) string { return "stub-" + string(d) }

// whole is the non-streaming answer of a route.
func whole(d v1.Dialect, rt routeKind, req parsed, b Behavior) any {
	content := Content(d, req.model, req.text)
	in, out := b.InputTokens, b.OutputTokens
	switch rt {
	case routeChat:
		return map[string]any{
			"id": chatID, "object": "chat.completion", "created": 0, "model": req.model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out, "prompt_tokens_details": map[string]any{"cached_tokens": 0}},
		}
	case routeResponses:
		return map[string]any{
			"id": responseID, "object": "response", "model": req.model, "status": "completed",
			"output": []any{responsesMessage(content)},
			"usage":  map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out, "input_tokens_details": map[string]any{"cached_tokens": 0}},
		}
	case routeEmbeddings:
		return map[string]any{
			"object": "list", "model": req.model,
			"data":  []any{map[string]any{"object": "embedding", "index": 0, "embedding": vector(req.text)}},
			"usage": map[string]any{"prompt_tokens": in, "total_tokens": in},
		}
	case routeMessages:
		return map[string]any{
			"id": messageID, "type": "message", "role": "assistant", "model": req.model,
			"content":     []any{map[string]any{"type": "text", "text": content}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": anthropicUsage(in, out),
		}
	case routeAnthropicCount, routeLuxCount:
		return map[string]any{"input_tokens": in}
	case routeGeminiGenerate:
		return map[string]any{
			"candidates":    []any{geminiCandidate(content, true)},
			"usageMetadata": geminiUsage(in, out),
			"modelVersion":  req.model,
		}
	case routeGeminiCount:
		return map[string]any{"totalTokens": in}
	case routeGeminiEmbed:
		return map[string]any{"embedding": map[string]any{"values": vector(req.text)}}
	case routeGenerate:
		return map[string]any{
			"id": luxID, "model": req.model,
			"blocks":      []any{map[string]any{"type": "text", "text": content}},
			"stop_reason": "end_turn",
			"usage":       luxUsage(in, out),
		}
	case routeGemini, routeGeminiStream, routeModels:
	}
	return map[string]any{}
}

func responsesMessage(content string) map[string]any {
	return map[string]any{
		"type": "message", "id": messageID, "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
	}
}

func anthropicUsage(in, out int64) map[string]any {
	return map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}
}

func geminiUsage(in, out int64) map[string]any {
	return map[string]any{"promptTokenCount": in, "candidatesTokenCount": out, "totalTokenCount": in + out, "cachedContentTokenCount": 0}
}

func luxUsage(in, out int64) map[string]any {
	return map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 0, "cache_write_input_tokens": 0}
}

func geminiCandidate(text string, final bool) map[string]any {
	parts := []any{}
	if text != "" || !final {
		parts = append(parts, map[string]any{"text": text})
	}
	c := map[string]any{"content": map[string]any{"parts": parts, "role": "model"}, "index": 0}
	if final {
		c["finishReason"] = "STOP"
	}
	return c
}

// vector is a three-dimensional embedding read off the digest of the
// text, so an embedding is a function of its input too.
func vector(text string) []float64 {
	sum := Digest(text)
	out := make([]float64, 3)
	for i := range out {
		n, _ := strconv.ParseUint(sum[i*2:i*2+2], 16, 8)
		out[i] = float64(n) / 255
	}
	return out
}

// modelList is the dialect's models route: one entry, ModelName, in the
// shape spec 005's discovery table reads.
func modelList(d v1.Dialect) any {
	name := ModelName(d)
	switch d {
	case v1.DialectAnthropic:
		return map[string]any{
			"data":     []any{map[string]any{"type": "model", "id": name, "display_name": name, "created_at": "1970-01-01T00:00:00Z"}},
			"has_more": false, "first_id": name, "last_id": name,
		}
	case v1.DialectGemini:
		return map[string]any{
			"models": []any{map[string]any{"name": "models/" + name, "displayName": name, "supportedGenerationMethods": []any{"generateContent", "countTokens"}}},
		}
	case v1.DialectOpenAI, v1.DialectLux:
	}
	return map[string]any{"object": "list", "data": []any{map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "stub"}}}
}

// errorBody is the dialect's error shape for a status.
func errorBody(d v1.Dialect, status int, message string) any {
	switch d {
	case v1.DialectAnthropic:
		return map[string]any{"type": "error", "error": map[string]any{"type": anthropicErrorType(status), "message": message}}
	case v1.DialectGemini:
		return map[string]any{"error": map[string]any{"code": status, "message": message, "status": geminiStatus(status)}}
	case v1.DialectLux:
		return map[string]any{"error": map[string]any{"code": luxCode(status), "message": message}}
	case v1.DialectOpenAI:
	}
	return map[string]any{"error": map[string]any{"message": message, "type": openaiErrorType(status), "param": nil, "code": nil}}
}

func openaiErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_request_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	return "server_error"
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

func geminiStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case 529:
		return "UNAVAILABLE"
	}
	return "INTERNAL"
}

func luxCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case 529:
		return "provider_unavailable"
	}
	return "internal"
}

// pieces splits content into n parts whose concatenation is content, so
// a client that joins the deltas reads the whole answer; a part past the
// text is empty.
func pieces(content string, n int) []string {
	out := make([]string, n)
	if n == 0 {
		return out
	}
	runes := []rune(content)
	per := (len(runes) + n - 1) / n
	for i := range out {
		lo := min(i*per, len(runes))
		hi := min(lo+per, len(runes))
		out[i] = string(runes[lo:hi])
	}
	return out
}

// streamer writes one route's streamed answer: the opening frames, one
// content event per piece, and the closing frames with the usage. A
// gemini stream without alt=sse is a JSON array of the same chunks.
type streamer struct {
	w      http.ResponseWriter
	d      v1.Dialect
	rt     routeKind
	req    parsed
	b      Behavior
	text   string
	pieces []string
	sse    bool
	wrote  bool // an array element was written, so the next needs a comma
}

func newStreamer(w http.ResponseWriter, d v1.Dialect, rt routeKind, req parsed, b Behavior) *streamer {
	content := Content(d, req.model, req.text)
	return &streamer{
		w: w, d: d, rt: rt, req: req, b: b, text: content, pieces: pieces(content, b.Events),
		sse: rt != routeGeminiStream || req.sse,
	}
}

// piece is the i-th content delta; one past the pieces is empty.
func (s *streamer) piece(i int) string {
	if i < len(s.pieces) {
		return s.pieces[i]
	}
	return ""
}

// emit writes one frame: an SSE frame with the event name when the
// dialect names its events, or one JSON array element.
func (s *streamer) emit(event string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	if !s.sse {
		if s.wrote {
			_, _ = io.WriteString(s.w, ",")
		}
		s.wrote = true
		_, _ = s.w.Write(raw)
		flush(s.w)
		return
	}
	if event != "" {
		_, _ = io.WriteString(s.w, "event: "+event+"\n")
	}
	_, _ = io.WriteString(s.w, "data: ")
	_, _ = s.w.Write(raw)
	_, _ = io.WriteString(s.w, "\n\n")
	flush(s.w)
}

// begin writes the headers and the frames before the first content.
func (s *streamer) begin() {
	if s.sse {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
	} else {
		s.w.Header().Set("Content-Type", "application/json")
	}
	s.w.WriteHeader(http.StatusOK)
	switch s.rt {
	case routeResponses:
		s.emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "model": s.req.model, "status": "in_progress"}})
		s.emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{}}})
	case routeMessages:
		s.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": messageID, "type": "message", "role": "assistant", "model": s.req.model, "content": []any{}, "stop_reason": nil,
			"usage": anthropicUsage(s.b.InputTokens, 0),
		}})
		s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	case routeGenerate:
		s.emit("message_start", map[string]any{"type": "message_start", "id": luxID, "model": s.req.model, "usage": luxUsage(s.b.InputTokens, 0)})
		s.emit("block_start", map[string]any{"type": "block_start", "index": 0, "block": map[string]any{"type": "text"}})
	case routeGeminiStream:
		if !s.sse {
			_, _ = io.WriteString(s.w, "[")
		}
	case routeChat, routeEmbeddings, routeAnthropicCount, routeGemini, routeGeminiGenerate, routeGeminiCount, routeGeminiEmbed, routeLuxCount, routeModels:
	}
	flush(s.w)
}

// content writes the i-th content event.
func (s *streamer) content(i int) {
	p := s.piece(i)
	switch s.rt {
	case routeChat:
		delta := map[string]any{"content": p}
		if i == 0 {
			delta["role"] = "assistant"
		}
		// The last content event carries the finish reason, so a stream
		// of n content events is n frames and then the route's final
		// usage event, as spec 015 has it, rather than one more.
		var finish any
		if i == s.b.Events-1 {
			finish = "stop"
		}
		s.emit("", map[string]any{"id": chatID, "object": "chat.completion.chunk", "created": 0, "model": s.req.model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	case routeResponses:
		s.emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0, "delta": p})
	case routeMessages:
		s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": p}})
	case routeGeminiStream:
		s.emit("", map[string]any{"candidates": []any{geminiCandidate(p, false)}, "modelVersion": s.req.model})
	case routeGenerate:
		s.emit("text_delta", map[string]any{"type": "text_delta", "index": 0, "delta": p})
	case routeEmbeddings, routeAnthropicCount, routeGemini, routeGeminiGenerate, routeGeminiCount, routeGeminiEmbed, routeLuxCount, routeModels:
	}
}

// finish writes the route's final usage event and the terminator.
func (s *streamer) finish() {
	in, out := s.b.InputTokens, s.b.OutputTokens
	switch s.rt {
	case routeChat:
		if s.b.Events == 0 {
			// No content event carried the finish reason, so one frame
			// of its own closes the choice.
			s.emit("", map[string]any{"id": chatID, "object": "chat.completion.chunk", "created": 0, "model": s.req.model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
		}
		s.emit("", map[string]any{"id": chatID, "object": "chat.completion.chunk", "created": 0, "model": s.req.model, "choices": []any{},
			"usage": map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out, "prompt_tokens_details": map[string]any{"cached_tokens": 0}}})
		_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	case routeResponses:
		s.emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": responsesMessage(s.text)})
		s.emit("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{
			"id": responseID, "object": "response", "model": s.req.model, "status": "completed",
			"output": []any{responsesMessage(s.text)},
			"usage":  map[string]any{"input_tokens": in, "output_tokens": out, "total_tokens": in + out, "input_tokens_details": map[string]any{"cached_tokens": 0}},
		}})
	case routeMessages:
		s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"input_tokens": in, "output_tokens": out}})
		s.emit("message_stop", map[string]any{"type": "message_stop"})
	case routeGeminiStream:
		s.emit("", map[string]any{"candidates": []any{geminiCandidate("", true)}, "usageMetadata": geminiUsage(in, out), "modelVersion": s.req.model})
		if !s.sse {
			_, _ = io.WriteString(s.w, "]")
		}
	case routeGenerate:
		s.emit("block_stop", map[string]any{"type": "block_stop", "index": 0})
		s.emit("message_delta", map[string]any{"type": "message_delta", "stop_reason": "end_turn", "usage": luxUsage(in, out)})
		s.emit("message_stop", map[string]any{"type": "message_stop"})
	case routeEmbeddings, routeAnthropicCount, routeGemini, routeGeminiGenerate, routeGeminiCount, routeGeminiEmbed, routeLuxCount, routeModels:
	}
	flush(s.w)
}
