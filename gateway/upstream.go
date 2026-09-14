// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"latere.ai/x/pkg/semaphore"

	v1 "latere.ai/x/lux/manifest/v1"
)

// This file is the upstream client of spec 005: one *http.Client per
// Provider, built under the rules of that spec's table, which is the one
// place invariant 2 of spec 001 is enforced in code. The door handler of
// spec 004 takes it through the ClientSource interface; internal/serve
// and a platform that imports the handler construct it here, so there is
// one implementation in the tree.

// The fixed values of the client table.
const (
	dialTimeout         = 10 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	maxIdleConnsPerHost = 32
	idleConnTimeout     = 90 * time.Second
)

// The failures the client raises before any byte reaches an upstream.
// The door maps them: ErrHostPinned to upstream_error, ErrProviderBusy
// to provider_unavailable; ErrPrivateAddress and ErrTunnelled surface as
// transport failures, which are provider_unavailable too.
var (
	// ErrHostPinned is a request whose URL scheme, host, or port differs
	// from the Provider's baseURL, refused before any dial.
	ErrHostPinned = errors.New("gateway: the request's URL is not under the Provider's baseURL")
	// ErrProviderBusy is a request that found no concurrency slot before
	// its own deadline.
	ErrProviderBusy = errors.New("gateway: no concurrency slot toward the Provider before the request's deadline")
	// ErrPrivateAddress is a host whose every address is loopback,
	// link-local, unique-local, or private, without
	// LUX_UPSTREAM_ALLOW_PRIVATE.
	ErrPrivateAddress = errors.New("gateway: the host resolves to no address outside the private ranges")
	// ErrTunnelled is a tunnel: true Provider, whose client is the
	// carrier transport of spec 013 and is not in this build.
	ErrTunnelled = errors.New("gateway: a tunnelled Provider has no client in this build")
)

// ClientOptions is what NewClientSource builds every client under: the
// two values from the configuration and the seams a test needs to reach
// a loopback server and to watch the dial.
type ClientOptions struct {
	// AllowPrivate admits a loopback, link-local, unique-local, or
	// private address at dial: LUX_UPSTREAM_ALLOW_PRIVATE.
	AllowPrivate bool
	// Version is the build version, written as User-Agent luxd/<Version>
	// on every outbound request.
	Version string
	// LookupIPAddr resolves a host name to its addresses, which the
	// private-address rule then filters; nil is the default resolver's.
	LookupIPAddr func(ctx context.Context, host string) ([]net.IPAddr, error)
	// Dial connects to one admitted address; nil is a net.Dialer with the
	// table's connect timeout. A test that must observe no dial fails
	// here.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs replaces the system roots a certificate is verified
	// against; nil is the system roots. Verification itself has no
	// switch: a test adds its server's certificate here.
	RootCAs *x509.CertPool
	// Now is the clock the concurrency wait is measured against; nil is
	// time.Now.
	Now func() time.Time
}

func (o ClientOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Clients builds and holds one client per Provider. Client returns the
// held one while the Provider's baseURL, timeout, and concurrency are
// what it was built for, and rebuilds it when any of the three changes,
// closing the old pool's idle connections. It satisfies the ClientSource
// of spec 004.
type Clients struct {
	o       ClientOptions
	mu      sync.Mutex
	entries map[string]*entry
}

// entry is one built client and what it was built for.
type entry struct {
	key       clientKey
	client    *http.Client
	transport *http.Transport
}

// clientKey is the three fields whose change rebuilds a client.
type clientKey struct {
	baseURL     string
	timeout     v1.Duration
	concurrency int
}

// NewClientSource returns the one builder of upstream clients.
func NewClientSource(o ClientOptions) *Clients {
	return &Clients{o: o, entries: map[string]*entry{}}
}

// Client returns the client of p, pinned to the scheme, host, and port
// of p.Spec.BaseURL, with a concurrency semaphore of p.Spec.Concurrency
// slots, no proxy, no redirect following, no compression, TLS 1.2 or
// later with verification on, and the private-address rule at dial.
//
// The client has no Timeout of its own: the caller sets the request
// context's deadline from p.Spec.Timeout, over the whole request
// including its stream, and the semaphore waits at most that long.
func (c *Clients) Client(_ context.Context, p *v1.Provider) (*http.Client, error) {
	if p == nil {
		return nil, errors.New("gateway: Client of a nil Provider")
	}
	if p.Spec.Tunnel {
		return nil, fmt.Errorf("%w: %s", ErrTunnelled, p.Metadata.Name)
	}
	id := p.Status.ID
	if id == "" {
		id = p.Metadata.Name
	}
	if id == "" {
		return nil, errors.New("gateway: Client of a Provider with no id and no name")
	}
	key := clientKey{baseURL: p.Spec.BaseURL, timeout: p.Spec.Timeout, concurrency: p.Spec.Concurrency}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[id]; ok && e.key == key {
		return e.client, nil
	}
	e, err := c.build(p, key)
	if err != nil {
		return nil, err
	}
	if old, ok := c.entries[id]; ok {
		old.transport.CloseIdleConnections()
	}
	c.entries[id] = e
	return e.client, nil
}

// Revoke forgets the client of a deleted Provider and closes its idle
// connections; a request in flight finishes or fails on its own
// deadline.
func (c *Clients) Revoke(providerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[providerID]; ok {
		e.transport.CloseIdleConnections()
		delete(c.entries, providerID)
	}
}

// build constructs the transport and the client of the table.
func (c *Clients) build(p *v1.Provider, key clientKey) (*entry, error) {
	base, err := url.Parse(key.baseURL)
	if err != nil {
		return nil, fmt.Errorf("gateway: Provider %s: baseURL %q: %w", p.Metadata.Name, key.baseURL, err)
	}
	scheme := strings.ToLower(base.Scheme)
	if (scheme != "http" && scheme != "https") || base.Hostname() == "" {
		return nil, fmt.Errorf("gateway: Provider %s: baseURL %q is not an http:// or https:// URL with a host", p.Metadata.Name, key.baseURL)
	}
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         c.dialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: c.o.RootCAs},
		TLSHandshakeTimeout: tlsHandshakeTimeout,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleConnTimeout,
		DisableCompression:  true,
	}
	// otelhttp.NewTransport is what latere.ai/x/pkg/otel.Transport wraps:
	// a client span per hop with the trace context injected. It is
	// reached directly because that package also carries the SDK and its
	// exporters, which a root package's importer sets up and which reach
	// os/exec, and spec 001's rule keeps both out of gateway.
	rt := &pinned{
		base:      otelhttp.NewTransport(transport),
		scheme:    scheme,
		host:      strings.ToLower(base.Hostname()),
		port:      portOf(base),
		sem:       semaphore.New(key.concurrency),
		userAgent: "luxd/" + c.o.Version,
		now:       c.o.now,
	}
	if c.o.Version == "" {
		rt.userAgent = "luxd"
	}
	client := &http.Client{
		Transport: rt,
		// A 3xx is an upstream asking for the credential at another
		// location; the response is returned as it is and the door
		// reports it as upstream_error.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &entry{key: key, client: client, transport: transport}, nil
}

