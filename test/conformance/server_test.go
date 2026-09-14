// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/metrics"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/api"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/filemode"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/manifest"
)

// The reference server the package's own tests drive: luxd's serve role
// assembled in process from the packages cmd/luxd wires, the memory
// store, the stub issuer and authorizer of latere.ai/x/pkg, and the stub
// providers above, on loopback listeners. cmd/luxd's run is package
// main's and cannot be imported, so the wiring is repeated here in the
// order serveCmd has it; a drift between the two is what
// TestReferenceServerConforms and cmd/luxd's own tests would show from
// either side.

// The audience and the key encryption key the harness serves under.
const (
	harnessAudience = "lux"
	harnessKEK      = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
)

// dialects are the four doors the reference server mounts.
var dialects = []string{"openai", "anthropic", "gemini", "lux"}

// serverOptions selects how the reference server is assembled.
type serverOptions struct {
	// fileMode reads desired state from dir and mounts /v1 on the
	// internal listener with no bearer.
	fileMode bool
	dir      string
	getenv   func(string) string
	// ownerPolicy decides with the owner policy and the default subject
	// as an admin, instead of the stub authorizer.
	ownerPolicy bool
	// stubs starts the stub providers and the document; without them the
	// suite's Providers name hosts under example.com.
	stubs bool
}

// server is one running reference server.
type server struct {
	public, internal *httptest.Server
	iss              *issuertest.Server
	az               *stub.Server
	stubs            *stubSet
	st               store.Store
	publicURL        *url.URL
}

// URL is the public listener as the server names itself, the value of
// LUX_TEST_URL.
func (s *server) URL() string { return s.publicURL.String() }

// mint mints a token for sub with the harness's audience.
func (s *server) mint(sub string) string {
	return s.iss.Mint(issuertest.Claims{Sub: sub, Aud: issuertest.StringList{harnessAudience}})
}

// subject renders sub as the harness's issuer would.
func (s *server) subject(sub string) string { return authz.Subject(s.iss.URL(), sub) }

// control sends one control plane request with a bearer, outside any
// run, for a test that checks the server's state around one.
func (s *server) control(t testing.TB, token, method, path string, body any) *response {
	t.Helper()
	c := &client{cfg: Config{URL: s.URL()}, http: newHTTPClient(), ctx: t.Context(), rep: newReport("outside"), sentences: map[string]string{}, violations: map[string]bool{}}
	return c.request(t, method, s.URL()+path, body, bearer(token))
}

// config is the Config a run against this server uses: the default
// subject alice, tokens minted per subject through the stub issuer, and
// the stubs document when there are stubs.
func (s *server) config(subject string) Config {
	cfg := Config{URL: s.URL(), Subject: s.subject(subject), Token: func(rendered string) (string, bool) {
		iss, sub, ok := authz.SplitSubject(rendered)
		if !ok || iss != s.iss.URL() {
			return "", false
		}
		return s.mint(sub), true
	}}
	if s.stubs != nil {
		cfg.StubsURL = s.stubs.URL()
	}
	if s.internal != nil {
		cfg.InternalURL = s.internal.URL
	}
	return cfg
}

// noLookup is the resolver of the harness's upstream clients: a name
// resolves to nothing, so a Provider under example.com fails before any
// dial and the test process reaches no network.
func noLookup(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, errors.New("the conformance harness resolves no host: " + host)
}

