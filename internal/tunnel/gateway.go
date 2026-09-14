// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/semaphore"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// The fixed values of the design.
const (
	// MetricSessions is the gauge of spec 019: the sessions this replica
	// holds.
	MetricSessions = "lux_tunnel_sessions"
	// DefaultTTL is LUX_TUNNEL_REGISTRY_TTL's default.
	DefaultTTL = 30 * time.Second
	// DefaultCarriers is how many carriers the ready frame asks for.
	DefaultCarriers = 4
	// MaxParked is the ceiling of carriers one session parks.
	MaxParked = 64
)

// Verifier checks a bearer a heartbeat carries as a fresh token, exactly
// as the connect's bearer was checked: *auth.Verifier.
type Verifier interface {
	Verify(token string) (auth.Caller, error)
}

// Clients is spec 005's client per Provider, for every Provider that is
// not tunnelled: *gateway.Clients.
type Clients interface {
	gateway.ClientSource
	Revoke(providerID string)
}

// Options is what New builds the Gateway from. Store, Verifier, and
// Clients are required.
type Options struct {
	// Store holds the registry, Tunnels.
	Store store.Store
	// Verifier checks the fresh bearer of a heartbeat.
	Verifier Verifier
	// Clients answers every Provider that is not tunnelled.
	Clients Clients
	// Replica is LUX_TUNNEL_FORWARD_ADDR: the address other replicas
	// reach this one's internal listener at, written into every
	// registry row this replica holds. Empty is no forwarding.
	Replica string
	// Secrets is LUX_TUNNEL_FORWARD_SECRET: the first is sent, every one
	// is accepted.
	Secrets []string
	// TTL is LUX_TUNNEL_REGISTRY_TTL; zero is DefaultTTL.
	TTL time.Duration
	// Carriers is what the ready frame asks the agent to park; zero is
	// DefaultCarriers.
	Carriers int
	// Version is the build's, written as the User-Agent luxd/<Version>
	// toward a runtime.
	Version string
	// Metrics receives MetricSessions; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints a prefixed ULID; nil uses v1.NewID.
	NewID func(prefix string) string
	// OnConnect runs in its own goroutine once a session is ready, with
	// a context that ends with the session: the wiring lists the
	// Provider's models through it, which is discovery once at connect.
	OnConnect func(ctx context.Context, p *v1.Provider)
	// ForwardClient sends a forwarded request to another replica; nil
	// builds an unencrypted HTTP/2 client, which is what an internal
	// listener speaks.
	ForwardClient *http.Client
}

// Gateway is this replica's side of every tunnel.
type Gateway struct {
	o        Options
	ttl      time.Duration
	carriers int
	logger   *slog.Logger
	forward  *http.Client

	mu       sync.Mutex
	sessions map[string]*session // by provider id
	clients  map[string]*clientEntry
}

// clientEntry is one carrier-backed client and what it was built for.
type clientEntry struct {
	concurrency int
	client      *http.Client
}

// New builds the Gateway and registers its gauge. A nil Store,
// Verifier, or Clients is a panic, because a tunnel that cannot
// register, verify, or delegate should not start.
func New(o Options) *Gateway {
	switch {
	case o.Store == nil:
		panic("tunnel.New: Options.Store is nil")
	case o.Verifier == nil:
		panic("tunnel.New: Options.Verifier is nil")
	case o.Clients == nil:
		panic("tunnel.New: Options.Clients is nil")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = func(prefix string) string { return v1.NewID(prefix, o.Now(), nil) }
	}
	g := &Gateway{o: o, ttl: o.TTL, carriers: o.Carriers, logger: o.Logger, forward: o.ForwardClient, sessions: map[string]*session{}, clients: map[string]*clientEntry{}}
	if g.ttl <= 0 {
		g.ttl = DefaultTTL
	}
	if g.carriers <= 0 {
		g.carriers = DefaultCarriers
	}
	if g.forward == nil {
		g.forward = newForwardClient()
	}
	if o.Metrics != nil {
		o.Metrics.Gauge(MetricSessions, sessionsHelp, func() []metrics.LabeledValue {
			return []metrics.LabeledValue{{Labels: map[string]string{}, Value: float64(g.Sessions())}}
		})
	}
	return g
}

// sessionsHelp is MetricSessions's help text, one string for the
// Gateway and the idle registration.
const sessionsHelp = "Tunnel sessions this replica holds."

