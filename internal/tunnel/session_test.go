// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	agent "latere.ai/x/lux/client/tunnel"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestTunnelRequiresHTTP2: a connect that negotiated HTTP/1.1 is
// refused with not_found and the HTTP/2 detail on both routes, and over
// plaintext h2c the same connect succeeds and serves.
func TestTunnelRequiresHTTP2(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{h2c: true})
	p := tunnelProvider(t, st, "laptop", nil)
	token := r.mint("alice", time.Hour)
	// The public listener speaks HTTP/1.1 too, and an HTTP/1.1 client is
	// refused before any frame.
	h1 := &http.Client{}
	for _, path := range []string{"/tunnel", "/tunnel/carry"} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.public.URL+"/v1/providers/laptop"+path, strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(wire.HeaderSession, "tun_none")
		resp, err := h1.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		code, detail := envelopeCode(t, resp)
		if resp.StatusCode != 404 || code != "not_found" || !strings.Contains(detail, "the tunnel needs HTTP/2") || !strings.Contains(detail, "HTTP/1.1") {
			t.Errorf("%s over HTTP/1.1: %d %s %q", path, resp.StatusCode, code, detail)
		}
	}
	if r.g.Sessions() != 0 {
		t.Fatal("an HTTP/1.1 connect opened a session")
	}
	// The agent's plaintext transport negotiates h2c and serves.
	rt := newRuntime(t)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return token, nil })
	defer stop()
	resp, body := send(t, r.g, p, http.MethodPost, "/chat/completions", `{"model":"llama3.1"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"echo":{"model":"llama3.1"}`) {
		t.Fatalf("through h2c: %d %s", resp.StatusCode, body)
	}
}

