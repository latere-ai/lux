// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/lux/manifest/v1"
)

// dialects are the four, in the order of spec 005's table.
var dialects = []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux}

// server is one stub on a loopback listener.
type server struct {
	*httptest.Server
	stub *Stub
}

func start(t *testing.T, d v1.Dialect) *server {
	t.Helper()
	return serve(t, New(Options{Dialect: d}))
}

// serve puts one stub on a loopback listener for the test.
func serve(t *testing.T, s *Stub) *server {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &server{Server: srv, stub: s}
}

// decodeJSON reads body into v.
func decodeJSON(body []byte, v any) error { return json.Unmarshal(body, v) }

// credential writes the dialect's default credential header.
func credential(d v1.Dialect, h http.Header, value string) {
	if d.CredentialScheme() == v1.SchemeBearer {
		value = "Bearer " + value
	}
	h.Set(d.CredentialHeader(), value)
}

// response is one answer read whole.
type response struct {
	status int
	header http.Header
	body   []byte
	err    error // the read error, for a stream the stub cut
}

// send drives one request with the configured credential and reads the
// answer whole; redirects are not followed.
func send(t *testing.T, srv *server, method, path, body string, header http.Header) response {
	t.Helper()
	return sendCtx(t, t.Context(), srv, method, path, body, header)
}

func sendCtx(t *testing.T, ctx context.Context, srv *server, method, path, body string, header http.Header) response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if req.Header.Get(srv.stub.Dialect().CredentialHeader()) == "" {
		credential(srv.stub.Dialect(), req.Header, DefaultCredential)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, rerr := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: data, err: rerr}
}

// primary is the dialect's generation route: the path and a body naming
// model with one user message of text, streamed or not.
func primary(d v1.Dialect, model, text string, stream bool) (path, body string) {
	switch d {
	case v1.DialectOpenAI:
		return "/v1/chat/completions", marshal(map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": text}}})
	case v1.DialectAnthropic:
		return "/v1/messages", marshal(map[string]any{"model": model, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": text}}})
	case v1.DialectGemini:
		verb := "generateContent"
		if stream {
			verb = "streamGenerateContent?alt=sse"
		}
		return "/v1beta/models/" + model + ":" + verb, marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}})
	case v1.DialectLux:
		return "/v1/generate", marshal(map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "blocks": []any{map[string]any{"type": "text", "text": text}}}}})
	}
	return "", ""
}

func marshal(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// decode reads a JSON body into a generic document.
func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	return doc
}

// lookup reads a dotted path of object members and list indexes off a
// document: "choices.0.message.content".
func lookup(doc any, path string) (any, bool) {
	cur := doc
	for part := range strings.SplitSeq(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[part]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			var i int
			for _, c := range part {
				i = i*10 + int(c-'0')
			}
			if i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// number reads a JSON number at path, or fails.
func number(t *testing.T, doc any, path string) int64 {
	t.Helper()
	v, ok := lookup(doc, path)
	if !ok {
		t.Fatalf("no %s in %v", path, doc)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s is %T, not a number", path, v)
	}
	return int64(f)
}

// frame is one SSE frame: the event name and the data.
type frame struct {
	event string
	data  string
}

// frames splits an SSE body into frames.
func frames(body []byte) []frame {
	var out []frame
	sc := bufio.NewScanner(bytes.NewReader(body))
	var cur frame
	var seen bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if seen {
				out = append(out, cur)
			}
			cur, seen = frame{}, false
		case strings.HasPrefix(line, "event: "):
			cur.event, seen = strings.TrimPrefix(line, "event: "), true
		case strings.HasPrefix(line, "data: "):
			cur.data, seen = strings.TrimPrefix(line, "data: "), true
		}
	}
	if seen {
		out = append(out, cur)
	}
	return out
}

// contentEvents counts the frames that carry a content delta on the
// dialect's primary route, and joins their text.
func contentEvents(d v1.Dialect, fs []frame) (int, string) {
	var n int
	var text strings.Builder
	for _, f := range fs {
		var doc map[string]any
		if json.Unmarshal([]byte(f.data), &doc) != nil {
			continue
		}
		var path string
		switch d {
		case v1.DialectOpenAI:
			path = "choices.0.delta.content"
		case v1.DialectAnthropic:
			if doc["type"] != "content_block_delta" {
				continue
			}
			path = "delta.text"
		case v1.DialectGemini:
			path = "candidates.0.content.parts.0.text"
		case v1.DialectLux:
			if doc["type"] != "text_delta" {
				continue
			}
			path = "delta"
		}
		if v, ok := lookup(doc, path); ok {
			n++
			s, _ := v.(string)
			text.WriteString(s)
		}
	}
	return n, text.String()
}

// usageEvents counts the frames that carry the route's final usage: the
// output member, on the event that closes the message where the dialect
// has one, since message_start reports the input side alone.
func usageEvents(d v1.Dialect, fs []frame) int {
	var n int
	for _, f := range fs {
		var doc map[string]any
		if json.Unmarshal([]byte(f.data), &doc) != nil {
			continue
		}
		var path string
		switch d {
		case v1.DialectOpenAI:
			path = "usage.completion_tokens"
		case v1.DialectAnthropic, v1.DialectLux:
			if doc["type"] != "message_delta" {
				continue
			}
			path = "usage.output_tokens"
		case v1.DialectGemini:
			path = "usageMetadata.candidatesTokenCount"
		}
		if _, ok := lookup(doc, path); ok {
			n++
		}
	}
	return n
}

// shortly is a context that ends soon, for the hang row.
func shortly(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}
