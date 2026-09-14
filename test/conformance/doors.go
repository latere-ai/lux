// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The doors group is spec 004 through each door the server lists: the
// route table by its refusals, the error envelope per dialect, the model
// list as the Key's view, dialect bridging, and, with stubs behind the
// Providers, same-dialect same-bytes, the caller's credentials never
// forwarded, the loss report, streaming, the name rewrite, the upstream
// failures, the timeout, spec 008's fallback, and the opaque route.
var doorsCases = []testCase{
	{group: "doors", name: "case004RouteTable", spec: 4, bearer: true, key: true, fn: case004RouteTable},
	{group: "doors", name: "case004ErrorEnvelopePerDialect", spec: 4, bearer: true, key: true, fn: case004ErrorEnvelopePerDialect},
	{group: "doors", name: "case004ModelsListIsTheKeysView", spec: 4, bearer: true, key: true, fn: case004ModelsListIsTheKeysView},
	{group: "doors", name: "case004DialectBridging", spec: 4, bearer: true, key: true, fn: case004DialectBridging},
	{group: "doors", name: "case004CredentialForms", spec: 4, bearer: true, key: true, fn: case004CredentialForms},
	{group: "doors", name: "case004CountTokens", spec: 4, bearer: true, key: true, fn: case004CountTokens},
	{group: "doors", name: "case004SameDialectSameBytes", spec: 4, bearer: true, key: true, stubs: true, fn: case004SameDialectSameBytes},
	{group: "doors", name: "case004CallerCredentialsNeverForwarded", spec: 4, bearer: true, key: true, stubs: true, fn: case004CallerCredentialsNeverForwarded},
	{group: "doors", name: "case004TranslationLoss", spec: 4, bearer: true, key: true, stubs: true, fn: case004TranslationLoss},
	{group: "doors", name: "case004Streaming", spec: 4, bearer: true, key: true, stubs: true, fn: case004Streaming},
	{group: "doors", name: "case004ModelNameRewrite", spec: 4, bearer: true, key: true, stubs: true, fn: case004ModelNameRewrite},
	{group: "doors", name: "case004UpstreamError", spec: 4, bearer: true, key: true, stubs: true, fn: case004UpstreamError},
	{group: "doors", name: "case004UpstreamTimeout", spec: 4, bearer: true, key: true, stubs: true, fn: case004UpstreamTimeout},
	{group: "doors", name: "case008Fallback", spec: 8, bearer: true, key: true, stubs: true, fn: case008Fallback},
	{group: "doors", name: "case004OpaqueRoute", spec: 4, bearer: true, key: true, stubs: true, fn: case004OpaqueRoute},
}

// modelsListTimeout bounds the wait for a Model to appear in a door's
// list, which needs the server's health tick to fill its availability.
const modelsListTimeout = 45 * time.Second

// chat is a chat completion request naming model.
func chat(model string, stream bool) map[string]any {
	b := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": "hello from the conformance suite"}}}
	if stream {
		b["stream"] = true
	}
	return b
}

// messages is a Messages API request naming model.
func messages(model string) map[string]any {
	return map[string]any{"model": model, "max_tokens": 32, "messages": []map[string]any{{"role": "user", "content": "hello from the conformance suite"}}}
}

// generate is a lux generate request naming model.
func generate(model string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "blocks": []map[string]any{{"type": "text", "text": "hello from the conformance suite"}}}}}
}

// gemini is a generateContent request.
func gemini() map[string]any {
	return map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hello from the conformance suite"}}}}}
}

// row is one row of the door route table as the suite drives it: a
// request that names an unknown Model, whose refusal proves the route
// dispatched to a class that reads the model from where the table says.
type row struct {
	dialect, method, path string
	body                  any
	code                  string // the refusal expected
}

