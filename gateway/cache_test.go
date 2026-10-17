// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// An Anthropic model served on Chat Completions by a gateway that names
// it under the anthropic vendor, as OpenRouter does, and that gateway's
// answer to a request which wrote the provider's prompt cache: the
// written tokens are counted in prompt_tokens and named apart.
const (
	gatewayClaude         = "anthropic/claude-sonnet-5.5"
	gatewayCacheWriteJSON = `{"id":"gen-1","object":"chat.completion","created":1,"model":"anthropic/claude-sonnet-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":26799,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":26797}}}`
)

// agentMessages is a Messages request as an agent loop sends it, with a
// breakpoint after the system prompt and one after the latest tool
// result.
func agentMessages(model string) string {
	return `{"model":"` + model + `","max_tokens":64,
		"system":[{"type":"text","text":"You are an agent.","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":"read a.txt"},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read","input":{"path":"a.txt"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"contents","cache_control":{"type":"ephemeral"}}]}
		]}`
}

// TestBreakpointsReachAnAnthropicModelBehindAnOpenAIGateway: the
// /anthropic door translates toward an openai Provider, and for an
// Anthropic model the breakpoints reach the Provider as cache_control,
// so the provider serves the conversation from its cache; the cache
// writes the answer reports are metered as cache writes.
func TestBreakpointsReachAnAnthropicModelBehindAnOpenAIGateway(t *testing.T) {
	w := newWorld(t)
	w.model("claude-routed", target("oai", gatewayClaude))
	w.openai.respondJSON(200, gatewayCacheWriteJSON)
	rec := w.post("/anthropic/v1/messages", agentMessages("claude-routed"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if loss := rec.Header().Get(HeaderLoss); strings.Contains(loss, "cache_control") {
		t.Errorf("Lux-Loss %q reports the breakpoints lost", loss)
	}
	var sent struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.openai.last(t).Body, &sent); err != nil {
		t.Fatal(err)
	}
	marked := map[string]bool{}
	for _, m := range sent.Messages {
		if strings.Contains(string(m.Content), `"cache_control":{"type":"ephemeral"}`) {
			marked[m.Role] = true
		}
	}
	if !marked["system"] || !marked["tool"] {
		t.Errorf("the provider received breakpoints on %v, want system and tool: %s", marked, w.openai.last(t).Body)
	}
	if got := w.recorder.last(t).Tokens; got != (Tokens{Input: 2, Output: 8, CacheWrite: 26797}) {
		t.Errorf("tokens %+v", got)
	}
}

// TestPassthroughMetersAnOpenAIGatewaysCacheWrites: a Chat Completions
// request on the /openai door to an openai Provider is sent as it is,
// its cache_control included, and the cache writes in the answer are
// metered as cache writes, not as fresh input.
func TestPassthroughMetersAnOpenAIGatewaysCacheWrites(t *testing.T) {
	w := newWorld(t)
	w.model(gatewayClaude, target("oai", gatewayClaude))
	w.openai.respondJSON(200, gatewayCacheWriteJSON)
	body := `{"model":"` + gatewayClaude + `","messages":[{"role":"system","content":[{"type":"text","text":"You are an agent.","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"hi"}]}`
	rec := w.do(w.request(http.MethodPost, "/openai/v1/chat/completions", body))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := string(w.openai.last(t).Body); !strings.Contains(got, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("the provider received %s", got)
	}
	if got := w.recorder.last(t).Tokens; got != (Tokens{Input: 2, Output: 8, CacheWrite: 26797}) {
		t.Errorf("tokens %+v", got)
	}
}

// TestBreakpointsStayOffOtherOpenAIModels: OpenAI refuses an unknown
// member, so a model not named under the anthropic vendor gets no
// cache_control and the breakpoints are reported lost.
func TestBreakpointsStayOffOtherOpenAIModels(t *testing.T) {
	w := newWorld(t)
	w.openai.respondJSON(200, openaiChatResponse)
	rec := w.post("/anthropic/v1/messages", agentMessages("gpt"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := string(w.openai.last(t).Body); strings.Contains(got, "cache_control") {
		t.Errorf("the provider received %s", got)
	}
	if loss := rec.Header().Get(HeaderLoss); !strings.Contains(loss, "cache_control") {
		t.Errorf("Lux-Loss %q does not report the breakpoints lost", loss)
	}
}
