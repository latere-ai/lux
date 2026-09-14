// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestDialectsPerRouteAndTarget: each translated route and each count
// names its door's codec dialect and a route with none names none; an
// openai target is the Responses dialect for the reasoning family and
// Chat for every other name, a gemini target has none; a dropped dialect
// header's loss entry is header.<name> in lower case.
func TestDialectsPerRouteAndTarget(t *testing.T) {
	routes := map[operation]ir.Dialect{
		opChatCompletions: ir.DialectOpenAIChat, opResponses: ir.DialectOpenAIResponses,
		opMessages: ir.DialectAnthropicMessages, opAnthropicCount: ir.DialectAnthropicMessages,
		opGenerate: ir.DialectLux, opLuxCount: ir.DialectLux,
		opEmbeddings: "", opGeminiGenerate: "", opGeminiStream: "", opGeminiCount: "", opGeminiEmbed: "",
		opModelsList: "", opModelsRead: "", opOpaque: "", opNone: "",
	}
	for op, want := range routes {
		if got := doorDialect(op); got != want {
			t.Errorf("doorDialect(%d) = %q, want %q", op, got, want)
		}
	}
	targets := []struct {
		d        v1.Dialect
		upstream string
		want     ir.Dialect
	}{
		{v1.DialectOpenAI, "gpt-5", ir.DialectOpenAIResponses},
		{v1.DialectOpenAI, "o3-mini", ir.DialectOpenAIResponses},
		{v1.DialectOpenAI, "gpt-4.1", ir.DialectOpenAIChat},
		{v1.DialectAnthropic, "claude", ir.DialectAnthropicMessages},
		{v1.DialectLux, "x", ir.DialectLux},
		{v1.DialectGemini, "x", ""},
		{"", "x", ""},
	}
	for _, c := range targets {
		if got := targetDialect(c.d, c.upstream); got != c.want {
			t.Errorf("targetDialect(%s, %s) = %q, want %q", c.d, c.upstream, got, c.want)
		}
	}
	if lossHeader("Anthropic-Beta") != "header.anthropic-beta" {
		t.Error("lossHeader")
	}
}

