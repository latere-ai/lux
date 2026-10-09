// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	v1 "latere.ai/x/lux/manifest/v1"
)

// zeroRetentionValue is the fixture Key that asks for zero retention.
const zeroRetentionValue = "lux_zeroretention0123456789abcdefghijklmnop"

// generalFields are a Provider's requestFields that name the member the
// zero-retention fields hold, so a test sees the zero-retention value win
// over the Provider's own as well as over the caller's.
func generalFields() map[string]any {
	return map[string]any{"provider": map[string]any{"zdr": false, "order": []any{"vendor-b"}}}
}

// zeroFields are a Provider's spec.zeroRetention.requestFields.
func zeroFields() map[string]any {
	return map[string]any{"provider": map[string]any{"zdr": true}}
}

// zeroRetentionKey adds the fixture's zero-retention Key over every
// Model, with passthrough so it reaches opaque routes.
func (w *world) zeroRetentionKey() *v1.Key {
	k := w.key("zr", zeroRetentionValue, "*")
	k.Spec.ZeroRetention, k.Spec.Passthrough = true, true
	return k
}

// declare sets one of the fixture's Providers' spec.zeroRetention: nil
// declares nothing, an empty map declares {}.
func (w *world) declare(name string, fields map[string]any) {
	w.t.Helper()
	p, err := w.catalog.Provider(context.Background(), name)
	if err != nil || p == nil {
		w.t.Fatalf("no Provider %s: %v", name, err)
	}
	if fields == nil {
		p.Spec.ZeroRetention = nil
		return
	}
	p.Spec.ZeroRetention = &v1.ZeroRetention{RequestFields: fields}
}

// addStub adds one openai Provider over a stub of its own, which the
// transport admits, and returns the stub.
func (w *world) addStub(name string) *stub {
	w.t.Helper()
	s := newStub(w.t)
	w.clients.transport.hosts[s.host()] = true
	w.provider(name, v1.DialectOpenAI, s.URL+"/v1", "sk-"+name)
	return s
}

// as sends r with the zero-retention Key's value in place of the
// fixture's default Key.
func as(r *http.Request, value string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+value)
	return r
}