// routeRows are spec 004's table for a Key without passthrough and a
// model no Model has: every translated and model route is
// model_not_found, every opaque route is route_not_allowed, and every
// off-table path or method is not_found.
func routeRows(nobody string) []row {
	return []row{
		{"openai", http.MethodPost, "/v1/chat/completions", chat(nobody, false), "model_not_found"},
		{"openai", http.MethodPost, "/v1/responses", map[string]any{"model": nobody, "input": "hi"}, "model_not_found"},
		{"openai", http.MethodPost, "/v1/embeddings", map[string]any{"model": nobody, "input": "hi"}, "model_not_found"},
		{"openai", http.MethodGet, "/v1/models/" + nobody, nil, "model_not_found"},
		{"openai", http.MethodPost, "/v1/files", map[string]any{"purpose": "x"}, "route_not_allowed"},
		{"openai", http.MethodGet, "/v1/chat/completions", nil, "not_found"},
		{"openai", http.MethodGet, "/v2/models", nil, "not_found"},
		{"anthropic", http.MethodPost, "/v1/messages", messages(nobody), "model_not_found"},
		{"anthropic", http.MethodPost, "/v1/messages/count_tokens", messages(nobody), "model_not_found"},
		{"anthropic", http.MethodGet, "/v1/models/" + nobody, nil, "model_not_found"},
		{"anthropic", http.MethodPost, "/v1/files", map[string]any{}, "route_not_allowed"},
		{"anthropic", http.MethodGet, "/v1/messages", nil, "not_found"},
		{"gemini", http.MethodPost, "/v1beta/models/" + nobody + ":generateContent", gemini(), "model_not_found"},
		{"gemini", http.MethodPost, "/v1beta/models/" + nobody + ":streamGenerateContent", gemini(), "model_not_found"},
		{"gemini", http.MethodPost, "/v1beta/models/" + nobody + ":countTokens", gemini(), "model_not_found"},
		{"gemini", http.MethodPost, "/v1beta/models/" + nobody + ":embedContent", map[string]any{"content": map[string]any{"parts": []map[string]any{{"text": "hi"}}}}, "model_not_found"},
		{"gemini", http.MethodGet, "/v1beta/models/" + nobody, nil, "model_not_found"},
		{"gemini", http.MethodPost, "/v1beta/files", map[string]any{}, "route_not_allowed"},
		{"gemini", http.MethodGet, "/v1beta/models/" + nobody + ":generateContent", nil, "not_found"},
		{"gemini", http.MethodGet, "/v2/models", nil, "not_found"},
		{"lux", http.MethodPost, "/v1/generate", generate(nobody), "model_not_found"},
		{"lux", http.MethodPost, "/v1/count_tokens", generate(nobody), "model_not_found"},
		{"lux", http.MethodGet, "/v1/models/" + nobody, nil, "model_not_found"},
		{"lux", http.MethodPost, "/v1/files", map[string]any{}, "not_found"},
		{"lux", http.MethodGet, "/v1/generate", nil, "not_found"},
		// A body that is not a JSON object, and one without a model, are
		// invalid_request before any Model is looked up.
		{"openai", http.MethodPost, "/v1/chat/completions", "[]", "invalid_request"},
		{"openai", http.MethodPost, "/v1/chat/completions", map[string]any{"messages": []any{}}, "invalid_request"},
		{"anthropic", http.MethodPost, "/v1/messages", map[string]any{"max_tokens": 1}, "invalid_request"},
		{"lux", http.MethodPost, "/v1/generate", "not json", "invalid_request"},
	}
}

// case004RouteTable drives every row of the door route table on every
// door the server lists, with a Model no Key has, and reads the refusal
// that proves the row dispatched as the table says.
func case004RouteTable(t testing.TB, c *client) {
	nobody := c.name("nobody")
	for _, r := range routeRows(nobody) {
		if !slices.Contains(c.well.Dialects, r.dialect) {
			continue
		}
		resp := c.door(t, r.dialect, r.method, r.path, r.body, c.key.value)
		if got := c.doorCode(t, r.dialect, resp); got != r.code {
			t.Errorf("%s %s %s: %s, want %s", r.dialect, r.method, r.path, got, r.code)
		}
	}
	for _, d := range c.well.Dialects {
		list := c.door(t, d, http.MethodGet, "/v1/models", nil, c.key.value)
		if d == "gemini" {
			list = c.door(t, d, http.MethodGet, "/v1beta/models", nil, c.key.value)
		}
		if list.Status != http.StatusOK {
			t.Errorf("%s GET models: %d %s", d, list.Status, excerpt(list.Body))
		}
	}
}

