// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/tunnel/agent"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

const audience = "lux"

// The forward secrets of the rotation cases, each 32 bytes.
const (
	secretNew = "new-0123456789abcdef0123456789ab"
	secretOld = "old-0123456789abcdef0123456789ab"
)

// replica is one gateway replica in a test: its Gateway over a store
// shared with the others, its public HTTP/2 server carrying the two
// routes as internal/api mounts them, and its internal h2c server
// carrying the forward route.
type replica struct {
	t        *testing.T
	st       store.Store
	iss      *issuertest.Server
	verifier *auth.Verifier
	g        *Gateway
	public   *httptest.Server
	internal *httptest.Server
	reg      *metrics.Registry
	log      *syncWriter
	forwards *counter
}

// counter counts requests on the forward route.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// replicaOptions are what differs between replicas of one test.
type replicaOptions struct {
	secrets  []string
	ttl      time.Duration
	forward  bool // serve and advertise the internal listener
	h2c      bool // a plaintext public listener
	connect  func(ctx context.Context, p *v1.Provider)
	verifier *auth.Verifier
	iss      *issuertest.Server
}

// newReplica builds one replica over st.
func newReplica(t *testing.T, st store.Store, o replicaOptions) *replica {
	t.Helper()
	r := &replica{t: t, st: st, log: &syncWriter{}, reg: metrics.NewRegistry(), forwards: &counter{}}
	r.iss = o.iss
	if r.iss == nil {
		r.iss = issuertest.New(t, issuertest.WithDefaultAudience(audience))
	}
	r.verifier = o.verifier
	if r.verifier == nil {
		v, err := auth.NewVerifier(t.Context(), auth.VerifierOptions{Issuers: []string{r.iss.URL()}, Audience: audience, HTTP: &http.Client{}})
		if err != nil {
			t.Fatal(err)
		}
		r.verifier = v
	}
	if o.forward {
		r.internal = httptest.NewUnstartedServer(nil)
		r.internal.Config.Protocols = h2cProtocols()
		r.internal.Start()
		t.Cleanup(r.internal.Close)
	}
	opts := Options{
		Store: st, Verifier: r.verifier, Clients: gateway.NewClientSource(gateway.ClientOptions{Version: "test"}),
		Secrets: o.secrets, TTL: o.ttl, Version: "test", Metrics: r.reg, OnConnect: o.connect,
		Logger: slog.New(slog.NewTextHandler(r.log, nil)),
	}
	if r.internal != nil {
		opts.Replica = strings.TrimPrefix(r.internal.URL, "http://")
	}
	r.g = New(opts)
	if r.internal != nil {
		mux := http.NewServeMux()
		mux.Handle(ForwardPattern, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			r.forwards.inc()
			r.g.Forward().ServeHTTP(w, req)
		}))
		r.internal.Config.Handler = mux
	}
	r.public = httptest.NewUnstartedServer(r.routes())
	if o.h2c {
		r.public.Config.Protocols = h2cProtocols()
		r.public.Start()
	} else {
		r.public.EnableHTTP2 = true
		r.public.StartTLS()
	}
	t.Cleanup(r.public.Close)
	t.Cleanup(r.g.Drain)
	return r
}

// h2cProtocols is HTTP/1 and unencrypted HTTP/2, what a plaintext
// listener of luxd serves with the tunnel on.
func h2cProtocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// routes are the two public routes as internal/api mounts them: the
// bearer verified, the Provider loaded, the session authorized by the
// caller (the owner policy is internal/api's test), and the refusal
// written in the envelope.
func (r *replica) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/providers/{name}/tunnel", func(w http.ResponseWriter, req *http.Request) {
		caller, err := r.verifier.Authenticate(req)
		if err != nil {
			writeError(w, "req_test", refuse(CodeUnauthenticated, err.Error()))
			return
		}
		p := r.provider(req.Context(), req.PathValue("name"))
		if p == nil {
			writeError(w, "req_test", refuse(CodeNotFound, "no Provider "+req.PathValue("name")))
			return
		}
		if e := r.g.ServeSession(w, req, SessionRequest{Provider: p, Subject: caller.Subject, ExpiresAt: expiryOf(caller), Agent: req.UserAgent(), RequestID: "req_test"}); e != nil {
			writeError(w, "req_test", e)
		}
	})
	mux.HandleFunc("POST /v1/providers/{name}/tunnel/carry", func(w http.ResponseWriter, req *http.Request) {
		caller, err := r.verifier.Authenticate(req)
		if err != nil {
			writeError(w, "req_test", refuse(CodeUnauthenticated, err.Error()))
			return
		}
		if e := r.g.ServeCarrier(w, req, CarrierRequest{Provider: req.PathValue("name"), Session: req.Header.Get(wire.HeaderSession), Subject: caller.Subject}); e != nil {
			writeError(w, "req_test", e)
		}
	})
	return mux
}