// TestNewestSessionWins: a second session for one Provider supersedes
// the first, which is closed with superseded, and the second serves;
// on one replica at once, across replicas within one heartbeat.
func TestNewestSessionWins(t *testing.T) {
	st := memory.New()
	rt := newRuntime(t)
	token := func() (string, error) { return "", nil }
	t.Run("on one replica", func(t *testing.T) {
		r := newReplica(t, st, replicaOptions{ttl: 300 * time.Millisecond})
		p := tunnelProvider(t, st, "laptop-one", nil)
		alice := r.mint("alice", time.Hour)
		token = func() (string, error) { return alice, nil }
		first, stopFirst := r.attach(t, "laptop-one", rt.upstream(), token)
		defer stopFirst()
		row1, err := st.Tunnels().Get(t.Context(), p.Status.ID)
		if err != nil {
			t.Fatal(err)
		}
		second, stopSecond := r.attach(t, "laptop-one", rt.upstream(), token)
		defer stopSecond()
		select {
		case err := <-first:
			if closeReason(err) != wire.ReasonSuperseded {
				t.Fatalf("the first session ended with %v, want superseded", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the first session was not closed")
		}
		row2, err := st.Tunnels().Get(t.Context(), p.Status.ID)
		if err != nil || row2.Session == row1.Session {
			t.Fatalf("the registry row after the second connect: %+v %v", row2, err)
		}
		if r.g.Sessions() != 1 {
			t.Fatalf("%d sessions held, want 1", r.g.Sessions())
		}
		if resp, body := send(t, r.g, p, http.MethodPost, "/chat/completions", `{"n":2}`); resp.StatusCode != 200 || !strings.Contains(body, `"echo":{"n":2}`) {
			t.Fatalf("the second session serves: %d %s", resp.StatusCode, body)
		}
		select {
		case err := <-second:
			t.Fatalf("the second session ended: %v", err)
		default:
		}
	})
	t.Run("across replicas", func(t *testing.T) {
		a := newReplica(t, st, replicaOptions{ttl: 300 * time.Millisecond})
		b := newReplica(t, st, replicaOptions{ttl: 300 * time.Millisecond, iss: a.iss, verifier: a.verifier})
		p := tunnelProvider(t, st, "laptop-two", nil)
		alice := a.mint("alice", time.Hour)
		token = func() (string, error) { return alice, nil }
		first, stopFirst := a.attach(t, "laptop-two", rt.upstream(), token)
		defer stopFirst()
		_, stopSecond := b.attach(t, "laptop-two", rt.upstream(), token)
		defer stopSecond()
		select {
		case err := <-first:
			if closeReason(err) != wire.ReasonSuperseded {
				t.Fatalf("the first session ended with %v, want superseded", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the superseded session on the other replica was not closed within one heartbeat")
		}
		waitFor(t, func() bool { return a.g.Sessions() == 0 }, "replica A to forget the session")
		if resp, body := send(t, b.g, p, http.MethodPost, "/chat/completions", `{"n":3}`); resp.StatusCode != 200 || !strings.Contains(body, `"echo":{"n":3}`) {
			t.Fatalf("the second session serves: %d %s", resp.StatusCode, body)
		}
	})
}

// TestSessionEndsWithTheToken: a session whose bearer expires with no
// fresh token is closed with token_expired at the exp, and its
// in-flight carriers are cancelled.
func TestSessionEndsWithTheToken(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{ttl: 300 * time.Millisecond})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	short := r.mint("alice", 1500*time.Millisecond)
	result, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return short, nil })
	defer stop()
	// A stream in flight when the token expires.
	resp, err := sendErr(t, r.g, p, http.MethodGet, "/slow", "")
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 64)
	n, _ := resp.Body.Read(first)
	if !strings.HasPrefix(string(first[:n]), "data: start") {
		t.Fatalf("the stream's first bytes: %q", first[:n])
	}
	started := time.Now()
	select {
	case err := <-result:
		if closeReason(err) != wire.ReasonTokenExpired {
			t.Fatalf("the session ended with %v, want token_expired", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("the session outlived its token")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("the close took %s after the stream started", elapsed)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("the in-flight stream finished cleanly after the session closed")
	}
	_ = resp.Body.Close()
	if _, err := st.Tunnels().Get(t.Context(), p.Status.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the registry row after token_expired: %v", err)
	}
	if !strings.Contains(r.log.String(), "reason=token_expired") {
		t.Errorf("the log does not name the reason:\n%s", r.log.String())
	}
}

// TestSessionTokenRefresh: a heartbeat carrying a fresh bearer of the
// session's subject moves the expiry to the new exp and the session
// serves past the old one; a bearer of another subject or one that does
// not verify is ignored and the old expiry stands.
func TestSessionTokenRefresh(t *testing.T) {
	st := memory.New()
	rt := newRuntime(t)
	for _, tc := range []struct {
		name    string
		fresh   func(r *replica) string
		refresh bool // the session serves past the first token's exp
	}{
		{"a fresh token of the subject", func(r *replica) string { return r.mint("alice", time.Minute) }, true},
		{"another subject's token", func(r *replica) string { return r.mint("bob", time.Minute) }, false},
		{"a token that does not verify", func(r *replica) string { return "not.a.jws" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReplica(t, st, replicaOptions{ttl: 300 * time.Millisecond})
			p := tunnelProvider(t, st, "laptop-"+strings.Fields(tc.name)[0]+strings.Fields(tc.name)[1], nil)
			first := r.mint("alice", 2*time.Second)
			fresh := tc.fresh(r)
			var current = first
			var switched time.Time
			result, stop := r.attach(t, p.Metadata.Name, rt.upstream(), func() (string, error) {
				if switched.IsZero() {
					switched = time.Now()
					return first, nil
				}
				return fresh, nil
			})
			defer stop()
			_ = current
			// The agent sends the fresh token on the first heartbeat after
			// its source yields it; wait past the first token's exp.
			select {
			case err := <-result:
				if tc.refresh {
					t.Fatalf("the session ended with %v", err)
				}
				if closeReason(err) != wire.ReasonTokenExpired {
					t.Fatalf("the session ended with %v, want token_expired", err)
				}
				if !strings.Contains(r.log.String(), "a heartbeat's token was ignored") {
					t.Errorf("the ignored token was not logged:\n%s", r.log.String())
				}
				return
			case <-time.After(3 * time.Second):
				if !tc.refresh {
					t.Fatal("the session outlived the first token although the fresh one was not the subject's")
				}
			}
			if resp, body := send(t, r.g, p, http.MethodPost, "/chat/completions", `{"n":9}`); resp.StatusCode != 200 || !strings.Contains(body, `"echo":{"n":9}`) {
				t.Fatalf("serving past the first exp: %d %s", resp.StatusCode, body)
			}
			if !strings.Contains(r.log.String(), "the session's token was refreshed") {
				t.Errorf("the refresh was not logged:\n%s", r.log.String())
			}
		})
	}
}

// TestCleanDisconnectIsImmediate: a clean agent stop unregisters at
// once, so the Provider has no live row long before the TTL lapses.
func TestCleanDisconnectIsImmediate(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	result, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	if _, err := st.Tunnels().Get(t.Context(), p.Status.ID); err != nil {
		t.Fatalf("no row while attached: %v", err)
	}
	stop()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("a clean stop returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent did not stop")
	}
	waitFor(t, func() bool {
		_, err := st.Tunnels().Get(t.Context(), p.Status.ID)
		return errors.Is(err, store.ErrNotFound)
	}, "the registry row to go")
	if r.g.Sessions() != 0 {
		t.Errorf("%d sessions held after the stop", r.g.Sessions())
	}
}

// TestDrainAndRevoke: Drain closes every session with draining, and a
// deleted Provider's session is closed with provider_deleted while spec
// 005's clients are told.
func TestDrainAndRevoke(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	token := func() (string, error) { return alice, nil }
	p1 := tunnelProvider(t, st, "laptop-one", nil)
	p2 := tunnelProvider(t, st, "laptop-two", nil)
	one, stopOne := r.attach(t, "laptop-one", rt.upstream(), token)
	defer stopOne()
	two, stopTwo := r.attach(t, "laptop-two", rt.upstream(), token)
	defer stopTwo()
	if _, err := r.g.Client(t.Context(), p1); err != nil {
		t.Fatal(err)
	}
	r.g.Revoke(p1.Status.ID)
	select {
	case err := <-one:
		if closeReason(err) != wire.ReasonProviderDeleted {
			t.Fatalf("the revoked session ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the revoked session was not closed")
	}
	if _, ok := r.g.clients[p1.Status.ID]; ok {
		t.Error("the revoked Provider's client was kept")
	}
	r.g.Drain(t.Context())
	select {
	case err := <-two:
		if closeReason(err) != wire.ReasonDraining {
			t.Fatalf("the drained session ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the drained session was not closed")
	}
	if r.g.Sessions() != 0 {
		t.Errorf("%d sessions held after the drain", r.g.Sessions())
	}
	for _, p := range []*v1.Provider{p1, p2} {
		if _, err := st.Tunnels().Get(t.Context(), p.Status.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a row survived: %v", err)
		}
	}
}

// TestMissingHeartbeatsEndTheSession: an agent that stops heartbeating
// is dropped after one TTL even though its stream is open, and a stale
// row is re-registered rather than mistaken for a supersede.
func TestMissingHeartbeatsEndTheSession(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{ttl: 300 * time.Millisecond})
	p := tunnelProvider(t, st, "laptop", nil)
	s := openRaw(t, r, "laptop", r.mint("alice", time.Hour))
	if s.Ready.TTL != "300ms" || s.Ready.Carriers != DefaultCarriers || !strings.HasPrefix(s.Ready.Session, v1.PrefixTunnel) {
		t.Fatalf("ready frame %+v", s.Ready)
	}
	// One heartbeat is acknowledged.
	s.heartbeat("")
	if f, err := s.next(time.Second); err != nil || f.Type != wire.TypeHeartbeat {
		t.Fatalf("the acknowledgement: %+v %v", f, err)
	}
	// The row lapsed and was removed behind the session's back; the next
	// heartbeat registers it again instead of closing as superseded.
	if err := st.Tunnels().Unregister(t.Context(), p.Status.ID, s.Ready.Session); err != nil {
		t.Fatal(err)
	}
	s.heartbeat("")
	if f, err := s.next(time.Second); err != nil || f.Type != wire.TypeHeartbeat {
		t.Fatalf("the acknowledgement after re-registering: %+v %v", f, err)
	}
	if row, err := st.Tunnels().Get(t.Context(), p.Status.ID); err != nil || row.Session != s.Ready.Session {
		t.Fatalf("the row was not re-registered: %+v %v", row, err)
	}
	// Then silence: the stream ends without a close frame within a TTL
	// or so, and the row goes.
	if _, err := s.next(2 * time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("the silent session ended with %v, want EOF", err)
	}
	if _, err := st.Tunnels().Get(t.Context(), p.Status.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the row after the silence: %v", err)
	}
	if !strings.Contains(r.log.String(), "no heartbeat within the TTL") {
		t.Errorf("the silence was not logged:\n%s", r.log.String())
	}
}

// TestSessionRefusals: what ServeSession refuses before the response is
// committed, and what the ready frame carries.
func TestSessionRefusals(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	tunnelProvider(t, st, "laptop", nil)
	// An expired bearer's exp is refused as unauthenticated even when a
	// verifier let it through.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	req.ProtoMajor = 2
	if e := r.g.ServeSession(nil, req, SessionRequest{Provider: &v1.Provider{Status: v1.ProviderStatus{ID: "prv_x"}}, ExpiresAt: time.Now().Add(-time.Second)}); e == nil || e.Code != CodeUnauthenticated {
		t.Errorf("an expired exp: %v", e)
	}
	if e := r.g.ServeSession(nil, req, SessionRequest{ExpiresAt: time.Now().Add(time.Hour)}); e == nil || e.Code != CodeNotFound {
		t.Errorf("no Provider: %v", e)
	}
	// A store that cannot register is store_unavailable.
	g := New(Options{Store: failingStore{st}, Verifier: r.verifier, Clients: r.g.o.Clients})
	e := g.ServeSession(nil, req, SessionRequest{Provider: &v1.Provider{Status: v1.ProviderStatus{ID: "prv_x"}}, ExpiresAt: time.Now().Add(time.Hour)})
	if e == nil || e.Code != CodeStoreUnavailable || !strings.Contains(e.Error(), "store_unavailable: registering session") || !strings.Contains(e.Error(), "the database is away") {
		t.Errorf("a failing store: %v", e)
	}
	// Every code has a status and a sentence.
	for _, c := range Codes() {
		if c.Status() == 0 || c.Message() == "" {
			t.Errorf("code %s has status %d and message %q", c, c.Status(), c.Message())
		}
	}
	if (&Error{Code: CodeNotFound}).Error() != "not_found" {
		t.Error("an Error without a detail renders more than its code")
	}
	// New refuses to build without its seams.
	for name, o := range map[string]Options{
		"store":    {Verifier: r.verifier, Clients: r.g.o.Clients},
		"verifier": {Store: st, Clients: r.g.o.Clients},
		"clients":  {Store: st, Verifier: r.verifier},
	} {
		func() {
			defer func() {
				if p := recover(); p == nil || !strings.Contains(strings.ToLower(p.(string)), name) {
					t.Errorf("New without %s: %v", name, p)
				}
			}()
			New(o)
		}()
	}
	// An agent whose bearer the gateway refuses is told so, with the
	// envelope's code.
	err := agent.Run(t.Context(), agent.Options{Gateway: r.public.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1/v1", Token: func() (string, error) { return "bad", nil }, Client: r.client()})
	var refused *agent.RefusedError
	if !errors.As(err, &refused) || refused.Status != 401 || refused.Code != "unauthenticated" || !strings.Contains(refused.Error(), "401 unauthenticated") {
		t.Errorf("a refused connect: %v", err)
	}
	err = agent.Run(t.Context(), agent.Options{Gateway: r.public.URL, Provider: "nobody", Upstream: "http://127.0.0.1:1/v1", Token: func() (string, error) { return r.mint("alice", time.Hour), nil }, Client: r.client()})
	if !errors.As(err, &refused) || refused.Code != "not_found" {
		t.Errorf("a connect to no Provider: %v", err)
	}
	// A context that ended before the connect is a stop that landed
	// early: a clean stop, nil, and no session anywhere.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := r.g.Sessions()
	if err := agent.Run(ctx, agent.Options{Gateway: r.public.URL, Provider: "laptop", Upstream: "http://127.0.0.1:1/v1", Token: func() (string, error) { return r.mint("alice", time.Hour), nil }, Client: r.client()}); err != nil {
		t.Errorf("a connect under an ended context returned %v, want the clean nil", err)
	}
	if r.g.Sessions() != before {
		t.Errorf("a connect under an ended context left a session held")
	}
}