// TestZeroRetentionFieldsReachTheUpstream: a request on a zero-retention
// Key reaches the upstream with the Provider's zero-retention fields
// merged after its requestFields, winning over the caller's members and
// the Provider's general ones, on passthrough on every dialect and on
// translation in both directions, stream and not, with GetBody replaying
// what the upstream read; the same request on a Key without the flag
// carries the general fields alone.
func TestZeroRetentionFieldsReachTheUpstream(t *testing.T) {
	w := newWorld(t)
	w.zeroRetentionKey()
	for _, name := range []string{"oai", "ant", "gem", "lx"} {
		w.setRequestFields(name, generalFields())
		w.declare(name, zeroFields())
	}
	openaiFrames := []string{
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1}}\n\n",
		"data: [DONE]\n\n",
	}
	callerProvider := `"provider":{"zdr":false,"ZDR":false,"allow_fallbacks":true}`
	cases := []struct {
		name, path, body string
		upstream         *stub
		respond          func(*stub)
		stream           bool
		caller           bool // the caller's provider member reaches the upstream
	}{
		{"openai passthrough", "/openai/v1/chat/completions", `{"model":"gpt","messages":[{"role":"user","content":"hi"}],` + callerProvider + `}`, w.openai, func(s *stub) { s.respondJSON(200, openaiChatResponse) }, false, true},
		{"openai passthrough stream", "/openai/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}],` + callerProvider + `}`, w.openai, func(s *stub) { s.respondSSE(openaiFrames...) }, true, true},
		{"anthropic door to openai", "/anthropic/v1/messages", messagesBody("gpt", false), w.openai, func(s *stub) { s.respondJSON(200, openaiChatResponse) }, false, false},
		{"anthropic door to openai stream", "/anthropic/v1/messages", messagesBody("gpt", true), w.openai, func(s *stub) { s.respondSSE(openaiFrames...) }, true, false},
		{"openai door to anthropic", "/openai/v1/chat/completions", chatBody("claude", false), w.anthropic, func(s *stub) { s.respondJSON(200, anthropicResponse) }, false, false},
		{"openai door to anthropic stream", "/openai/v1/chat/completions", chatBody("claude", true), w.anthropic, func(s *stub) { s.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...) }, true, false},
		{"anthropic passthrough stream", "/anthropic/v1/messages", messagesBody("claude", true), w.anthropic, func(s *stub) { s.respondSSE(strings.SplitAfter(anthropicStream, "\n\n")...) }, true, false},
		{"gemini passthrough", "/gemini/v1beta/models/gemini:generateContent", `{"contents":[],` + callerProvider + `}`, w.gemini, func(s *stub) { s.respondJSON(200, geminiResponse) }, false, true},
		{"lux passthrough", "/lux/v1/generate", luxBody("luxm", false), w.lux, func(s *stub) { s.respondJSON(200, luxResponse) }, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.respond(c.upstream)
			var replay []byte
			w.clients.transport.seen = func(r *http.Request) {
				if r.GetBody == nil {
					t.Error("the outbound request has no GetBody")
					return
				}
				rc, err := r.GetBody()
				if err != nil {
					t.Error(err)
					return
				}
				replay, err = io.ReadAll(rc)
				if err != nil {
					t.Error(err)
				}
			}
			defer func() { w.clients.transport.seen = nil }()
			rec := w.do(as(w.request(http.MethodPost, c.path, c.body), zeroRetentionValue))
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if c.stream {
				_, _ = io.Copy(io.Discard, rec.Body)
			}
			got := upstreamJSON(t, c.upstream)
			provider, _ := got["provider"].(map[string]any)
			if provider["zdr"] != true || !reflect.DeepEqual(provider["order"], []any{"vendor-b"}) {
				t.Errorf("provider %v", got["provider"])
			}
			if _, kept := provider["ZDR"]; kept {
				t.Errorf("the caller's case variant reached the upstream: %v", got["provider"])
			}
			if c.caller && provider["allow_fallbacks"] != true {
				t.Errorf("the caller's sibling was lost: %v", got["provider"])
			}
			if !bytes.Equal(replay, c.upstream.last(t).Body) {
				t.Errorf("GetBody replays\n%s\nthe upstream read\n%s", replay, c.upstream.last(t).Body)
			}
			if !w.recorder.last(t).ZeroRetention {
				t.Error("the record does not say the request was on a zero-retention Key")
			}

			// The same request on a Key without the flag carries the
			// Provider's general fields alone.
			c.respond(c.upstream)
			if rec := w.post(c.path, c.body); rec.Code != http.StatusOK {
				t.Fatalf("without the flag: %d %s", rec.Code, rec.Body.String())
			}
			got = upstreamJSON(t, c.upstream)
			if provider, _ := got["provider"].(map[string]any); provider["zdr"] != false {
				t.Errorf("without the flag the upstream read provider %v", got["provider"])
			}
			if w.recorder.last(t).ZeroRetention {
				t.Error("the record says a request without the flag was on a zero-retention Key")
			}
		})
	}
	t.Run("the caller cannot turn the fields off", func(t *testing.T) {
		w.openai.respondJSON(200, openaiChatResponse)
		for _, member := range []string{`"provider":{"zdr":false}`, `"provider":{"ZDR":false}`, `"provider":null`, `"Provider":{"zdr":false}`, `"provider":"anything"`, `"provider":{"zdr":false},"provider":{"zdr":false}`} {
			body := `{"model":"gpt","messages":[{"role":"user","content":"hi"}],` + member + `}`
			if rec := w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", body), zeroRetentionValue)); rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", member, rec.Code, rec.Body.String())
			}
			got := upstreamJSON(t, w.openai)
			if provider, _ := got["provider"].(map[string]any); provider["zdr"] != true {
				t.Errorf("%s: the upstream read provider %v", member, got["provider"])
			}
			if _, kept := got["Provider"]; kept {
				t.Errorf("%s: the caller's case variant reached the upstream", member)
			}
		}
	})
	t.Run("a Provider with zero-retention fields alone writes them", func(t *testing.T) {
		w.setRequestFields("oai", nil)
		defer w.setRequestFields("oai", generalFields())
		w.openai.respondJSON(200, openaiChatResponse)
		if rec := w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)), zeroRetentionValue)); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if got := upstreamJSON(t, w.openai); !reflect.DeepEqual(got["provider"], map[string]any{"zdr": true}) {
			t.Errorf("provider %v", got["provider"])
		}
		body := chatBody("gpt-4.1", false)
		if rec := w.post("/openai/v1/chat/completions", body); rec.Code != http.StatusOK || string(w.openai.last(t).Body) != body {
			t.Errorf("without the flag: %d, the upstream read %s", rec.Code, w.openai.last(t).Body)
		}
	})
	t.Run("fields with no JSON form fail closed", func(t *testing.T) {
		w.declare("oai", map[string]any{"f": func() {}})
		defer w.declare("oai", zeroFields())
		before := w.openai.count()
		if code := errorCode(t, w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)), zeroRetentionValue))); code != CodeProviderUnavailable {
			t.Errorf("code %s", code)
		}
		if w.openai.count() != before {
			t.Error("the upstream received a body without the fields")
		}
	})
}

