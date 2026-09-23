// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TestTunnelStreamsWithoutBuffering: a streamed response reaches the
// caller event by event. The runtime holds its second event behind a
// gate; the caller reads the first before the gate opens, so nothing
// between them buffered the stream.
func TestTunnelStreamsWithoutBuffering(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	defer stop()
	resp, err := sendErr(t, r.g, p, http.MethodPost, "/chat/completions", `{"model":"llama3.1","stream":true}`, "Accept", "text/event-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.ContentLength != -1 {
		t.Fatalf("stream response: %d %v %d", resp.StatusCode, resp.Header, resp.ContentLength)
	}
	rd := bufio.NewReader(resp.Body)
	first, err := rd.ReadString('\n')
	if err != nil || first != "data: {\"n\":1}\n" {
		t.Fatalf("first event %q %v", first, err)
	}
	close(rt.gate)
	rest, err := io.ReadAll(rd)
	if err != nil || string(rest) != "\ndata: {\"n\":2}\n\ndata: [DONE]\n\n" {
		t.Fatalf("rest %q %v", rest, err)
	}
}

// TestCarrierAuthentication: a carrier with another subject's bearer,
// with an unknown session, with an expired bearer, or for another
// Provider is unauthenticated; one with a newer token of the session's
// subject is accepted and serves.
func TestCarrierAuthentication(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	tunnelProvider(t, st, "other", nil)
	alice := r.mint("alice", time.Hour)
	s := openRaw(t, r, "laptop", alice)
	expired := r.mint("alice", -time.Second)
	for _, tc := range []struct {
		name, token, session, detail string
	}{
		{"another subject's bearer", r.mint("bob", time.Hour), s.Ready.Session, "is not the session's"},
		{"an unknown session", alice, "tun_01J9ZK2P7Q8R9S0T1U2V3W4X64", "no live session"},
		{"an expired bearer", expired, s.Ready.Session, "token expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, resp := s.carry(tc.token, tc.session)
			if c != nil {
				t.Fatal("the carrier was parked")
			}
			code, detail := envelopeCode(t, resp)
			if resp.StatusCode != 401 || code != "unauthenticated" || !strings.Contains(detail, tc.detail) {
				t.Errorf("%d %s %q", resp.StatusCode, code, detail)
			}
		})
	}
	// A carrier naming another Provider with this session is refused too.
	other := &rawSession{t: t, r: r, name: "other"}
	if c, resp := other.carry(alice, s.Ready.Session); c != nil || resp.StatusCode != 401 {
		t.Errorf("a carrier for another Provider: %d", resp.StatusCode)
	}
	// A newer token of the subject parks, and the carrier serves a
	// request end to end.
	c, resp := s.carry(r.mint("alice", 2*time.Hour), s.Ready.Session)
	if c == nil {
		t.Fatalf("a newer token was refused: %d", resp.StatusCode)
	}
	done := make(chan string, 1)
	go func() {
		_, body := send(t, r.g, p, http.MethodPost, "/chat/completions?x=1", `{"hello":"world"}`, "Content-Type", "application/json")
		done <- body
	}()
	var req wire.Request
	if err := wire.ReadLine(c.in, &req); err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.Path != "/chat/completions" || req.Query != "x=1" || req.Headers["Content-Type"][0] != "application/json" || req.Headers["Content-Length"][0] != "17" || !strings.HasPrefix(req.ID, "req_") || req.Headers["User-Agent"][0] != "luxd/test" {
		t.Fatalf("request line %+v", req)
	}
	body, err := io.ReadAll(wire.NewBodyReader(c.in))
	if err != nil || string(body) != `{"hello":"world"}` {
		t.Fatalf("request body %q %v", body, err)
	}
	if err := wire.WriteLine(c.out, wire.Response{Status: 200, Headers: map[string][]string{"content-type": {"application/json"}, "transfer-encoding": {"chunked"}}}); err != nil {
		t.Fatal(err)
	}
	bw := wire.NewBodyWriter(c.out)
	_, _ = io.WriteString(bw, `{"ok":true}`)
	_ = bw.Close()
	select {
	case got := <-done:
		if got != `{"ok":true}` {
			t.Fatalf("the caller read %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the caller did not get the answer")
	}
}