// TestCodecOptionsFollowTheModel: the codec options are this package's,
// computed per target as spec 004's table says. A Model's
// maxOutputTokens is the anthropic codec's max_tokens when the caller
// sent none, and a Model without one leaves the codec's 4096.
func TestCodecOptionsFollowTheModel(t *testing.T) {
	w := newWorld(t)
	w.model("capped", target("ant", "claude-3")).Spec.MaxOutputTokens = 256
	w.anthropic.respondJSON(200, anthropicResponse)
	if rec := w.post("/openai/v1/chat/completions", chatBody("capped", false)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := string(w.anthropic.last(t).Body); !strings.Contains(got, `"max_tokens":256`) {
		t.Errorf("the Model's maxOutputTokens is not the codec's default: %s", got)
	}
	w.anthropic.respondJSON(200, anthropicResponse)
	w.post("/openai/v1/chat/completions", chatBody("claude", false))
	if got := string(w.anthropic.last(t).Body); !strings.Contains(got, `"max_tokens":4096`) {
		t.Errorf("no maxOutputTokens does not fall back to the codec's 4096: %s", got)
	}
}

func TestBridgeable(t *testing.T) {
	cases := []struct {
		op     operation
		door   v1.Dialect
		target v1.Dialect
		want   bool
	}{
		{opChatCompletions, v1.DialectOpenAI, v1.DialectOpenAI, true},
		{opChatCompletions, v1.DialectOpenAI, v1.DialectAnthropic, true},
		{opChatCompletions, v1.DialectOpenAI, v1.DialectGemini, false},
		{opEmbeddings, v1.DialectOpenAI, v1.DialectOpenAI, true},
		{opEmbeddings, v1.DialectOpenAI, v1.DialectAnthropic, false},
		{opGeminiGenerate, v1.DialectGemini, v1.DialectGemini, true},
		{opGeminiGenerate, v1.DialectGemini, v1.DialectOpenAI, false},
		{opAnthropicCount, v1.DialectAnthropic, v1.DialectGemini, true},
		{opLuxCount, v1.DialectLux, v1.DialectGemini, true},
		{opGeminiCount, v1.DialectGemini, v1.DialectAnthropic, false},
		{opGenerate, v1.DialectLux, v1.DialectAnthropic, true},
	}
	for _, c := range cases {
		rt := route{op: c.op, door: c.door}
		if c.op == opChatCompletions || c.op == opGenerate {
			rt.class = ClassTranslated
		} else {
			rt.class = ClassModel
		}
		if got := rt.bridgeable(c.target); got != c.want {
			t.Errorf("%d %s -> %s: %v", c.op, c.door, c.target, got)
		}
	}
}

// TestBridgeFailuresMapToCodes: the bridge's seven codes each map to the
// code spec 004's table names, and a decode refusal is invalid_request
// whatever its RefusalScope, because on this path the body is only ever
// sent translated. A stream failure is classified like any cut after the
// first byte, client_closed when the caller is gone and upstream_timeout
// when the Provider's timeout passed; an error that is not the bridge's
// is upstream_error with its message.
func TestBridgeFailuresMapToCodes(t *testing.T) {
	live := &call{r: httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)}
	cause := errors.New("the codec's words")
	cases := []struct {
		name   string
		err    error
		code   Code
		detail string
	}{
		{"decode_request, surface", &bridge.Error{Code: bridge.DecodeRequest, Detail: "d", Scope: llmdialect.ScopeSurface, Err: cause}, CodeInvalidRequest, "d"},
		{"decode_request, dialect", &bridge.Error{Code: bridge.DecodeRequest, Detail: "d", Scope: llmdialect.ScopeDialect, Err: cause}, CodeInvalidRequest, "d"},
		{"encode_request", &bridge.Error{Code: bridge.EncodeRequest, Detail: "d", Err: cause}, CodeInvalidRequest, "d"},
		{"decode_response", &bridge.Error{Code: bridge.DecodeResponse, Detail: "d", Err: cause}, CodeUpstreamError, "decoding the upstream response: d"},
		{"encode_response", &bridge.Error{Code: bridge.EncodeResponse, Detail: "d", Err: cause}, CodeUpstreamError, "encoding the response for the door: d"},
		{"stream_failed", &bridge.Error{Code: bridge.StreamFailed, Detail: cause.Error(), Err: cause}, CodeUpstreamError, "the upstream stream failed: " + cause.Error()},
		{"stream_failed without a cause", &bridge.Error{Code: bridge.StreamFailed, Detail: "d"}, CodeUpstreamError, "the upstream stream failed: stream_failed: d"},
		{"write_failed", &bridge.Error{Code: bridge.WriteFailed, Detail: "d", Err: cause}, ClientClosed, ""},
		{"unsupported", &bridge.Error{Code: bridge.Unsupported, Detail: "d"}, CodeDialectUnsupported, "d"},
		{"wrapped", fmt.Errorf("attempt: %w", &bridge.Error{Code: bridge.EncodeRequest, Detail: "d"}), CodeInvalidRequest, "d"},
		{"not the bridge's", cause, CodeUpstreamError, cause.Error()},
	}
	for _, c := range cases {
		f := live.bridgeFailure(t.Context(), c.err)
		if f.code != c.code || f.detail != c.detail {
			t.Errorf("%s: %s %q, want %s %q", c.name, f.code, f.detail, c.code, c.detail)
		}
	}
	// A stream's failure follows the caller and the Provider's timeout.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	closed := &call{r: httptest.NewRequest("POST", "/openai/v1/chat/completions", nil).WithContext(gone)}
	if f := closed.bridgeFailure(t.Context(), &bridge.Error{Code: bridge.StreamFailed, Err: cause}); f.code != ClientClosed {
		t.Errorf("caller gone: %s", f.code)
	}
	late, cancelLate := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelLate()
	if f := live.bridgeFailure(late, &bridge.Error{Code: bridge.StreamFailed, Err: cause}); f.code != CodeUpstreamTimeout {
		t.Errorf("timeout: %s", f.code)
	}
}

// TestWriteError: a failure writing to the caller renders and unwraps.
func TestWriteError(t *testing.T) {
	we := &writeError{err: errors.New("pipe")}
	if we.Error() != "writing to the caller: pipe" || !errors.Is(we, we.err) {
		t.Error("writeError")
	}
}
