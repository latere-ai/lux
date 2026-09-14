// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/httpjson"
)

// recorded is one request the fake server saw.
type recorded struct {
	Method, Path, Query, Auth, UA, Accept, ContentType, IfMatch string
	Body                                                        string
}

// fake is an httptest server that records every request and answers
// what the test scripted for the path.
type fake struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []recorded
	// answers maps "METHOD path" to a handler; a path with no entry is
	// 404 in the envelope.
	answers map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{answers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAll(r)
		f.mu.Lock()
		f.got = append(f.got, recorded{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
			UA: r.Header.Get("User-Agent"), Accept: r.Header.Get("Accept"), ContentType: r.Header.Get("Content-Type"),
			IfMatch: r.Header.Get("If-Match"), Body: body,
		})
		f.mu.Unlock()
		w.Header().Set(HeaderRequestID, "req_TEST")
		if h, ok := f.answers[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		httpjson.WriteError(w, http.StatusNotFound, httpjson.Error{Code: "not_found", Message: "There is no such object.", Details: map[string]any{"request_id": "req_TEST", "detail": r.Method + " " + r.URL.Path + " is not in the route table"}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func readAll(r *http.Request) (string, error) {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String(), nil
		}
	}
}

func (f *fake) on(method, path string, status int, body string) {
	f.answers[method+" "+path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (f *fake) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[len(f.got)-1]
}

func (f *fake) client() *Client {
	return &Client{BaseURL: f.srv.URL + "/", Token: StaticToken("tok-1"), UserAgent: "lux/test", HTTP: f.srv.Client()}
}

func TestDoSendsTheHeadersAndReturnsTheBytes(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/v1/self", 200, `{"subject":"a|b","policy":"owner"}`+"\n")
	resp, err := f.client().Self(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || string(resp.Body) != `{"subject":"a|b","policy":"owner"}`+"\n" || resp.RequestID != "req_TEST" {
		t.Fatalf("response %+v", resp)
	}
	got := f.last()
	if got.Auth != "Bearer tok-1" || got.UA != "lux/test" || got.Accept != "application/json" || got.ContentType != "" {
		t.Fatalf("request %+v", got)
	}
}

func TestRoutesAndAddressing(t *testing.T) {
	f := newFake(t)
	c := f.client()
	ctx := t.Context()
	for path := range map[string]bool{
		"PUT /v1/providers/openai": true, "GET /v1/keys/run-42": true, "DELETE /v1/budgets/team": true,
		"POST /v1/keys/run-42/rotate": true, "GET /v1/models/openai/gpt-5": true, "GET /v1/usage": true,
		"GET /lux/v1/models": true, "GET /.well-known/lux": true, "GET /v1/keys/a b": true,
	} {
		method, p, _ := strings.Cut(path, " ")
		f.on(method, p, 200, `{}`)
	}
	cases := []struct {
		name    string
		call    func() (*Response, error)
		method  string
		path    string
		ifMatch string
		body    string
		ctype   string
		auth    string
	}{
		{"apply", func() (*Response, error) {
			return c.Apply(ctx, "providers", "openai", []byte(`{"spec":{}}`), "application/json", "7")
		}, "PUT", "/v1/providers/openai", `"7"`, `{"spec":{}}`, "application/json", "Bearer tok-1"},
		{"apply star", func() (*Response, error) {
			return c.Apply(ctx, "providers", "openai", []byte(`a: b`), "application/yaml", "*")
		}, "PUT", "/v1/providers/openai", `*`, `a: b`, "application/yaml", "Bearer tok-1"},
		{"apply quoted", func() (*Response, error) {
			return c.Apply(ctx, "providers", "openai", []byte(`{}`), "application/json", `"9"`)
		}, "PUT", "/v1/providers/openai", `"9"`, `{}`, "application/json", "Bearer tok-1"},
		{"get", func() (*Response, error) { return c.Get(ctx, "keys", "run-42") }, "GET", "/v1/keys/run-42", "", "", "", "Bearer tok-1"},
		{"get model with a slash", func() (*Response, error) { return c.Get(ctx, "models", "openai/gpt-5") }, "GET", "/v1/models/openai/gpt-5", "", "", "", "Bearer tok-1"},
		{"get escapes", func() (*Response, error) { return c.Get(ctx, "keys", "a b") }, "GET", "/v1/keys/a b", "", "", "", "Bearer tok-1"},
		{"delete", func() (*Response, error) { return c.Delete(ctx, "budgets", "team", "3") }, "DELETE", "/v1/budgets/team", `"3"`, "", "", "Bearer tok-1"},
		{"rotate", func() (*Response, error) { return c.Rotate(ctx, "run-42") }, "POST", "/v1/keys/run-42/rotate", "", "", "", "Bearer tok-1"},
		{"usage", func() (*Response, error) { return c.Usage(ctx, url.Values{"by": {"model"}}) }, "GET", "/v1/usage", "", "", "", "Bearer tok-1"},
		{"door models", func() (*Response, error) { return c.Models(ctx) }, "GET", "/lux/v1/models", "", "", "", "Bearer tok-1"},
		{"well-known without a bearer", func() (*Response, error) { return c.WellKnown(ctx) }, "GET", "/.well-known/lux", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); err != nil {
				t.Fatal(err)
			}
			got := f.last()
			if got.Method != tc.method || got.Path != tc.path || got.IfMatch != tc.ifMatch || got.Body != tc.body || got.ContentType != tc.ctype || got.Auth != tc.auth {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if q := f.got[len(f.got)-3].Query; q != "by=model" {
		t.Fatalf("usage query %q", q)
	}
}

func TestRefusalIsDecodedFromTheEnvelope(t *testing.T) {
	f := newFake(t)
	f.answers["PUT /v1/providers/x"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		httpjson.WriteError(w, http.StatusTooManyRequests, httpjson.Error{Code: "rate_limited", Message: "Too many requests; wait and retry.",
			Details: map[string]any{"request_id": "req_ENV", "paths": []any{"spec.baseURL", 7}, "detail": "the bucket is empty"}})
	}
	_, err := f.client().Apply(t.Context(), "providers", "x", []byte(`{}`), "application/json", "")
	e, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("error %T %v", err, err)
	}
	if e.Status != 429 || e.Code != "rate_limited" || e.Message != "Too many requests; wait and retry." || e.RequestID != "req_ENV" || e.Detail != "the bucket is empty" || e.RetryAfter != 30 || strings.Join(e.Paths, ",") != "spec.baseURL" {
		t.Fatalf("decoded %+v", e)
	}
	if got := e.Error(); got != "rate_limited at spec.baseURL: the bucket is empty" {
		t.Fatalf("Error() = %q", got)
	}
	if got := (&Error{Code: "x"}).Error(); got != "x" {
		t.Fatalf("bare Error() = %q", got)
	}
}

func TestRequestIDFallsBackToTheHeader(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/v1/keys/x", 404, `{"error":{"code":"not_found","message":"There is no such object."}}`)
	_, err := f.client().Get(t.Context(), "keys", "x")
	e, ok := errors.AsType[*Error](err)
	if !ok || e.RequestID != "req_TEST" || e.Detail != "" || e.Paths != nil {
		t.Fatalf("error %T %+v", err, err)
	}
}

func TestUnreadableResponse(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/v1/keys/x", 502, "<html>bad gateway"+strings.Repeat("!", 300)+"</html>")
	_, err := f.client().Get(t.Context(), "keys", "x")
	e, ok := errors.AsType[*UnreadableError](err)
	if !ok || e.Status != 502 || e.RequestID != "req_TEST" || !strings.HasPrefix(string(e.Body), "<html>") {
		t.Fatalf("error %T %+v", err, err)
	}
	if msg := e.Error(); !strings.Contains(msg, "status 502") || !strings.Contains(msg, "...") || len(msg) > 320 {
		t.Fatalf("Error() = %q", msg)
	}
	f.on(http.MethodGet, "/v1/keys/y", 500, `{"error":{"message":"no code"}}`)
	if _, err := f.client().Get(t.Context(), "keys", "y"); !errors.Is(err, err) || func() bool { _, ok := errors.AsType[*UnreadableError](err); return !ok }() {
		t.Fatalf("an envelope without a code: %T", err)
	}
}

func TestTransportFailureIsATransportError(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", Token: StaticToken("t"), HTTP: &http.Client{Timeout: 2 * time.Second}}
	_, err := c.Self(t.Context())
	e, ok := errors.AsType[*TransportError](err)
	if !ok || e.Method != "GET" || !strings.HasSuffix(e.URL, "/v1/self") || e.Unwrap() == nil || !strings.Contains(e.Error(), "GET http://127.0.0.1:1/v1/self: ") {
		t.Fatalf("error %T %v", err, err)
	}
	bad := &Client{BaseURL: "http://127.0.0.1:1"}
	if _, err := bad.Do(t.Context(), Request{Method: "BAD METHOD", Path: "/x"}); func() bool { _, ok := errors.AsType[*TransportError](err); return !ok }() {
		t.Fatalf("a request that cannot be built: %T %v", err, err)
	}
}

func TestTokenSourceErrorStopsTheRequest(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/v1/self", 200, `{}`)
	c := f.client()
	c.Token = FileToken(filepath.Join(t.TempDir(), "missing"))
	if _, err := c.Self(t.Context()); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v", err)
	}
	if len(f.got) != 0 {
		t.Fatal("a request was sent without a token")
	}
}

func TestTokenSources(t *testing.T) {
	if s, err := StaticToken("abc")(); err != nil || s != "abc" {
		t.Fatalf("static: %q %v", s, err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := FileToken(path)
	if s, err := src(); err != nil || s != "first" {
		t.Fatalf("file: %q %v", s, err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _ := src(); s != "second" {
		t.Fatalf("the file is read per call: %q", s)
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src(); !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("empty file: %v", err)
	}
}

func TestDefaultHTTPClientIsUsedWhenNoneIsSet(t *testing.T) {
	f := newFake(t)
	f.on(http.MethodGet, "/v1/self", 200, `{}`)
	c := f.client()
	c.HTTP = nil
	if _, err := c.Self(t.Context()); err != nil {
		t.Fatal(err)
	}
	tr, ok := NewHTTPClient().Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout != FirstByteTimeout || tr.Proxy == nil {
		t.Fatalf("transport %+v", tr)
	}
}

func TestListFollowsNextCursor(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	f.answers["GET /v1/keys"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = w.Write([]byte(`{"items":[{"b":1,"a": "x"},{"c":[1, 2]}],"next_cursor":"c2"}` + "\n"))
		case "c2":
			_, _ = w.Write([]byte(`{"items":[{"d":null}],"next_cursor":"c3"}` + "\n"))
		default:
			_, _ = w.Write([]byte(`{"items":[{"e":true}],"source":"memory"}` + "\n"))
		}
	}
	c := f.client()
	resp, err := c.List(t.Context(), "/v1/keys", url.Values{"label": {"team=a"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"items":[{"b":1,"a": "x"},{"c":[1, 2]},{"d":null},{"e":true}],"source":"memory"}` + "\n"
	if string(resp.Body) != want {
		t.Fatalf("assembled\n%s\nwant\n%s", resp.Body, want)
	}
	if len(f.got) != 3 || f.got[1].Query != "cursor=c2&label=team%3Da" || f.got[2].Query != "cursor=c3&label=team%3Da" {
		t.Fatalf("requests %+v", f.got)
	}

	// --limit is a count of items: three stop the walk after the second
	// page and truncate to three.
	f.got = nil
	resp, err = c.List(t.Context(), "/v1/keys", nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"items":[{"b":1,"a": "x"},{"c":[1, 2]},{"d":null}]}`+"\n" || len(f.got) != 2 {
		t.Fatalf("limited body %s after %d requests", resp.Body, len(f.got))
	}

	// One page under the bound is the server's bytes untouched.
	f.on(http.MethodGet, "/v1/budgets", 200, `{ "items" : [ {"x":1} ] }`)
	resp, err = c.List(t.Context(), "/v1/budgets", nil, 5)
	if err != nil || string(resp.Body) != `{ "items" : [ {"x":1} ] }` {
		t.Fatalf("one page: %s %v", resp.Body, err)
	}
	// One page over the bound is rebuilt from its items.
	resp, err = c.List(t.Context(), "/v1/budgets", nil, 0)
	if err != nil || string(resp.Body) != `{ "items" : [ {"x":1} ] }` {
		t.Fatalf("unbounded one page: %s %v", resp.Body, err)
	}
	f.on(http.MethodGet, "/v1/models", 200, `{"items":[{"x":1},{"y":2}]}`+"\n")
	resp, err = c.Requests(t.Context(), nil, 1)
	if err == nil {
		t.Fatal("requests has no answer here and must be not_found")
	}
	resp, err = c.List(t.Context(), "/v1/models", nil, 1)
	if err != nil || string(resp.Body) != `{"items":[{"x":1}]}`+"\n" {
		t.Fatalf("truncated one page: %s %v", resp.Body, err)
	}
}

func TestListRefusesABodyThatIsNotAList(t *testing.T) {
	f := newFake(t)
	for _, body := range []string{`[]`, `{"foo":1}`, `{"items":`, `{"items":[], "next_cursor": 5}`, `{"items": {}}`, `{`, `x`} {
		f.on(http.MethodGet, "/v1/keys", 200, body)
		_, err := f.client().List(t.Context(), "/v1/keys", nil, 0)
		if _, ok := errors.AsType[*UnreadableError](err); !ok {
			t.Fatalf("body %q: %T %v", body, err, err)
		}
	}
	f.answers["GET /v1/keys"] = func(w http.ResponseWriter, _ *http.Request) {
		httpjson.WriteError(w, http.StatusForbidden, httpjson.Error{Code: "forbidden", Message: "You do not have permission to do this."})
	}
	_, err := f.client().List(t.Context(), "/v1/keys", nil, 0)
	if e, ok := errors.AsType[*Error](err); !ok || e.Code != "forbidden" {
		t.Fatalf("refusal on a page: %T %v", err, err)
	}
}

func TestObjectPath(t *testing.T) {
	for in, want := range map[string]string{
		"providers/openai":      "/v1/providers/openai",
		"models/openai/gpt-5":   "/v1/models/openai/gpt-5",
		"keys/a b":              "/v1/keys/a%20b",
		"budgets/team?x":        "/v1/budgets/team%3Fx",
		"models/mdl_01J9ZK2P7Q": "/v1/models/mdl_01J9ZK2P7Q",
	} {
		plural, name, _ := strings.Cut(in, "/")
		if got := ObjectPath(plural, name); got != want {
			t.Errorf("ObjectPath(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestContextCancellationIsATransportError(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.client().Do(ctx, Request{Method: http.MethodGet, Path: "/v1/self"})
	if _, ok := errors.AsType[*TransportError](err); !ok {
		t.Fatalf("%T %v", err, err)
	}
}
