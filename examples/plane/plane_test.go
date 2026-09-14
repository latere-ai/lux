// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
	"latere.ai/x/lux/test/conformance"
	"latere.ai/x/lux/test/stubs/provider"
)

// The example front is held to the contract the same way any platform's
// own front is: the conformance suite of spec 018, run against it over
// HTTP with a token from an issuer it lists, with no state of this
// repository's behind it.

// harness is one running front with the stubs it verifies against.
type harness struct {
	plane     *Plane
	server    *httptest.Server
	issuer    *issuertest.Server
	authz     *stub.Server
	upstreams *upstreams
	url       *url.URL
}

// start builds the front over a stub issuer and a stub authorizer on
// loopback, or over the authorizer the caller names.
func start(t *testing.T, authorizerURL, authorizerToken string) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{issuer: issuertest.New(t, issuertest.WithDefaultAudience("lux")), upstreams: newUpstreams(t)}
	if authorizerURL == "" {
		h.authz = stub.New(t)
		authorizerURL, authorizerToken = h.authz.URL(), h.authz.Token()
	}
	h.server = httptest.NewUnstartedServer(nil)
	// The front names itself localhost rather than by its address,
	// because the contract's loop check compares the host of a
	// Provider's baseURL with the public URL's host alone.
	_, port, err := net.SplitHostPort(h.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	h.url, err = url.Parse("http://localhost:" + port)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(ctx, Options{
		PublicURL: h.url, IssuerURL: h.issuer.URL(), Audience: "lux",
		AuthorizerURL: authorizerURL, AuthorizerToken: authorizerToken,
		Defaults: manifest.Defaults{Timeout: 10 * time.Minute}, AllowPrivateUpstreams: true,
		RequestsPerMinute: 6000, Version: "example", Flush: 100 * time.Millisecond,
		HTTP:         &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}},
		Dial:         h.upstreams.dial,
		LookupIPAddr: h.upstreams.lookup,
		RootCAs:      h.upstreams.roots,
		Logger:       slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.plane = p
	h.server.Config.Handler = p.Handler()
	h.server.Start()
	t.Cleanup(h.server.Close)
	go p.Run(ctx)
	return h
}

// upstreams are the providers the front forwards to in these tests:
// one stub provider per dialect of test/stubs/provider, behind a
// resolver and a dialer the front is built with, so a Provider under
// example.com is answered by the stub of its dialect and no test
// reaches a network. The suite runs without a stubs document, so the
// cases that read what a provider received skip by name; what these
// stubs are here for is that a forward succeeds, which is what keeps
// the circuit of spec 008 closed through a whole run.
type upstreams struct {
	byDialect map[string]*httptest.Server
	byIP      map[string]string
	roots     *x509.CertPool
}

// newUpstreams starts one stub provider per dialect over TLS, each on
// its own loopback address, and returns the resolver, the dialer, and
// the root the front verifies them against.
func newUpstreams(t *testing.T) *upstreams {
	t.Helper()
	cert, roots := wildcardCertificate(t)
	u := &upstreams{byDialect: map[string]*httptest.Server{}, byIP: map[string]string{}, roots: roots}
	for i, d := range dialects {
		server := httptest.NewUnstartedServer(provider.New(provider.Options{Dialect: v1.Dialect(d)}))
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		server.StartTLS()
		t.Cleanup(server.Close)
		u.byDialect[d] = server
		u.byIP[fmt.Sprintf("127.0.0.%d", i+1)] = strings.TrimPrefix(server.URL, "https://")
	}
	return u
}

// lookup resolves a Provider's host to the address its dialect's stub
// is reached at; a host outside the pattern resolves to nothing.
func (u *upstreams) lookup(_ context.Context, host string) ([]net.IPAddr, error) {
	dialect, _, _ := strings.Cut(host, ".")
	for i, d := range dialects {
		if d == dialect {
			return []net.IPAddr{{IP: net.IPv4(127, 0, 0, byte(i+1))}}, nil
		}
	}
	return nil, errors.New("the example's tests resolve no host: " + host)
}

// dial connects to the stub the resolved address names.
func (u *upstreams) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	target, ok := u.byIP[host]
	if !ok {
		return nil, errors.New("the example's tests dial their own stubs alone, not " + addr)
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	return d.DialContext(ctx, network, target)
}

// wildcardCertificate is one self-signed certificate for the hosts the
// suite's Providers name, and the pool that verifies it.
func wildcardCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "the example's stub providers"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"*.provider.example.com", "provider.example.com"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// config is the suite's configuration against this front: the default
// subject, and a token per subject of the front's own issuer, with the
// plan claim the authorizer reads.
func (h *harness) config(plan string) conformance.Config {
	mint := func(sub string) string {
		claims := issuertest.Claims{Sub: sub, Aud: issuertest.StringList{"lux"}}
		if plan != "" {
			claims.Extra = map[string]any{"plan": plan}
		}
		return h.issuer.Mint(claims)
	}
	return conformance.Config{
		URL:     h.url.String(),
		Subject: authz.Subject(h.issuer.URL(), "alice"),
		Token: func(rendered string) (string, bool) {
			iss, sub, ok := authz.SplitSubject(rendered)
			if !ok || iss != h.issuer.URL() {
				return "", false
			}
			return mint(sub), true
		},
	}
}

