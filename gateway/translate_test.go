// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	v1 "latere.ai/x/lux/manifest/v1"
)

func TestCodecsPerRoute(t *testing.T) {
	for _, op := range []operation{opChatCompletions, opResponses, opMessages, opAnthropicCount, opGenerate, opLuxCount} {
		if frontendFor(op) == nil {
			t.Errorf("no frontend for operation %d", op)
		}
	}
	for _, op := range []operation{opEmbeddings, opGeminiGenerate, opModelsList, opOpaque, opNone} {
		if frontendFor(op) != nil {
			t.Errorf("a frontend for operation %d", op)
		}
	}
	m := &v1.Model{}
	m.Spec.MaxOutputTokens = 256
	if be, responses := backendFor(v1.DialectOpenAI, m, "gpt-5"); be == nil || !responses || be.Name() != ir.DialectOpenAIResponses {
		t.Error("gpt-5 is not served on /responses")
	}
	if be, responses := backendFor(v1.DialectOpenAI, m, "gpt-4.1"); be == nil || responses || be.Name() != ir.DialectOpenAIChat {
		t.Error("gpt-4.1 is not served on /chat/completions")
	}
	be, _ := backendFor(v1.DialectAnthropic, m, "claude")
	body, err := be.EncodeRequest(&ir.Request{Model: "claude", Messages: []ir.Message{{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "x"}}}}})
	if err != nil || !strings.Contains(string(body), `"max_tokens":256`) {
		t.Errorf("the Model's maxOutputTokens is not the codec's default: %s %v", body, err)
	}
	be, _ = backendFor(v1.DialectAnthropic, nil, "claude")
	body, _ = be.EncodeRequest(&ir.Request{Model: "claude", Messages: []ir.Message{{Role: ir.RoleUser, Blocks: []ir.Block{{Type: ir.BlockText, Text: "x"}}}}})
	if !strings.Contains(string(body), `"max_tokens":4096`) {
		t.Errorf("no Model falls back to the codec's 4096: %s", body)
	}
	if be, _ := backendFor(v1.DialectLux, m, "x"); be == nil || be.Name() != ir.DialectLux {
		t.Error("no lux backend")
	}
	if be, _ := backendFor(v1.DialectGemini, m, "x"); be != nil {
		t.Error("a gemini backend exists")
	}
	if be, _ := backendFor("", m, "x"); be != nil {
		t.Error("a backend for no dialect")
	}
	if lossHeader("Anthropic-Beta") != "header.anthropic-beta" {
		t.Error("lossHeader")
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

// TestResponseEvents re-emits a whole response as the event grammar,
// one block each of text, thinking with a signature, a tool use, and a
// block no dialect streams.
func TestResponseEvents(t *testing.T) {
	resp := &ir.Response{
		ID: "r", Model: "m", StopReason: ir.StopToolUse, StopSequence: "",
		Usage: ir.Usage{InputTokens: 1, OutputTokens: 2},
		Blocks: []ir.Block{
			{Type: ir.BlockText, Text: "hi"},
			{Type: ir.BlockThinking, Text: "hm", Signature: "sig"},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t", Name: "f", Args: json.RawMessage(`{"a":1}`)}},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "t2", Name: "g"}},
			{Type: ir.BlockRedactedThinking, Redacted: "x"},
		},
	}
	events := responseEvents(resp)
	var types []string
	for _, ev := range events {
		types = append(types, string(ev.Type))
	}
	want := "message_start block_start text_delta block_stop block_start thinking_delta signature_delta block_stop block_start args_delta block_stop block_start block_stop block_start block_stop message_delta message_stop"
	if got := strings.Join(types, " "); got != want {
		t.Errorf("events\n got %s\nwant %s", got, want)
	}
	last := events[len(events)-2]
	if last.Usage == nil || *last.Usage != resp.Usage || last.StopReason != ir.StopToolUse {
		t.Errorf("message_delta %+v", last)
	}
	if events[0].ID != "r" || events[0].Model != "m" {
		t.Errorf("message_start %+v", events[0])
	}
}

func TestRewriteFrame(t *testing.T) {
	frame := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"up\"}}\r\n: comment\ndata:{\"model\":\"up\"}\n\n"
	want := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"name\"}}\r\n: comment\ndata: {\"model\":\"name\"}\n\n"
	if got := string(rewriteFrame([]byte(frame), "name")); got != want {
		t.Errorf("rewriteFrame\n got %q\nwant %q", got, want)
	}
	if got := string(rewriteFrame([]byte("data: [DONE]\n\n"), "name")); got != "data: [DONE]\n\n" {
		t.Errorf("[DONE] %q", got)
	}
	we := &writeError{err: errors.New("pipe")}
	if we.Error() != "writing to the caller: pipe" || !errors.Is(we, we.err) {
		t.Error("writeError")
	}
}

func TestRemoveMember(t *testing.T) {
	cases := []struct{ in, key, want string }{
		{`{"a":1,"max_tokens":5,"b":2}`, "max_tokens", `{"a":1,"b":2}`},
		{`{"max_tokens":5,"b":2}`, "max_tokens", `{"b":2}`},
		{`{"a":1,"max_tokens":5}`, "max_tokens", `{"a":1}`},
		{`{"a":1, "max_tokens" : {"x":[1]} }`, "max_tokens", `{"a":1 }`},
		{`{"max_tokens":5}`, "max_tokens", `{}`},
		{`{"a":1}`, "max_tokens", `{"a":1}`},
		{`[1]`, "max_tokens", `[1]`},
	}
	for _, c := range cases {
		got := string(removeMember([]byte(c.in), c.key))
		if got != c.want {
			t.Errorf("removeMember(%s, %s) = %s, want %s", c.in, c.key, got, c.want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("removeMember(%s) = %s is not valid JSON", c.in, got)
		}
	}
}
