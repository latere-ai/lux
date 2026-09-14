// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// SessionRequest is what internal/api hands ServeSession once the bearer
// is verified and provider.tunnel allowed: the Provider it loaded, the
// caller, and the bearer's expiry, which is the session's.
type SessionRequest struct {
	Provider  *v1.Provider
	Subject   string
	ExpiresAt time.Time
	Agent     string // the agent's User-Agent
	RequestID string
}

// session is one accepted session: the Provider it serves, the subject
// that opened it, the carriers parked for it, and its expiry, which a
// heartbeat's fresh token moves.
type session struct {
	g        *Gateway
	id       string
	provider *v1.Provider
	subject  string
	agent    string

	parked chan *carrier   // the carriers waiting for work
	outbox chan wire.Frame // the frames the writer sends the agent
	done   chan struct{}   // closed when the session ends
	ctx    context.Context // ends with the session
	cancel context.CancelFunc

	mu        sync.Mutex
	expiresAt time.Time // the bearer's exp
	lastBeat  time.Time // the connect, then every heartbeat
	closed    bool
	reason    string // the close frame's reason, "" for a stream that ended
}

// ServeSession is POST /v1/providers/{id-or-name}/tunnel after
// internal/api authenticated and authorized it: the HTTP/2 check, the
// registry row, the ready frame, then the session's life, which ends
// when the agent's stream ends, the bearer expires with no fresh token,
// another session supersedes this one, the Provider is deleted, or the
// replica drains. The returned refusal is written by the caller and is
// never nil after the response is committed.
func (g *Gateway) ServeSession(w http.ResponseWriter, r *http.Request, sr SessionRequest) *Error {
	if r.ProtoMajor < 2 {
		return refuse(CodeNotFound, "the tunnel needs HTTP/2; this connect negotiated "+r.Proto)
	}
	p := sr.Provider
	if p == nil || p.Status.ID == "" {
		return refuse(CodeNotFound, "the session names no Provider")
	}
	now := g.now()
	if !sr.ExpiresAt.After(now) {
		return refuse(CodeUnauthenticated, "the bearer's exp "+sr.ExpiresAt.UTC().Format(time.RFC3339)+" has passed")
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	s := &session{
		g: g, id: g.o.NewID(v1.PrefixTunnel), provider: p, subject: sr.Subject, agent: sr.Agent,
		parked: make(chan *carrier, MaxParked), outbox: make(chan wire.Frame, 16), done: make(chan struct{}),
		ctx: ctx, cancel: cancel, expiresAt: sr.ExpiresAt, lastBeat: now,
	}
	row := store.Tunnel{ProviderID: p.Status.ID, Session: s.id, Replica: g.o.Replica, Subject: sr.Subject, Agent: sr.Agent, ConnectedAt: now}
	if err := g.o.Store.Tunnels().Register(r.Context(), row, g.ttl); err != nil {
		cancel()
		return refuse(CodeStoreUnavailable, "registering session "+s.id+" of Provider "+p.Status.ID+": "+err.Error())
	}
	g.attach(ctx, s)
	defer s.close(ctx, "")
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	out := wire.Flushing(w)
	if err := wire.WriteLine(out, wire.Frame{Type: wire.TypeReady, Session: s.id, TTL: g.ttl.String(), Carriers: g.carriers}); err != nil {
		return nil
	}
	g.logger.InfoContext(ctx, "tunnel: session opened", "session", s.id, "provider", p.Status.ID, "name", p.Metadata.Name, "subject", sr.Subject, "agent", sr.Agent, "request_id", sr.RequestID, "expires_at", sr.ExpiresAt.UTC())
	if g.o.OnConnect != nil {
		go g.o.OnConnect(ctx, p)
	}
	go s.readFrames(ctx, r.Body)
	s.run(r.Context(), out)
	return nil
}

// run is the session's writer and its clock: it sends the frames the
// reader queues, checks the expiry and the heartbeat's freshness every
// third of the TTL, and writes the close frame when the session ends.
func (s *session) run(ctx context.Context, out io.Writer) {
	tick := time.NewTicker(s.g.ttl / 3)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			if reason := s.closeReason(); reason != "" {
				_ = wire.WriteLine(out, wire.Frame{Type: wire.TypeClose, Reason: reason})
			}
			return
		case f := <-s.outbox:
			if err := wire.WriteLine(out, f); err != nil {
				s.close(ctx, "")
				return
			}
		case <-tick.C:
			now := s.g.now()
			s.mu.Lock()
			expiresAt, lastBeat := s.expiresAt, s.lastBeat
			s.mu.Unlock()
			switch {
			case !now.Before(expiresAt):
				s.close(ctx, wire.ReasonTokenExpired)
			case now.Sub(lastBeat) > s.g.ttl:
				// The agent stopped heartbeating: its stream is dead
				// whatever the socket says, and the row lapses with it.
				s.g.logger.WarnContext(ctx, "tunnel: no heartbeat within the TTL", "session", s.id, "provider", s.provider.Status.ID, "last_heartbeat_at", lastBeat.UTC(), "ttl", s.g.ttl)
				s.close(ctx, "")
			}
		case <-ctx.Done():
			s.close(ctx, "")
			return
		}
	}
}

