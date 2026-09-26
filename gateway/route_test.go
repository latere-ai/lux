// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// tableRows is every row of the door table as a request, with the class,
// the operation, and the model the path carries where the table says
// path.
var tableRows = []struct {
	method, path string
	door         v1.Dialect
	class        RouteClass
	op           operation
	model        string
	template     string
}{
	{"POST", "/openai/v1/chat/completions", v1.DialectOpenAI, ClassTranslated, opChatCompletions, "", "/openai/v1/chat/completions"},
	{"POST", "/openai/v1/responses", v1.DialectOpenAI, ClassTranslated, opResponses, "", "/openai/v1/responses"},
	{"POST", "/openai/v1/embeddings", v1.DialectOpenAI, ClassModel, opEmbeddings, "", "/openai/v1/embeddings"},
	{"GET", "/openai/v1/models", v1.DialectOpenAI, ClassServed, opModelsList, "", "/openai/v1/models"},
	{"GET", "/openai/v1/models/gpt-5", v1.DialectOpenAI, ClassServed, opModelsRead, "gpt-5", "/openai/v1/models/{model}"},
	{"GET", "/openai/v1/models/openai/gpt-5", v1.DialectOpenAI, ClassServed, opModelsRead, "openai/gpt-5", "/openai/v1/models/{model}"},
	{"POST", "/openai/v1/files", v1.DialectOpenAI, ClassOpaque, opOpaque, "", "/openai/v1/*"},
	{"GET", "/openai/v1/batches/b1", v1.DialectOpenAI, ClassOpaque, opOpaque, "", "/openai/v1/*"},
	{"POST", "/anthropic/v1/messages", v1.DialectAnthropic, ClassTranslated, opMessages, "", "/anthropic/v1/messages"},
	{"POST", "/anthropic/v1/messages/count_tokens", v1.DialectAnthropic, ClassModel, opAnthropicCount, "", "/anthropic/v1/messages/count_tokens"},
	{"GET", "/anthropic/v1/models", v1.DialectAnthropic, ClassServed, opModelsList, "", "/anthropic/v1/models"},
	{"GET", "/anthropic/v1/models/claude", v1.DialectAnthropic, ClassServed, opModelsRead, "claude", "/anthropic/v1/models/{model}"},
	{"POST", "/anthropic/v1/messages/batches", v1.DialectAnthropic, ClassOpaque, opOpaque, "", "/anthropic/v1/*"},
	{"POST", "/gemini/v1beta/models/gemini-pro:generateContent", v1.DialectGemini, ClassModel, opGeminiGenerate, "gemini-pro", "/gemini/v1beta/models/{model}:generateContent"},
	{"POST", "/gemini/v1beta/models/gemini-pro:streamGenerateContent", v1.DialectGemini, ClassModel, opGeminiStream, "gemini-pro", "/gemini/v1beta/models/{model}:streamGenerateContent"},
	{"POST", "/gemini/v1beta/models/gemini-pro:countTokens", v1.DialectGemini, ClassModel, opGeminiCount, "gemini-pro", "/gemini/v1beta/models/{model}:countTokens"},
	{"POST", "/gemini/v1beta/models/embed-1:embedContent", v1.DialectGemini, ClassModel, opGeminiEmbed, "embed-1", "/gemini/v1beta/models/{model}:embedContent"},
	{"GET", "/gemini/v1beta/models", v1.DialectGemini, ClassServed, opModelsList, "", "/gemini/v1beta/models"},
	{"GET", "/gemini/v1beta/models/gemini-pro", v1.DialectGemini, ClassServed, opModelsRead, "gemini-pro", "/gemini/v1beta/models/{model}"},
	{"POST", "/gemini/v1beta/files", v1.DialectGemini, ClassOpaque, opOpaque, "", "/gemini/*"},
	{"GET", "/gemini/v1/files/f1", v1.DialectGemini, ClassOpaque, opOpaque, "", "/gemini/*"},
	{"POST", "/lux/v1/generate", v1.DialectLux, ClassTranslated, opGenerate, "", "/lux/v1/generate"},
	{"POST", "/lux/v1/count_tokens", v1.DialectLux, ClassModel, opLuxCount, "", "/lux/v1/count_tokens"},
	{"GET", "/lux/v1/models", v1.DialectLux, ClassServed, opModelsList, "", "/lux/v1/models"},
	{"GET", "/lux/v1/models/m", v1.DialectLux, ClassServed, opModelsRead, "m", "/lux/v1/models/{model}"},
}

// offTable is ten requests the table does not list, each not_found.
var offTable = []struct {
	method, path string
	door         v1.Dialect
}{
	{"GET", "/openai/v1/chat/completions", v1.DialectOpenAI},
	{"POST", "/openai/v1/models", v1.DialectOpenAI},
	{"POST", "/openai/v2/chat/completions", v1.DialectOpenAI},
	{"GET", "/openai", v1.DialectOpenAI},
	{"DELETE", "/anthropic/v1/messages", v1.DialectAnthropic},
	{"GET", "/anthropic/v1/models/", v1.DialectAnthropic},
	{"GET", "/gemini/v1beta/models/gemini-pro:generateContent", v1.DialectGemini},
	{"POST", "/gemini/v2/models/x:generateContent", v1.DialectGemini},
	{"POST", "/lux/v1/files", v1.DialectLux},
	{"GET", "/lux/v1/generate", v1.DialectLux},
	{"POST", "/v1/chat/completions", ""},
	{"GET", "/", ""},
	{"GET", "/openaix/v1/models", ""},
}