// portOf is the URL's port, or the scheme's default.
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// dialContext is the table's DialContext: the name is resolved, the
// addresses the private-address rule refuses are dropped unless
// AllowPrivate, and only the admitted ones are connected to, in order,
// with the connect timeout.
func (c *Clients) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("gateway: dial %s: %w", addr, err)
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		addrs, err := c.lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("gateway: dial %s: resolving %s: %w", addr, host, err)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	var admitted, refused []string
	for _, ip := range ips {
		if !c.o.AllowPrivate && isPrivate(ip) {
			refused = append(refused, ip.String())
			continue
		}
		admitted = append(admitted, net.JoinHostPort(ip.String(), port))
	}
	if len(admitted) == 0 {
		return nil, fmt.Errorf("%w: %s resolves to %s; LUX_UPSTREAM_ALLOW_PRIVATE admits it", ErrPrivateAddress, host, strings.Join(refused, ", "))
	}
	var last error
	for _, a := range admitted {
		conn, err := c.dial(ctx, network, a)
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, fmt.Errorf("gateway: dial %s: %w", addr, last)
}

func (c *Clients) lookup(ctx context.Context, host string) ([]net.IPAddr, error) {
	if c.o.LookupIPAddr != nil {
		return c.o.LookupIPAddr(ctx, host)
	}
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func (c *Clients) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if c.o.Dial != nil {
		return c.o.Dial(ctx, network, addr)
	}
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, network, addr)
}

// isPrivate is the address half of the upstream host rule: loopback,
// link-local, unique-local and private, unspecified, and multicast
// addresses are not an upstream a public name may resolve to.
func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast()
}

// pinned is the RoundTripper of one Provider: the host pin, the
// concurrency semaphore, and the User-Agent, over the instrumented
// transport.
type pinned struct {
	base               http.RoundTripper
	scheme, host, port string
	sem                *semaphore.Semaphore
	userAgent          string
	now                func() time.Time
}

// RoundTrip refuses a request whose URL is not under the Provider's
// baseURL before any dial, acquires a slot for at most the request's
// remaining deadline, and holds it until the response body is closed,
// so an in-flight stream counts as in flight.
func (t *pinned) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if !strings.EqualFold(u.Scheme, t.scheme) || !strings.EqualFold(u.Hostname(), t.host) || portOf(u) != t.port {
		return nil, fmt.Errorf("%w: %s://%s is not %s://%s", ErrHostPinned, u.Scheme, u.Host, t.scheme, net.JoinHostPort(t.host, t.port))
	}
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wait := time.Duration(math.MaxInt64)
	if deadline, ok := ctx.Deadline(); ok {
		wait = deadline.Sub(t.now())
	}
	release, ok := t.sem.Acquire(ctx, wait)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %d slot(s) toward %s all in flight", ErrProviderBusy, t.sem.Size(), net.JoinHostPort(t.host, t.port))
	}
	out := req.Clone(ctx)
	out.Header.Set("User-Agent", t.userAgent)
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasing{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// releasing gives the slot back when the body is closed.
type releasing struct {
	io.ReadCloser
	release func()
}

func (r *releasing) Close() error {
	defer r.release()
	return r.ReadCloser.Close()
}

// InjectCredential writes the Provider's credential into h under its
// credential.header and credential.scheme, last, so it wins over a
// caller's copy of the header and over a static header of the same name;
// a Provider with no value has the caller's copy stripped all the same,
// so a caller's own key never reaches an upstream. The two rules are
// spec 005's, because they are custody rather than transport.
func InjectCredential(h http.Header, p *v1.Provider, value []byte) {
	header, scheme := "", v1.Scheme("")
	if c := p.Spec.Credential; c != nil {
		header, scheme = c.Header, c.Scheme
	}
	if header == "" {
		header = p.Spec.Dialect.CredentialHeader()
	}
	if scheme == "" {
		scheme = p.Spec.Dialect.CredentialScheme()
	}
	if header == "" {
		return
	}
	h.Del(header)
	if len(value) == 0 {
		return
	}
	if scheme == v1.SchemeBearer {
		h.Set(header, "Bearer "+string(value))
		return
	}
	h.Set(header, string(value))
}