// provider reads a Provider by name from the store.
func (r *replica) provider(ctx context.Context, name string) *v1.Provider {
	obj, _, err := r.st.Objects().ByName(ctx, v1.KindProvider, name)
	if err != nil {
		return nil
	}
	p, _ := obj.(*v1.Provider)
	return p
}

// mint mints a bearer for sub with the lifetime.
func (r *replica) mint(sub string, lifetime time.Duration) string {
	return r.iss.Mint(issuertest.Claims{Sub: sub, Exp: time.Now().Add(lifetime).Unix()})
}

// subject is sub's rendered subject.
func (r *replica) subject(sub string) string { return r.iss.URL() + "|" + sub }

// client is an HTTP/2 client toward the public listener.
func (r *replica) client() *http.Client {
	if r.public.TLS == nil {
		protocols := new(http.Protocols)
		protocols.SetUnencryptedHTTP2(true)
		return &http.Client{Transport: &http.Transport{Protocols: protocols}}
	}
	return r.public.Client()
}

// attach runs an agent toward this replica for the Provider against the
// runtime and returns its result channel and a stop; it waits until the
// session is held.
func (r *replica) attach(t *testing.T, name, upstream string, token func() (string, error)) (result <-chan error, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, agent.Options{
			Gateway: r.public.URL, Provider: name, Upstream: upstream, Token: token, Client: r.client(), UserAgent: "lux/test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	// The session is held once the registry names a session that was not
	// there before, which covers a first connect and one that supersedes
	// another on this very replica alike.
	previous := ""
	if p := r.provider(ctx, name); p != nil {
		if row, err := r.st.Tunnels().Get(ctx, p.Status.ID); err == nil {
			previous = row.Session
		}
	}
	waitFor(t, func() bool {
		select {
		case err := <-done:
			t.Fatalf("the agent ended before the session was held: %v", err)
		default:
		}
		p := r.provider(ctx, name)
		if p == nil {
			return false
		}
		row, err := r.st.Tunnels().Get(ctx, p.Status.ID)
		return err == nil && row.Session != previous && r.g.Sessions() > 0
	}, "the session to be held")
	return done, cancel
}

// tunnelProvider writes a tunnel: true Provider to the store and returns
// it.
func tunnelProvider(t *testing.T, st store.Store, name string, edit func(*v1.Provider)) *v1.Provider {
	t.Helper()
	p := &v1.Provider{
		Metadata: v1.ObjectMeta{Name: name},
		Spec:     v1.ProviderSpec{Dialect: v1.DialectOpenAI, Tunnel: true, Discovery: v1.Discovery{Mode: v1.DiscoveryAuto}, Health: v1.Health{Mode: v1.HealthProbe}, Timeout: "5s", Concurrency: 8},
		Status:   v1.ProviderStatus{ID: v1.NewID(v1.PrefixProvider, time.Now(), nil), Owner: "https://login.example.com|alice", Warnings: []string{}},
	}
	if edit != nil {
		edit(p)
	}
	if _, err := st.Objects().Put(t.Context(), p, 0); err != nil {
		t.Fatal(err)
	}
	return p
}

// runtime is a fake local model server: the models route, a chat route
// that echoes or streams, and a record of every header it saw.
type runtime struct {
	srv     *httptest.Server
	mu      sync.Mutex
	headers []http.Header
	// gate holds the second event of a stream until released, so a test
	// proves the first reached the caller before the second was written.
	gate chan struct{}
}

func newRuntime(t *testing.T) *runtime {
	t.Helper()
	rt := &runtime{gate: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		rt.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"llama3.1"},{"id":"qwen2.5"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		rt.record(r)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
			http.NewResponseController(w).Flush()
			select {
			case <-rt.gate:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "data: {\"n\":2}\n\n")
			http.NewResponseController(w).Flush()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Runtime", "fake")
		_, _ = fmt.Fprintf(w, `{"echo":%s,"query":%q,"path":%q}`, body, r.URL.RawQuery, r.URL.Path)
	})
	mux.HandleFunc("GET /v1/slow", func(w http.ResponseWriter, r *http.Request) {
		rt.record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: start\n\n")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /v1/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		var code int
		_, _ = fmt.Sscan(r.PathValue("code"), &code)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":"as asked"}`)
	})
	rt.srv = httptest.NewServer(mux)
	t.Cleanup(rt.srv.Close)
	return rt
}

func (rt *runtime) record(r *http.Request) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.headers = append(rt.headers, r.Header.Clone())
}