// RegisterIdle registers MetricSessions reading zero, for a process
// with the tunnel off, so the registry carries every metric of spec
// 019's table whether or not the tunnel is on. A process with a
// Gateway must not call it, since New registers the gauge itself.
func RegisterIdle(reg *metrics.Registry) {
	reg.Gauge(MetricSessions, sessionsHelp, func() []metrics.LabeledValue {
		return []metrics.LabeledValue{{Labels: map[string]string{}, Value: 0}}
	})
}

// TTL is the registry's liveness window.
func (g *Gateway) TTL() time.Duration { return g.ttl }

func (g *Gateway) now() time.Time { return g.o.Now() }

// Sessions is how many sessions this replica holds.
func (g *Gateway) Sessions() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sessions)
}

// session is the live session of a Provider by id, nil when this replica
// holds none.
func (g *Gateway) session(providerID string) *session {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessions[providerID]
}

// sessionByRef is the live session of a Provider by id or by name.
func (g *Gateway) sessionByRef(ref string) *session {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.sessions[ref]; ok {
		return s
	}
	for _, s := range g.sessions {
		if s.provider.Metadata.Name == ref {
			return s
		}
	}
	return nil
}

// attach makes s the Provider's session on this replica and closes the
// one it replaces with superseded: the newest session wins.
func (g *Gateway) attach(ctx context.Context, s *session) {
	g.mu.Lock()
	old := g.sessions[s.provider.Status.ID]
	g.sessions[s.provider.Status.ID] = s
	g.mu.Unlock()
	if old != nil {
		old.close(ctx, wire.ReasonSuperseded)
	}
}

// detach forgets s when it is still the Provider's session.
func (g *Gateway) detach(s *session) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions[s.provider.Status.ID] == s {
		delete(g.sessions, s.provider.Status.ID)
	}
}

// Drain closes every session with draining, so each agent reconnects
// at once and lands on another replica. The serve role calls it when
// the stop signal fires, before the listeners close, on a context that
// outlives the signal.
func (g *Gateway) Drain(ctx context.Context) {
	g.mu.Lock()
	all := make([]*session, 0, len(g.sessions))
	for _, s := range g.sessions {
		all = append(all, s)
	}
	g.mu.Unlock()
	for _, s := range all {
		s.close(ctx, wire.ReasonDraining)
	}
}

// Revoke is a deleted Provider: its session, if this replica holds it,
// is closed with provider_deleted, its carrier-backed client is
// forgotten, and spec 005's clients are told. It satisfies
// internal/api's ClientRevoker in place of *gateway.Clients.
func (g *Gateway) Revoke(providerID string) {
	if s := g.session(providerID); s != nil {
		s.close(context.Background(), wire.ReasonProviderDeleted)
	}
	g.mu.Lock()
	delete(g.clients, providerID)
	g.mu.Unlock()
	g.o.Clients.Revoke(providerID)
}

// Client is spec 004's ClientSource over both kinds of Provider: a
// tunnel: true Provider gets the client whose transport writes onto a
// carrier, held while its concurrency is what it was built for; every
// other Provider is spec 005's clients' answer.
func (g *Gateway) Client(ctx context.Context, p *v1.Provider) (*http.Client, error) {
	if p == nil {
		return nil, errors.New("tunnel: Client of a nil Provider")
	}
	if !p.Spec.Tunnel {
		return g.o.Clients.Client(ctx, p)
	}
	if p.Status.ID == "" {
		return nil, errors.New("tunnel: Client of a tunnelled Provider with no id")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.clients[p.Status.ID]; ok && e.concurrency == p.Spec.Concurrency {
		return e.client, nil
	}
	t := &transport{g: g, providerID: p.Status.ID, name: p.Metadata.Name, sem: semaphore.New(p.Spec.Concurrency), userAgent: "luxd/" + g.o.Version}
	if g.o.Version == "" {
		t.userAgent = "luxd"
	}
	client := &http.Client{
		Transport: t,
		// A 3xx is the runtime asking for another location; it is
		// returned as it is, as for any Provider.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	g.clients[p.Status.ID] = &clientEntry{concurrency: p.Spec.Concurrency, client: client}
	return client, nil
}

// acceptsSecret reports whether token is an entry of the forward
// secrets, compared in constant time each.
func (g *Gateway) acceptsSecret(token string) bool {
	ok := false
	for _, s := range g.o.Secrets {
		if subtle.ConstantTimeCompare([]byte(s), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

// The seams: the doors and the jobs take the Gateway through spec 004's
// interface, and the API tells it of a deleted Provider.
var _ gateway.ClientSource = (*Gateway)(nil)
