// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/lux/internal/tunnel/wire"
)

// fakeGateway speaks the wire protocol by hand over h2c, so the agent is
// held to the bytes rather than to the gateway package: the session
// route writes ready, reads heartbeats, and closes with the reason the
// test picks once a heartbeat carries the fresh token; the carrier route
// hands each carrier one request from a queue and records the answer.
type fakeGateway struct {
	srv      *httptest.Server
	token    string // the bearer the routes accept
	fresh    string // the token whose arrival in a heartbeat closes the session
	reason   string
	requests chan wire.Request
	mu       sync.Mutex
	frames   []wire.Frame
	answers  []answer
	carriers []http.Header
	refuse   int // carriers to refuse before accepting
}

// answer is what one carrier brought back.
type answer struct {
	resp wire.Response
	body string
}

func newFakeGateway(t *testing.T, reason string) *fakeGateway {
	t.Helper()
	g := &fakeGateway{token: "t1", fresh: "t2", reason: reason, requests: make(chan wire.Request, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/providers/{name}/tunnel", g.session)
	mux.HandleFunc("POST /v1/providers/{name}/tunnel/carry", g.carry)
	g.srv = httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	g.srv.Config.Protocols = protocols
	g.srv.Start()
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) session(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+g.token || r.PathValue("name") != "laptop" || r.ProtoMajor != 2 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"You do not have permission to do this.","details":{"detail":"authz deny"}}}`)
		return
	}
	out := wire.Flushing(w)
	w.WriteHeader(http.StatusOK)
	_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeReady, Session: "tun_1", TTL: "300ms", Carriers: 2})
	rd := bufio.NewReader(r.Body)
	for {
		var f wire.Frame
		if err := wire.ReadLine(rd, &f); err != nil {
			return
		}
		g.mu.Lock()
		g.frames = append(g.frames, f)
		g.mu.Unlock()
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeHeartbeat})
		if f.Token == g.fresh && g.reason != "" {
			_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeClose, Reason: g.reason})
			return
		}
	}
}

func (g *fakeGateway) carry(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.carriers = append(g.carriers, r.Header.Clone())
	refuse := g.refuse > 0
	if refuse {
		g.refuse--
	}
	g.mu.Unlock()
	if refuse || r.Header.Get(wire.HeaderSession) != "tun_1" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer t") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"This request needs a valid credential.","details":{}}}`)
		return
	}
	out := wire.Flushing(w)
	w.WriteHeader(http.StatusOK)
	_ = wire.Keepalive(out)
	var req wire.Request
	select {
	case req = <-g.requests:
	case <-r.Context().Done():
		return
	}
	_ = wire.WriteLine(out, req)
	bw := wire.NewBodyWriter(out)
	if body, ok := req.Headers["X-Test-Body"]; ok {
		_, _ = io.WriteString(bw, body[0])
	}
	_ = bw.Close()
	rd := bufio.NewReader(r.Body)
	var resp wire.Response
	if err := wire.ReadLine(rd, &resp); err != nil {
		return
	}
	body, _ := io.ReadAll(wire.NewBodyReader(rd))
	g.mu.Lock()
	g.answers = append(g.answers, answer{resp, string(body)})
	g.mu.Unlock()
}

func (g *fakeGateway) answered() []answer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]answer(nil), g.answers...)
}

func (g *fakeGateway) heartbeats() []wire.Frame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]wire.Frame(nil), g.frames...)
}

