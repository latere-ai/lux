// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// zdrFields are the requestFields an aggregator's zero data retention
// routing is set with.
func zdrFields() map[string]any {
	return map[string]any{"provider": map[string]any{"zdr": true, "data_collection": "deny"}}
}

// TestMergeRequestFields: the Provider's members win on every leaf they
// name, an object merges member by member, any other value, a list
// included, replaces the caller's whole, the caller's other members keep
// their bytes, a member that differs only in case or a duplicate member
// cannot carry the caller's value past the merge, and a body that is not
// a JSON object passes unchanged.
func TestMergeRequestFields(t *testing.T) {
	cases := []struct {
		name, body string
		fields     map[string]any
		want       string
	}{
		{"an absent member is added", `{"model":"m"}`, zdrFields(), `{"model":"m","provider":{"data_collection":"deny","zdr":true}}`},
		{"a caller's leaf is overridden", `{"provider":{"zdr":false}}`, zdrFields(), `{"provider":{"data_collection":"deny","zdr":true}}`},
		{"a caller's siblings are kept", `{"provider":{"order":["x"],"allow_fallbacks":false},"temperature":0.5}`, zdrFields(), `{"provider":{"allow_fallbacks":false,"data_collection":"deny","order":["x"],"zdr":true},"temperature":0.5}`},
		{"objects merge at every depth", `{"a":{"b":{"c":1,"d":2},"e":3}}`, map[string]any{"a": map[string]any{"b": map[string]any{"c": float64(4)}}}, `{"a":{"b":{"c":4,"d":2},"e":3}}`},
		{"a list replaces a list whole", `{"provider":{"order":["x","y"]}}`, map[string]any{"provider": map[string]any{"order": []any{"a"}}}, `{"provider":{"order":["a"]}}`},
		{"an object replaces a string", `{"provider":"fast"}`, zdrFields(), `{"provider":{"data_collection":"deny","zdr":true}}`},
		{"an object replaces null", `{"provider":null}`, zdrFields(), `{"provider":{"data_collection":"deny","zdr":true}}`},
		{"an object replaces a list", `{"provider":[{"zdr":false}]}`, zdrFields(), `{"provider":{"data_collection":"deny","zdr":true}}`},
		{"a scalar replaces an object", `{"user":{"id":1}}`, map[string]any{"user": "anonymous"}, `{"user":"anonymous"}`},
		{"an empty object adds an empty object", `{"model":"m"}`, map[string]any{"provider": map[string]any{}}, `{"model":"m","provider":{}}`},
		{"a member in another case is removed", `{"Provider":{"zdr":false},"provider":{"ZDR":false,"order":["x"]}}`, map[string]any{"provider": map[string]any{"zdr": true}}, `{"provider":{"order":["x"],"zdr":true}}`},
		{"a member equal under folding is removed", `{"Key":1}`, map[string]any{"key": float64(2)}, `{"key":2}`},
		{"a duplicate collapses to the last", `{"provider":{"zdr":false},"a":1,"a":2,"provider":{"order":["x"],"zdr":false,"zdr":false}}`, map[string]any{"provider": map[string]any{"zdr": true}}, `{"a":2,"provider":{"order":["x"],"zdr":true}}`},
		{"untouched values keep their bytes", `{ "seed": 12345678901234567890, "x": 1.0, "s": "<a> & é", "m": [1, 2], "n": null }`, map[string]any{"z": true}, `{"m":[1, 2],"n":null,"s":"<a> & é","seed":12345678901234567890,"x":1.0,"z":true}`},
		{"a Provider's string is not escaped", `{}`, map[string]any{"route": "<a&b>"}, `{"route":"<a&b>"}`},
		{"a whole number stays whole", `{}`, map[string]any{"cap": float64(1000000)}, `{"cap":1000000}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := fmt.Sprint(c.fields)
			got, err := mergeRequestFields([]byte(c.body), c.fields)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if !json.Valid(got) {
				t.Errorf("not JSON: %s", got)
			}
			if fmt.Sprint(c.fields) != before {
				t.Errorf("the merge changed the Provider's fields: %v", c.fields)
			}
		})
	}
	t.Run("a body that is not a JSON object is unchanged", func(t *testing.T) {
		for _, body := range []string{"", "null", "[1,2]", `"s"`, "3", "not json", `{"a":`, "--b\r\nContent-Disposition: form-data; name=\"x\"\r\n\r\n{}\r\n--b--\r\n"} {
			got, err := mergeRequestFields([]byte(body), zdrFields())
			if err != nil || string(got) != body {
				t.Errorf("%q became %q, %v", body, got, err)
			}
		}
	})
	t.Run("a value with no JSON form is an error", func(t *testing.T) {
		for _, fields := range []map[string]any{{"f": func() {}}, {"o": map[string]any{"f": func() {}}}} {
			if _, err := mergeRequestFields([]byte(`{}`), fields); err == nil {
				t.Errorf("%v merged", fields)
			}
		}
	})
}

// setRequestFields gives one of the fixture's Providers requestFields.
func (w *world) setRequestFields(name string, fields map[string]any) {
	w.t.Helper()
	p, err := w.catalog.Provider(context.Background(), name)
	if err != nil || p == nil {
		w.t.Fatalf("no Provider %s: %v", name, err)
	}
	p.Spec.RequestFields = fields
}

// upstreamJSON decodes the body a stub received.
func upstreamJSON(t *testing.T, s *stub) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.last(t).Body, &m); err != nil {
		t.Fatalf("the upstream body is not a JSON object: %v\n%s", err, s.last(t).Body)
	}
	return m
}

// TestRequestFieldsReachTheUpstream: a Provider's requestFields arrive in
// the body of every model route sent to it, on every dialect, passthrough
// and translated, streaming and not, merged over the caller's members;
// GetBody replays the merged body; an encoded body is refused before any
// upstream sees it; and neither the record nor the log line carries a
// body.
func TestRequestFieldsReachTheUpstream(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"oai", "ant", "gem", "lx"} {
		w.setRequestFields(name, zdrFields())
	}
	openaiFrames := []string{
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"id\":\"c1\",\"model\":\"gpt-4.1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1}}\n\n",
		"data: [DONE]\n\n",
	}
	callerProvider := `"provider":{"order":["vendor-a"],"zdr":false}`
	cases := []struct {
		name, path, body string
		upstream         *stub
		respond          func(*stub)
		stream           bool
		callerOrder      bool // the caller's provider.order reaches the upstream
	}{
		{"openai passthrough", "/openai/v1/chat/completions", `{"model":"gpt","messages":[{"role":"user","content":"hi"}],` + callerProvider + `}`, w.openai, func(s *stub) { s.respondJSON(200, openaiChatResponse) }, false, true},
		{"openai passthrough stream", "/openai/v1/chat/completions", `{"model":"gpt","stream":true,"messages":[{"role":"user","content":"hi"}],` + callerProvider + `}`, w.openai, func(s *stub) { s.respondSSE(openaiFrames...) }, true, true},
		{"anthropic door to openai", "/anthropic/v1/messages", messagesBody("gpt", false), w.openai, func(s *stub) { s.respondJSON(200, openaiChatResponse) }, false, false},
		{"anthropic door to openai stream", "/anthropic/v1/messages", messagesBody("gpt", true), w.openai, func(s *stub) { s.respondSSE(openaiFrames...) }, true, false},
		{"openai door to anthropic", "/openai/v1/chat/completions", chatBody("claude", false), w.anthropic, func(s *stub) { s.respondJSON(200, anthropicResponse) }, false, false},
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
				replay, _ = io.ReadAll(rc)
			}
			defer func() { w.clients.transport.seen = nil }()
			rec := w.post(c.path, c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			got := upstreamJSON(t, c.upstream)
			provider, _ := got["provider"].(map[string]any)
			if provider["zdr"] != true || provider["data_collection"] != "deny" {
				t.Errorf("provider %v", got["provider"])
			}
			if c.callerOrder && !reflect.DeepEqual(provider["order"], []any{"vendor-a"}) {
				t.Errorf("the caller's sibling was lost: %v", got["provider"])
			}
			if c.stream && c.upstream != w.gemini && got["stream"] != true {
				t.Errorf("stream %v", got["stream"])
			}
			if !bytes.Equal(replay, c.upstream.last(t).Body) {
				t.Errorf("GetBody replays\n%s\nthe upstream read\n%s", replay, c.upstream.last(t).Body)
			}
		})
	}
	t.Run("the gateway's own members survive", func(t *testing.T) {
		w.openai.respondSSE(openaiFrames...)
		if rec := w.post("/openai/v1/chat/completions", `{"model":"alias","stream":true,"messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		got := upstreamJSON(t, w.openai)
		options, _ := got["stream_options"].(map[string]any)
		if got["model"] != "gpt-4.1-mini" || options["include_usage"] != true {
			t.Errorf("model %v, stream_options %v", got["model"], got["stream_options"])
		}
	})
	t.Run("an encoded body is refused", func(t *testing.T) {
		before := w.openai.count()
		r := w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false))
		r.Header.Set("Content-Encoding", "gzip")
		if code := errorCode(t, w.do(r)); code != CodeInvalidRequest {
			t.Errorf("code %s", code)
		}
		if w.openai.count() != before {
			t.Error("the upstream received an encoded body")
		}
		w.openai.respondJSON(200, openaiChatResponse)
		r = w.request(http.MethodPost, "/openai/v1/chat/completions", chatBody("gpt", false))
		r.Header.Set("Content-Encoding", "identity")
		if rec := w.do(r); rec.Code != http.StatusOK {
			t.Errorf("identity: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("fields with no JSON form fail closed", func(t *testing.T) {
		// Resolve refuses such fields; a Provider a platform built
		// without it is still never sent the body without them.
		w.setRequestFields("oai", map[string]any{"f": func() {}})
		defer w.setRequestFields("oai", zdrFields())
		before := w.openai.count()
		if code := errorCode(t, w.post("/openai/v1/chat/completions", chatBody("gpt", false))); code != CodeProviderUnavailable {
			t.Errorf("code %s", code)
		}
		if w.openai.count() != before {
			t.Error("the upstream received a body without the fields")
		}
	})
	if strings.Contains(w.log.String(), "data_collection") || strings.Contains(fmt.Sprintf("%+v", w.recorder.records), "data_collection") {
		t.Error("a log line or a record carries the merged body")
	}
}

// TestRequestFieldsOnOpaqueRoutes: toward a Provider with requestFields,
// an opaque route's JSON object body is read whole and merged, any other
// body passes unchanged, a request with no body still has none, an
// encoded body is invalid_request, and a body above the limit is
// body_too_large, the last two before any upstream sees them.
func TestRequestFieldsOnOpaqueRoutes(t *testing.T) {
	w := newWorld(t)
	w.setRequestFields("oai", zdrFields())
	w.openai.respondJSON(200, `{}`)
	opaque := func(method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
		r := w.request(method, path, body)
		r.Header.Set("Authorization", "Bearer "+passthroughValue)
		r.ContentLength = -1 // read to the end, as a chunked upload is
		if edit != nil {
			edit(r)
		}
		return w.do(r)
	}
	rec := opaque(http.MethodPost, "/openai/v1/completions", `{"model":"x","prompt":"hi","provider":{"zdr":false,"order":["vendor-a"]}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := upstreamJSON(t, w.openai)
	if want := map[string]any{"model": "x", "prompt": "hi", "provider": map[string]any{"zdr": true, "data_collection": "deny", "order": []any{"vendor-a"}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("opaque JSON body %v", got)
	}
	multipart := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b--\r\n"
	rec = opaque(http.MethodPost, "/openai/v1/files", multipart, func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=b") })
	if rec.Code != http.StatusOK || string(w.openai.last(t).Body) != multipart {
		t.Errorf("a multipart body: %d %q", rec.Code, w.openai.last(t).Body)
	}
	rec = opaque(http.MethodGet, "/openai/v1/files/f1", "", nil)
	if last := w.openai.last(t); rec.Code != http.StatusOK || last.Method != http.MethodGet || len(last.Body) != 0 {
		t.Errorf("no body: %d %s %q", rec.Code, last.Method, last.Body)
	}
	before := w.openai.count()
	rec = opaque(http.MethodPost, "/openai/v1/completions", "\x1f\x8b\x08\x00", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") })
	if code := errorCode(t, rec); code != CodeInvalidRequest {
		t.Errorf("an encoded body: %s", code)
	}
	rec = opaque(http.MethodPost, "/openai/v1/completions", `{"prompt":"`+strings.Repeat("x", int(w.h.maxBody))+`"}`, nil)
	if code := errorCode(t, rec); code != CodeBodyTooLarge {
		t.Errorf("a body above the limit: %s", code)
	}
	if w.openai.count() != before {
		t.Error("a refused body reached the upstream")
	}
	// Without requestFields the opaque body is streamed as it was.
	w.setRequestFields("oai", nil)
	body := `{"provider":{"zdr":false}}`
	if rec := opaque(http.MethodPost, "/openai/v1/completions", body, nil); rec.Code != http.StatusOK || string(w.openai.last(t).Body) != body {
		t.Errorf("without requestFields: %d %s", rec.Code, w.openai.last(t).Body)
	}
	if w.recorder.last(t).Provider != "oai" {
		t.Errorf("record %+v", w.recorder.last(t))
	}
}