// case004ErrorEnvelopePerDialect: a refusal on each door is in that
// dialect's own error shape with the code in the shape's code member and
// in Lux-Error, for a missing credential and for a Model no Key has.
func case004ErrorEnvelopePerDialect(t testing.TB, c *client) {
	for _, d := range c.well.Dialects {
		prefix := "/v1"
		if d == "gemini" {
			prefix = "/v1beta"
		}
		c.expectDoor(t, d, c.door(t, d, http.MethodGet, prefix+"/models", nil, ""), "unauthenticated")
		c.expectDoor(t, d, c.door(t, d, http.MethodGet, prefix+"/models", nil, "lux_"+c.run+"-no-such-key"), "unauthenticated")
		resp := c.door(t, d, http.MethodGet, prefix+"/models/"+c.name("nobody"), nil, c.key.value)
		c.expectDoor(t, d, resp, "model_not_found")
		if resp.Header.Get(gateway.HeaderErrorDetail) == "" {
			t.Errorf("%s: no Lux-Error-Detail on a refusal", d)
		}
		for _, r := range []*response{resp, c.door(t, d, http.MethodGet, prefix+"/models", nil, c.key.value)} {
			if r.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s: a door answer without X-Content-Type-Options: nosniff", d)
			}
		}
	}
}

// case004CredentialForms: every door takes the Key from Authorization:
// Bearer, x-api-key, x-goog-api-key, and the query parameter key.
func case004CredentialForms(t testing.TB, c *client) {
	for _, d := range c.well.Dialects {
		for _, form := range []struct{ name, query string }{{"Authorization", ""}, {"x-api-key", ""}, {"x-goog-api-key", ""}, {"", "?key=" + url.QueryEscape(c.key.value)}} {
			var opts []reqOpt
			if form.name == "Authorization" {
				opts = append(opts, bearer(c.key.value))
			} else if form.name != "" {
				opts = append(opts, header(form.name, c.key.value))
			}
			resp := c.door(t, d, http.MethodGet, modelsPath(d)+form.query, nil, "", opts...)
			if resp.Status != http.StatusOK {
				t.Errorf("%s door with the Key in %s%s: %d %s", d, form.name, form.query, resp.Status, excerpt(resp.Body))
			}
		}
	}
}

// case004CountTokens: a token count no upstream answers, the lux count
// and the anthropic count toward an openai target, is answered from the
// estimate with Lux-Estimated: true and reaches no provider.
func case004CountTokens(t testing.TB, c *client) {
	for _, r := range []struct {
		dialect, path string
		body          any
	}{
		{"lux", "/v1/count_tokens", generate(c.name("gpt"))},
		{"anthropic", "/v1/messages/count_tokens", messages(c.name("gpt"))},
	} {
		if !slices.Contains(c.well.Dialects, r.dialect) {
			continue
		}
		resp := c.door(t, r.dialect, http.MethodPost, r.path, r.body, c.key.value)
		if resp.Status != http.StatusOK || resp.Header.Get(gateway.HeaderEstimated) != "true" {
			t.Errorf("%s count: %d Lux-Estimated %q %s", r.dialect, resp.Status, resp.Header.Get(gateway.HeaderEstimated), excerpt(resp.Body))
			continue
		}
		if n := num(resp.json(t), "input_tokens"); n <= 0 {
			t.Errorf("%s count answered %s", r.dialect, excerpt(resp.Body))
		}
	}
}

