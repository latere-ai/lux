// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestBodyUsagePerDialect reads each dialect's usage members off one
// body, with input excluding cached input on every dialect.
func TestBodyUsagePerDialect(t *testing.T) {
	cases := []struct {
		d    v1.Dialect
		body string
		want Tokens
		ok   bool
	}{
		{v1.DialectOpenAI, openaiChatResponse, Tokens{Input: 10, Output: 3, CachedInput: 2}, true},
		{v1.DialectOpenAI, `{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":2}}}`, Tokens{Input: 0, CachedInput: 5, Reasoning: 2}, true},
		{v1.DialectAnthropic, anthropicResponse, Tokens{Input: 10, Output: 4, CachedInput: 1, CacheWrite: 5}, true},
		{v1.DialectGemini, geminiResponse, Tokens{Input: 6, Output: 2, CachedInput: 3}, true},
		{v1.DialectGemini, `{"usageMetadata":{"promptTokenCount":9,"thoughtsTokenCount":4}}`, Tokens{Input: 9, Reasoning: 4}, true},
		{v1.DialectLux, luxResponse, Tokens{Input: 7, Output: 1, Reasoning: 1}, true},
		{v1.DialectLux, `{"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_write_input_tokens":4}}`, Tokens{Input: 1, Output: 2, CachedInput: 3, CacheWrite: 4}, true},
		{v1.DialectOpenAI, `{"choices":[]}`, Tokens{}, false},
		{v1.DialectOpenAI, `not json`, Tokens{}, false},
		{v1.DialectAnthropic, `{"usage":{"prompt_tokens":5}}`, Tokens{}, false},
		{"", `{"usage":{"input_tokens":5}}`, Tokens{}, false},
	}
	for _, c := range cases {
		got, ok := bodyUsage(c.d, []byte(c.body))
		if ok != c.ok || got != c.want {
			t.Errorf("%s %s: %+v %v, want %+v %v", c.d, c.body, got, ok, c.want, c.ok)
		}
	}
}

// TestSSESniffer feeds a stream in awkward chunks and reads the last
// value of each member, with a final frame the stream did not terminate.
func TestSSESniffer(t *testing.T) {
	s := newSSESniffer(v1.DialectOpenAI)
	stream := "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\r\n\r\n: comment\n\ndata: {\"usage\":\ndata: {\"completion_tokens\":4}}\n\ndata: [DONE]\n\ndata: {\"usage\":{\"completion_tokens\":9}}"
	for i := 0; i < len(stream); i += 7 {
		end := min(i+7, len(stream))
		if _, err := s.Write([]byte(stream[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s.Close() // idempotent
	got, ok := s.Tokens()
	if !ok || got != (Tokens{Input: 10, Output: 9}) {
		t.Errorf("%+v %v", got, ok)
	}
	empty := newSSESniffer(v1.DialectAnthropic)
	_, _ = empty.Write([]byte("event: ping\ndata: {}\n\n"))
	empty.Close()
	if _, ok := empty.Tokens(); ok {
		t.Error("a stream without usage reported some")
	}
	if end, size := frameEnd([]byte("a\r\n\r\nb\n\n")); end != 1 || size != 4 {
		t.Errorf("frameEnd crlf: %d %d", end, size)
	}
	if end, size := frameEnd([]byte("a\n\nb\r\n\r\n")); end != 1 || size != 2 {
		t.Errorf("frameEnd lf: %d %d", end, size)
	}
}

// TestJSONSniffer reads a Gemini array element by element across chunk
// boundaries, strings with brackets and escapes included, and one object
// that is not an array.
func TestJSONSniffer(t *testing.T) {
	s := newJSONSniffer(v1.DialectGemini)
	array := "  [ {\"text\":\"a [ } \\\" ] {\",\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":1}}\n, {\"nested\":[{\"x\":[1,2]}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":5}} , {\"usageMetadata\":{\"cachedContentTokenCount\":2}}]"
	for i := 0; i < len(array); i += 5 {
		end := min(i+5, len(array))
		_, _ = s.Write([]byte(array[i:end]))
	}
	s.Close()
	got, ok := s.Tokens()
	if !ok || got != (Tokens{Input: 1, Output: 5, CachedInput: 2}) {
		t.Errorf("%+v %v", got, ok)
	}
	one := newJSONSniffer(v1.DialectGemini)
	_, _ = one.Write([]byte(geminiResponse))
	if got, ok := one.Tokens(); !ok || got != (Tokens{Input: 6, Output: 2, CachedInput: 3}) {
		t.Errorf("one object: %+v %v", got, ok)
	}
	cut := newJSONSniffer(v1.DialectGemini)
	_, _ = cut.Write([]byte(`[{"usageMetadata":{"promptTokenCount":3}},{"usageMetadata":{"promptTokenCount":9}`))
	cut.Close()
	if got, _ := cut.Tokens(); got.Input != 3 {
		t.Errorf("a cut element was read: %+v", got)
	}
	none := newJSONSniffer(v1.DialectGemini)
	_, _ = none.Write([]byte(`  "just a string"`))
	if _, ok := none.Tokens(); ok {
		t.Error("a scalar body reported usage")
	}
}

// TestUsagePartsFromIR merges IR usage member by member: a later event's
// nonzero member replaces an earlier one, and a zero never erases a
// value already reported.
func TestUsagePartsFromIR(t *testing.T) {
	var p usageParts
	p.fromIR(nil)
	if _, ok := p.tokens(); ok {
		t.Error("nil usage reported something")
	}
	p.fromIR(&ir.Usage{InputTokens: 10, CacheReadInputTokens: 2})
	p.fromIR(&ir.Usage{OutputTokens: 5})
	p.fromIR(&ir.Usage{OutputTokens: 7, CacheWriteInputTokens: 1, ReasoningTokens: 3})
	got, ok := p.tokens()
	if !ok || got != (Tokens{Input: 10, Output: 7, CachedInput: 2, CacheWrite: 1, Reasoning: 3}) {
		t.Errorf("%+v %v", got, ok)
	}
	var zero usageParts
	zero.fromIR(&ir.Usage{})
	if got, ok := zero.tokens(); !ok || got != (Tokens{}) {
		t.Errorf("an all-zero usage is still a report: %+v %v", got, ok)
	}
}