// TestZeroRetentionDeclaringNothingAdded: a Provider that declares {}
// serves a zero-retention Key with nothing added, the caller's bytes as
// they were sent.
func TestZeroRetentionDeclaringNothingAdded(t *testing.T) {
	w := newWorld(t)
	w.zeroRetentionKey()
	w.declare("oai", map[string]any{})
	w.openai.respondJSON(200, openaiChatResponse)
	body := `{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"provider":{"zdr":false}}`
	if rec := w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", body), zeroRetentionValue)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := string(w.openai.last(t).Body); got != body {
		t.Errorf("the upstream read\n%s\nthe caller sent\n%s", got, body)
	}
	if rec := w.post("/openai/v1/chat/completions", body); rec.Code != http.StatusOK || string(w.openai.last(t).Body) != body {
		t.Errorf("without the flag: %d %s", rec.Code, w.openai.last(t).Body)
	}
}

// TestZeroRetentionRouting: a Model none of whose targets names a
// declaring Provider refuses a zero-retention Key
// zero_retention_unavailable, 403, before any upstream, limit, or
// Router; a Model with declaring and non-declaring targets serves it
// through the declaring ones alone, failing over among them and never
// to the other; declaring targets the Router cannot try now answer
// provider_unavailable; and a Key without the flag is routed as before.
func TestZeroRetentionRouting(t *testing.T) {
	w := newWorld(t)
	w.zeroRetentionKey()
	first, second := w.addStub("zr-first"), w.addStub("zr-second")
	w.declare("zr-first", map[string]any{})
	w.declare("zr-second", zeroFields())
	w.model("mixed", target("oai", "gpt-4.1"), target("zr-first", "gpt-4.1"), target("zr-second", "gpt-4.1"))
	zr := func(model string) *httptest.ResponseRecorder {
		return w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody(model, false)), zeroRetentionValue))
	}

	t.Run("no declaring target is a refusal no retry changes", func(t *testing.T) {
		upstream, reservations, routes := w.openai.count(), w.limiter.calls.Load(), w.router.calls.Load()
		rec := zr("gpt")
		if code := errorCode(t, rec); code != CodeZeroRetentionUnavailable || rec.Code != http.StatusForbidden {
			t.Fatalf("%d %s", rec.Code, code)
		}
		if detail := rec.Header().Get(HeaderErrorDetail); !strings.Contains(detail, `Model "gpt"`) {
			t.Errorf("detail %q", detail)
		}
		if w.openai.count() != upstream || w.limiter.calls.Load() != reservations || w.router.calls.Load() != routes {
			t.Error("the refusal reached an upstream, a limit, or the Router")
		}
		r := w.recorder.last(t)
		if r.Status != StatusRefused || r.Error != CodeZeroRetentionUnavailable || !r.ZeroRetention || len(r.Attempts) != 0 {
			t.Errorf("record %+v", r)
		}
		w.openai.respondJSON(200, openaiChatResponse)
		if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != http.StatusOK {
			t.Errorf("a Key without the flag: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a Provider the catalog does not hold declares nothing", func(t *testing.T) {
		w.model("orphan", target("gone", "gpt-4.1"))
		if code := errorCode(t, zr("orphan")); code != CodeZeroRetentionUnavailable {
			t.Errorf("code %s", code)
		}
	})
	t.Run("the declaring targets alone, failover included", func(t *testing.T) {
		w.openai.respondJSON(200, openaiChatResponse)
		first.respondJSON(500, `{"error":"down"}`)
		second.respondJSON(200, openaiChatResponse)
		before, f, s := w.openai.count(), first.count(), second.count()
		if rec := zr("mixed"); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if w.openai.count() != before || first.count() != f+1 || second.count() != s+1 {
			t.Errorf("attempts: oai %d, first %d, second %d", w.openai.count()-before, first.count()-f, second.count()-s)
		}
		if got := upstreamJSON(t, second); !reflect.DeepEqual(got["provider"], map[string]any{"zdr": true}) {
			t.Errorf("the second declaring target read provider %v", got["provider"])
		}
		if got := string(first.last(t).Body); got != chatBody("gpt-4.1", false) {
			t.Errorf("the {} target read %s", got)
		}
		r := w.recorder.last(t)
		if len(r.Attempts) != 2 || r.Attempts[0].Provider != "zr-first" || r.Attempts[1].Provider != "zr-second" {
			t.Errorf("attempts %+v", r.Attempts)
		}
		// Both declaring targets failing ends the request; the other is
		// never tried.
		second.respondJSON(500, `{"error":"down"}`)
		before = w.openai.count()
		if code := errorCode(t, zr("mixed")); code != CodeUpstreamError {
			t.Errorf("code %s", code)
		}
		if w.openai.count() != before {
			t.Error("a failover reached the Provider that declares nothing")
		}
		// A Key without the flag takes the first target, as before.
		if rec := w.post("/openai/v1/chat/completions", chatBody("mixed", false)); rec.Code != http.StatusOK || w.openai.count() != before+1 {
			t.Errorf("a Key without the flag: %d, oai attempts %d", rec.Code, w.openai.count()-before)
		}
	})
	t.Run("declaring targets that cannot be tried now are an outage", func(t *testing.T) {
		w.router.exclude["zr-first/gpt-4.1"] = true
		w.router.exclude["zr-second/gpt-4.1"] = true
		defer delete(w.router.exclude, "zr-first/gpt-4.1")
		defer delete(w.router.exclude, "zr-second/gpt-4.1")
		before := w.openai.count()
		if code := errorCode(t, zr("mixed")); code != CodeProviderUnavailable {
			t.Errorf("code %s", code)
		}
		if w.openai.count() != before {
			t.Error("the outage sent the request to the Provider that declares nothing")
		}
	})
	t.Run("a catalog that does not answer", func(t *testing.T) {
		model, err := w.catalog.Model(context.Background(), "mixed")
		if err != nil {
			t.Fatal(err)
		}
		c := &call{h: w.h, key: &v1.Key{Spec: v1.KeySpec{ZeroRetention: true}}, model: model}
		w.catalog.err = errors.New("catalog down")
		defer func() { w.catalog.err = nil }()
		if f := c.selectTargets(context.Background()); f == nil || f.code != CodeStoreUnavailable {
			t.Errorf("failure %v", f)
		}
	})
}

// fixedRouter answers every Model with the same targets, as a Router
// that ignores the Model it is handed would.
type fixedRouter struct {
	*fakeRouter
	targets []Target
}

func (f *fixedRouter) Targets(context.Context, *v1.Model) ([]Target, error) { return f.targets, nil }

// TestZeroRetentionDecorateChecksAgain: whatever chose the target, the
// builder of the upstream request refuses a zero-retention Key toward a
// Provider that declares nothing, before any byte leaves.
func TestZeroRetentionDecorateChecksAgain(t *testing.T) {
	w := newWorld(t)
	w.zeroRetentionKey()
	oai, err := w.catalog.Provider(context.Background(), "oai")
	if err != nil {
		t.Fatal(err)
	}
	w.declare("ant", map[string]any{})
	o := w.options()
	o.Router = &fixedRouter{fakeRouter: w.router, targets: []Target{{Provider: oai, Model: "gpt-4.1"}}}
	w.h = New(o)
	before := w.openai.count()
	if code := errorCode(t, w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("dual", false)), zeroRetentionValue))); code != CodeZeroRetentionUnavailable {
		t.Errorf("code %s", code)
	}
	if w.openai.count() != before {
		t.Error("a Provider that declares nothing received the request")
	}
}

