// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"strconv"
	"strings"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/llmdialect/openaichat"
	"latere.ai/x/pkg/llmdialect/openairesp"

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

// frontendFor is the door's codec for a translated route: the caller
// side of the translation.
func frontendFor(op operation) llmdialect.Frontend {
	switch op {
	case opChatCompletions:
		return openaichat.NewFrontend()
	case opResponses:
		return openairesp.NewFrontend()
	case opMessages, opAnthropicCount:
		return anthropic.NewFrontend()
	case opGenerate, opLuxCount:
		return lux.NewFrontend()
	case opEmbeddings, opGeminiGenerate, opGeminiStream, opGeminiCount, opGeminiEmbed, opModelsList, opModelsRead, opOpaque, opNone:
		return nil
	}
	return nil
}

// backendFor is the target's codec for a translated route, with the
// options spec 004's table sets: DefaultMaxTokens is the Model's
// maxOutputTokens when set and the codec's 4096 otherwise; DropSampling
// is false, because the gateway carries no table of which models refuse
// a sampling parameter; UseMaxCompletionTokens follows the reasoning
// family predicate. responses reports that an openai target is reached
// on /responses rather than /chat/completions.
func backendFor(target v1.Dialect, m *v1.Model, upstream string) (backend llmdialect.Backend, responses bool) {
	switch target {
	case v1.DialectOpenAI:
		if OpenAIReasoningFamily(upstream) {
			return openairesp.NewBackend(), true
		}
		return openaichat.NewBackend(openaichat.BackendOptions{}), false
	case v1.DialectAnthropic:
		var maxTokens int64
		if m != nil {
			maxTokens = int64(m.Spec.MaxOutputTokens)
		}
		return anthropic.NewBackend(anthropic.BackendOptions{DefaultMaxTokens: maxTokens}), false
	case v1.DialectLux:
		return lux.NewBackend(), false
	case v1.DialectGemini:
		return nil, false
	}
	return nil, false
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
// translation.
func lossHeader(name string) ir.LossField {
	return ir.LossField("header." + strings.ToLower(name))
}

// usageParts is the usage of a translated response or stream as the
// codecs report it, each member a pointer so a member that was reported
// is told from one that was not. It is read by respondWhole's and
// streamTranslated's translate arms until those call the bridge, whose
// Response and Stream return the same reading; step 6 of spec 021
// deletes it.
type usageParts struct {
	prompt, completion, cached, cacheWrite, reasoning *int64
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// tokens folds the parts into the record's block. ok is false when no
// usage member was reported at all.
func (p usageParts) tokens() (Tokens, bool) {
	if p.prompt == nil && p.completion == nil && p.cached == nil && p.cacheWrite == nil && p.reasoning == nil {
		return Tokens{}, false
	}
	return Tokens{
		Input:       deref(p.prompt),
		Output:      deref(p.completion),
		CachedInput: deref(p.cached),
		CacheWrite:  deref(p.cacheWrite),
		Reasoning:   deref(p.reasoning),
	}, true
}

// fromIR merges an IR usage, member-wise: a translated stream reports
// usage on message_start and on message_delta, and a member reported
// later replaces one reported earlier.
func (p *usageParts) fromIR(u *ir.Usage) {
	if u == nil {
		return
	}
	in, out, cached, write, reasoning := u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens, u.ReasoningTokens
	if in > 0 || p.prompt == nil {
		p.prompt = &in
	}
	if out > 0 || p.completion == nil {
		p.completion = &out
	}
	if cached > 0 || p.cached == nil {
		p.cached = &cached
	}
	if write > 0 || p.cacheWrite == nil {
		p.cacheWrite = &write
	}
	if reasoning > 0 || p.reasoning == nil {
		p.reasoning = &reasoning
	}
}

// responseEvents re-emits a whole response as the event sequence a
// stream would have carried, for a target that answered a stream request
// with one JSON body: message_start, one block with its deltas per
// block, message_delta with the usage, message_stop.
func responseEvents(resp *ir.Response) []ir.Event {
	events := []ir.Event{{Type: ir.EventMessageStart, ID: resp.ID, Model: resp.Model}}
	for i, b := range resp.Blocks {
		header := ir.Block{Type: b.Type}
		if b.ToolUse != nil {
			header.ToolUse = &ir.ToolUse{ID: b.ToolUse.ID, Name: b.ToolUse.Name}
		}
		events = append(events, ir.Event{Type: ir.EventBlockStart, Index: i, Block: &header})
		switch b.Type {
		case ir.BlockText:
			events = append(events, ir.Event{Type: ir.EventTextDelta, Index: i, Delta: b.Text, LogProbs: resp.LogProbs})
		case ir.BlockThinking:
			events = append(events, ir.Event{Type: ir.EventThinkingDelta, Index: i, Delta: b.Text})
			if b.Signature != "" {
				events = append(events, ir.Event{Type: ir.EventSignatureDelta, Index: i, Delta: b.Signature})
			}
		case ir.BlockToolUse:
			if len(b.ToolUse.Args) > 0 {
				events = append(events, ir.Event{Type: ir.EventArgsDelta, Index: i, Delta: string(b.ToolUse.Args)})
			}
		case ir.BlockImage, ir.BlockToolResult, ir.BlockRedactedThinking:
			// Not an output block any dialect streams; the header alone
			// is emitted so the block count stays the response's.
		}
		events = append(events, ir.Event{Type: ir.EventBlockStop, Index: i})
	}
	usage := resp.Usage
	events = append(events,
		ir.Event{Type: ir.EventMessageDelta, StopReason: resp.StopReason, StopSequence: resp.StopSequence, Usage: &usage},
		ir.Event{Type: ir.EventMessageStop},
	)
	return events
}
