// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/s3/s3test"

	"latere.ai/x/lux/internal/config"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/test/stubs/provider"
	"latere.ai/x/lux/test/stubs/sink"
)

// kek is one 32-byte key in the variable's syntax; otherKEK is another,
// which no row sealed under kek opens.
var (
	kek      = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, secrets.KeySize))
	otherKEK = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, secrets.KeySize))
	// forwardSecret is one entry of LUX_TUNNEL_FORWARD_SECRET, 32 bytes.
	forwardSecret = strings.Repeat("f", 32)
)

// env reads a map and nothing else.
func env(m map[string]string) config.Getenv {
	return func(k string) string { return m[k] }
}

// stack is every endpoint a check dials, each a stub on loopback: the
// issuer, the authorizer, the sink, the archive, and one stub provider
// per dialect, plus a manifest directory naming the providers.
type stack struct {
	iss       *issuertest.Server
	authz     *stub.Server
	sink      *sink.Sink
	sinkURL   string
	s3        *s3test.Server
	providers map[v1.Dialect]string
	dir       string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{
		iss:       issuertest.New(t, issuertest.WithDefaultAudience("lux")),
		authz:     stub.New(t),
		sink:      sink.New(sink.Options{Secret: []byte(sink.DefaultSecret)}),
		s3:        s3test.New(t, "lux"),
		providers: map[v1.Dialect]string{},
	}
	srv := httptest.NewServer(s.sink)
	t.Cleanup(srv.Close)
	s.sinkURL = srv.URL
	for _, d := range []v1.Dialect{v1.DialectOpenAI, v1.DialectAnthropic, v1.DialectGemini, v1.DialectLux} {
		p := httptest.NewServer(provider.New(provider.Options{Dialect: d, Credential: provider.DefaultCredential}))
		t.Cleanup(p.Close)
		s.providers[d] = p.URL
	}
	s.dir = s.manifests(t, s.providers)
	return s
}

// manifests writes one Provider per dialect at the given base, a Model,
// a Budget, and a Key into a fresh directory for the file mode.
func (s *stack) manifests(t *testing.T, providers map[v1.Dialect]string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for d, base := range providers {
		path := "/v1"
		if d == v1.DialectGemini {
			path = "/v1beta"
		}
		write("provider-"+string(d)+".yaml", fmt.Sprintf(`apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: %s
spec:
  dialect: %s
  baseURL: %s%s
  credential:
    value: %s
  discovery:
    mode: none
`, d, d, base, path, provider.DefaultCredential))
	}
	write("model.yaml", `apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: stub-openai
spec:
  targets:
    - provider: openai
      model: stub-openai
  pricing:
    input: "2.50"
    output: "10"
`)
	write("budget.yaml", `apiVersion: lux.latere.ai/v1beta1
kind: Budget
metadata:
  name: dev
spec:
  amount: "10"
  currency: USD
  window: month
`)
	write("key.yaml", `apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: dev
spec:
  models: ["*"]
  budget: dev
  valueFrom:
    env: STUB_DEV_KEY
`)
	return dir
}

// common is what both modes share: the endpoints and the archive.
func (s *stack) common() map[string]string {
	return map[string]string{
		"LUX_PUBLIC_ADDR":            "127.0.0.1:0",
		"LUX_INTERNAL_ADDR":          "127.0.0.1:0",
		"LUX_PUBLIC_URL":             "https://lux.example.com",
		"LUX_OIDC_ISSUERS":           s.iss.URL(),
		"LUX_AUTHORIZER_URL":         s.authz.URL(),
		"LUX_AUTHORIZER_TOKEN":       s.authz.Token(),
		"LUX_EVENTS_URL":             s.sinkURL,
		"LUX_EVENTS_SECRET":          sink.DefaultSecret,
		"LUX_REQUESTLOG_EXPORTER":    config.ExporterS3,
		"LUX_S3_ENDPOINT":            s.s3.URL(),
		"LUX_S3_BUCKET":              "lux",
		"LUX_S3_ACCESS_KEY":          s3test.Key,
		"LUX_S3_SECRET_KEY":          s3test.Secret,
		"LUX_UPSTREAM_ALLOW_PRIVATE": "1",
	}
}