// loopbackDial dials loopback addresses alone, which is where every
// stub listens.
func loopbackDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("the conformance harness dials loopback alone, not " + addr)
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// startServer assembles and starts the reference server, stopped with
// the test.
func startServer(t testing.TB, o serverOptions) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var jobs sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		jobs.Wait()
	})
	logger := slog.New(slog.DiscardHandler)
	reg := metrics.NewRegistry()
	httpClient := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}

	s := &server{}
	s.public = httptest.NewUnstartedServer(nil)
	// The public URL names the listener as localhost rather than by its
	// address, because the loop check of spec 003 compares the host of a
	// Provider's baseURL with the public URL's host alone, and every stub
	// listens on the loopback address the listener does.
	_, port, err := net.SplitHostPort(s.public.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	publicURL, err := url.Parse("http://localhost:" + port)
	if err != nil {
		t.Fatal(err)
	}
	s.publicURL = publicURL
	defaults := manifest.Defaults{Timeout: 10 * time.Minute}

	var st store.Store
	var files *filemode.Store
	var identity *auth.Auth
	if o.fileMode {
		files, err = filemode.Load(ctx, filemode.Options{Dir: o.dir, Getenv: o.getenv, Defaults: defaults, AllowPrivateUpstreams: true, PublicURL: publicURL})
		if err != nil {
			t.Fatal(err)
		}
		st = store.Instrument(files, reg)
		identity, err = auth.New(ctx, auth.Options{HTTP: httpClient})
	} else {
		st = store.Instrument(memory.New(), reg)
		s.iss = issuertest.New(t, issuertest.WithDefaultAudience(harnessAudience))
		s.az = stub.New(t)
		opts := auth.Options{Issuers: []string{s.iss.URL()}, Audience: harnessAudience, HTTP: httpClient, AuthorizerTimeout: 2 * time.Second}
		if o.ownerPolicy {
			opts.AdminSubjects = []string{s.subject("alice")}
		} else {
			opts.AuthorizerURL, opts.AuthorizerToken = s.az.URL(), s.az.Token()
		}
		identity, err = auth.New(ctx, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	s.st = st
	if o.stubs {
		s.stubs = newStubSet(t, s.iss, s.az)
	}

	keyring, err := secrets.Parse(harnessKEK)
	if err != nil {
		t.Fatal(err)
	}
	keys := serve.NewKeyCache(serve.KeyCacheOptions{Store: st, TTL: time.Second, Tail: 100 * time.Millisecond, Metrics: reg, Logger: logger})
	limiter := serve.NewLimiter(serve.LimiterOptions{Store: st, Budgets: keys, Defaults: defaults, Flush: 200 * time.Millisecond, Logger: logger})
	recorder := serve.NewRecorder(serve.RecorderOptions{Store: st, Catalog: &serve.Catalog{Objects: st.Objects()}, Limiter: limiter, Metrics: reg, Flush: 200 * time.Millisecond, Logger: logger})
	var credentials gateway.CredentialSource = &serve.StoreCredentials{Credentials: st.Credentials(), Keys: keyring}
	if files != nil {
		credentials = &serve.FileCredentials{Files: files}
	}
	clients := gateway.NewClientSource(gateway.ClientOptions{AllowPrivate: true, Version: "conformance", LookupIPAddr: noLookup, Dial: loopbackDial})
	health := serve.NewHealth(serve.HealthOptions{Store: st, Clients: clients, Credentials: credentials, Interval: 300 * time.Millisecond, Logger: logger})
	catalog := &serve.Catalog{Objects: st.Objects()}
	doors := gateway.New(gateway.Options{
		Keys: keys, Catalog: catalog, Credentials: credentials,
		Router:   gateway.NewTargetRouter(gateway.RouterOptions{Catalog: catalog, Health: health.View, Metrics: reg}),
		Limiter:  serve.MutateLimiter(limiter),
		Recorder: recorder, Clients: serve.MutateClients(clients), Health: health, Metrics: reg, Version: "conformance",
	})
	var authorizer *auth.Authorizer
	if !o.fileMode {
		authorizer = identity.Authorizer(&serve.ObjectOwners{Objects: st.Objects()})
	}
	var apiKeys *secrets.Keyring
	if !o.fileMode {
		apiKeys = keyring
	}
	control := api.New(api.Options{
		Store: st, Auth: identity, Authorizer: authorizer, PublicURL: publicURL, Version: "conformance",
		RequestsPerMinute: 600, Defaults: defaults, AllowPrivateUpstreams: true, Keys: apiKeys, Clients: clients,
		ReadOnlyDir: o.dir, Metrics: reg, Logger: logger,
	})

	planes := http.NewServeMux()
	for _, d := range dialects {
		planes.Handle("/"+d, doors)
		planes.Handle("/"+d+"/", doors)
	}
	planes.Handle("/.well-known/lux", control)
	if o.fileMode {
		planes.Handle("/v1/", control.Unmounted())
		internal := http.NewServeMux()
		internal.Handle("/v1/", control)
		s.internal = httptest.NewServer(internal)
		t.Cleanup(s.internal.Close)
	} else {
		planes.Handle("/v1/", control)
	}
	limited := serve.LimitUnauthenticated(planes, serve.AddressLimiterOptions{PerMinute: 600})
	public := http.NewServeMux()
	for _, prefix := range append(append([]string{}, dialects...), "v1", ".well-known/lux") {
		public.Handle("/"+prefix, limited)
		public.Handle("/"+prefix+"/", limited)
	}
	s.public.Config.Handler = serve.Mutate(public)
	s.public.Start()
	t.Cleanup(s.public.Close)

	for _, job := range []func(context.Context){keys.Run, limiter.Run, recorder.Run, health.Run} {
		jobs.Go(func() { job(ctx) })
	}
	return s
}

// fileModeDir writes a directory of manifests for the file mode server:
// one Provider under example.com with its credential from the
// environment, one Model, and one Key whose value is a variable's.
func fileModeDir(t testing.TB) (dir string, getenv func(string) string) {
	t.Helper()
	dir = t.TempDir()
	files := map[string]string{
		"provider.yaml": "apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata:\n  name: openai\nspec:\n  dialect: openai\n  baseURL: https://api.provider.example.com/v1\n  credential:\n    valueFrom:\n      env: CONF_PROVIDER_KEY\n  discovery:\n    mode: none\n  health:\n    mode: none\n",
		"model.yaml":    "apiVersion: lux.latere.ai/v1beta1\nkind: Model\nmetadata:\n  name: gpt-5\nspec:\n  targets:\n    - provider: openai\n",
		"key.yaml":      "apiVersion: lux.latere.ai/v1beta1\nkind: Key\nmetadata:\n  name: dev\nspec:\n  models: [\"*\"]\n  valueFrom:\n    env: CONF_DEV_KEY\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"CONF_PROVIDER_KEY": "sk-file-mode-example", "CONF_DEV_KEY": "lux_" + "0123456789abcdefghijklmnopqrstuvwxyzABCD"}
	return dir, func(k string) string { return env[k] }
}