// TestZeroRetentionOnOpaqueRoutes: on a zero-retention Key an opaque
// route is refused toward a Provider whose declaration names
// requestFields and toward one that declares nothing, before the
// reservation and any upstream, and served toward one that declares {},
// its body streamed as it was sent.
func TestZeroRetentionOnOpaqueRoutes(t *testing.T) {
	w := newWorld(t)
	w.zeroRetentionKey()
	w.openai.respondJSON(200, `{}`)
	opaque := func(value, body string) *httptest.ResponseRecorder {
		r := as(w.request(http.MethodPost, "/openai/v1/files", body), value)
		r.Header.Set("Lux-Provider", "oai")
		r.ContentLength = -1
		return w.do(r)
	}
	body := `{"purpose":"batch","provider":{"zdr":false}}`
	for _, c := range []struct {
		name   string
		fields map[string]any
	}{{"a declaration with requestFields", zeroFields()}, {"no declaration", nil}} {
		t.Run(c.name, func(t *testing.T) {
			w.declare("oai", c.fields)
			upstream, reservations := w.openai.count(), w.limiter.calls.Load()
			rec := opaque(zeroRetentionValue, body)
			if code := errorCode(t, rec); code != CodeZeroRetentionUnavailable {
				t.Fatalf("code %s", code)
			}
			if w.openai.count() != upstream || w.limiter.calls.Load() != reservations {
				t.Error("the refusal reached the upstream or took a reservation")
			}
			if r := w.recorder.last(t); r.Status != StatusRefused || !r.ZeroRetention {
				t.Errorf("record %+v", r)
			}
			if rec := opaque(passthroughValue, body); rec.Code != http.StatusOK || string(w.openai.last(t).Body) != body {
				t.Errorf("a Key without the flag: %d %s", rec.Code, w.openai.last(t).Body)
			}
		})
	}
	t.Run("a declaration of {}", func(t *testing.T) {
		w.declare("oai", map[string]any{})
		if rec := opaque(zeroRetentionValue, body); rec.Code != http.StatusOK || string(w.openai.last(t).Body) != body {
			t.Errorf("%d, the upstream read %s", rec.Code, w.openai.last(t).Body)
		}
		// The Provider's general fields still apply as they do to any
		// opaque request.
		w.setRequestFields("oai", generalFields())
		defer w.setRequestFields("oai", nil)
		if rec := opaque(zeroRetentionValue, body); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if got := upstreamJSON(t, w.openai); !reflect.DeepEqual(got["provider"], map[string]any{"zdr": false, "order": []any{"vendor-b"}}) {
			t.Errorf("provider %v", got["provider"])
		}
	})
}