// fileEnv is the file mode over the stack's directory, every endpoint
// set, so every row does its work.
func (s *stack) fileEnv() map[string]string {
	m := s.common()
	m["LUX_MANIFEST_DIR"] = s.dir
	m["STUB_DEV_KEY"] = "lux_" + strings.Repeat("a", 40)
	return m
}

// serverEnv is the server mode over the memory store.
func (s *stack) serverEnv() map[string]string {
	m := s.common()
	m["LUX_SECRETS_KEK"] = kek
	return m
}

// h2c starts a plaintext HTTP/2 server standing in for another replica's
// forward route, answering status to every request that carries the
// bearer and 401 otherwise, and returns its address.
func h2c(t *testing.T, secret string, status int) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 2 {
			http.Error(w, "not HTTP/2", http.StatusHTTPVersionNotSupported)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// seeded returns an open hook over one memory store seed fills.
func seeded(seed func(t *testing.T, st store.Store)) func(*testing.T) func(context.Context, config.Config, config.Getenv) (opened, error) {
	return func(t *testing.T) func(context.Context, config.Config, config.Getenv) (opened, error) {
		st := memory.New()
		seed(t, st)
		return func(context.Context, config.Config, config.Getenv) (opened, error) {
			return opened{st: st}, nil
		}
	}
}

// closedPort is a loopback port nothing listens at, for a database that
// is down.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// putProvider stores a Provider directly, bypassing Resolve, so a row
// can be given what Resolve would refuse: a baseURL on the gateway's own
// host, or a tunnel with no session.
func putProvider(t *testing.T, st store.Store, name string, spec v1.ProviderSpec) *v1.Provider {
	t.Helper()
	p := &v1.Provider{Metadata: v1.ObjectMeta{Name: name}, Spec: spec}
	p.Status.ID = v1.NewID(v1.PrefixProvider, time.Now(), nil)
	p.Status.Owner = "test"
	if _, err := st.Objects().Put(t.Context(), p, 0); err != nil {
		t.Fatal(err)
	}
	return p
}

// byName indexes lines by requirement.
func byName(lines []Line) map[string]Line {
	out := map[string]Line{}
	for _, l := range lines {
		out[l.Name] = l
	}
	return out
}

// TestCheckNamesEachFailure is spec 017's row for the role: one line per
// requirement in the fixed order, and with each requirement removed one
// at a time exactly that requirement's line fails and the exit code is
// 1. Two baselines, the file mode and the server mode, pass every row;
// the rows over the memory store are driven with a seeded store, since a
// check process over the memory store holds no object of its own.
func TestCheckNamesEachFailure(t *testing.T) {
	s := newStack(t)
	cases := []struct {
		name    string
		env     func(s *stack) map[string]string
		edit    func(t *testing.T, s *stack, m map[string]string)
		seed    func(t *testing.T, st store.Store)
		arrange func(t *testing.T, s *stack)
		failing []string
		warning []string
	}{
		{name: "baseline in the file mode", env: (*stack).fileEnv, warning: []string{"dialects"}},
		{name: "baseline in the server mode", env: (*stack).serverEnv, warning: []string{"store"}},
		{
			name: "the owner policy is probed when no authorizer is set", env: (*stack).serverEnv,
			edit: func(_ *testing.T, _ *stack, m map[string]string) {
				delete(m, "LUX_AUTHORIZER_URL")
				delete(m, "LUX_AUTHORIZER_TOKEN")
				m["LUX_ADMIN_SUBJECTS"] = "https://issuer.example.com|admin"
			},
			warning: []string{"store"},
		},
		{
			name: "configuration", env: (*stack).fileEnv,
			edit:    func(_ *testing.T, _ *stack, m map[string]string) { m["LUX_PUBLIC_ADDR"] = "nonsense" },
			failing: []string{"configuration"},
			warning: unconfigured()[2:],
		},
		{
			name: "public url", env: (*stack).serverEnv,
			edit: func(_ *testing.T, _ *stack, m map[string]string) { m["LUX_PUBLIC_URL"] = "http://127.0.0.1:1" },
			seed: func(t *testing.T, st store.Store) {
				putProvider(t, st, "openai", v1.ProviderSpec{Dialect: v1.DialectOpenAI, BaseURL: s.providers[v1.DialectOpenAI] + "/v1"})
			},
			failing: []string{"public url"},
			warning: []string{"store", "providers"}, // the seeded Provider holds no credential, which the stub refuses
		},
		{
			name: "issuers", env: (*stack).fileEnv,
			edit:    func(_ *testing.T, _ *stack, m map[string]string) { m["LUX_OIDC_ISSUERS"] = "http://127.0.0.1:1" },
			failing: []string{"issuers"},
			warning: []string{"dialects"},
		},
		{
			name: "authorizer", env: (*stack).fileEnv,
			arrange: func(t *testing.T, s *stack) {
				s.authz.Fail(http.StatusInternalServerError)
				t.Cleanup(func() { s.authz.Fail(0) })
			},
			failing: []string{"authorizer"},
			warning: []string{"dialects"},
		},
		{
			name: "events", env: (*stack).fileEnv,
			edit:    func(_ *testing.T, _ *stack, m map[string]string) { m["LUX_EVENTS_SECRET"] = "not-the-sinks-secret" },
			failing: []string{"events"},
			warning: []string{"dialects"},
		},
		{
			name: "requestlog", env: (*stack).fileEnv,
			arrange: func(t *testing.T, s *stack) {
				// The client retries three times, so three refusals fail
				// the PUT and none leaks into the next case.
				s.s3.Fail(3, http.StatusInternalServerError)
				t.Cleanup(func() { s.s3.Fail(0, 0) })
			},
			failing: []string{"requestlog"},
			warning: []string{"dialects"},
		},
		{
			name: "store", env: (*stack).serverEnv,
			edit: func(t *testing.T, _ *stack, m map[string]string) {
				m["LUX_DB_URL"] = "postgres://lux:secret@" + closedPort(t) + "/lux?sslmode=disable"
			},
			failing: []string{"store"},
			warning: []string{"public url", "migrations", "db conns", "credentials", "providers", "dialects"},
		},
		{
			name: "manifest dir", env: (*stack).fileEnv,
			edit: func(t *testing.T, _ *stack, m map[string]string) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("apiVersion: lux.latere.ai/v1beta1\nkind: Provider\nmetadata: {name: broken}\nspec: {dialect: openai}\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				m["LUX_MANIFEST_DIR"] = dir
			},
			failing: []string{"manifest dir"},
			warning: []string{"public url", "credentials", "providers", "dialects"},
		},
		{
			name: "credentials", env: (*stack).serverEnv,
			seed: func(t *testing.T, st store.Store) {
				other, err := secrets.Parse(otherKEK)
				if err != nil {
					t.Fatal(err)
				}
				row, err := other.Seal("prv_sealed", 1, []byte("sk-canary"))
				if err != nil {
					t.Fatal(err)
				}
				if err := st.Credentials().Put(t.Context(), "prv_sealed", row); err != nil {
					t.Fatal(err)
				}
			},
			failing: []string{"credentials"},
			warning: []string{"store"},
		},
		{
			name: "providers", env: (*stack).fileEnv,
			edit: func(t *testing.T, s *stack, m map[string]string) {
				m["LUX_MANIFEST_DIR"] = s.manifests(t, map[v1.Dialect]string{v1.DialectOpenAI: "http://127.0.0.1:1"})
			},
			failing: []string{"providers"},
		},
		{
			name: "tunnels without a session", env: (*stack).serverEnv,
			edit: func(_ *testing.T, _ *stack, m map[string]string) { m["LUX_TUNNEL_ENABLED"] = "1" },
			seed: func(t *testing.T, st store.Store) {
				putProvider(t, st, "laptop", v1.ProviderSpec{Dialect: v1.DialectOpenAI, Tunnel: true})
			},
			failing: []string{"tunnels"},
			warning: []string{"store"},
		},
		{
			name: "tunnels with a live session", env: (*stack).serverEnv,
			edit: func(_ *testing.T, _ *stack, m map[string]string) { m["LUX_TUNNEL_ENABLED"] = "1" },
			seed: func(t *testing.T, st store.Store) {
				p := putProvider(t, st, "laptop", v1.ProviderSpec{Dialect: v1.DialectOpenAI, Tunnel: true})
				if err := st.Tunnels().Register(t.Context(), store.Tunnel{ProviderID: p.Status.ID, Session: "tun_1", Subject: "test"}, time.Minute); err != nil {
					t.Fatal(err)
				}
			},
			warning: []string{"store"},
		},
		{
			name: "tunnels with a forward route that refuses the secret", env: (*stack).serverEnv,
			edit: func(t *testing.T, _ *stack, m map[string]string) {
				m["LUX_TUNNEL_ENABLED"] = "1"
				m["LUX_TUNNEL_FORWARD_SECRET"] = forwardSecret
				m["LUX_TUNNEL_FORWARD_ADDR"] = h2c(t, strings.Repeat("b", 32), http.StatusServiceUnavailable)
			},
			failing: []string{"tunnels"},
			warning: []string{"store"},
		},
		{
			name: "tunnels with a forward route nothing answers", env: (*stack).serverEnv,
			edit: func(_ *testing.T, _ *stack, m map[string]string) {
				m["LUX_TUNNEL_ENABLED"] = "1"
				m["LUX_TUNNEL_FORWARD_SECRET"] = forwardSecret
				m["LUX_TUNNEL_FORWARD_ADDR"] = "127.0.0.1:1"
			},
			failing: []string{"tunnels"},
			warning: []string{"store"},
		},
		{
			name: "tunnels with a forward route that accepts the secret", env: (*stack).serverEnv,
			edit: func(t *testing.T, _ *stack, m map[string]string) {
				m["LUX_TUNNEL_ENABLED"] = "1"
				m["LUX_TUNNEL_FORWARD_SECRET"] = forwardSecret
				m["LUX_TUNNEL_FORWARD_ADDR"] = h2c(t, forwardSecret, http.StatusServiceUnavailable)
			},
			warning: []string{"store"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.env(s)
			if tc.edit != nil {
				tc.edit(t, s, m)
			}
			if tc.arrange != nil {
				tc.arrange(t, s)
			}
			o := Options{Getenv: env(m)}
			if tc.seed != nil {
				o.open = seeded(tc.seed)(t)
			}
			lines := Lines(t.Context(), o)
			var names []string
			for _, l := range lines {
				names = append(names, l.Name)
			}
			if want := unconfigured(); !slices.Equal(names, want) {
				t.Fatalf("the lines are %v, want %v", names, want)
			}
			for _, l := range lines {
				want := OK
				switch {
				case slices.Contains(tc.failing, l.Name):
					want = Fail
				case slices.Contains(tc.warning, l.Name):
					want = Warn
				}
				if l.State != want {
					t.Errorf("%s: want %s, got %s", l.Name, want, l)
				}
				if l.Detail == "" {
					t.Errorf("%s has no sentence", l.Name)
				}
			}
			// Run prints the same lines and exits 1 on any fail; the
			// stubs' injected failures are spent by the run above, so
			// the exit code is read from the lines here and Run's own
			// printing is TestCheckRunPrintsEveryLine's.
			code := 0
			for _, l := range lines {
				if l.State == Fail {
					code = 1
				}
			}
			if want := min(len(tc.failing), 1); code != want {
				t.Errorf("exit %d, want %d", code, want)
			}
		})
	}
}

// TestCheckRunPrintsEveryLine: Run prints one line per requirement in
// order, exits 0 when nothing failed, and 1 when one line did.
func TestCheckRunPrintsEveryLine(t *testing.T) {
	s := newStack(t)
	var out bytes.Buffer
	if code := Run(t.Context(), Options{Getenv: env(s.fileEnv())}, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	printed := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	names := unconfigured()
	if len(printed) != len(names) {
		t.Fatalf("%d lines printed, want %d:\n%s", len(printed), len(names), out.String())
	}
	for i, line := range printed {
		if !strings.HasPrefix(line, "ok   "+names[i]+": ") && !strings.HasPrefix(line, "warn "+names[i]+": ") {
			t.Errorf("line %d is %q, want %s", i+1, line, names[i])
		}
	}
	m := s.fileEnv()
	m["LUX_OIDC_ISSUERS"] = "http://127.0.0.1:1"
	out.Reset()
	if code := Run(t.Context(), Options{Getenv: env(m)}, &out); code != 1 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "\nfail issuers: ") {
		t.Fatalf("no failing issuers line:\n%s", out.String())
	}
}

// TestCheckNamesWhatItReads holds the sentences to what an operator
// needs from them: the file mode's manifest dir line counts the kinds,
// the db conns line carries the arithmetic, the memory store line says
// the check reads none of the serving replica's objects, and no line
// carries a secret.
func TestCheckNamesWhatItReads(t *testing.T) {
	s := newStack(t)
	file := byName(Lines(t.Context(), Options{Getenv: env(s.fileEnv())}))
	if want := "4 providers, 1 budgets, 1 models, 1 keys"; !strings.Contains(file["manifest dir"].Detail, want) {
		t.Errorf("manifest dir: %s, want %q", file["manifest dir"], want)
	}
	if want := "LUX_DB_MAX_CONNS is 8, so each replica opens up to 9 connections and the Deployment's 2 replicas 18"; !strings.Contains(file["db conns"].Detail, want) {
		t.Errorf("db conns: %s, want %q", file["db conns"], want)
	}
	if want := "gemini Provider(s) gemini are reached through the /gemini door alone"; !strings.Contains(file["dialects"].Detail, want) {
		t.Errorf("dialects: %s", file["dialects"])
	}
	if want := "unset; the file mode seals nothing"; file["secrets kek"].Detail != want {
		t.Errorf("secrets kek: %s", file["secrets kek"])
	}
	if !strings.HasPrefix(file["version"].Detail, "luxd ") {
		t.Errorf("version: %s", file["version"])
	}
	if want := " inside 5s"; !strings.HasSuffix(file["authorizer"].Detail, want) {
		t.Errorf("authorizer: %s", file["authorizer"])
	}

	m := s.serverEnv()
	m["LUX_TUNNEL_ENABLED"] = "1"
	m["LUX_DB_MAX_CONNS"] = "20" // read only with a database, so the default holds
	server := byName(Lines(t.Context(), Options{Getenv: env(m), Replicas: 3}))
	if want := "reads none of the objects a serving replica holds"; !strings.Contains(server["store"].Detail, want) {
		t.Errorf("store: %s", server["store"])
	}
	if want := "the Deployment's 3 replicas 27"; !strings.Contains(server["db conns"].Detail, want) {
		t.Errorf("db conns: %s", server["db conns"])
	}
	if want := "1 key(s) of 32 bytes; the first wraps"; !strings.Contains(server["secrets kek"].Detail, want) {
		t.Errorf("secrets kek: %s", server["secrets kek"])
	}
	if want := "none declared; over the memory store"; !strings.Contains(server["providers"].Detail, want) {
		t.Errorf("providers: %s", server["providers"])
	}
	if want := "on; 0 tunnelled Provider(s)"; !strings.Contains(server["tunnels"].Detail, want) {
		t.Errorf("tunnels: %s", server["tunnels"])
	}
	for _, lines := range []map[string]Line{file, server} {
		for _, l := range lines {
			for _, secret := range []string{kek, s.authz.Token(), sink.DefaultSecret, s3test.Secret} {
				if strings.Contains(l.Detail, secret) {
					t.Errorf("%s carries a secret", l)
				}
			}
		}
	}
	if got := (Line{Warn, "store", "x"}).String(); got != "warn store: x" {
		t.Errorf("String() = %q", got)
	}
	if got := (Line{OK, "version", "y"}).String(); got != "ok   version: y" {
		t.Errorf("String() = %q", got)
	}
}

// TestCheckAuthorizerProbe is spec 017's row for the probe: it is
// authz.Probe("provider.read", "Provider") carrying authz.ProbeID under
// no subject, the stub of latere.ai/x/pkg/authz/stub denies it and the
// row passes, a second probe through the same client is answered from
// the cache as any deny is, an authorizer that allows everything fails
// the row with authz.ErrProbeAllowed's sentence, and one that answers
// nothing fails it too.
func TestCheckAuthorizerProbe(t *testing.T) {
	ctx := t.Context()
	client := func(t *testing.T, url, token string) *authz.Client {
		t.Helper()
		c, err := authz.NewClient(authz.Options{URL: url, Token: token, HTTP: &http.Client{Timeout: time.Second, Transport: http.DefaultTransport}, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	t.Run("the stub denies", func(t *testing.T) {
		s := stub.New(t)
		s.Allow(stub.Rule{}) // every subject, every action, every resource: the probe is denied regardless
		c := client(t, s.URL(), s.Token())
		if l := probe(ctx, c, s.URL()); l.State != OK || l.Detail != s.URL()+" denies the probe" {
			t.Fatalf("%s", l)
		}
		if l := probe(ctx, c, s.URL()); l.State != OK {
			t.Fatalf("second probe: %s", l)
		}
		reqs := s.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%d requests reached the stub, want one: the deny is cached", len(reqs))
		}
		want := authz.Probe("provider.read", v1.KindProvider)
		if reqs[0].Action != want.Action || reqs[0].Resource.Kind != want.Resource.Kind || reqs[0].Resource.ID != authz.ProbeID || reqs[0].Subject != "" {
			t.Errorf("the probe was %+v, want action %s on kind %s with id %s and no subject", reqs[0], want.Action, want.Resource.Kind, authz.ProbeID)
		}
	})
	t.Run("an authorizer that allows everything", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"allow": true}`))
		}))
		t.Cleanup(srv.Close)
		l := probe(ctx, client(t, srv.URL, "t"), srv.URL)
		if l.State != Fail || !strings.Contains(l.Detail, authz.ErrProbeAllowed.Error()) {
			t.Fatalf("%s", l)
		}
	})
	t.Run("an authorizer that answers nothing", func(t *testing.T) {
		l := probe(ctx, client(t, "http://127.0.0.1:1", "t"), "http://127.0.0.1:1")
		if l.State != Fail || !strings.Contains(l.Detail, "http://127.0.0.1:1: ") {
			t.Fatalf("%s", l)
		}
	})
	t.Run("through Lines against the stub", func(t *testing.T) {
		s := newStack(t)
		lines := byName(Lines(ctx, Options{Getenv: env(s.serverEnv())}))
		if l := lines["authorizer"]; l.State != OK || !strings.HasPrefix(l.Detail, s.authz.URL()+" denies the probe inside ") {
			t.Fatalf("%s", l)
		}
		probes := 0
		for _, r := range s.authz.Requests() {
			if r.Resource.ID == authz.ProbeID {
				probes++
			}
		}
		if probes != 1 {
			t.Errorf("%d probes reached the stub, want one", probes)
		}
	})
}

// TestCheckDoesNotEchoTheEnvironment: the configuration line names the
// mode and never a value, so a failing configuration prints the
// variable and not the URL that may carry a password.
func TestCheckDoesNotEchoTheEnvironment(t *testing.T) {
	s := newStack(t)
	m := s.serverEnv()
	m["LUX_DB_URL"] = "mysql://lux:hunter2@db.example.com/lux"
	var out bytes.Buffer
	if code := Run(t.Context(), Options{Getenv: env(m)}, &out); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out.String(), "hunter2") {
		t.Fatalf("the password is echoed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "fail configuration: LUX_DB_URL has scheme") {
		t.Fatalf("stdout:\n%s", out.String())
	}
	if got, want := strings.Count(out.String(), "warn "), len(unconfigured())-2; got != want {
		t.Fatalf("%d rows not checked, want %d:\n%s", got, want, out.String())
	}
}

// optional are the rows of a feature an installation configures, each
// of which prints no line when the feature is not configured.
var optional = []string{"local issuer"}

// unconfigured is Names without the optional rows: the lines of an
// installation that configures none of those features.
func unconfigured() []string {
	return slices.DeleteFunc(slices.Clone(Names), func(n string) bool { return slices.Contains(optional, n) })
}
