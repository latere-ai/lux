// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	v1 "latere.ai/x/lux/manifest/v1"
)

// provider is one resolved Provider toward baseURL, as the store hands
// it out: the defaults filled, the credential set.
func provider(baseURL string) *v1.Provider {
	return &v1.Provider{
		Metadata: v1.ObjectMeta{Name: "openai"},
		Spec: v1.ProviderSpec{
			Dialect:    v1.DialectOpenAI,
			BaseURL:    baseURL,
			Credential: &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer},
			Timeout:    "10m",
		},
		Status: v1.ProviderStatus{ID: "prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y", Owner: "https://login.example.com|alice"},
	}
}

// recorder is an upstream that remembers what it was sent.
type recorder struct {
	mu       sync.Mutex
	requests []*http.Request
	handle   func(w http.ResponseWriter, r *http.Request)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.requests = append(rec.requests, r.Clone(context.Background()))
	rec.mu.Unlock()
	if rec.handle != nil {
		rec.handle(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (rec *recorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.requests)
}

func (rec *recorder) last() *http.Request {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) == 0 {
		return nil
	}
	return rec.requests[len(rec.requests)-1]
}

// loopback is a client source that may reach the test servers on
// 127.0.0.1, as an operator with a runtime on the same host would set.
func loopback(o ClientOptions) *Clients {
	o.AllowPrivate = true
	if o.Version == "" {
		o.Version = "test"
	}
	return NewClientSource(o)
}

// get sends one GET through the Provider's client with a deadline.
func get(t *testing.T, c *Clients, p *v1.Provider, rawURL string, timeout time.Duration) (*http.Response, error) {
	t.Helper()
	client, err := c.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

// TestUpstreamClientIsInstrumented: the outbound request carries the
// trace context, which is what the otelhttp transport injects and a bare
// transport never would.
func TestUpstreamClientIsInstrumented(t *testing.T) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		_ = tp.Shutdown(context.Background())
	})
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := loopback(ClientOptions{})
	client, err := c.Client(t.Context(), provider(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := tp.Tracer("test").Start(t.Context(), "caller")
	defer span.End()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/models", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := rec.last().Header.Get("traceparent")
	if !strings.HasPrefix(got, "00-"+span.SpanContext().TraceID().String()+"-") {
		t.Fatalf("traceparent = %q, want the caller's trace %s", got, span.SpanContext().TraceID())
	}
}

// TestNoCompressionAdded: the transport adds no Accept-Encoding of its
// own and decodes nothing, so the header reaches the upstream as the
// caller sent it, or not at all.
func TestNoCompressionAdded(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := loopback(ClientOptions{})
	p := provider(srv.URL)
	if _, err := get(t, c, p, srv.URL+"/v1/models", time.Second); err != nil {
		t.Fatal(err)
	}
	if got, ok := rec.last().Header["Accept-Encoding"]; ok {
		t.Fatalf("Accept-Encoding = %q, want none", got)
	}
	client, _ := c.Client(t.Context(), p)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/files", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := rec.last().Header.Get("Accept-Encoding"); got != "gzip, br" {
		t.Fatalf("Accept-Encoding = %q, want the caller's", got)
	}
	if resp.Uncompressed {
		t.Fatal("the transport decoded a body")
	}
}

