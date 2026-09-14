// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/llmdialect/openaichat"
	"latere.ai/x/pkg/llmdialect/openairesp"

	v1 "latere.ai/x/lux/manifest/v1"
)

// TestProviderStub drives every route of every dialect, whole and
// streamed, the models route, the catch-all, and the control routes, and
// reads each streamed answer back through the codec the gateway
// translates that dialect with, so the frames are the dialect's and not
// a shape only this package understands.
func TestProviderStub(t *testing.T) {
	for _, d := range dialects {
		t.Run(string(d), func(t *testing.T) {
			srv := start(t, d)
			path, body := primary(d, "m1", "hello", false)
			resp := send(t, srv, http.MethodPost, path, body, nil)
			if resp.status != http.StatusOK {
				t.Fatalf("%s = %d %s", path, resp.status, resp.body)
			}
			want := Content(d, "m1", "hello")
			if !strings.Contains(string(resp.body), `"`+want+`"`) {
				t.Fatalf("the answer does not carry %q:\n%s", want, resp.body)
			}

			path, body = primary(d, "m1", "hello", true)
			resp = send(t, srv, http.MethodPost, path, body, nil)
			if resp.status != http.StatusOK || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("stream: %d %s\n%s", resp.status, resp.header.Get("Content-Type"), resp.body)
			}
			fs := frames(resp.body)
			if n, text := contentEvents(d, fs); n != DefaultEvents || text != want {
				t.Fatalf("stream: %d content events joining to %q, want %d and %q\n%s", n, text, DefaultEvents, want, resp.body)
			}
			if n := usageEvents(d, fs); n < 1 {
				t.Fatalf("stream: no final usage event\n%s", resp.body)
			}
			if be := backendFor(d); be != nil {
				checkCodec(t, be, resp.body, want)
			}

			resp = send(t, srv, http.MethodGet, modelsPath(d), "", nil)
			if resp.status != http.StatusOK || !strings.Contains(string(resp.body), ModelName(d)) {
				t.Fatalf("models = %d %s", resp.status, resp.body)
			}
			resp = send(t, srv, http.MethodGet, "/v1/some/opaque/route?x=1", "", nil)
			if resp.status != http.StatusOK || string(resp.body) != "{}" {
				t.Fatalf("catch-all = %d %s", resp.status, resp.body)
			}
			resp = send(t, srv, http.MethodPut, "/_nothing", "", nil)
			if resp.status != http.StatusNotFound {
				t.Fatalf("an unknown control route = %d", resp.status)
			}
		})
	}
}

