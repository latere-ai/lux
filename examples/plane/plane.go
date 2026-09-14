// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// dialects are the four doors this front serves, which are the four the
// gateway package answers.
var dialects = []string{
	string(v1.DialectOpenAI), string(v1.DialectAnthropic), string(v1.DialectGemini), string(v1.DialectLux),
}

// Options is what a platform hands this front: where it answers, whose
// tokens it accepts, who decides permission, and the defaults a
// manifest inherits.
type Options struct {
	// PublicURL is the address this front answers at, which every URL in
	// an answer is built from and which the contract's loop check reads.
	PublicURL *url.URL
	// IssuerURL is the platform's own OIDC issuer and Audience the value
	// a token's aud must carry.
	IssuerURL string
	Audience  string
	// AuthorizerURL and AuthorizerToken are the platform's permission
	// endpoint and the bearer it requires. Both are required: this front
	// decides nothing itself.
	AuthorizerURL   string
	AuthorizerToken string
	// Defaults are the rates and the timeout a manifest that names none
	// inherits.
	Defaults manifest.Defaults
	// AllowPrivateUpstreams admits a Provider on a loopback or private
	// address, which a test installation needs and a public one does not.
	AllowPrivateUpstreams bool
	// RequestsPerMinute is the control plane rate per subject; zero is
	// no rate.
	RequestsPerMinute int
	// Version is the build's, reported by the well-known document.
	Version string
	// Flush is how often the spend deltas reach the store; zero is the
	// metering package's default.
	Flush time.Duration
	// HTTP is the client the issuer and the authorizer are reached
	// through; nil is a client of this package's own.
	HTTP *http.Client
	// Dial and LookupIPAddr are the upstream client's seams, which a
	// test uses to keep every dial on loopback.
	Dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	LookupIPAddr func(ctx context.Context, host string) ([]net.IPAddr, error)
	// RootCAs replaces the roots an upstream's certificate is verified
	// against; nil is the system's. Verification itself has no switch.
	RootCAs *x509.CertPool
	// Logger receives the one line per data plane request; nil is
	// slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Plane is one platform front: its own store, its own identity, its own
// permission endpoint, and the gateway's data plane behind its own
// control plane.
type Plane struct {
	o          Options
	store      *store
	identity   *identity
	authorizer authz.Authorizer
	limiter    *limiter
	clients    *gateway.Clients
	doors      *gateway.Handler
	subjects   *ratelimit.Buckets
	openapi    []byte
	handler    http.Handler
	now        func() time.Time
}

// New builds the front. It fetches the issuer's discovery document
// once, which is the one call it makes before it serves.
func New(ctx context.Context, o Options) (*Plane, error) {
	switch {
	case o.PublicURL == nil:
		return nil, errors.New("plane: Options.PublicURL is required")
	case o.IssuerURL == "":
		return nil, errors.New("plane: Options.IssuerURL is required")
	case o.AuthorizerURL == "":
		return nil, errors.New("plane: Options.AuthorizerURL is required; this front decides nothing itself")
	}
	if o.Audience == "" {
		o.Audience = "lux"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
	}
	p := &Plane{o: o, store: newStore(), now: o.Now}
	id, err := newIdentity(ctx, o.HTTP, o.IssuerURL, o.Audience)
	if err != nil {
		return nil, err
	}
	p.identity = id
	client, err := authz.NewClient(authz.Options{URL: o.AuthorizerURL, Token: o.AuthorizerToken, HTTP: o.HTTP, Now: o.Now})
	if err != nil {
		return nil, err
	}
	p.authorizer = client
	p.subjects = ratelimit.New(ratelimit.Config{Idle: 10 * time.Minute, Now: o.Now})
	p.limiter = newLimiter(p.store, o.Defaults, o.Flush, o.Now)
	p.clients = gateway.NewClientSource(gateway.ClientOptions{
		AllowPrivate: o.AllowPrivateUpstreams, Version: o.Version,
		Dial: o.Dial, LookupIPAddr: o.LookupIPAddr, RootCAs: o.RootCAs, Now: o.Now,
	})
	p.doors = gateway.New(gateway.Options{
		Keys: p.store, Catalog: p.store, Credentials: p.store,
		Router:   gateway.NewTargetRouter(gateway.RouterOptions{Catalog: p.store, Health: healthy, Now: o.Now}),
		Limiter:  p.limiter,
		Recorder: &recorder{store: p.store, catalog: p.store},
		Clients:  p.clients, Health: noHealth{}, Version: o.Version, Now: o.Now, Logger: o.Logger,
	})
	p.openapi = openAPIDocument(o.PublicURL.String())
	p.handler = p.mount()
	return p, nil
}

// healthy is this front's health view: it runs no probe, so every
// Provider is healthy and health takes nothing out of target selection.
// A platform that probes its providers answers from what it observed.
func healthy(string) v1.HealthState { return v1.HealthHealthy }

// noHealth takes each attempt's outcome and keeps none.
type noHealth struct{}

func (noHealth) Observe(string, bool) {}

// Handler is the whole front: the four doors, the control plane, and
// the well-known document.
func (p *Plane) Handler() http.Handler { return p.handler }

// Run flushes the spend counters until the context ends.
func (p *Plane) Run(ctx context.Context) { p.limiter.Run(ctx) }

// mount puts the two planes behind one mux: a door per dialect, /v1,
// and the document a client reads first.
func (p *Plane) mount() http.Handler {
	control := p.control()
	mux := http.NewServeMux()
	for _, d := range dialects {
		mux.Handle("/"+d, p.doors)
		mux.Handle("/"+d+"/", p.doors)
	}
	mux.Handle("/v1/", control)
	mux.Handle("/.well-known/lux", control)
	mux.Handle("/", control)
	return mux
}