// TestZeroRetentionRefusesWhatCannotCarryTheFields: a model-route body
// that is not a JSON object, and a body under a coding other than
// identity in any value of Content-Encoding, are invalid_request before
// any upstream reads them; the encoding rule holds for a Provider's
// general requestFields too.
func TestZeroRetentionRefusesWhatCannotCarryTheFields(t *testing.T) {
	w := newWorld(t)
	w.zeroRetentionKey()
	w.declare("oai", zeroFields())
	w.openai.respondJSON(200, openaiChatResponse)
	before := w.openai.count()
	for _, body := range []string{`[{"model":"gpt"}]`, `"gpt"`, `null`} {
		if code := errorCode(t, w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", body), zeroRetentionValue))); code != CodeInvalidRequest {
			t.Errorf("%s: code %s", body, code)
		}
	}
	encodings := [][]string{{"gzip"}, {"identity", "gzip"}, {"identity, br"}, {"", "deflate"}}
	for _, values := range encodings {
		r := as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)), zeroRetentionValue)
		r.Header["Content-Encoding"] = values
		if code := errorCode(t, w.do(r)); code != CodeInvalidRequest {
			t.Errorf("Content-Encoding %q: code %s", values, code)
		}
	}
	w.declare("oai", nil)
	w.setRequestFields("oai", zdrFields())
	for _, values := range encodings {
		r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false))
		r.Header["Content-Encoding"] = values
		if code := errorCode(t, w.do(r)); code != CodeInvalidRequest {
			t.Errorf("requestFields, Content-Encoding %q: code %s", values, code)
		}
	}
	if w.openai.count() != before {
		t.Error("a body that cannot carry the fields reached the upstream")
	}
	r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false))
	r.Header["Content-Encoding"] = []string{"identity", " Identity "}
	if rec := w.do(r); rec.Code != http.StatusOK {
		t.Errorf("identity twice: %d %s", rec.Code, rec.Body.String())
	}

	// The body the builder holds is not a JSON object only if a builder
	// produced one; the merge refuses it rather than send it without the
	// fields, and sends it unchanged under requestFields alone.
	c := &call{h: w.h}
	p := &v1.Provider{Metadata: v1.ObjectMeta{Name: "p"}, Spec: v1.ProviderSpec{RequestFields: zdrFields(), ZeroRetention: &v1.ZeroRetention{RequestFields: zeroFields()}}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("[1]"))
	if f := c.writeRequestFields(req, p, zeroFields()); f == nil || f.code != CodeInvalidRequest {
		t.Errorf("a list under zero-retention fields: %v", f)
	}
	req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("[1]"))
	if f := c.writeRequestFields(req, p, nil); f != nil {
		t.Fatalf("a list under requestFields alone: %v", f)
	}
	if got, err := io.ReadAll(req.Body); err != nil || string(got) != "[1]" {
		t.Errorf("a list under requestFields alone became %q, %v", got, err)
	}
}