// listedNames reads a door's model list in the door's shape.
func listedNames(t testing.TB, dialect string, resp *response) []string {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("%s GET models: %d %s", dialect, resp.Status, excerpt(resp.Body))
	}
	body := resp.json(t)
	var names []string
	switch dialect {
	case "gemini":
		for _, m := range arr(body, "models") {
			names = append(names, strings.TrimPrefix(str(m, "name"), "models/"))
			if str(m, "displayName") == "" || len(arr(m, "supportedGenerationMethods")) == 0 {
				t.Errorf("gemini entry %v", m)
			}
		}
	case "anthropic":
		for _, m := range arr(body, "data") {
			names = append(names, str(m, "id"))
			if str(m, "type") != "model" || str(m, "display_name") != str(m, "id") || str(m, "created_at") == "" {
				t.Errorf("anthropic entry %v", m)
			}
		}
		if field(body, "has_more") != false {
			t.Errorf("anthropic has_more %v", body["has_more"])
		}
		if len(names) > 0 && (str(body, "first_id") != names[0] || str(body, "last_id") != names[len(names)-1]) {
			t.Errorf("anthropic first_id %q last_id %q for %v", str(body, "first_id"), str(body, "last_id"), names)
		}
	default:
		if str(body, "object") != "list" {
			t.Errorf("%s object %q", dialect, str(body, "object"))
		}
		for _, m := range arr(body, "data") {
			names = append(names, str(m, "id"))
			if str(m, "object") != "model" || str(m, "owned_by") != "lux" {
				t.Errorf("%s entry %v", dialect, m)
			}
		}
	}
	return names
}

// modelsPath is the door's model list path.
func modelsPath(dialect string) string {
	if dialect == "gemini" {
		return "/v1beta/models"
	}
	return "/v1/models"
}

// case004ModelsListIsTheKeysView: GET models on each door lists exactly
// the available Models the Key's selectors match, sorted, in the
// dialect's shape; the read answers one entry; a Key whose selectors
// name one Model lists that one and reads another as model_not_allowed.
func case004ModelsListIsTheKeysView(t testing.TB, c *client) {
	for _, d := range c.well.Dialects {
		c.eventually(t, modelsListTimeout, d+" door lists the Key's Models", func() (bool, string) {
			names := listedNames(t, d, c.door(t, d, http.MethodGet, modelsPath(d), nil, c.key.value))
			if slices.Equal(names, c.models) {
				return true, ""
			}
			return false, sprintf("listed %v, the Key's view is %v", names, c.models)
		})
		one := c.door(t, d, http.MethodGet, modelsPath(d)+"/"+c.name("gpt"), nil, c.key.value)
		if one.Status != http.StatusOK || !strings.Contains(string(one.Body), c.name("gpt")) {
			t.Errorf("%s read: %d %s", d, one.Status, excerpt(one.Body))
		}
	}
	narrow := c.object(t, v1.KindKey, "narrow", map[string]any{"models": []string{c.name("gpt")}})
	defer c.mustDelete(t, v1.KindKey, str(narrow, "status.id"))
	value := str(narrow, "status.value")
	if names := listedNames(t, "openai", c.door(t, "openai", http.MethodGet, "/v1/models", nil, value)); !slices.Equal(names, []string{c.name("gpt")}) {
		t.Errorf("a Key naming one Model lists %v", names)
	}
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodGet, "/v1/models/"+c.name("claude"), nil, value), "model_not_allowed")
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("claude"), false), value), "model_not_allowed")
}

// case004DialectBridging: a gemini door to a non-gemini target and a
// non-gemini door to a gemini target are dialect_unsupported, before any
// provider is reached.
func case004DialectBridging(t testing.TB, c *client) {
	if slices.Contains(c.well.Dialects, "gemini") && slices.Contains(c.well.Dialects, "openai") {
		c.expectDoor(t, "gemini", c.door(t, "gemini", http.MethodPost, "/v1beta/models/"+c.name("gpt")+":generateContent", gemini(), c.key.value), "dialect_unsupported")
		c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("gemini"), false), c.key.value), "dialect_unsupported")
		c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/embeddings", map[string]any{"model": c.name("claude"), "input": "hi"}, c.key.value), "dialect_unsupported")
	}
	if slices.Contains(c.well.Dialects, "gemini") && slices.Contains(c.well.Dialects, "lux") {
		c.expectDoor(t, "lux", c.door(t, "lux", http.MethodPost, "/v1/generate", generate(c.name("gemini")), c.key.value), "dialect_unsupported")
	}
}