func TestMatch(t *testing.T) {
	for _, row := range tableRows {
		rt, f := match(row.method, row.path)
		if f != nil {
			t.Errorf("%s %s: %v", row.method, row.path, f)
			continue
		}
		if rt.door != row.door || rt.class != row.class || rt.op != row.op || rt.model != row.model || rt.template != row.template {
			t.Errorf("%s %s: got door=%s class=%s op=%d model=%q template=%q", row.method, row.path, rt.door, rt.class, rt.op, rt.model, rt.template)
		}
		if rt.class == ClassOpaque && rt.rest == "" {
			t.Errorf("%s %s: an opaque route carries no rest", row.method, row.path)
		}
	}
	for _, row := range offTable {
		rt, f := match(row.method, row.path)
		if f == nil {
			t.Errorf("%s %s: matched %+v, want not_found", row.method, row.path, rt)
			continue
		}
		if f.code != CodeNotFound {
			t.Errorf("%s %s: code %s", row.method, row.path, f.code)
		}
		if d, _ := door(row.path); d != row.door {
			t.Errorf("%s %s: door %q, want %q", row.method, row.path, d, row.door)
		}
	}
}

// TestRouteTemplate: the exported template is the table's for every row
// and empty for every request the table does not list, so a listener in
// front of the doors labels a request with the route the handler records
// and never with the caller's path.
func TestRouteTemplate(t *testing.T) {
	for _, row := range tableRows {
		if got := RouteTemplate(row.method, row.path); got != row.template {
			t.Errorf("RouteTemplate(%s, %s) = %q, want %q", row.method, row.path, got, row.template)
		}
	}
	for _, row := range offTable {
		if got := RouteTemplate(row.method, row.path); got != "" {
			t.Errorf("RouteTemplate(%s, %s) = %q, want empty", row.method, row.path, got)
		}
	}
}

func TestRouteHelpers(t *testing.T) {
	for _, row := range tableRows {
		rt, _ := match(row.method, row.path)
		fromBody := rt.modelFromBody()
		wantBody := rt.class != ClassServed && rt.class != ClassOpaque && rt.door != v1.DialectGemini
		if fromBody != wantBody {
			t.Errorf("%s: modelFromBody %v, want %v", row.path, fromBody, wantBody)
		}
		wantCount := rt.op == opAnthropicCount || rt.op == opLuxCount || rt.op == opGeminiCount
		if rt.count() != wantCount {
			t.Errorf("%s: count %v, want %v", row.path, rt.count(), wantCount)
		}
	}
	if (route{}).modelFromBody() || (route{}).count() {
		t.Error("the zero route reads a model or counts")
	}
	cases := []struct {
		target    v1.Dialect
		op        operation
		responses bool
		want      string
	}{
		{v1.DialectOpenAI, opMessages, false, "/chat/completions"},
		{v1.DialectOpenAI, opMessages, true, "/responses"},
		{v1.DialectAnthropic, opChatCompletions, false, "/messages"},
		{v1.DialectAnthropic, opLuxCount, false, "/messages/count_tokens"},
		{v1.DialectAnthropic, opAnthropicCount, false, "/messages/count_tokens"},
		{v1.DialectLux, opMessages, false, "/generate"},
		{v1.DialectGemini, opMessages, false, ""},
		{"", opMessages, false, ""},
	}
	for _, c := range cases {
		if got := upstreamPath(c.target, c.op, c.responses); got != c.want {
			t.Errorf("upstreamPath(%s, %d, %v) = %q, want %q", c.target, c.op, c.responses, got, c.want)
		}
	}
	paths := []struct {
		d          v1.Dialect
		rest, want string
	}{
		{v1.DialectOpenAI, "/v1/chat/completions", "/chat/completions"},
		{v1.DialectAnthropic, "/v1/messages", "/messages"},
		{v1.DialectGemini, "/v1beta/models/x:generateContent", "/models/x:generateContent"},
		{v1.DialectGemini, "/v1/files/f", "/files/f"},
		{v1.DialectOpenAI, "/other", "/other"},
	}
	for _, c := range paths {
		if got := passthroughPath(c.d, c.rest); got != c.want {
			t.Errorf("passthroughPath(%s, %q) = %q, want %q", c.d, c.rest, got, c.want)
		}
	}
	if versionPrefix(v1.DialectGemini) != "/v1beta" || versionPrefix(v1.DialectLux) != "/v1" {
		t.Error("versionPrefix")
	}
	if _, f := match("GET", "/lux"); f == nil {
		t.Error("the bare door path matched")
	}
}