// TestZeroRetentionIsRecorded: the record, the log line, and every
// lux.upstream span say whether the request was on a zero-retention Key,
// a refused one included, and never carry a body.
func TestZeroRetentionIsRecorded(t *testing.T) {
	sr := recordSpans(t)
	w := newWorld(t)
	w.zeroRetentionKey()
	w.declare("oai", zeroFields())
	w.openai.respondJSON(200, openaiChatResponse)
	if rec := w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false)), zeroRetentionValue)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := w.post("/openai/v1/chat/completions", chatBody("gpt", false)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("claude", false)), zeroRetentionValue))); code != CodeZeroRetentionUnavailable {
		t.Fatalf("code %s", code)
	}
	var flags []bool
	for _, r := range w.recorder.records {
		flags = append(flags, r.ZeroRetention)
	}
	if !reflect.DeepEqual(flags, []bool{true, false, true}) {
		t.Errorf("records say %v", flags)
	}
	var logged []any
	for _, line := range logLines(t, w.log.String()) {
		logged = append(logged, line["zero_retention"])
	}
	if !reflect.DeepEqual(logged, []any{true, false, true}) {
		t.Errorf("log lines say %v", logged)
	}
	_, upstreams, _ := byName(sr.Ended())
	var spans []bool
	for _, u := range upstreams {
		spans = append(spans, attrsOf(u)[AttrZeroRetention].AsBool())
	}
	if !reflect.DeepEqual(spans, []bool{true, false}) {
		t.Errorf("lux.upstream spans say %v", spans)
	}
	if strings.Contains(w.log.String(), `"zdr"`) {
		t.Error("a log line carries the merged body")
	}
}

// TestZeroRetentionRereadClearsTheFlag: a Key the store no longer holds,
// found on the reread before a model refusal, leaves the record with no
// Key and no flag.
func TestZeroRetentionRereadClearsTheFlag(t *testing.T) {
	w, keys := staleWorld(t)
	const value = "lux_narrow0123456789abcdefghijklmnopqrstuvw"
	k := w.key("narrow", value, "gpt")
	k.Spec.ZeroRetention = true
	keys.gone[hashValue(value)] = true
	if code := errorCode(t, w.do(as(w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("claude", false)), value))); code != CodeUnauthenticated {
		t.Fatalf("code %s", code)
	}
	if r := w.recorder.last(t); r.ZeroRetention || r.KeyID != "" {
		t.Errorf("record %+v", r)
	}
}