// case004SameDialectSameBytes: a request through the openai door to an
// openai target whose upstream name equals the Model's arrives at the
// stub byte-identical, with the Provider's credential and the gateway's
// own headers and nothing of the caller's credential.
func case004SameDialectSameBytes(t testing.TB, c *client) {
	stub := c.stubs.Providers["openai"]
	c.clearReceived(t, stub)
	body := encode(t, chat(c.name("same"), false))
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", body, c.key.value, header("X-Conf-Extra", "kept"))
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	got := c.lastReceived(t, stub)
	if got.Body != string(body) {
		t.Errorf("the stub received\n%s\nthe caller sent\n%s", got.Body, body)
	}
	if got.Path != "/v1/chat/completions" || got.Method != http.MethodPost {
		t.Errorf("the stub received %s %s", got.Method, got.Path)
	}
	h := http.Header(got.Headers)
	if h.Get("Authorization") != "Bearer "+c.credential("openai") {
		t.Errorf("Authorization upstream %q is not the Provider's credential", h.Get("Authorization"))
	}
	if !strings.HasPrefix(h.Get("User-Agent"), "luxd/") || h.Get(gateway.HeaderRequestID) != resp.ID || h.Get("X-Conf-Extra") != "kept" {
		t.Errorf("upstream headers %v", got.Headers)
	}
	if h.Get("Accept-Encoding") != "" {
		t.Error("Accept-Encoding was forwarded on a route the gateway reads")
	}
	out := resp.json(t)
	if str(out, "model") != c.name("same") || str(out, "object") != "chat.completion" {
		t.Errorf("the caller received %s", excerpt(resp.Body))
	}
	// A body the door's codec could not read reaches a same-dialect
	// target untouched, and the target answers for itself.
	c.clearReceived(t, stub)
	odd := []byte(`{"model":"` + c.name("same") + `","messages":"not a list","stream":false}`)
	if resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", odd, c.key.value); resp.Status != http.StatusOK {
		t.Errorf("an undecodable passthrough: %d %s", resp.Status, excerpt(resp.Body))
	}
	if got := c.lastReceived(t, stub); got.Body != string(odd) {
		t.Errorf("the stub received\n%s\nthe caller sent\n%s", got.Body, odd)
	}
}

// case004CallerCredentialsNeverForwarded: the caller's Authorization,
// x-api-key, Cookie, Lux-* and X-Forwarded-* headers are absent from the
// outbound request, and the Key value appears in no header of it.
func case004CallerCredentialsNeverForwarded(t testing.TB, c *client) {
	stub := c.stubs.Providers["openai"]
	c.clearReceived(t, stub)
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("gpt"), false), c.key.value,
		header("x-api-key", "conf-canary-"+c.run), header("Cookie", "session=conf-"+c.run), header("Lux-Labels", "run="+c.run),
		header("X-Forwarded-For", "203.0.113.9"), header("Proxy-Authorization", "Basic conf"))
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	got := c.lastReceived(t, stub)
	h := http.Header(got.Headers)
	for _, name := range []string{"x-api-key", "Cookie", "Lux-Labels", "X-Forwarded-For", "Proxy-Authorization"} {
		if h.Get(name) != "" {
			t.Errorf("%s reached the provider as %q", name, h.Get(name))
		}
	}
	for name, values := range got.Headers {
		for _, v := range values {
			if strings.Contains(v, c.key.value) {
				t.Errorf("the Key value reached the provider in %s", name)
			}
		}
	}
}