// TestParkedCarrierKeepalive: a carrier parked for three times the TTL
// with no work receives an empty line every ttl/3 and is still given
// work afterwards.
func TestParkedCarrierKeepalive(t *testing.T) {
	st := memory.New()
	ttl := 300 * time.Millisecond
	r := newReplica(t, st, replicaOptions{ttl: ttl})
	p := tunnelProvider(t, st, "laptop", nil)
	alice := r.mint("alice", time.Hour)
	s := openRaw(t, r, "laptop", alice)
	go func() {
		for {
			time.Sleep(ttl / 3)
			if err := wire.WriteLine(s.send, wire.Frame{Type: wire.TypeHeartbeat}); err != nil {
				return
			}
		}
	}()
	c, resp := s.carry(alice, s.Ready.Session)
	if c == nil {
		t.Fatalf("the carrier was refused: %d", resp.StatusCode)
	}
	// One reader hands every line over; count the empty ones for three
	// TTLs, then give the carrier work and read the header line from the
	// same reader.
	type read struct {
		line string
		err  error
	}
	lines := make(chan read, 64)
	go func() {
		for {
			line, err := c.in.ReadString('\n')
			lines <- read{line, err}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.After(3 * ttl)
	empty := 0
counting:
	for {
		select {
		case got := <-lines:
			if got.err != nil {
				t.Fatalf("the parked carrier ended: %v", got.err)
			}
			if got.line != "\n" {
				t.Fatalf("a parked carrier received %q", got.line)
			}
			empty++
		case <-deadline:
			break counting
		}
	}
	// One line at once when parked, then one per ttl/3: at least six over
	// three TTLs even with scheduling slack.
	if empty < 6 {
		t.Fatalf("%d keepalive lines over three TTLs, want at least 6", empty)
	}
	// Work still arrives on it.
	served := make(chan struct{})
	go func() {
		defer close(served)
		send(t, r.g, p, http.MethodGet, "/models", "")
	}()
	var header string
	for header == "" {
		select {
		case got := <-lines:
			if got.err != nil {
				t.Fatalf("the carrier ended before work: %v", got.err)
			}
			header = strings.TrimSpace(got.line)
		case <-time.After(5 * time.Second):
			t.Fatal("no work after the keepalives")
		}
	}
	var req wire.Request
	if err := json.Unmarshal([]byte(header), &req); err != nil || req.Method != "GET" || req.Path != "/models" {
		t.Fatalf("work after the keepalives: %q %v", header, err)
	}
	// The body's terminator follows on the same stream.
	if got := <-lines; got.err != nil || got.line != "0\r\n" {
		t.Fatalf("a GET's body terminator: %q %v", got.line, got.err)
	}
	_ = wire.WriteLine(c.out, wire.Response{Status: 200})
	_ = wire.NewBodyWriter(c.out).Close()
	<-served
}

// TestTunnelCarriesNoCredential: what reaches the runtime is
// the header set the request carried, the User-Agent, the request id,
// and the length, and nothing else: no bearer is added by the tunnel.
func TestTunnelCarriesNoCredential(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	defer stop()
	resp, body := send(t, r.g, p, http.MethodPost, "/chat/completions?k=v", `{"model":"m"}`, "Content-Type", "application/json", "X-Custom", "yes")
	if resp.StatusCode != 200 || resp.Header.Get("X-Runtime") != "fake" || !strings.Contains(body, `"query":"k=v"`) || !strings.Contains(body, `"path":"/v1/chat/completions"`) {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	seen := rt.seen()
	if len(seen) != 1 {
		t.Fatalf("%d requests reached the runtime", len(seen))
	}
	h := seen[0]
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Lux-Tunnel-Session"} {
		if h.Get(name) != "" {
			t.Errorf("the runtime saw %s", name)
		}
	}
	if h.Get("X-Custom") != "yes" || h.Get("Content-Type") != "application/json" || h.Get("User-Agent") != "luxd/test" || !strings.HasPrefix(h.Get("Lux-Request-Id"), "req_") || h.Get("Content-Length") != "13" {
		t.Errorf("the runtime saw %v", h)
	}
	if !strings.Contains(r.log.String(), "session opened") || !strings.Contains(r.log.String(), "agent=lux/test") {
		t.Errorf("the log:\n%s", r.log.String())
	}
}

// TestCallerDisconnectCancelsTheRuntime: a caller that goes away mid
// stream cancels the carrier, which the agent turns into a cancellation
// of its request to the runtime.
func TestCallerDisconnectCancelsTheRuntime(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	defer stop()
	client, err := r.g.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/slow", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 32)
	if n, _ := resp.Body.Read(first); !strings.HasPrefix(string(first[:n]), "data: start") {
		t.Fatalf("first bytes %q", first[:n])
	}
	cancel()
	_ = resp.Body.Close()
	// The runtime's handler ends when its request context is cancelled,
	// which frees the slot the next request needs: with concurrency 1 the
	// next request would otherwise wait for its deadline.
	p.Spec.Concurrency = 1
	resp2, body := send(t, r.g, p, http.MethodPost, "/chat/completions", `{"after":"cancel"}`)
	if resp2.StatusCode != 200 || !strings.Contains(body, `"after":"cancel"`) {
		t.Fatalf("after the cancel: %d %s", resp2.StatusCode, body)
	}
}

// TestRuntimeFailuresAreTheProvidersOwn: a runtime the agent cannot
// reach is a transport failure naming no address of the agent's
// machine, and a runtime's error status passes through as it is.
func TestRuntimeFailuresAreTheProvidersOwn(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	alice := r.mint("alice", time.Hour)
	token := func() (string, error) { return alice, nil }
	unreachable := tunnelProvider(t, st, "unreachable", nil)
	_, stopU := r.attach(t, "unreachable", "http://127.0.0.1:9/v1", token)
	defer stopU()
	_, err := sendErr(t, r.g, unreachable, http.MethodGet, "/models", "")
	if err == nil || !strings.Contains(err.Error(), "the agent could not serve request") || strings.Contains(err.Error(), "127.0.0.1:9") {
		t.Fatalf("an unreachable runtime: %v", err)
	}
	rt := newRuntime(t)
	p := tunnelProvider(t, st, "laptop", nil)
	_, stop := r.attach(t, "laptop", rt.upstream(), token)
	defer stop()
	resp, body := send(t, r.g, p, http.MethodGet, "/status/503", "")
	if resp.StatusCode != 503 || body != `{"error":"as asked"}` || resp.Status != "503 Service Unavailable" {
		t.Fatalf("a 503 from the runtime: %d %q %s", resp.StatusCode, resp.Status, body)
	}
}

// TestNoParkedCarrierWaitsForTheDeadline: a request that finds no
// parked carrier waits until its deadline and fails then, and a session
// that closes while it waits fails it at once.
func TestNoParkedCarrierWaitsForTheDeadline(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", nil)
	alice := r.mint("alice", time.Hour)
	s := openRaw(t, r, "laptop", alice) // a session with no carriers
	client, err := r.g.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/models", nil)
	started := time.Now()
	_, err = client.Do(req)
	if !errors.Is(err, ErrNoCarrier) || time.Since(started) < 250*time.Millisecond {
		t.Fatalf("no carrier: %v after %s", err, time.Since(started))
	}
	// A session that closes while a request waits fails it at once.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = s.send.Close()
	}()
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, "/models", nil)
	started = time.Now()
	if _, err = client.Do(req); !errors.Is(err, ErrSessionClosed) || time.Since(started) > 3*time.Second {
		t.Fatalf("session closed while waiting: %v after %s", err, time.Since(started))
	}
	// With no session at all the failure is immediate.
	if _, err = client.Do(req); !errors.Is(err, ErrNoSession) {
		t.Fatalf("no session: %v", err)
	}
	// A cancelled request never waits.
	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	req, _ = http.NewRequestWithContext(cancelled, http.MethodGet, "/models", nil)
	if _, err = client.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled request: %v", err)
	}
}