func (rt *runtime) seen() []http.Header {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]http.Header, len(rt.headers))
	copy(out, rt.headers)
	return out
}

// upstream is the runtime's base URL as lux serve's --upstream names it.
func (rt *runtime) upstream() string { return rt.srv.URL + "/v1" }

// send sends one request through the Provider's carrier-backed client
// and returns the response with its body read.
func send(t *testing.T, g *Gateway, p *v1.Provider, method, path, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	resp, err := sendErr(t, g, p, method, path, body, headers...)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, path, err)
	}
	return resp, string(data)
}

// sendErr is send without the assertions, for the failure cases.
func sendErr(t *testing.T, g *Gateway, p *v1.Provider, method, path, body string, headers ...string) (*http.Response, error) {
	t.Helper()
	client, err := g.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rd)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return client.Do(req)
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// closeReason is the reason an agent's Run ended with, or "" when it
// ended otherwise.
func closeReason(err error) string {
	var ce *agent.CloseError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

// rawSession drives the protocol without the agent package: one session
// stream and the carriers a test opens by hand, so what the bytes look
// like is asserted rather than what the agent does with them.
type rawSession struct {
	t      *testing.T
	r      *replica
	name   string
	token  string
	frames *bufio.Reader
	send   *io.PipeWriter
	resp   *http.Response
	Ready  wire.Frame
}

// openRaw opens a session with the bearer and reads the ready frame.
func openRaw(t *testing.T, r *replica, name, token string) *rawSession {
	t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.public.URL+"/v1/providers/"+name+"/tunnel", pr)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "raw/test")
	resp, err := r.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("session: %d %s", resp.StatusCode, body)
	}
	t.Cleanup(func() { _ = pw.Close(); _ = resp.Body.Close() })
	s := &rawSession{t: t, r: r, name: name, token: token, frames: bufio.NewReader(resp.Body), send: pw, resp: resp}
	if err := wire.ReadLine(s.frames, &s.Ready); err != nil || s.Ready.Type != wire.TypeReady {
		t.Fatalf("ready frame: %+v %v", s.Ready, err)
	}
	return s
}

// heartbeat sends one heartbeat frame, with a token when given.
func (s *rawSession) heartbeat(token string) {
	s.t.Helper()
	if err := wire.WriteLine(s.send, wire.Frame{Type: wire.TypeHeartbeat, Token: token}); err != nil {
		s.t.Fatal(err)
	}
}

// next reads the next frame from the gateway within the timeout.
func (s *rawSession) next(timeout time.Duration) (wire.Frame, error) {
	type result struct {
		f   wire.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var f wire.Frame
		err := wire.ReadLine(s.frames, &f)
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-time.After(timeout):
		return wire.Frame{}, errors.New("no frame within " + timeout.String())
	}
}

// rawCarrier is one carrier a test opened by hand.
type rawCarrier struct {
	resp *http.Response
	in   *bufio.Reader
	out  *io.PipeWriter
}

// carry opens a carrier with the bearer for the session and returns it
// once the gateway answered; a refusal is returned as the response.
func (s *rawSession) carry(token, session string) (*rawCarrier, *http.Response) {
	s.t.Helper()
	pr, pw := io.Pipe()
	req, _ := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.r.public.URL+"/v1/providers/"+s.name+"/tunnel/carry", pr)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(wire.HeaderSession, session)
	resp, err := s.r.client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = pw.Close(); _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		return nil, resp
	}
	return &rawCarrier{resp: resp, in: bufio.NewReader(resp.Body), out: pw}, resp
}

// envelopeCode reads the code of an error envelope.
func envelopeCode(t *testing.T, resp *http.Response) (code, detail string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("not an envelope: %s", body)
	}
	if env.Error.Message != Code(env.Error.Code).Message() {
		t.Errorf("message %q is not the sentence of %s", env.Error.Message, env.Error.Code)
	}
	detail, _ = env.Error.Details["detail"].(string)
	return env.Error.Code, detail
}

// errStore is what a failing registry answers.
var errStore = errors.New("the database is away")

// failingStore is a store whose registry fails every call, for the
// store-failure cases; every other collection is the wrapped store's.
type failingStore struct{ store.Store }

func (failingStore) Tunnels() store.Tunnels { return failingTunnels{} }

type failingTunnels struct{}

func (failingTunnels) Register(context.Context, store.Tunnel, time.Duration) error { return errStore }
func (failingTunnels) Heartbeat(context.Context, string, string, time.Duration) (bool, error) {
	return false, errStore
}
func (failingTunnels) Get(context.Context, string) (store.Tunnel, error) {
	return store.Tunnel{}, errStore
}
func (failingTunnels) Unregister(context.Context, string, string) error { return errStore }

// syncWriter guards a buffer the logger and the test share.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