// TestExamplePlaneConforms is spec 020's second acceptance row and spec
// 018's row for a front that is not luxd: a server built from manifest,
// gateway, and metering, with a store and an identity of its own,
// answers the contract. It runs with a stub provider per dialect, so
// the cases that assert what a provider received run too.
func TestExamplePlaneConforms(t *testing.T) {
	h := start(t, "", "")
	conformance.Run(t, h.config(""))
}

// TestPlaneServesTheDoorsFromTheKey is the data plane half in one
// request: a Key the front minted opens a door, and the front's own
// store answered the lookup.
func TestPlaneServesTheDoorsFromTheKey(t *testing.T) {
	h := start(t, "", "")
	token := h.issuer.Mint(issuertest.Claims{Sub: "alice", Aud: issuertest.StringList{"lux"}})
	body := `{"metadata":{"name":"dev"},"spec":{"models":["*"]}}`
	resp := h.do(t, http.MethodPut, "/v1/keys/dev", token, body)
	if resp.status != http.StatusCreated {
		t.Fatalf("PUT /v1/keys/dev = %d %s", resp.status, resp.body)
	}
	value := readString(resp.body, `"value":"`)
	if !strings.HasPrefix(value, "lux_") {
		t.Fatalf("the create answer carries no value: %s", resp.body)
	}
	if list := h.do(t, http.MethodGet, "/openai/v1/models", value, ""); list.status != http.StatusOK {
		t.Fatalf("the door with the Key = %d %s", list.status, list.body)
	}
	if list := h.do(t, http.MethodGet, "/openai/v1/models", token, ""); list.status != http.StatusUnauthorized {
		t.Fatalf("the door with the issuer's token = %d, want unauthenticated", list.status)
	}
	if self := h.do(t, http.MethodGet, "/v1/self", value, ""); self.status != http.StatusUnauthorized {
		t.Fatalf("/v1 with the Key = %d, want unauthenticated", self.status)
	}
}

// answer is one response read whole.
type answer struct {
	status int
	body   string
}

// do sends one request with a bearer.
func (h *harness) do(t *testing.T, method, path, bearer, body string) answer {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if rd == nil {
		req, err = http.NewRequestWithContext(t.Context(), method, h.url.String()+path, nil)
	} else {
		req, err = http.NewRequestWithContext(t.Context(), method, h.url.String()+path, rd)
	}
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return answer{status: resp.StatusCode, body: string(data)}
}

// readString is the JSON string that follows the marker, for a test
// that needs one value out of an answer.
func readString(body, marker string) string {
	_, rest, ok := strings.Cut(body, marker)
	if !ok {
		return ""
	}
	value, _, _ := strings.Cut(rest, `"`)
	return value
}

// TestRunRefusesAnIncompleteConfiguration: the command says what it
// needs rather than serving without an issuer or an authorizer.
func TestRunRefusesAnIncompleteConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"no issuer", []string{"-authorizer", "http://127.0.0.1:1"}, 2},
		{"no authorizer", []string{"-issuer", "http://127.0.0.1:1"}, 2},
		{"a flag it does not have", []string{"-nonesuch"}, 2},
		{"an address in use", []string{"-issuer", "http://127.0.0.1:1", "-authorizer", "http://127.0.0.1:1", "-addr", "256.256.256.256:1"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if code := run(t.Context(), tc.args, &out); code != tc.want {
				t.Errorf("run = %d, want %d: %s", code, tc.want, out.String())
			}
		})
	}
}

// TestRunServesAndStops: the command serves over its own flags and
// stops when its context ends.
func TestRunServesAndStops(t *testing.T) {
	iss := issuertest.New(t, issuertest.WithDefaultAudience("lux"))
	az := stub.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	var out strings.Builder
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-issuer", iss.URL(), "-authorizer", az.URL(),
			"-authorizer-token", az.Token(), "-allow-private-upstreams",
		}, &out)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "plane: serving") {
		if time.Now().After(deadline) {
			t.Fatalf("the command never reported its address: %s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("run = %d: %s", code, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the command did not stop")
	}
}

// TestRunRefusesAnUnreachableIssuer: a front whose issuer does not
// answer discovery refuses to start rather than admitting every token.
func TestRunRefusesAnUnreachableIssuer(t *testing.T) {
	az := stub.New(t)
	var out strings.Builder
	code := run(t.Context(), []string{
		"-addr", "127.0.0.1:0", "-issuer", "http://127.0.0.1:1", "-authorizer", az.URL(),
	}, &out)
	if code != 1 || !strings.Contains(out.String(), "plane:") {
		t.Errorf("run = %d: %s", code, out.String())
	}
}