// TestRedirectNotFollowed: a 302 is returned as it is, with one request
// at the upstream and none at the location it named.
func TestRedirectNotFollowed(t *testing.T) {
	elsewhere := &recorder{}
	other := httptest.NewServer(elsewhere)
	t.Cleanup(other.Close)
	rec := &recorder{handle: func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{URL: &url.URL{}, Method: http.MethodGet}, other.URL+"/moved", http.StatusFound)
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	resp, err := get(t, loopback(ClientOptions{}), provider(srv.URL), srv.URL+"/v1/chat/completions", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || !strings.HasSuffix(resp.Header.Get("Location"), "/moved") {
		t.Fatalf("response = %d %q, want the 302 as it was", resp.StatusCode, resp.Header.Get("Location"))
	}
	if rec.count() != 1 || elsewhere.count() != 0 {
		t.Fatalf("upstream saw %d requests and the location %d; want 1 and 0", rec.count(), elsewhere.count())
	}
}

// TestPrivateAddressRefusedAtDial: a public name that resolves to a
// private address is refused before any dial without the flag and
// connected with it; a name with a public answer beside a private one
// is dialled on the public address only.
func TestPrivateAddressRefusedAtDial(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var dialled []string
	var mu sync.Mutex
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, addr)
		mu.Unlock()
		if strings.HasPrefix(addr, "127.0.0.1:") {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}
		return nil, errors.New("no route to " + addr)
	}
	resolve := func(_ context.Context, host string) ([]net.IPAddr, error) {
		switch host {
		case "inward.example.com":
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		case "mixed.example.com":
			return []net.IPAddr{{IP: net.ParseIP("10.1.2.3")}, {IP: net.ParseIP("fe80::1")}, {IP: net.ParseIP("203.0.113.9")}}, nil
		}
		return nil, errors.New("no such host " + host)
	}
	base := "http://inward.example.com:" + port
	p := provider(base)

	refusing := NewClientSource(ClientOptions{LookupIPAddr: resolve, Dial: dial})
	_, err := get(t, refusing, p, base+"/v1/models", time.Second)
	if !errors.Is(err, ErrPrivateAddress) || !strings.Contains(err.Error(), "inward.example.com resolves to 127.0.0.1") {
		t.Fatalf("without the flag: %v", err)
	}
	if len(dialled) != 0 || rec.count() != 0 {
		t.Fatalf("a refused address was dialled: %q", dialled)
	}
	literal := provider("http://127.0.0.1:" + port)
	if _, err := get(t, refusing, literal, literal.Spec.BaseURL+"/v1/models", time.Second); !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("an IP literal without the flag: %v", err)
	}

	admitting := NewClientSource(ClientOptions{AllowPrivate: true, LookupIPAddr: resolve, Dial: dial})
	resp, err := get(t, admitting, p, base+"/v1/models", time.Second)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("with the flag: %v", err)
	}
	if rec.count() != 1 || rec.last().Host != "inward.example.com:"+port {
		t.Fatalf("with the flag the upstream saw %d requests for host %q", rec.count(), rec.last().Host)
	}

	mixed := provider("http://mixed.example.com:" + port)
	_, err = get(t, refusing, mixed, mixed.Spec.BaseURL+"/v1/models", time.Second)
	if err == nil || errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("a mixed answer: %v, want the public address dialled and failing", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialled) != 2 || dialled[1] != "203.0.113.9:"+port {
		t.Fatalf("dialled %q, want the public address of the mixed answer alone", dialled)
	}
}

// TestHostPin: a request toward another scheme, host, or port is refused
// before any dial; the same host under another path, and the default
// port spelled out, pass the pin.
func TestHostPin(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	var dials atomic.Int32
	c := loopback(ClientOptions{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}})
	p := provider(srv.URL + "/v1")
	for name, target := range map[string]string{
		"another host":   "http://api.example.com/v1/models",
		"another port":   "http://127.0.0.1:1/v1/models",
		"another scheme": "https://" + host + "/v1/models",
	} {
		_, err := get(t, c, p, target, time.Second)
		if !errors.Is(err, ErrHostPinned) {
			t.Errorf("%s: %v, want ErrHostPinned", name, err)
		}
	}
	if dials.Load() != 0 || rec.count() != 0 {
		t.Fatalf("a pinned request was dialled: %d dials, %d requests", dials.Load(), rec.count())
	}
	resp, err := get(t, c, p, srv.URL+"/v1/chat/completions", time.Second)
	if err != nil || resp.StatusCode != http.StatusOK || rec.count() != 1 {
		t.Fatalf("the same host: %v, %d requests", err, rec.count())
	}
	if got := rec.last().Header.Get("User-Agent"); got != "luxd/test" {
		t.Fatalf("User-Agent = %q", got)
	}

	// The default port: https://api.example.com pins to 443, so a request
	// spelling :443 passes the pin and reaches the dial, which fails here
	// with the hook's error and not with ErrHostPinned.
	nowhere := NewClientSource(ClientOptions{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial reached")
	}, LookupIPAddr: func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.9")}}, nil
	}})
	public := provider("https://api.example.com/v1")
	_, err = get(t, nowhere, public, "https://api.example.com:443/v1/models", time.Second)
	if err == nil || errors.Is(err, ErrHostPinned) || !strings.Contains(err.Error(), "dial reached") {
		t.Fatalf("the default port spelled out: %v", err)
	}
	_, err = get(t, nowhere, public, "https://API.example.com/v1/models", time.Second)
	if err == nil || errors.Is(err, ErrHostPinned) {
		t.Fatalf("the host in another case: %v", err)
	}
}