// case004TranslationLoss: a request through the anthropic door to an
// openai target is translated, a member the target cannot carry appears
// in Lux-Loss, and the header is absent when nothing was lost.
func case004TranslationLoss(t testing.TB, c *client) {
	body := messages(c.name("gpt"))
	body["top_k"] = 5
	resp := c.door(t, "anthropic", http.MethodPost, "/v1/messages", body, c.key.value)
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	if loss := resp.Header.Get(gateway.HeaderLoss); !strings.Contains(loss, "top_k") {
		t.Errorf("Lux-Loss %q does not name top_k", loss)
	}
	out := resp.json(t)
	if str(out, "type") != "message" || str(out, "model") != c.name("gpt") || len(arr(out, "content")) == 0 {
		t.Errorf("the caller received %s", excerpt(resp.Body))
	}
	clean := c.door(t, "anthropic", http.MethodPost, "/v1/messages", messages(c.name("gpt")), c.key.value)
	if clean.Status != http.StatusOK || clean.Header.Get(gateway.HeaderLoss) != "" {
		t.Errorf("nothing lost: %d Lux-Loss %q", clean.Status, clean.Header.Get(gateway.HeaderLoss))
	}
	// The other way, an openai door to an anthropic target, carries the
	// anthropic-version the Messages API requires and answers in the
	// door's shape.
	stub := c.stubs.Providers["anthropic"]
	c.clearReceived(t, stub)
	back := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("claude"), false), c.key.value)
	if back.Status != http.StatusOK || str(back.json(t), "object") != "chat.completion" || str(back.json(t), "model") != c.name("claude") {
		t.Errorf("toward anthropic: %d %s", back.Status, excerpt(back.Body))
	}
	if got := c.lastReceived(t, stub); http.Header(got.Headers).Get("anthropic-version") == "" || got.Path != "/v1/messages" {
		t.Errorf("the anthropic target received %s without anthropic-version: %v", got.Path, got.Headers)
	}
}

// frames splits an SSE body into its data lines.
func frames(body string) []string {
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			out = append(out, strings.TrimSpace(rest))
		}
	}
	return out
}

// case004Streaming: a streamed chat completion arrives as
// text/event-stream with the stub's event count, then one usage chunk,
// then [DONE], and the outbound request asked for the usage.
func case004Streaming(t testing.TB, c *client) {
	stub := c.stubs.Providers["openai"]
	c.clearReceived(t, stub)
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("events"), true), c.key.value)
	if resp.Status != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %v %s", resp.Status, resp.Header.Get("Content-Type"), excerpt(resp.Body))
	}
	data := frames(string(resp.Body))
	if len(data) != 5 || data[4] != "[DONE]" {
		t.Fatalf("%d data frames, want three content chunks, one usage chunk, and [DONE]:\n%s", len(data), resp.Body)
	}
	var usage map[string]any
	if err := decodeJSON(data[3], &usage); err != nil {
		t.Fatalf("the usage chunk: %v", err)
	}
	if num(usage, "usage.prompt_tokens") != 100 || num(usage, "usage.completion_tokens") != 20 || str(usage, "model") != c.name("events") {
		t.Errorf("the usage chunk %s", data[3])
	}
	for _, d := range data[:3] {
		var chunk map[string]any
		if err := decodeJSON(d, &chunk); err != nil || str(chunk, "object") != "chat.completion.chunk" {
			t.Errorf("content chunk %s: %v", d, err)
		}
	}
	if got := c.lastReceived(t, stub); !strings.Contains(got.Body, `"include_usage":true`) {
		t.Errorf("the outbound request did not ask for the usage: %s", got.Body)
	}
	// A translated stream, the anthropic door over the same openai
	// target, re-encodes each event in the door's dialect and names the
	// Model on message_start.
	body := messages(c.name("events"))
	body["stream"] = true
	translated := c.door(t, "anthropic", http.MethodPost, "/v1/messages", body, c.key.value)
	if translated.Status != http.StatusOK || !strings.HasPrefix(translated.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("translated stream: %d %v %s", translated.Status, translated.Header.Get("Content-Type"), excerpt(translated.Body))
	}
	text := string(translated.Body)
	for _, want := range []string{"event: message_start", `"model":"` + c.name("events") + `"`, "event: content_block_delta", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(text, want) {
			t.Errorf("the translated stream lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "events-3") {
		t.Error("the upstream name reached the caller")
	}
}

// case004ModelNameRewrite: the body's model is the upstream name on the
// way out and the Model's name on the way back, on a passthrough.
func case004ModelNameRewrite(t testing.TB, c *client) {
	stub := c.stubs.Providers["openai"]
	c.clearReceived(t, stub)
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("alias"), false), c.key.value)
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	var sent map[string]any
	if err := decodeJSON(c.lastReceived(t, stub).Body, &sent); err != nil || str(sent, "model") != "stub-alias-upstream" {
		t.Errorf("the provider received model %q", str(sent, "model"))
	}
	if str(resp.json(t), "model") != c.name("alias") {
		t.Errorf("the caller received model %q", str(resp.json(t), "model"))
	}
}