// readFrames reads the agent's frames until its stream ends, which
// ends the session: a clean stop and a broken network look the same
// from here, and the registry row goes at once either way.
func (s *session) readFrames(ctx context.Context, body io.Reader) {
	rd := bufio.NewReader(body)
	for {
		var f wire.Frame
		if err := wire.ReadLine(rd, &f); err != nil {
			if !errors.Is(err, io.EOF) {
				s.g.logger.InfoContext(ctx, "tunnel: the agent's stream ended", "session", s.id, "provider", s.provider.Status.ID, "err", err)
			}
			s.close(ctx, "")
			return
		}
		if f.Type != wire.TypeHeartbeat {
			s.g.logger.WarnContext(ctx, "tunnel: a frame the session does not take was ignored", "session", s.id, "type", f.Type)
			continue
		}
		s.beat(ctx, f.Token)
	}
}

// beat is one heartbeat: a fresh token, when carried, verified exactly as
// at connect and made the session's expiry when it names the session's
// subject; the registry row renewed; superseded when another session
// took the row; and the acknowledgement queued.
func (s *session) beat(ctx context.Context, token string) {
	g := s.g
	now := g.now()
	if token != "" {
		c, err := g.o.Verifier.Verify(token)
		switch {
		case err != nil:
			g.logger.WarnContext(ctx, "tunnel: a heartbeat's token was ignored", "session", s.id, "provider", s.provider.Status.ID, "err", err)
		case c.Subject != s.subject:
			g.logger.WarnContext(ctx, "tunnel: a heartbeat's token was ignored", "session", s.id, "provider", s.provider.Status.ID, "err", "the token names subject "+c.Subject+", not the session's "+s.subject)
		default:
			if exp := expiryOf(c); exp.After(now) {
				s.mu.Lock()
				s.expiresAt = exp
				s.mu.Unlock()
				g.logger.InfoContext(ctx, "tunnel: the session's token was refreshed", "session", s.id, "provider", s.provider.Status.ID, "expires_at", exp.UTC())
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	held, err := g.o.Store.Tunnels().Heartbeat(ctx, s.provider.Status.ID, s.id, g.ttl)
	switch {
	case err != nil:
		g.logger.ErrorContext(ctx, "tunnel: renewing the registry row", "session", s.id, "provider", s.provider.Status.ID, "err", err)
	case !held:
		// Another session took the row, or the row is gone. Gone is
		// re-registered, because the session is alive and the row is
		// what says so; replaced is superseded.
		if _, gerr := g.o.Store.Tunnels().Get(ctx, s.provider.Status.ID); !errors.Is(gerr, store.ErrNotFound) {
			s.close(ctx, wire.ReasonSuperseded)
			return
		}
		row := store.Tunnel{ProviderID: s.provider.Status.ID, Session: s.id, Replica: g.o.Replica, Subject: s.subject, Agent: s.agent}
		if rerr := g.o.Store.Tunnels().Register(ctx, row, g.ttl); rerr != nil {
			g.logger.ErrorContext(ctx, "tunnel: re-registering a row that lapsed", "session", s.id, "provider", s.provider.Status.ID, "err", rerr)
		}
	}
	s.mu.Lock()
	s.lastBeat = now
	s.mu.Unlock()
	select {
	case s.outbox <- wire.Frame{Type: wire.TypeHeartbeat}:
	default:
		// A full outbox drops the acknowledgement; the next one answers.
	}
}

// expiryOf is the exp of a verified bearer.
func expiryOf(c auth.Caller) time.Time {
	switch exp := c.Claims["exp"].(type) {
	case float64:
		return time.Unix(int64(exp), 0)
	case int64:
		return time.Unix(exp, 0)
	}
	return time.Time{}
}

// close ends the session once: the reason is kept for the close frame,
// every in-flight carrier is cancelled, the session leaves the map, and
// the registry row goes at once, so a clean disconnect is visible
// immediately rather than at the TTL. The row's removal runs on ctx's
// values with its cancellation lifted, because the caller's context may
// be the very stream that just ended.
func (s *session) close(ctx context.Context, reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.reason = reason
	s.mu.Unlock()
	s.g.detach(s)
	close(s.done)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.g.o.Store.Tunnels().Unregister(ctx, s.provider.Status.ID, s.id); err != nil {
		s.g.logger.ErrorContext(ctx, "tunnel: unregistering the session", "session", s.id, "provider", s.provider.Status.ID, "err", err)
	}
	s.cancel()
	s.g.logger.InfoContext(ctx, "tunnel: session closed", "session", s.id, "provider", s.provider.Status.ID, "reason", reason)
}

// closeReason is the reason the session closed with.
func (s *session) closeReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}
