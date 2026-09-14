// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"encoding/json"

	"latere.ai/x/pkg/llmdialect/ir"

	v1 "latere.ai/x/lux/manifest/v1"
)

// usageParts is the usage members of one dialect as they appear on the
// wire, each a pointer so a member that was present is told from one
// that was not: the record takes the last value of each member across a
// stream, and only a member that appeared can be a last value.
type usageParts struct {
	prompt, completion, cached, cacheWrite, reasoning *int64
	subtractCached                                    bool // openai and gemini report a prompt total that includes cache reads
}

// wireUsage is every dialect's usage object in one struct, so one decode
// reads whichever members the frame carries.
type wireUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`

	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheWriteInputTokens    *int64 `json:"cache_write_input_tokens"`
	ReasoningTokens          *int64 `json:"reasoning_tokens"`

	PromptTokenCount        *int64 `json:"promptTokenCount"`
	CandidatesTokenCount    *int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      *int64 `json:"thoughtsTokenCount"`
}

// wireEnvelope is where a usage object sits: usage on a body or a frame,
// message.usage on an Anthropic message_start, usageMetadata on Gemini.
type wireEnvelope struct {
	Usage         *wireUsage `json:"usage"`
	UsageMetadata *wireUsage `json:"usageMetadata"`
	Message       *struct {
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
}

// merge takes every member present in data into p, so a later frame's
// value replaces an earlier one member by member.
func (p *usageParts) merge(d v1.Dialect, data []byte) {
	var env wireEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	for _, u := range []*wireUsage{env.Usage, env.UsageMetadata} {
		if u != nil {
			p.mergeUsage(d, u)
		}
	}
	if env.Message != nil && env.Message.Usage != nil {
		p.mergeUsage(d, env.Message.Usage)
	}
}

func set(dst **int64, src *int64) {
	if src != nil {
		*dst = src
	}
}

func (p *usageParts) mergeUsage(d v1.Dialect, u *wireUsage) {
	switch d {
	case v1.DialectOpenAI:
		p.subtractCached = true
		set(&p.prompt, u.PromptTokens)
		set(&p.completion, u.CompletionTokens)
		if u.PromptTokensDetails != nil {
			set(&p.cached, u.PromptTokensDetails.CachedTokens)
		}
		if u.CompletionTokensDetails != nil {
			set(&p.reasoning, u.CompletionTokensDetails.ReasoningTokens)
		}
	case v1.DialectAnthropic:
		set(&p.prompt, u.InputTokens)
		set(&p.completion, u.OutputTokens)
		set(&p.cached, u.CacheReadInputTokens)
		set(&p.cacheWrite, u.CacheCreationInputTokens)
	case v1.DialectGemini:
		p.subtractCached = true
		set(&p.prompt, u.PromptTokenCount)
		set(&p.completion, u.CandidatesTokenCount)
		set(&p.cached, u.CachedContentTokenCount)
		set(&p.reasoning, u.ThoughtsTokenCount)
	case v1.DialectLux:
		set(&p.prompt, u.InputTokens)
		set(&p.completion, u.OutputTokens)
		set(&p.cached, u.CacheReadInputTokens)
		set(&p.cacheWrite, u.CacheWriteInputTokens)
		set(&p.reasoning, u.ReasoningTokens)
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// tokens folds the parts into the record's block: input excludes cached
// input on every dialect, floored at zero. ok is false when no usage
// member appeared at all.
func (p usageParts) tokens() (Tokens, bool) {
	if p.prompt == nil && p.completion == nil && p.cached == nil && p.cacheWrite == nil && p.reasoning == nil {
		return Tokens{}, false
	}
	t := Tokens{
		Input:       deref(p.prompt),
		Output:      deref(p.completion),
		CachedInput: deref(p.cached),
		CacheWrite:  deref(p.cacheWrite),
		Reasoning:   deref(p.reasoning),
	}
	if p.subtractCached {
		t.Input = max(t.Input-t.CachedInput, 0)
	}
	return t, true
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

// sniffer reads usage off a stream as it is relayed.
type sniffer interface {
	Write(p []byte) (int, error)
	Close()
	Tokens() (Tokens, bool)
}

// sseSniffer reads usage off an SSE stream: every complete frame's data
// lines are joined and read as one JSON value.
type sseSniffer struct {
	d     v1.Dialect
	buf   []byte
	parts usageParts
}

func newSSESniffer(d v1.Dialect) *sseSniffer { return &sseSniffer{d: d} }

// Write consumes p and reads every frame it completes.
func (s *sseSniffer) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		end, size := frameEnd(s.buf)
		if end < 0 {
			return len(p), nil
		}
		s.frame(s.buf[:end])
		s.buf = s.buf[end+size:]
	}
}

// frameEnd finds the first blank line in b: the index where the frame
// ends and the size of the separator, or -1.
func frameEnd(b []byte) (int, int) {
	lf := bytes.Index(b, []byte("\n\n"))
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case crlf >= 0 && (lf < 0 || crlf < lf):
		return crlf, 4
	default:
		return lf, 2
	}
}

// Close reads a final frame the stream ended without terminating.
func (s *sseSniffer) Close() {
	if len(bytes.TrimSpace(s.buf)) > 0 {
		s.frame(s.buf)
		s.buf = nil
	}
}

// Tokens is the last value of each member across the stream.
func (s *sseSniffer) Tokens() (Tokens, bool) { return s.parts.tokens() }

func (s *sseSniffer) frame(frame []byte) {
	if data := frameData(frame); len(data) > 0 {
		s.parts.merge(s.d, data)
	}
}

// frameData joins the data lines of one SSE frame with \n, as the SSE
// rule says.
func frameData(frame []byte) []byte {
	var data [][]byte
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(rest, []byte(" ")))
		}
	}
	return bytes.Join(data, []byte("\n"))
}

// jsonSniffer reads usage off a JSON body relayed in chunks: one object,
// or a Gemini :streamGenerateContent array whose elements are read one
// at a time as each closes, so the body is never held whole.
type jsonSniffer struct {
	d                v1.Dialect
	parts            usageParts
	buf              []byte
	depth            int
	inString, escape bool
	started, inElem  bool
}

func newJSONSniffer(d v1.Dialect) *jsonSniffer { return &jsonSniffer{d: d} }

// Write consumes p and reads every element it completes.
func (s *jsonSniffer) Write(p []byte) (int, error) {
	for _, c := range p {
		if !s.started {
			switch c {
			case '[':
				s.started = true
				continue
			case '{':
				s.started = true
			default:
				continue
			}
		}
		if s.inElem {
			s.buf = append(s.buf, c)
		}
		if s.inString {
			switch {
			case s.escape:
				s.escape = false
			case c == '\\':
				s.escape = true
			case c == '"':
				s.inString = false
			}
			continue
		}
		switch c {
		case '"':
			if s.inElem {
				s.inString = true
			}
		case '{', '[':
			if !s.inElem {
				s.inElem = true
				s.buf = append(s.buf[:0], c)
			}
			s.depth++
		case '}', ']':
			if s.inElem {
				s.depth--
				if s.depth == 0 {
					s.parts.merge(s.d, s.buf)
					s.inElem = false
				}
			}
		}
	}
	return len(p), nil
}

// Close is a no-op: an element the stream cut short is not read.
func (*jsonSniffer) Close() {}

// Tokens is the last value of each member across the elements.
func (s *jsonSniffer) Tokens() (Tokens, bool) { return s.parts.tokens() }

// bodyUsage reads the usage members off one whole body.
func bodyUsage(d v1.Dialect, body []byte) (Tokens, bool) {
	var p usageParts
	p.merge(d, body)
	return p.tokens()
}