// case004UpstreamError: a 500 from the only target is upstream_error, a
// 400 is upstream_rejected, each with the upstream's answer in the
// detail header and never in the caller's body.
func case004UpstreamError(t testing.TB, c *client) {
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("fail500"), false), c.key.value)
	c.expectDoor(t, "openai", resp, "upstream_error")
	if !strings.Contains(resp.Header.Get(gateway.HeaderErrorDetail), "500") || strings.Contains(string(resp.Body), "stub failure") {
		t.Errorf("detail %q body %s", resp.Header.Get(gateway.HeaderErrorDetail), excerpt(resp.Body))
	}
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("fail400"), false), c.key.value), "upstream_rejected")
}

// case004UpstreamTimeout: a Provider whose timeout passes is
// upstream_timeout.
func case004UpstreamTimeout(t testing.TB, c *client) {
	started := time.Now()
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("hang"), false), c.key.value)
	c.expectDoor(t, "openai", resp, "upstream_timeout")
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the timeout took %s against a one second Provider timeout", took)
	}
}

// case008Fallback: a retryable failure on the first target hands the
// request to the second, and the answer names the second.
func case008Fallback(t testing.TB, c *client) {
	stub := c.stubs.Providers["openai"]
	c.clearReceived(t, stub)
	resp := c.door(t, "openai", http.MethodPost, "/v1/chat/completions", chat(c.name("fallback"), false), c.key.value)
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "stub-gpt") {
		t.Errorf("the answer was not the second target's: %s", excerpt(resp.Body))
	}
	got := c.received(t, stub)
	if len(got) != 2 {
		t.Fatalf("the stub received %d requests, want the failed first and the served second", len(got))
	}
	var first, second map[string]any
	_ = decodeJSON(got[0].Body, &first)
	_ = decodeJSON(got[1].Body, &second)
	if str(first, "model") != "fail-500" || str(second, "model") != "stub-gpt" {
		t.Errorf("attempt order %q then %q", str(first, "model"), str(second, "model"))
	}
}

// case004OpaqueRoute: an opaque route is route_not_allowed without
// passthrough, provider_required with two candidates and no
// Lux-Provider, and relayed whole to the Provider Lux-Provider names.
func case004OpaqueRoute(t testing.TB, c *client) {
	pass := c.object(t, v1.KindKey, "pass", map[string]any{"models": []string{c.name("*")}, "passthrough": true})
	defer c.mustDelete(t, v1.KindKey, str(pass, "status.id"))
	value := str(pass, "status.value")
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/files", map[string]any{"purpose": "x"}, c.key.value), "route_not_allowed")
	c.expectDoor(t, "openai", c.door(t, "openai", http.MethodPost, "/v1/files", map[string]any{"purpose": "x"}, value), "provider_required")
	stub := c.stubs.Providers["openai"]
	c.clearReceived(t, stub)
	resp := c.door(t, "openai", http.MethodPost, "/v1/files", map[string]any{"purpose": "x"}, value, header(gateway.HeaderProvider, c.name("openai")))
	if resp.Status != http.StatusOK {
		t.Fatalf("%d %s", resp.Status, excerpt(resp.Body))
	}
	if got := c.lastReceived(t, stub); got.Path != "/v1/files" || got.Body != `{"purpose":"x"}` {
		t.Errorf("the provider received %s %s", got.Path, got.Body)
	}
}