// runtime is a fake local model server that echoes what it saw.
func runtime(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Transfer-Encoding", "chunked")
		if r.URL.Path == "/v1/fail" {
			w.WriteHeader(http.StatusBadGateway)
		}
		_, _ = fmt.Fprintf(w, `{"method":%q,"path":%q,"query":%q,"body":%q,"length":%d,"te":%q,"auth":%q}`, r.Method, r.URL.Path, r.URL.RawQuery, body, r.ContentLength, strings.Join(r.TransferEncoding, ","), r.Header.Get("Authorization"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunServesAndReturnsTheCloseReason: the agent opens the session
// with the token, parks the carriers the ready frame asks for, serves a
// GET, a POST with a body, and an error status against the runtime with
// the runtime's own headers and no bearer, sends the fresh token in a
// heartbeat once the source yields it, and returns the close reason.
func TestRunServesAndReturnsTheCloseReason(t *testing.T) {
	g := newFakeGateway(t, wire.ReasonDraining)
	rt := runtime(t)
	g.requests <- wire.Request{ID: "req_1", Method: "GET", Path: "/models", Query: "limit=1", Headers: map[string][]string{"Accept": {"application/json"}, "Authorization": {"Bearer leaked"}}}
	g.requests <- wire.Request{ID: "req_2", Method: "POST", Path: "/chat/completions", Headers: map[string][]string{"Content-Type": {"application/json"}, "Content-Length": {"11"}, "X-Test-Body": {`{"n":"two"}`}, "Connection": {"close"}}}
	g.requests <- wire.Request{ID: "req_3", Method: "GET", Path: "/fail", Headers: map[string][]string{}}
	// The source yields the fresh token once every queued request has
	// been answered, so several plain heartbeats come first.
	token := func() (string, error) {
		if len(g.answered()) == 3 {
			return "t2", nil
		}
		return "t1", nil
	}
	err := Run(t.Context(), Options{Gateway: g.srv.URL, Provider: "laptop", Upstream: rt.URL + "/v1/", Token: token, UserAgent: "lux/test", Logger: slog.New(slog.DiscardHandler)})
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Reason != wire.ReasonDraining || !strings.Contains(ce.Error(), "draining") {
		t.Fatalf("Run returned %v, want the close reason draining", err)
	}
	answers := g.answered()
	if len(answers) != 3 {
		t.Fatalf("%d answers: %+v", len(answers), answers)
	}
	byPath := map[string]answer{}
	for _, a := range answers {
		byPath[a.body[strings.Index(a.body, `"path":"`)+8:][:strings.Index(a.body[strings.Index(a.body, `"path":"`)+8:], `"`)]] = a
	}
	if a := byPath["/v1/models"]; a.resp.Status != 200 || a.resp.Headers["Content-Type"][0] != "application/json" || !strings.Contains(a.body, `"query":"limit=1"`) || !strings.Contains(a.body, `"auth":"Bearer leaked"`) || !strings.Contains(a.body, `"length":0`) {
		t.Errorf("GET /models: %+v", a)
	}
	if a := byPath["/v1/chat/completions"]; a.resp.Status != 200 || !strings.Contains(a.body, `"body":"{\"n\":\"two\"}"`) || !strings.Contains(a.body, `"length":11`) || !strings.Contains(a.body, `"te":""`) {
		t.Errorf("POST with a body: %+v", a)
	}
	if a := byPath["/v1/fail"]; a.resp.Status != 502 {
		t.Errorf("an error status: %+v", a)
	}
	for _, a := range answers {
		if _, ok := a.resp.Headers["Transfer-Encoding"]; ok {
			t.Errorf("a hop header crossed: %v", a.resp.Headers)
		}
	}
	// The last heartbeat carried the fresh token, and none before it did.
	beats := g.heartbeats()
	if len(beats) == 0 || beats[len(beats)-1].Token != "t2" {
		t.Errorf("heartbeats %+v", beats)
	}
	for _, b := range beats[:len(beats)-1] {
		if b.Token != "" {
			t.Errorf("an unchanged token was sent: %+v", b)
		}
	}
	g.mu.Lock()
	carriers := len(g.carriers)
	g.mu.Unlock()
	// Two parked, one replacement per request served.
	if carriers < 5 {
		t.Errorf("%d carriers were opened, want at least 5", carriers)
	}
}

// TestRunReportsAnUnreachableRuntime: a runtime the agent cannot reach
// is the response line's error, naming no address of this machine, and
// a carrier the gateway refuses is retried while the session lives.
func TestRunReportsAnUnreachableRuntime(t *testing.T) {
	g := newFakeGateway(t, wire.ReasonSuperseded)
	g.refuse = 1
	g.requests <- wire.Request{ID: "req_1", Method: "GET", Path: "/models", Headers: map[string][]string{}}
	token := func() (string, error) {
		if len(g.answered()) == 1 {
			return "t2", nil
		}
		return "t1", nil
	}
	err := Run(t.Context(), Options{Gateway: g.srv.URL, Provider: "laptop", Upstream: "http://127.0.0.1:9/v1", Token: token, Carriers: 1, Logger: slog.New(slog.DiscardHandler)})
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Reason != wire.ReasonSuperseded {
		t.Fatalf("Run returned %v", err)
	}
	answers := g.answered()
	if len(answers) != 1 || answers[0].resp.Error == "" || answers[0].resp.Status != 0 || strings.Contains(answers[0].resp.Error, "127.0.0.1:9") || !strings.Contains(answers[0].resp.Error, "the runtime") {
		t.Fatalf("answers %+v", answers)
	}
}

// TestRunRefusals: what Run returns before a session: the options it
// cannot open with, a refused connect with the envelope's code or the
// body's excerpt, a first frame that is not ready, and a stream that
// ends.
func TestRunRefusals(t *testing.T) {
	ok := func() (string, error) { return "t1", nil }
	for name, o := range map[string]Options{
		"no gateway":   {Provider: "p", Upstream: "http://127.0.0.1:1", Token: ok},
		"no provider":  {Gateway: "http://127.0.0.1:1", Upstream: "http://127.0.0.1:1", Token: ok},
		"no upstream":  {Gateway: "http://127.0.0.1:1", Provider: "p", Token: ok},
		"no token":     {Gateway: "http://127.0.0.1:1", Provider: "p", Upstream: "http://127.0.0.1:1"},
		"bad gateway":  {Gateway: "ftp://x", Provider: "p", Upstream: "http://127.0.0.1:1", Token: ok},
		"bad upstream": {Gateway: "http://127.0.0.1:1", Provider: "p", Upstream: "127.0.0.1:1", Token: ok},
	} {
		if err := Run(t.Context(), o); err == nil || !strings.HasPrefix(err.Error(), "agent: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Run(t.Context(), Options{Gateway: "http://127.0.0.1:1", Provider: "p", Upstream: "http://127.0.0.1:1", Token: func() (string, error) { return "", errors.New("no file") }}); err == nil || !strings.Contains(err.Error(), "reading the token") {
		t.Errorf("a token source that fails: %v", err)
	}
	if err := Run(t.Context(), Options{Gateway: "http://127.0.0.1:1", Provider: "p", Upstream: "http://127.0.0.1:1", Token: ok}); err == nil || !strings.Contains(err.Error(), "connecting to") {
		t.Errorf("a gateway that is not there: %v", err)
	}
	g := newFakeGateway(t, "")
	err := Run(t.Context(), Options{Gateway: g.srv.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1", Token: func() (string, error) { return "wrong", nil }})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Status != 403 || refused.Code != "forbidden" || refused.Detail != "authz deny" || refused.Message == "" || !strings.Contains(refused.Error(), "403 forbidden: authz deny") {
		t.Errorf("a refused connect: %v", err)
	}
	// Servers that do not speak the protocol.
	for name, h := range map[string]http.HandlerFunc{
		"a plain refusal": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "upstream  down\n")
		},
		"a first frame that is not ready": func(w http.ResponseWriter, _ *http.Request) {
			_ = wire.WriteLine(wire.Flushing(w), wire.Frame{Type: wire.TypeHeartbeat})
		},
		"a stream that ends": func(w http.ResponseWriter, _ *http.Request) {
			out := wire.Flushing(w)
			_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeReady, Session: "tun_1", TTL: "1s", Carriers: 1})
			_ = wire.WriteLine(out, wire.Frame{Type: "unknown"})
		},
		"no frame at all": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(h)
			protocols := new(http.Protocols)
			protocols.SetUnencryptedHTTP2(true)
			srv.Config.Protocols = protocols
			srv.Start()
			defer srv.Close()
			err := Run(t.Context(), Options{Gateway: srv.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1", Token: ok, Logger: slog.New(slog.DiscardHandler)})
			if err == nil {
				t.Fatal("Run returned nil")
			}
			switch name {
			case "a plain refusal":
				if !errors.As(err, &refused) || refused.Status != 502 || refused.Code != "" || refused.Detail != "upstream down" || refused.Error() != "the gateway refused the connect with 502: upstream down" {
					t.Errorf("%v", err)
				}
			case "a first frame that is not ready":
				if !strings.Contains(err.Error(), "not ready") {
					t.Errorf("%v", err)
				}
			case "a stream that ends":
				if !strings.Contains(err.Error(), "the session stream ended") {
					t.Errorf("%v", err)
				}
			case "no frame at all":
				if !strings.Contains(err.Error(), "reading the ready frame") {
					t.Errorf("%v", err)
				}
			}
		})
	}
}

// TestRunStopsCleanly: an ended context closes the session's request
// body, which the gateway sees as the end of the stream, and Run returns
// nil.
func TestRunStopsCleanly(t *testing.T) {
	g := newFakeGateway(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Gateway: g.srv.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1", Token: func() (string, error) { return "t1", nil }, Logger: slog.New(slog.DiscardHandler)})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(g.heartbeats()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a clean stop returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// TestFileToken reads the token file on every call, trimmed, and
// reports an empty or missing file.
func TestFileToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	src := FileToken(path)
	if _, err := src(); err == nil {
		t.Error("a missing file yielded a token")
	}
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src(); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("an empty file: %v", err)
	}
	if err := os.WriteFile(path, []byte(" first \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := src(); err != nil || tok != "first" {
		t.Errorf("first read: %q %v", tok, err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := src(); err != nil || tok != "second" {
		t.Errorf("second read: %q %v", tok, err)
	}
}

// TestHelpers covers the small rules: which methods carry a body, and
// that the runtime's address is redacted from what leaves the machine.
func TestHelpers(t *testing.T) {
	for method, want := range map[string]bool{"GET": false, "HEAD": false, "DELETE": false, "OPTIONS": false, "TRACE": false, "POST": true, "PUT": true, "PATCH": true} {
		if hasBody(method) != want {
			t.Errorf("hasBody(%s) = %v", method, !want)
		}
	}
	a, err := newAgent(Options{Gateway: "https://lux.example.com", Provider: "p", Upstream: "http://127.0.0.1:11434/v1", Token: func() (string, error) { return "", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.redact("dial tcp 127.0.0.1:11434: connection refused"); got != "dial tcp the runtime: connection refused" {
		t.Errorf("redact: %q", got)
	}
	if a.client.Transport.(*http.Transport).Protocols != nil {
		t.Error("an https gateway got the plaintext protocols")
	}
	if a.o.UserAgent != "lux" {
		t.Errorf("default User-Agent %q", a.o.UserAgent)
	}
}

// holdingHandler is a slog.Handler that holds the first carrier warning
// it is given until released, and notes a record that arrived after the
// test marked Run as returned.
type holdingHandler struct {
	held     chan struct{} // closed when a carrier warning is being held
	release  chan struct{} // closed by the test to let it through
	once     sync.Once
	returned atomic.Bool // set by the test the moment Run returned
	late     atomic.Bool // a record was handled after Run returned
}

func (h *holdingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *holdingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *holdingHandler) WithGroup(string) slog.Handler            { return h }
func (h *holdingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "agent: a carrier ended without work" {
		h.once.Do(func() { close(h.held) })
		<-h.release
	}
	if h.returned.Load() {
		h.late.Store(true)
	}
	return nil
}

// TestRunReturnsAfterItsGoroutines: Run's return is the end of every
// goroutine it started, so a caller may write to whatever the agent's
// logger writes to, its stderr, the moment Run returns. A gateway that
// refuses every carrier makes a carrier goroutine log a warning; the
// test's handler holds that record, the gateway closes the session
// while it is held, and Run must not return before the release.
func TestRunReturnsAfterItsGoroutines(t *testing.T) {
	closeNow := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/providers/{name}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		out := wire.Flushing(w)
		w.WriteHeader(http.StatusOK)
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeReady, Session: "tun_1", TTL: "1s", Carriers: 2})
		select {
		case <-closeNow:
		case <-r.Context().Done():
			return
		}
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeClose, Reason: wire.ReasonTokenExpired})
	})
	mux.HandleFunc("POST /v1/providers/{name}/tunnel/carry", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"This request needs a valid credential.","details":{}}}`)
	})
	srv := httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)

	h := &holdingHandler{held: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Options{Gateway: srv.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1", Token: func() (string, error) { return "t1", nil }, Logger: slog.New(h)})
	}()
	select {
	case <-h.held:
	case err := <-done:
		t.Fatalf("Run returned %v before a carrier was refused", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no carrier was refused")
	}
	close(closeNow)
	select {
	case err := <-done:
		t.Fatalf("Run returned %v while a goroutine it started was still logging", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(h.release)
	select {
	case err := <-done:
		h.returned.Store(true)
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Reason != wire.ReasonTokenExpired {
			t.Fatalf("Run returned %v, want the close reason", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once the record was released")
	}
	if h.late.Load() {
		t.Fatal("a record was handled after Run returned")
	}
}

// TestRunStopsCleanlyWhileConnecting: a stop that lands while the
// connect is still in flight, before the gateway has answered, is a
// clean stop and returns nil, not the transport's "context canceled":
// a caller that stopped the agent is not told its connect failed.
func TestRunStopsCleanlyWhileConnecting(t *testing.T) {
	arrived := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/providers/{name}/tunnel", func(_ http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done() // never answers; the connect ends with the caller's stop
	})
	srv := httptest.NewUnstartedServer(mux)
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Gateway: srv.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1", Token: func() (string, error) { return "t1", nil }, Logger: slog.New(slog.DiscardHandler)})
	}()
	select {
	case <-arrived:
	case err := <-done:
		t.Fatalf("Run returned %v before the connect reached the gateway", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the connect never reached the gateway")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a stop while connecting returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the stop")
	}
}
