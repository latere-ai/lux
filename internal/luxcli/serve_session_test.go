// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxcli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/metrics"

	agent "latere.ai/x/lux/client/tunnel"
	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/api"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/tunnel"
	"latere.ai/x/lux/internal/tunnel/wire"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// sessionTTL is the registry TTL of the gateway these tests assemble.
// It is short so a heartbeat, an expiry check, and a lapsed row all
// happen inside one test.
const sessionTTL = 300 * time.Millisecond

// runIn drives the command with a context of the caller's, which is how
// a test stops a lux serve the way SIGINT and SIGTERM stop it.
func runIn(ctx context.Context, env map[string]string, args ...string) result {
	var out, errOut bytes.Buffer
	code := Run(ctx, Options{
		Args:   args,
		Getenv: func(k string) string { return env[k] },
		Stdout: &out, Stderr: &errOut,
		Version: "1.2.3", Commit: "abc1234", Date: "2026-09-14",
		Now: func() time.Time { return now },
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// shortBackoff puts the reconnect delays in the millisecond range for
// the length of one test, so several reconnects run in a moment.
func shortBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := backoffMin, backoffMax
	backoffMin, backoffMax = 5*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { backoffMin, backoffMax = oldMin, oldMax })
}

// waitFor holds the test until cond is true, or fails naming what did
// not happen.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %s and it did not happen", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeRuntime is a model server on loopback, the thing lux serve
// attaches: the openai models route, and a record of what reached it.
type fakeRuntime struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	rt := &fakeRuntime{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		rt.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"llama3.1"}]}`)
	})
	rt.srv = httptest.NewServer(mux)
	t.Cleanup(rt.srv.Close)
	return rt
}

// serveGateway is a whole gateway with the tunnel of spec 013 on, wired
// as cmd/luxd wires it, behind a listener the test can close and open
// again at the same address, so a reconnect across a restart is a real
// one. The store, the issuer, and the identity outlive a restart, as
// they do in an installation.
type serveGateway struct {
	t        *testing.T
	store    store.Store
	iss      *issuertest.Server
	identity *auth.Auth
	keys     *secrets.Keyring
	addr     string
	url      string
	token    string
	connects atomic.Int64

	mu  sync.Mutex
	tun *tunnel.Gateway
	srv *httptest.Server
}

func newServeGateway(t *testing.T) *serveGateway {
	t.Helper()
	keys, err := secrets.Parse(kek)
	if err != nil {
		t.Fatal(err)
	}
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	authorizer := stub.New(t)
	identity, err := auth.New(t.Context(), auth.Options{
		Issuers: []string{iss.URL()}, Audiences: []string{"lux"},
		AuthorizerURL: authorizer.URL(), AuthorizerToken: authorizer.Token(), AuthorizerTimeout: 2 * time.Second,
		HTTP: &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &serveGateway{t: t, store: memory.New(), iss: iss, identity: identity, keys: keys, addr: ln.Addr().String()}
	g.url = "http://" + g.addr
	g.token = iss.Mint(issuertest.Claims{Sub: "alice"})
	_ = ln.Close()
	g.start()
	t.Cleanup(func() {
		g.stop()
		_ = g.store.Close()
	})
	return g
}

// start assembles the replica and opens the listener at its address.
func (g *serveGateway) start() {
	g.t.Helper()
	reg := metrics.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	clients := gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: true, Version: "test"})
	tun := tunnel.New(tunnel.Options{
		Store: g.store, Verifier: g.identity.Verifier, Clients: clients,
		TTL: sessionTTL, Carriers: 2, Version: "test", Metrics: reg, Logger: logger,
	})
	base, err := url.Parse(g.url)
	if err != nil {
		g.t.Fatal(err)
	}
	control := api.New(api.Options{
		Store: g.store, Auth: g.identity, Authorizer: g.identity.Authorizer(&serve.ObjectOwners{Objects: g.store.Objects()}),
		PublicURL: base, Version: "0.1.0-test", RequestsPerMinute: 6000, Defaults: manifest.Defaults{Timeout: time.Minute},
		AllowPrivateUpstreams: true, Keys: g.keys, Clients: tun, Tunnel: tun, TunnelEnabled: true,
		Metrics: reg, Logger: logger,
	})
	mux := http.NewServeMux()
	mux.Handle("/.well-known/lux", control)
	mux.Handle("/v1/", control)
	ln, err := net.Listen("tcp", g.addr)
	if err != nil {
		g.t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tunnel") {
			g.connects.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	srv.Listener = ln
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = protocols
	srv.Start()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tun, g.srv = tun, srv
}

// stop closes the replica the way the serve role stops: every session
// drained, then the listener gone.
func (g *serveGateway) stop() {
	g.mu.Lock()
	tun, srv := g.tun, g.srv
	g.srv = nil
	g.mu.Unlock()
	if srv == nil {
		return
	}
	tun.Drain(context.Background())
	srv.CloseClientConnections()
	srv.Close()
}

// gateway is this replica's tunnel side, which a restart replaces.
func (g *serveGateway) gateway() *tunnel.Gateway {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tun
}

// sessions is how many sessions this replica holds.
func (g *serveGateway) sessions() int { return g.gateway().Sessions() }

// env is what a lux serve run reads: the gateway and a token.
func (g *serveGateway) env() map[string]string {
	return map[string]string{"LUX_URL": g.url, "LUX_TOKEN": g.token}
}

// serving starts one lux serve in the background and returns its exit,
// with the stop that a signal would be.
func (g *serveGateway) serving(env map[string]string, args ...string) (<-chan result, context.CancelFunc) {
	ctx, cancel := context.WithCancel(g.t.Context())
	out := make(chan result, 1)
	go func() { out <- runIn(ctx, env, args...) }()
	g.t.Cleanup(cancel)
	return out, cancel
}

// reaches sends one request through the tunnel the way a door does, and
// reports the runtime's answer.
func (g *serveGateway) reaches(name string) (int, error) {
	obj, _, err := g.store.Objects().ByName(g.t.Context(), v1.KindProvider, name)
	if err != nil {
		return 0, err
	}
	p, ok := obj.(*v1.Provider)
	if !ok {
		return 0, err
	}
	client, err := g.gateway().Client(g.t.Context(), p)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(g.t.Context(), http.MethodGet, "http://tunnelled/models", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// serveArgs is one lux serve invocation against the runtime.
func serveArgs(rt *fakeRuntime, name string, extra ...string) []string {
	return append([]string{"serve", "--dialect", "openai", "--upstream", rt.srv.URL + "/v1", "--as", name}, extra...)
}

// TestServeReconnects is spec 014's row for the session: lux serve
// applies the Provider, attaches, serves a request from the gateway
// against the runtime, reconnects at once when the replica drains,
// reconnects across a restart of the gateway with the backoff between,
// and closes cleanly when the signal ends the context, which is exit 0.
func TestServeReconnects(t *testing.T) {
	shortBackoff(t)
	g := newServeGateway(t)
	rt := newFakeRuntime(t)
	exit, stop := g.serving(g.env(), serveArgs(rt, "laptop")...)

	waitFor(t, "the first session", func() bool { return g.sessions() == 1 })
	if status, err := g.reaches("laptop"); err != nil || status != http.StatusOK {
		t.Fatalf("the gateway reached the runtime with %d, %v", status, err)
	}
	if rt.hits.Load() != 1 {
		t.Fatalf("the runtime saw %d request(s)", rt.hits.Load())
	}

	// draining: the agent connects again at once, on this same replica.
	g.gateway().Drain(t.Context())
	waitFor(t, "the session to come back after draining", func() bool { return g.sessions() == 1 && g.connects.Load() >= 2 })

	// A restart: the dial fails while the replica is gone and the
	// backoff carries the agent to the one that comes back.
	before := g.connects.Load()
	g.stop()
	time.Sleep(10 * backoffMax) // long enough that a dial is tried and refused
	g.start()
	waitFor(t, "the session after the restart", func() bool { return g.sessions() == 1 })
	if g.connects.Load() <= before {
		t.Fatalf("%d connect(s) after the restart, %d before it", g.connects.Load(), before)
	}
	if status, err := g.reaches("laptop"); err != nil || status != http.StatusOK {
		t.Fatalf("after the restart the gateway reached the runtime with %d, %v", status, err)
	}

	// The signal: the context ends, the session is closed cleanly, and
	// the registry row goes at once rather than at the TTL, which is
	// spec 013's TestCleanDisconnectIsImmediate seen from the command.
	stop()
	r := waitExit(t, exit)
	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	waitFor(t, "the session to go with the command", func() bool { return g.sessions() == 0 })
}

// TestServeExitsOnACloseReason is spec 014's close-reason table:
// superseded, provider_deleted, and token_expired each end the command
// with exit 1 and that reason's own sentence, token_expired after one
// reconnect when a token file could hold a fresh token and at once when
// none can.
func TestServeExitsOnACloseReason(t *testing.T) {
	shortBackoff(t)

	t.Run("superseded", func(t *testing.T) {
		g := newServeGateway(t)
		rt := newFakeRuntime(t)
		exit, _ := g.serving(g.env(), serveArgs(rt, "laptop", "-v")...)
		waitFor(t, "the command's session", func() bool { return g.sessions() == 1 })

		// A second agent takes the Provider, which is the newest session
		// winning; the command's own is closed with superseded.
		second, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			_ = agent.Run(second, agent.Options{
				Gateway: g.url, Provider: "laptop", Upstream: rt.srv.URL + "/v1", UserAgent: "lux/other",
				Token: func() (string, error) { return g.token, nil }, Logger: slog.New(slog.DiscardHandler),
			})
		}()
		r := waitExit(t, exit)
		assertClose(t, r, "superseded", "Another agent attached this provider, so this session ended.")
	})

	t.Run("provider_deleted", func(t *testing.T) {
		g := newServeGateway(t)
		rt := newFakeRuntime(t)
		exit, _ := g.serving(g.env(), serveArgs(rt, "laptop", "-v")...)
		waitFor(t, "the command's session", func() bool { return g.sessions() == 1 })
		if r := run(t, g.env(), "delete", "provider", "laptop"); r.code != ExitOK {
			t.Fatalf("the delete: exit %d %q", r.code, r.stderr)
		}
		r := waitExit(t, exit)
		assertClose(t, r, "provider_deleted", "The provider this session served no longer exists.")
	})

	t.Run("token_expired with no file to refresh from", func(t *testing.T) {
		g := newServeGateway(t)
		rt := newFakeRuntime(t)
		env := g.env()
		env["LUX_TOKEN"] = g.iss.Mint(issuertest.Claims{Sub: "alice", Exp: time.Now().Add(sessionTTL * 4).Unix()})
		exit, _ := g.serving(env, serveArgs(rt, "laptop", "-v")...)
		r := waitExit(t, exit)
		assertClose(t, r, "token_expired", "The token expired and no fresh one was there to take its place.")
		if !strings.Contains(r.stderr, EnvTokenFile+" is read again") {
			t.Errorf("the detail does not name the file that would refresh it:\n%s", r.stderr)
		}
		if g.connects.Load() != 1 {
			t.Errorf("%d connect(s); a token that cannot be refreshed is terminal at the first close", g.connects.Load())
		}
	})

	t.Run("token_expired connects again from the token file", func(t *testing.T) {
		g := newServeGateway(t)
		rt := newFakeRuntime(t)
		path := filepath.Join(t.TempDir(), "token")
		token := g.iss.Mint(issuertest.Claims{Sub: "alice", Exp: time.Now().Add(sessionTTL * 4).Unix()})
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"LUX_URL": g.url, "LUX_TOKEN_FILE": path}
		exit, _ := g.serving(env, serveArgs(rt, "laptop", "-v")...)
		r := waitExit(t, exit)
		// The file still holds the token that expired, so the one retry
		// the file earns is refused rather than closed, and that refusal
		// is what exit 1 carries.
		if r.code != ExitRefused || !strings.Contains(r.stderr, "code: unauthenticated\n") {
			t.Fatalf("exit %d\nstderr %q", r.code, r.stderr)
		}
		if g.connects.Load() != 2 {
			t.Errorf("%d connect(s); a token file earns exactly one reconnect", g.connects.Load())
		}
	})
}

// TestServeTokenExpiryIsTerminalAfterOneRetry holds the two branches of
// spec 014's token_expired rule to a gateway that closes every session
// with it: with a token file the command connects again once and the
// second close is terminal, with LUX_TOKEN the first is, and a reason
// this command does not know is terminal with its own sentence.
func TestServeTokenExpiryIsTerminalAfterOneRetry(t *testing.T) {
	f := newServeFake(t)
	var sessions atomic.Int64
	f.session = func(w http.ResponseWriter, _ *http.Request) {
		sessions.Add(1)
		out := wire.Flushing(w)
		w.WriteHeader(http.StatusOK)
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeReady, Session: "tun_1", TTL: "1s", Carriers: 1})
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeClose, Reason: wire.ReasonTokenExpired})
	}
	args := []string{"serve", "--dialect", "openai", "--upstream", "http://127.0.0.1:11434/v1", "--as", "laptop", "--no-apply", "-v"}

	// A token file earns one reconnect, and the second close is the end.
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(canaryFile), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, map[string]string{"LUX_URL": f.srv.URL, "LUX_TOKEN_FILE": path}, args...)
	if r.code != ExitRefused || !strings.Contains(r.stderr, "code: token_expired\n") || !strings.Contains(r.stderr, "expired too") {
		t.Fatalf("exit %d\nstderr %q", r.code, r.stderr)
	}
	if sessions.Load() != 2 {
		t.Errorf("%d session(s); a token file earns exactly one reconnect", sessions.Load())
	}

	// LUX_TOKEN cannot be read again, so the first close is the end.
	sessions.Store(0)
	r = run(t, map[string]string{"LUX_URL": f.srv.URL, "LUX_TOKEN": canaryToken}, args...)
	if r.code != ExitRefused || !strings.Contains(r.stderr, "code: token_expired\n") || !strings.Contains(r.stderr, EnvTokenFile+" is read again") {
		t.Fatalf("exit %d\nstderr %q", r.code, r.stderr)
	}
	if sessions.Load() != 1 {
		t.Errorf("%d session(s); a token that cannot be refreshed is terminal at the first close", sessions.Load())
	}

	// A reason a newer gateway sent and this command does not know.
	sessions.Store(0)
	f.session = func(w http.ResponseWriter, _ *http.Request) {
		sessions.Add(1)
		out := wire.Flushing(w)
		w.WriteHeader(http.StatusOK)
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeReady, Session: "tun_1", TTL: "1s", Carriers: 1})
		_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeClose, Reason: "relocated"})
	}
	r = run(t, map[string]string{"LUX_URL": f.srv.URL, "LUX_TOKEN": canaryToken}, args...)
	if r.code != ExitRefused || !strings.Contains(r.stderr, messageClosed+"\n") || !strings.Contains(r.stderr, "code: relocated\n") {
		t.Fatalf("exit %d\nstderr %q", r.code, r.stderr)
	}
	if sessions.Load() != 1 {
		t.Errorf("%d session(s); a reason this command does not know is terminal", sessions.Load())
	}
	if strings.Contains(r.stderr, canaryToken) || strings.Contains(r.stdout, canaryToken) {
		t.Fatal("the token was printed")
	}
}

// TestServeSendsAFreshTokenInAHeartbeat is spec 014's row for the token
// file: a token the file gains while the session runs is carried in a
// heartbeat, so the session outlives the token it opened with and the
// command never reconnects.
func TestServeSendsAFreshTokenInAHeartbeat(t *testing.T) {
	shortBackoff(t)
	g := newServeGateway(t)
	rt := newFakeRuntime(t)
	path := filepath.Join(t.TempDir(), "token")
	short := g.iss.Mint(issuertest.Claims{Sub: "alice", Exp: time.Now().Add(sessionTTL * 5).Unix()})
	if err := os.WriteFile(path, []byte(short), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LUX_URL": g.url, "LUX_TOKEN_FILE": path}
	exit, stop := g.serving(env, serveArgs(rt, "laptop")...)
	waitFor(t, "the session", func() bool { return g.sessions() == 1 })
	long := g.iss.Mint(issuertest.Claims{Sub: "alice", Exp: time.Now().Add(time.Hour).Unix()})
	if err := os.WriteFile(path, []byte(long+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Past the first token's expiry the one session still stands and no
	// second connect was made.
	time.Sleep(sessionTTL * 8)
	if g.sessions() != 1 || g.connects.Load() != 1 {
		t.Fatalf("%d session(s) after %d connect(s); the heartbeat's token did not carry", g.sessions(), g.connects.Load())
	}
	stop()
	if r := waitExit(t, exit); r.code != ExitOK {
		t.Fatalf("exit %d\nstderr %q", r.code, r.stderr)
	}
}

// waitExit reads one lux serve's exit, or fails when it runs on.
func waitExit(t *testing.T, exit <-chan result) result {
	t.Helper()
	select {
	case r := <-exit:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("lux serve did not stop")
		return result{}
	}
}

// assertClose holds one close reason to its exit, its sentence, and the
// code -v prints, which is spec 013's reason itself.
func assertClose(t *testing.T, r result, reason, sentence string) {
	t.Helper()
	if r.code != ExitRefused {
		t.Fatalf("exit %d, want 1\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, sentence+"\n") {
		t.Errorf("stderr does not carry the sentence of %s:\n%s", reason, r.stderr)
	}
	if !strings.Contains(r.stderr, "code: "+reason+"\n") {
		t.Errorf("stderr does not carry code: %s:\n%s", reason, r.stderr)
	}
}