// TestConcurrencyLimitsInFlight: concurrency 2 holds a third request
// until one finishes, a wait past the request deadline is
// ErrProviderBusy, and a slot is held for the life of the body.
func TestConcurrencyLimitsInFlight(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 8)
	rec := &recorder{handle: func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "done")
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := loopback(ClientOptions{})
	p := provider(srv.URL)
	p.Spec.Concurrency = 2
	client, err := c.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 3)
	send := func(timeout time.Duration) {
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/models", nil)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		results <- err
	}
	for range 3 {
		go send(5 * time.Second)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("two requests did not reach the upstream")
		}
	}
	select {
	case <-arrived:
		t.Fatal("a third request reached the upstream with two slots held")
	case <-time.After(100 * time.Millisecond):
	}
	// A fourth with a short deadline is refused at the semaphore.
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/models", nil)
	_, err = client.Do(req)
	if !errors.Is(err, ErrProviderBusy) || !strings.Contains(err.Error(), "2 slot(s)") {
		t.Fatalf("a wait past the deadline: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the refused wait outlived the deadline")
	}
	close(release)
	for range 3 {
		if err := <-results; err != nil {
			t.Fatalf("a held request failed: %v", err)
		}
	}
	if rec.count() != 3 {
		t.Fatalf("the upstream saw %d requests, want 3", rec.count())
	}
	// A request whose context ended before the round trip is the
	// context's error, not a busy slot.
	ended, cancelEnded := context.WithCancel(t.Context())
	cancelEnded()
	req, _ = http.NewRequestWithContext(ended, http.MethodGet, srv.URL+"/v1/models", nil)
	if _, err := client.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended context: %v", err)
	}
}

