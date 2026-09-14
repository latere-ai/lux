// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"

	v1 "latere.ai/x/lux/manifest/v1"
)

// AnthropicVersion is the anthropic-version a translated request toward
// an anthropic target carries, because the Messages API requires the
// header and the codec sets none. A Provider whose headers name the
// header wins over it.
const AnthropicVersion = "2023-06-01"

// OpenAIReasoningFamily is spec 008's predicate over an openai target's
// upstream name: the part before any slash, compared case-insensitively,
// begins with o1, o3, or o4, or with gpt- followed by an integer of 5 or
// more. Those models are served on /responses and take
// max_completion_tokens; every other openai dialect upstream is served
// on /chat/completions with max_tokens.
func OpenAIReasoningFamily(name string) bool {
	name, _, _ = strings.Cut(strings.ToLower(name), "/")
	if strings.HasPrefix(name, "o1") || strings.HasPrefix(name, "o3") || strings.HasPrefix(name, "o4") {
		return true
	}
	rest, ok := strings.CutPrefix(name, "gpt-")
	if !ok {
		return false
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	n, err := strconv.Atoi(rest[:i])
	return err == nil && n >= 5
}

// wireOf is the bridge wire of a door or a target dialect: the API
// family whose shapes the bridge writes and reads for it. A path under
// no door renders the lux shapes, as spec 004 says of the envelope.
func wireOf(d v1.Dialect) bridge.Wire {
	switch d {
	case v1.DialectOpenAI:
		return bridge.WireOpenAI
	case v1.DialectAnthropic:
		return bridge.WireAnthropic
	case v1.DialectGemini:
		return bridge.WireGoogle
	case v1.DialectLux, "":
		return bridge.WireLux
	}
	return bridge.WireLux
}

// doorDialect is the codec dialect of the door's translated route, the
// caller side of a translation; the two count routes read their body
// with the same codec. A route with no codec has none.
func doorDialect(op operation) ir.Dialect {
	switch op {
	case opChatCompletions:
		return ir.DialectOpenAIChat
	case opResponses:
		return ir.DialectOpenAIResponses
	case opMessages, opAnthropicCount:
		return ir.DialectAnthropicMessages
	case opGenerate, opLuxCount:
		return ir.DialectLux
	case opEmbeddings, opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed, opModelsList, opModelsRead, opOpaque, opNone:
		return ""
	}
	return ""
}

// targetDialect is the codec dialect of a target, the upstream side of
// a translation. An openai target serves two: a name in the reasoning
// family is served on /responses, every other name on /chat/completions
// (spec 008). A gemini target has none.
func targetDialect(d v1.Dialect, upstream string) ir.Dialect {
	switch d {
	case v1.DialectOpenAI:
		if OpenAIReasoningFamily(upstream) {
			return ir.DialectOpenAIResponses
		}
		return ir.DialectOpenAIChat
	case v1.DialectAnthropic:
		return ir.DialectAnthropicMessages
	case v1.DialectLux:
		return ir.DialectLux
	case v1.DialectGemini, "":
		return ""
	}
	return ""
}

// bridgeFor opens the codec pair one translated attempt runs through,
// with the options spec 004's table sets: DefaultMaxTokens is the
// Model's maxOutputTokens when set and the codec's 4096 otherwise;
// DropSampling is false, because the gateway carries no table of which
// models refuse a sampling parameter; UseMaxCompletionTokens follows the
// reasoning family predicate. A pair the bridge cannot open is a bug,
// because bridgeable refused the target first; it is answered through
// bridgeFailure rather than a panic.
func (c *call) bridgeFor(ctx context.Context, t Target) (*bridge.Bridge, *failure) {
	var maxTokens int64
	if c.model != nil {
		maxTokens = int64(c.model.Spec.MaxOutputTokens)
	}
	b, err := bridge.Open(doorDialect(c.route.op), targetDialect(t.Provider.Spec.Dialect, t.Model), bridge.Options{
		DefaultMaxTokens:       maxTokens,
		UseMaxCompletionTokens: OpenAIReasoningFamily(t.Model),
	})
	if err != nil {
		return nil, c.bridgeFailure(ctx, err)
	}
	return b, nil
}

// bridgeFailure maps the bridge's seven codes onto spec 004's table, here
// and nowhere else. A request the door's codec cannot decode, whatever
// its refusal scope, or the target's cannot encode is invalid_request
// with the codec's words as the developer detail, because on this path
// the body is only ever sent translated. An upstream body the codecs
// cannot read or write back is upstream_error. A stream that failed is
// classified as any other cut after the first byte: client_closed when
// the caller is gone, upstream_timeout when the Provider's timeout
// passed, upstream_error otherwise. A write to the caller that failed is
// client_closed, because there is nobody left to answer. Unsupported
// cannot reach a caller, because bridgeable refuses first, and is
// dialect_unsupported so that a bug in the route rule is still a fixed
// code. An error that is not the bridge's is upstream_error.
func (c *call) bridgeFailure(ctx context.Context, err error) *failure {
	var e *bridge.Error
	if !errors.As(err, &e) {
		return fail(CodeUpstreamError, err.Error())
	}
	switch e.Code {
	case bridge.DecodeRequest, bridge.EncodeRequest:
		return fail(CodeInvalidRequest, e.Detail)
	case bridge.DecodeResponse:
		return fail(CodeUpstreamError, "decoding the upstream response: "+e.Detail)
	case bridge.EncodeResponse:
		return fail(CodeUpstreamError, "encoding the response for the door: "+e.Detail)
	case bridge.StreamFailed:
		cause := e.Unwrap()
		if cause == nil {
			cause = e
		}
		return c.streamFailure(ctx, cause)
	case bridge.WriteFailed:
		return fail(ClientClosed, "")
	case bridge.Unsupported:
		return fail(CodeDialectUnsupported, e.Detail)
	}
	return fail(CodeUpstreamError, e.Error())
}

// translatedRoute reports whether the route has a codec, so a door and a
// target of different dialects can be bridged on it.
func (r route) translated() bool { return r.class == ClassTranslated }

// bridgeable reports whether a request on this route can reach a target
// of the given dialect: equal dialects always, a translated route between
// any two dialects with a codec, and a count on the /anthropic or /lux
// door toward any dialect, because it is answered from the estimate when
// the target cannot count. The gemini door and a gemini target are never
// translated, because llmdialect has no Gemini codec.
func (r route) bridgeable(target v1.Dialect) bool {
	if r.door == target {
		return true
	}
	if r.door == v1.DialectGemini || target == v1.DialectGemini {
		return r.op == opAnthropicCount || r.op == opLuxCount
	}
	return r.translated() || r.op == opAnthropicCount || r.op == opLuxCount
}

// dialectHeaders are the request headers of one dialect's API. They are
// forwarded on passthrough and dropped on translation with a loss entry
// naming the header, because they describe a request the target does not
// receive.
var dialectHeaders = []string{
	"anthropic-version", "anthropic-beta", "OpenAI-Beta", "OpenAI-Organization", "OpenAI-Project", "x-goog-api-client",
}

// lossHeader is the loss entry for a dialect header dropped on
// translation, a field path the bridge adds to the codecs' report.
func lossHeader(name string) string {
	return "header." + strings.ToLower(name)
}