// TestConcurrencyBoundsTheCarrierClient: spec.concurrency bounds the
// requests in flight toward a tunneled Provider as toward any other,
// and a wait past the deadline is the busy error.
func TestConcurrencyBoundsTheCarrierClient(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	p := tunnelProvider(t, st, "laptop", func(p *v1.Provider) { p.Spec.Concurrency = 1 })
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	defer stop()
	client, err := r.g.Client(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/slow", nil)
	resp, err := client.Do(held)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	second, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/models", nil)
	if _, err := client.Do(second); !errors.Is(err, gateway.ErrProviderBusy) {
		t.Fatalf("the second request under concurrency 1: %v", err)
	}
	// The client is held while the concurrency stands and rebuilt when it
	// changes.
	again, _ := r.g.Client(t.Context(), p)
	if again != client {
		t.Error("the client was rebuilt without a change")
	}
	p.Spec.Concurrency = 2
	if rebuilt, _ := r.g.Client(t.Context(), p); rebuilt == client {
		t.Error("the client was kept across a concurrency change")
	}
}

// TestCarrierCeiling: past MaxParked carriers the next is refused, so a
// misbehaving agent cannot park without bound.
func TestCarrierCeiling(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{})
	tunnelProvider(t, st, "laptop", nil)
	alice := r.mint("alice", time.Hour)
	s := openRaw(t, r, "laptop", alice)
	for i := range MaxParked {
		if c, resp := s.carry(alice, s.Ready.Session); c == nil {
			t.Fatalf("carrier %d refused: %d", i+1, resp.StatusCode)
		}
	}
	c, resp := s.carry(alice, s.Ready.Session)
	if c != nil {
		t.Fatal("the carrier past the ceiling was parked")
	}
	if code, detail := envelopeCode(t, resp); resp.StatusCode != 503 || code != "provider_unavailable" || !strings.Contains(detail, "64 carriers are already parked") {
		t.Errorf("%d %s %q", resp.StatusCode, code, detail)
	}
}