// TestDeadlineIsTheCallers: the client sets no Timeout; the request
// context's deadline is the one that cuts a stalled upstream.
func TestDeadlineIsTheCallers(t *testing.T) {
	rec := &recorder{handle: func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := loopback(ClientOptions{})
	client, err := c.Client(t.Context(), provider(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 0 {
		t.Fatalf("Client.Timeout = %s, want none", client.Timeout)
	}
	start := time.Now()
	_, err = get(t, c, provider(srv.URL), srv.URL+"/v1/models", 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stalled upstream: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the deadline did not cut the request")
	}
}

// TestTLSIsVerifiedAt12OrLater: a certificate the roots do not sign is
// refused, the test server's root admits it, the negotiated version is
// at least 1.2, and HTTP/2 is attempted.
func TestTLSIsVerifiedAt12OrLater(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewUnstartedServer(rec)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	p := provider(srv.URL)

	if _, err := get(t, loopback(ClientOptions{}), p, srv.URL+"/v1/models", time.Second); err == nil {
		t.Fatal("an unverifiable certificate was accepted")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("an unverifiable certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	c := loopback(ClientOptions{RootCAs: roots})
	resp, err := get(t, c, p, srv.URL+"/v1/models", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("TLS = %+v, want 1.2 or later", resp.TLS)
	}
	if resp.ProtoMajor != 2 {
		t.Fatalf("Proto = %s, want HTTP/2", resp.Proto)
	}

	// A server that speaks 1.1 at most is refused at the handshake.
	old := httptest.NewUnstartedServer(rec)
	old.TLS = &tls.Config{MaxVersion: tls.VersionTLS11, MinVersion: tls.VersionTLS10} //nolint:gosec // the test's server is the old one
	old.StartTLS()
	t.Cleanup(old.Close)
	oldRoots := x509.NewCertPool()
	oldRoots.AddCert(old.Certificate())
	if _, err := get(t, loopback(ClientOptions{RootCAs: oldRoots}), provider(old.URL), old.URL+"/v1/models", time.Second); err == nil {
		t.Fatal("a TLS 1.1 server was accepted")
	}
}

// TestTransportRows holds the built transport to the table, field by
// field, through the entry the source keeps.
func TestTransportRows(t *testing.T) {
	c := loopback(ClientOptions{})
	p := provider("https://api.example.com/v1")
	p.Status.ID = ""
	if _, err := c.Client(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	e := c.entries["openai"]
	if e == nil {
		t.Fatal("no entry under the Provider's name")
	}
	tr := e.transport
	if tr.Proxy != nil {
		t.Error("Proxy is set")
	}
	if !tr.DisableCompression || !tr.ForceAttemptHTTP2 {
		t.Error("DisableCompression or ForceAttemptHTTP2 is off")
	}
	if tr.MaxIdleConnsPerHost != 32 || tr.IdleConnTimeout != 90*time.Second || tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("pool = %d, %s, %s", tr.MaxIdleConnsPerHost, tr.IdleConnTimeout, tr.TLSHandshakeTimeout)
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("TLS = %+v", tr.TLSClientConfig)
	}
	if e.client.Timeout != 0 || e.client.CheckRedirect == nil {
		t.Errorf("client = %+v", e.client)
	}
	if err := e.client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v", err)
	}
}

// TestClientIsCachedAndRebuilt: one Provider gets one client until its
// baseURL, timeout, or concurrency changes; a revoked Provider gets a
// fresh one.
func TestClientIsCachedAndRebuilt(t *testing.T) {
	c := loopback(ClientOptions{})
	p := provider("https://api.example.com/v1")
	first, err := c.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := c.Client(t.Context(), p)
	if again != first {
		t.Fatal("an unchanged Provider was rebuilt")
	}
	p.Spec.Headers = map[string]string{"x-team": "research"}
	if same, _ := c.Client(t.Context(), p); same != first {
		t.Fatal("a header change rebuilt the client")
	}
	for name, change := range map[string]func(){
		"baseURL":     func() { p.Spec.BaseURL = "https://api.example.com/v2" },
		"timeout":     func() { p.Spec.Timeout = "5m" },
		"concurrency": func() { p.Spec.Concurrency = 4 },
	} {
		before, _ := c.Client(t.Context(), p)
		change()
		after, err := c.Client(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		if after == before {
			t.Errorf("a %s change kept the client", name)
		}
	}
	current, _ := c.Client(t.Context(), p)
	c.Revoke(p.Status.ID)
	c.Revoke("prv_unknown")
	if fresh, _ := c.Client(t.Context(), p); fresh == current {
		t.Fatal("a revoked Provider kept its client")
	}
}

// TestClientRefusals: a nil Provider, one with no id and no name, a
// tunnelled one, and a baseURL the transport cannot pin are refused
// with the developer's detail.
func TestClientRefusals(t *testing.T) {
	c := loopback(ClientOptions{})
	if _, err := c.Client(t.Context(), nil); err == nil {
		t.Fatal("a nil Provider")
	}
	if _, err := c.Client(t.Context(), &v1.Provider{Spec: v1.ProviderSpec{BaseURL: "https://api.example.com"}}); err == nil || !strings.Contains(err.Error(), "no id and no name") {
		t.Fatalf("no id: %v", err)
	}
	tunnelled := provider("")
	tunnelled.Spec.Tunnel = true
	if _, err := c.Client(t.Context(), tunnelled); !errors.Is(err, ErrTunnelled) {
		t.Fatalf("a tunnelled Provider: %v", err)
	}
	for _, bad := range []string{"", "ftp://api.example.com", "api.example.com/v1", "https://", "://nope"} {
		p := provider(bad)
		if _, err := c.Client(t.Context(), p); err == nil {
			t.Errorf("baseURL %q was accepted", bad)
		}
	}
}

// TestNoProxyEnvironmentHonoured: with HTTPS_PROXY and HTTP_PROXY set the
// request still reaches the Provider's host directly.
func TestNoProxyEnvironmentHonoured(t *testing.T) {
	proxy := &recorder{}
	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)
	t.Setenv("HTTPS_PROXY", proxySrv.URL)
	t.Setenv("HTTP_PROXY", proxySrv.URL)
	t.Setenv("NO_PROXY", "")
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	resp, err := get(t, loopback(ClientOptions{}), provider(srv.URL), srv.URL+"/v1/models", time.Second)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("through the proxy environment: %v", err)
	}
	if rec.count() != 1 || proxy.count() != 0 {
		t.Fatalf("upstream saw %d, proxy saw %d", rec.count(), proxy.count())
	}
}

// TestNoForwardedHeaders is the client half of the row: the transport
// adds no X-Forwarded-* or Forwarded header, and writes its own
// User-Agent over the caller's.
func TestNoForwardedHeaders(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c := loopback(ClientOptions{Version: "1.2.3"})
	client, err := c.Client(t.Context(), provider(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("User-Agent", "curl/8.0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	got := rec.last().Header
	for name := range got {
		if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || strings.EqualFold(name, "Forwarded") {
			t.Errorf("the upstream saw %s", name)
		}
	}
	if ua := got.Get("User-Agent"); ua != "luxd/1.2.3" {
		t.Fatalf("User-Agent = %q", ua)
	}
	if req.Header.Get("User-Agent") != "curl/8.0" {
		t.Fatal("the caller's request was changed in place")
	}
}

// TestCredentialSchemes: each dialect's default header and scheme, and a
// raw custom header, reach the upstream carrying the value as the scheme
// says.
func TestCredentialSchemes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dialect    v1.Dialect
		credential *v1.Credential
		header     string
		want       string
	}{
		{"openai", v1.DialectOpenAI, &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer}, "Authorization", "Bearer sk-value"},
		{"lux", v1.DialectLux, &v1.Credential{Header: "Authorization", Scheme: v1.SchemeBearer}, "Authorization", "Bearer sk-value"},
		{"anthropic", v1.DialectAnthropic, &v1.Credential{Header: "x-api-key", Scheme: v1.SchemeRaw}, "x-api-key", "sk-value"},
		{"gemini", v1.DialectGemini, &v1.Credential{Header: "x-goog-api-key", Scheme: v1.SchemeRaw}, "x-goog-api-key", "sk-value"},
		{"a raw custom header", v1.DialectOpenAI, &v1.Credential{Header: "api-key", Scheme: v1.SchemeRaw}, "api-key", "sk-value"},
		{"bearer on a custom header", v1.DialectAnthropic, &v1.Credential{Header: "x-token", Scheme: v1.SchemeBearer}, "x-token", "Bearer sk-value"},
		{"the dialect's defaults when the credential is bare", v1.DialectGemini, &v1.Credential{}, "x-goog-api-key", "sk-value"},
		{"the dialect's defaults when there is no credential block", v1.DialectAnthropic, nil, "x-api-key", "sk-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			srv := httptest.NewServer(rec)
			t.Cleanup(srv.Close)
			p := provider(srv.URL)
			p.Spec.Dialect, p.Spec.Credential = tc.dialect, tc.credential
			client, err := loopback(ClientOptions{}).Client(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models", nil)
			InjectCredential(req.Header, p, []byte("sk-value"))
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if got := rec.last().Header.Get(tc.header); got != tc.want {
				t.Fatalf("%s = %q, want %q", tc.header, got, tc.want)
			}
			n := 0
			for name := range rec.last().Header {
				if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "x-goog-api-key") || strings.EqualFold(name, "api-key") || strings.EqualFold(name, "x-token") {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("%d credential headers reached the upstream: %v", n, rec.last().Header)
			}
		})
	}
}

// TestCredentialHeaderWins is the custody half of the row: a caller's
// copy of the credential header and a static header of the same name
// are both replaced by the injected value, and a Provider without a
// value strips the caller's copy all the same.
func TestCredentialHeaderWins(t *testing.T) {
	p := provider("https://api.example.com/v1")
	h := http.Header{}
	h.Add("Authorization", "Bearer caller-key")
	h.Add("authorization", "Bearer static-key")
	InjectCredential(h, p, []byte("sk-provider"))
	if got := h.Values("Authorization"); len(got) != 1 || got[0] != "Bearer sk-provider" {
		t.Fatalf("Authorization = %q", got)
	}
	none := http.Header{"Authorization": {"Bearer caller-key"}, "X-Other": {"kept"}}
	InjectCredential(none, p, nil)
	if _, ok := none["Authorization"]; ok || none.Get("X-Other") != "kept" {
		t.Fatalf("a Provider without a value: %v", none)
	}
	unknown := &v1.Provider{Spec: v1.ProviderSpec{Dialect: "other"}}
	kept := http.Header{"Authorization": {"Bearer caller-key"}}
	InjectCredential(kept, unknown, []byte("v"))
	if kept.Get("Authorization") != "Bearer caller-key" {
		t.Fatal("a dialect with no header changed the headers")
	}
}

// TestIsPrivate holds the address rule to its ranges.
func TestIsPrivate(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.0.0.1": true, "172.16.5.4": true, "192.168.1.1": true,
		"169.254.1.1": true, "fe80::1": true, "fc00::1": true, "fd12::1": true, "0.0.0.0": true, "::": true,
		"224.0.0.1": true, "ff02::1": true, "::ffff:10.0.0.1": true,
		"203.0.113.9": false, "8.8.8.8": false, "2001:db8::1": false, "172.32.0.1": false,
	} {
		if got := isPrivate(net.ParseIP(addr)); got != want {
			t.Errorf("isPrivate(%s) = %v, want %v", addr, got, want)
		}
	}
}

// TestDialRefusesABadAddress: an address without a port and a name that
// does not resolve are the dial's errors with the address named.
func TestDialRefusesABadAddress(t *testing.T) {
	c := NewClientSource(ClientOptions{LookupIPAddr: func(_ context.Context, host string) ([]net.IPAddr, error) {
		return nil, errors.New("no such host " + host)
	}})
	if _, err := c.dialContext(t.Context(), "tcp", "no-port"); err == nil || !strings.Contains(err.Error(), "no-port") {
		t.Fatalf("no port: %v", err)
	}
	if _, err := c.dialContext(t.Context(), "tcp", "gone.example.com:443"); err == nil || !strings.Contains(err.Error(), "resolving gone.example.com: no such host") {
		t.Fatalf("no answer: %v", err)
	}
	// The default resolver and dialer are reached when no hook is set; a
	// loopback literal dialled to a closed port fails at the connect.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if _, err := NewClientSource(ClientOptions{AllowPrivate: true}).dialContext(t.Context(), "tcp", addr); err == nil || !strings.Contains(err.Error(), "dial "+addr) {
		t.Fatalf("a closed port: %v", err)
	}
	if _, err := NewClientSource(ClientOptions{}).dialContext(t.Context(), "tcp", "localhost:1"); err == nil {
		t.Fatal("localhost through the default resolver was admitted")
	}
}