// TestProviderStubRoutes covers the rows beyond the primary route: the
// second openai translated route, the embeddings, the counts, and the
// gemini verbs, each with the shape its API answers.
func TestProviderStubRoutes(t *testing.T) {
	for _, tc := range []struct {
		dialect v1.Dialect
		path    string
		body    string
		want    string // a member the shape carries
	}{
		{v1.DialectOpenAI, "/v1/responses", `{"model":"m","input":"hi"}`, `"output_text"`},
		{v1.DialectOpenAI, "/v1/responses", `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`, Content(v1.DialectOpenAI, "m", "hi")},
		{v1.DialectOpenAI, "/v1/embeddings", `{"model":"m","input":["a","b"]}`, `"embedding"`},
		{v1.DialectOpenAI, "/v1/embeddings", `{"model":"m","input":"a"}`, `"embedding"`},
		{v1.DialectAnthropic, "/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, `{"input_tokens":100}`},
		{v1.DialectGemini, "/v1beta/models/m:countTokens", `{"contents":[{"parts":[{"text":"hi"}]}]}`, `{"totalTokens":100}`},
		{v1.DialectGemini, "/v1beta/models/m:embedContent", `{"content":{"parts":[{"text":"hi"}]}}`, `"values"`},
		{v1.DialectLux, "/v1/count_tokens", `{"model":"m","messages":[]}`, `{"input_tokens":100}`},
	} {
		srv := start(t, tc.dialect)
		resp := send(t, srv, http.MethodPost, tc.path, tc.body, nil)
		if resp.status != http.StatusOK || !strings.Contains(string(resp.body), tc.want) {
			t.Errorf("%s %s = %d %s, want %s", tc.dialect, tc.path, resp.status, resp.body, tc.want)
		}
	}

	// The responses stream and the gemini stream as a JSON array.
	srv := start(t, v1.DialectOpenAI)
	resp := send(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`, nil)
	fs := frames(resp.body)
	var deltas int
	for _, f := range fs {
		if f.event == "response.output_text.delta" {
			deltas++
		}
	}
	if deltas != DefaultEvents || fs[len(fs)-1].event != "response.completed" {
		t.Fatalf("responses stream: %d deltas, last %q\n%s", deltas, fs[len(fs)-1].event, resp.body)
	}
	checkCodec(t, openairesp.NewBackend(), resp.body, Content(v1.DialectOpenAI, "m", "hi"))

	srv = start(t, v1.DialectGemini)
	resp = send(t, srv, http.MethodPost, "/v1beta/models/m:streamGenerateContent", `{"contents":[{"parts":[{"text":"hi"}]}]}`, nil)
	var chunks []any
	if err := decodeJSON(resp.body, &chunks); err != nil || len(chunks) != DefaultEvents+1 {
		t.Fatalf("gemini array stream: %v, %d chunks\n%s", err, len(chunks), resp.body)
	}
	if _, ok := lookup(chunks[DefaultEvents], "usageMetadata.candidatesTokenCount"); !ok {
		t.Fatalf("gemini array stream: no usage on the last chunk\n%s", resp.body)
	}
	resp = send(t, srv, http.MethodPost, "/v1beta/models/m:frobnicate", `{}`, nil)
	if resp.status != http.StatusNotFound {
		t.Fatalf("an unknown gemini verb = %d", resp.status)
	}
}

// TestProviderStubIsDeterministic: one request twice yields byte-identical
// bodies, and the content names the dialect, the model, and the digest of
// the last user text, which changes when the text does and when the
// model does.
func TestProviderStubIsDeterministic(t *testing.T) {
	for _, d := range dialects {
		srv := start(t, d)
		path, body := primary(d, "gpt", "hello", false)
		first := send(t, srv, http.MethodPost, path, body, nil)
		second := send(t, srv, http.MethodPost, path, body, nil)
		if !bytes.Equal(first.body, second.body) {
			t.Fatalf("%s: two answers differ:\n%s\n%s", d, first.body, second.body)
		}
		want := "stub:" + string(d) + ":gpt:" + Digest("hello")
		if !strings.Contains(string(first.body), want) {
			t.Fatalf("%s: no %q in %s", d, want, first.body)
		}
		path, body = primary(d, "gpt", "goodbye", false)
		if other := send(t, srv, http.MethodPost, path, body, nil); bytes.Equal(other.body, first.body) {
			t.Fatalf("%s: a different user text yields the same answer", d)
		}
	}
	if Digest("hello") != "2cf24dba" || len(Digest("")) != 8 {
		t.Fatalf("Digest = %q, %q", Digest("hello"), Digest(""))
	}
	// The last user message is the digested one; an assistant turn after
	// it is not.
	srv := start(t, v1.DialectOpenAI)
	body := `{"model":"m","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"x"},{"role":"user","content":[{"type":"text","text":"sec"},{"type":"text","text":"ond"}]},{"role":"assistant","content":"y"}]}`
	resp := send(t, srv, http.MethodPost, "/v1/chat/completions", body, nil)
	if want := Content(v1.DialectOpenAI, "m", "second"); !strings.Contains(string(resp.body), want) {
		t.Fatalf("no %q in %s", want, resp.body)
	}
	resp = send(t, srv, http.MethodPost, "/v1/chat/completions", `not json`, nil)
	if want := Content(v1.DialectOpenAI, "", ""); resp.status != http.StatusOK || !strings.Contains(string(resp.body), want) {
		t.Fatalf("a body that is not JSON = %d %s", resp.status, resp.body)
	}
}

// TestFailureInjection: every row of the table produces its behaviour on
// each of the four dialects, selected by upstream model name and by the
// Lux-Stub-Fail header.
func TestFailureInjection(t *testing.T) {
	for _, d := range dialects {
		for _, name := range Behaviours() {
			for _, by := range []string{"name", "header"} {
				t.Run(string(d)+"/"+name+"/by-"+by, func(t *testing.T) {
					srv := start(t, d)
					b := ParseBehaviour(name)
					stream := b.Kind == KindEvents || b.Kind == KindFailStreamMid
					model, header := name, http.Header{}
					if by == "header" {
						model = "plain"
						header.Set(HeaderFail, name)
					}
					path, body := primary(d, model, "hello", stream)
					checkRow(t, srv, d, path, body, header, b)
				})
			}
		}
	}
}

// checkRow asserts one row's behaviour on one dialect.
func checkRow(t *testing.T, srv *server, d v1.Dialect, path, body string, header http.Header, b Behaviour) {
	t.Helper()
	switch b.Kind {
	case KindHang:
		ctx := shortly(t, 150*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, strings.NewReader(body))
		for k, v := range header {
			req.Header[k] = v
		}
		credential(d, req.Header, DefaultCredential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("the headers never arrived: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hang: status %d, read error %v; the body should have waited for the caller", resp.StatusCode, err)
		}
		return
	case KindSlow:
		started := time.Now()
		resp := send(t, srv, http.MethodPost, path, body, header)
		if resp.status != http.StatusOK || time.Since(started) < b.Wait {
			t.Fatalf("slow: %d after %s", resp.status, time.Since(started))
		}
		return
	}
	resp := send(t, srv, http.MethodPost, path, body, header)
	switch b.Kind {
	case KindFail500, KindFail429, KindFail401, KindFail400, KindFail529:
		want := map[Kind]int{KindFail500: 500, KindFail429: 429, KindFail401: 401, KindFail400: 400, KindFail529: 529}[b.Kind]
		if resp.status != want {
			t.Fatalf("status %d, want %d", resp.status, want)
		}
		if b.Kind == KindFail429 && resp.header.Get("Retry-After") != "1" {
			t.Fatalf("Retry-After %q", resp.header.Get("Retry-After"))
		}
		if !strings.Contains(string(resp.body), `"error"`) {
			t.Fatalf("no error body: %s", resp.body)
		}
		if b.Kind == KindFail529 && d == v1.DialectAnthropic && !strings.Contains(string(resp.body), "overloaded_error") {
			t.Fatalf("529 on anthropic is not overloaded: %s", resp.body)
		}
	case KindFailStreamMid:
		if resp.status != http.StatusOK || resp.err == nil {
			t.Fatalf("status %d, read error %v; the stream should have been cut", resp.status, resp.err)
		}
		fs := frames(resp.body)
		if n, _ := contentEvents(d, fs); n != 2 || usageEvents(d, fs) != 0 {
			t.Fatalf("%d content events and %d usage events, want 2 and 0\n%s", n, usageEvents(d, fs), resp.body)
		}
	case KindFailBody:
		if resp.status != http.StatusOK || !strings.Contains(string(resp.body), `"stub"`) {
			t.Fatalf("%d %s", resp.status, resp.body)
		}
	case KindRedirect:
		if resp.status != http.StatusFound || !strings.HasPrefix(resp.header.Get("Location"), "http://elsewhere.example.com/") {
			t.Fatalf("%d Location %q", resp.status, resp.header.Get("Location"))
		}
	case KindFailHTML:
		if resp.status != http.StatusOK || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/html") || !strings.Contains(string(resp.body), "<html>") {
			t.Fatalf("%d %s %s", resp.status, resp.header.Get("Content-Type"), resp.body)
		}
	case KindTokens:
		in, out := usageOf(t, d, decode(t, resp.body))
		if in != b.InputTokens || out != b.OutputTokens {
			t.Fatalf("usage %d/%d, want %d/%d", in, out, b.InputTokens, b.OutputTokens)
		}
	case KindEvents:
		if n, _ := contentEvents(d, frames(resp.body)); n != b.Events {
			t.Fatalf("%d content events, want %d\n%s", n, b.Events, resp.body)
		}
	case KindNormal, KindSlow, KindHang:
	}
}

// usageOf reads the primary route's usage members.
func usageOf(t *testing.T, d v1.Dialect, doc map[string]any) (in, out int64) {
	t.Helper()
	switch d {
	case v1.DialectOpenAI:
		return number(t, doc, "usage.prompt_tokens"), number(t, doc, "usage.completion_tokens")
	case v1.DialectAnthropic, v1.DialectLux:
		return number(t, doc, "usage.input_tokens"), number(t, doc, "usage.output_tokens")
	case v1.DialectGemini:
		return number(t, doc, "usageMetadata.promptTokenCount"), number(t, doc, "usageMetadata.candidatesTokenCount")
	}
	return 0, 0
}

// TestProviderStubChecksTheCredential: a request whose credential is not
// the configured one is 401 and recorded; the header without its scheme,
// and no header at all, are refused the same way.
func TestProviderStubChecksTheCredential(t *testing.T) {
	for _, d := range dialects {
		srv := start(t, d)
		path, body := primary(d, "m", "hi", false)
		h := http.Header{}
		credential(d, h, "another-value")
		if resp := send(t, srv, http.MethodPost, path, body, h); resp.status != http.StatusUnauthorized || !strings.Contains(string(resp.body), `"error"`) {
			t.Fatalf("%s: a wrong credential = %d %s", d, resp.status, resp.body)
		}
		h = http.Header{}
		h.Set(d.CredentialHeader(), DefaultCredential) // the raw value where a scheme is required
		resp := send(t, srv, http.MethodPost, path, body, h)
		if want := d.CredentialScheme() == v1.SchemeRaw; (resp.status == http.StatusOK) != want {
			t.Fatalf("%s: the header without its scheme = %d", d, resp.status)
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+modelsPath(d), nil)
		raw, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = raw.Body.Close()
		if raw.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: no credential = %d", d, raw.StatusCode)
		}
		rec := srv.stub.Received()
		if len(rec) != 3 || rec[0].Header.Get(d.CredentialHeader()) == "" {
			t.Fatalf("%s: %d requests recorded; the refused ones should be among them", d, len(rec))
		}
	}
	custom := serve(t, New(Options{Dialect: v1.DialectOpenAI, Credential: "sk-custom"}))
	h := http.Header{}
	credential(v1.DialectOpenAI, h, "sk-custom")
	if resp := send(t, custom, http.MethodGet, "/v1/models", "", h); resp.status != http.StatusOK {
		t.Fatalf("the configured credential = %d", resp.status)
	}
}

// TestReceivedRecording: GET /_received returns every request in order
// with its method, path, query, headers, and body, DELETE /_received
// clears it, and neither control route is itself recorded.
func TestReceivedRecording(t *testing.T) {
	srv := start(t, v1.DialectOpenAI)
	h := http.Header{"X-Trace": {"one"}}
	send(t, srv, http.MethodPost, "/v1/chat/completions?tag=a", `{"model":"m"}`, h)
	send(t, srv, http.MethodGet, "/v1/models", "", nil)
	send(t, srv, http.MethodPost, "/v1/files", `raw bytes`, nil)
	send(t, srv, http.MethodGet, "/_received", "", nil)

	resp := send(t, srv, http.MethodGet, "/_received", "", nil)
	var rec []Received
	if err := decodeJSON(resp.body, &rec); err != nil || resp.status != http.StatusOK {
		t.Fatalf("GET /_received = %d %v %s", resp.status, err, resp.body)
	}
	if len(rec) != 3 {
		t.Fatalf("%d recorded, want 3: %+v", len(rec), rec)
	}
	first := rec[0]
	if first.Method != http.MethodPost || first.Path != "/v1/chat/completions" || first.Query != "tag=a" || first.Header.Get("X-Trace") != "one" || first.Body != `{"model":"m"}` {
		t.Fatalf("first = %+v", first)
	}
	if first.Header.Get("Authorization") != "Bearer "+DefaultCredential {
		t.Fatalf("the credential header is not recorded: %v", first.Header)
	}
	if rec[1].Method != http.MethodGet || rec[1].Path != "/v1/models" || rec[2].Body != "raw bytes" {
		t.Fatalf("order or body: %+v", rec[1:])
	}
	if resp := send(t, srv, http.MethodDelete, "/_received", "", nil); resp.status != http.StatusNoContent {
		t.Fatalf("DELETE /_received = %d", resp.status)
	}
	if resp := send(t, srv, http.MethodGet, "/_received", "", nil); string(resp.body) != "[]" {
		t.Fatalf("after DELETE: %s", resp.body)
	}
}

// TestProviderStubUsageShapes: the usage members are the route's, not
// the dialect's, so a /v1/responses answer and a /v1/chat/completions
// answer carry different member names; the count routes and the
// embeddings answer in their APIs' own shapes.
func TestProviderStubUsageShapes(t *testing.T) {
	for _, tc := range []struct {
		dialect     v1.Dialect
		path, body  string
		input       string
		output      string // "" where the API has no output member
		tokensInput int64  // what the input member reads for tokens-1000-500
	}{
		{v1.DialectOpenAI, "/v1/chat/completions", `{"model":"tokens-1000-500","messages":[]}`, "usage.prompt_tokens", "usage.completion_tokens", 1000},
		{v1.DialectOpenAI, "/v1/embeddings", `{"model":"tokens-1000-500","input":"x"}`, "usage.prompt_tokens", "", 1000},
		{v1.DialectOpenAI, "/v1/responses", `{"model":"tokens-1000-500","input":"x"}`, "usage.input_tokens", "usage.output_tokens", 1000},
		{v1.DialectAnthropic, "/v1/messages", `{"model":"tokens-1000-500","messages":[]}`, "usage.input_tokens", "usage.output_tokens", 1000},
		{v1.DialectAnthropic, "/v1/messages/count_tokens", `{"model":"tokens-1000-500","messages":[]}`, "input_tokens", "", 1000},
		{v1.DialectGemini, "/v1beta/models/tokens-1000-500:generateContent", `{"contents":[]}`, "usageMetadata.promptTokenCount", "usageMetadata.candidatesTokenCount", 1000},
		{v1.DialectGemini, "/v1beta/models/tokens-1000-500:countTokens", `{"contents":[]}`, "totalTokens", "", 1000},
		{v1.DialectLux, "/v1/generate", `{"model":"tokens-1000-500","messages":[]}`, "usage.input_tokens", "usage.output_tokens", 1000},
		{v1.DialectLux, "/v1/count_tokens", `{"model":"tokens-1000-500","messages":[]}`, "input_tokens", "", 1000},
	} {
		srv := start(t, tc.dialect)
		doc := decode(t, send(t, srv, http.MethodPost, tc.path, tc.body, nil).body)
		if got := number(t, doc, tc.input); got != tc.tokensInput {
			t.Errorf("%s %s: %s = %d, want %d", tc.dialect, tc.path, tc.input, got, tc.tokensInput)
		}
		if tc.output != "" {
			if got := number(t, doc, tc.output); got != 500 {
				t.Errorf("%s %s: %s = %d, want 500", tc.dialect, tc.path, tc.output, got)
			}
		}
		if tc.path == "/v1/chat/completions" {
			if _, ok := lookup(doc, "usage.input_tokens"); ok {
				t.Errorf("a chat completion carries the responses route's member")
			}
		}
	}
}

// TestParseBehaviour holds the parser to the table: a malformed figure
// is a model name and answers normally.
func TestParseBehaviour(t *testing.T) {
	for _, tc := range []struct {
		name string
		want Behaviour
	}{
		{"gpt-4o", Behaviour{InputTokens: 100, OutputTokens: 20, Events: 5}},
		{"fail-500", Behaviour{Kind: KindFail500, InputTokens: 100, OutputTokens: 20, Events: 5}},
		{"slow-1s", Behaviour{Kind: KindSlow, Wait: time.Second, InputTokens: 100, OutputTokens: 20, Events: 5}},
		{"slow-soon", Behaviour{InputTokens: 100, OutputTokens: 20, Events: 5}},
		{"tokens-7-3", Behaviour{Kind: KindTokens, InputTokens: 7, OutputTokens: 3, Events: 5}},
		{"tokens-7", Behaviour{InputTokens: 100, OutputTokens: 20, Events: 5}},
		{"tokens-x-3", Behaviour{InputTokens: 100, OutputTokens: 20, Events: 5}},
		{"events-0", Behaviour{Kind: KindEvents, InputTokens: 100, OutputTokens: 20, Events: 0}},
		{"events-many", Behaviour{InputTokens: 100, OutputTokens: 20, Events: 5}},
	} {
		if got := ParseBehaviour(tc.name); got != tc.want {
			t.Errorf("ParseBehaviour(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := pieces("abcdefg", 3); strings.Join(got, "") != "abcdefg" || len(got) != 3 {
		t.Fatalf("pieces = %q", got)
	}
	if got := pieces("ab", 0); len(got) != 0 {
		t.Fatalf("pieces of zero = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a dialect outside the four")
		}
	}()
	New(Options{Dialect: "cohere"})
}

// TestLastUserText covers the shapes the digest is read from.
func TestLastUserText(t *testing.T) {
	for _, tc := range []struct {
		dialect v1.Dialect
		rt      routeKind
		body    string
		want    string
	}{
		{v1.DialectGemini, routeGeminiGenerate, `{"contents":[{"role":"user","parts":[{"text":"a"},{"text":"b"}]},{"role":"model","parts":[{"text":"c"}]}]}`, "ab"},
		{v1.DialectLux, routeGenerate, `{"messages":[{"role":"user","blocks":[{"type":"text","text":"x"}]}]}`, "x"},
		{v1.DialectOpenAI, routeEmbeddings, `{"input":[1,"last"]}`, "last"},
		{v1.DialectOpenAI, routeEmbeddings, `{"input":[1]}`, ""},
		{v1.DialectOpenAI, routeChat, `{"messages":["not an object"]}`, ""},
		{v1.DialectOpenAI, routeChat, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`, ""},
	} {
		var doc map[string]any
		if err := decodeJSON([]byte(tc.body), &doc); err != nil {
			t.Fatal(err)
		}
		if got := lastUserText(tc.dialect, tc.rt, doc); got != tc.want {
			t.Errorf("%s %d: %q, want %q", tc.dialect, tc.rt, got, tc.want)
		}
	}
}

// checkCodec decodes the stub's stream through the backend the gateway
// translates the dialect with and joins the text deltas.
func checkCodec(t *testing.T, be llmdialect.Backend, body []byte, want string) {
	t.Helper()
	dec := be.NewEventDecoder(bytes.NewReader(body))
	var text strings.Builder
	var usage *ir.Usage
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("the codec refused the stream: %v\n%s", err, body)
		}
		if ev.Type == ir.EventTextDelta {
			text.WriteString(ev.Delta)
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
	}
	if text.String() != want {
		t.Fatalf("the codec read %q, want %q", text.String(), want)
	}
	if usage == nil || usage.OutputTokens != DefaultOutputTokens {
		t.Fatalf("the codec read usage %+v", usage)
	}
}

// backendFor is the gateway's codec for a target of the dialect; gemini
// has none.
func backendFor(d v1.Dialect) llmdialect.Backend {
	switch d {
	case v1.DialectOpenAI:
		return openaichat.NewBackend(openaichat.BackendOptions{})
	case v1.DialectAnthropic:
		return anthropic.NewBackend(anthropic.BackendOptions{})
	case v1.DialectLux:
		return lux.NewBackend()
	case v1.DialectGemini:
	}
	return nil
}

func modelsPath(d v1.Dialect) string {
	if d == v1.DialectGemini {
		return "/v1beta/models"
	}
	return "/v1/models"
}
