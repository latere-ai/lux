// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/store/memory"
	"latere.ai/x/lux/internal/tunnel/wire"
)

// TestForwardingAcrossReplicas: a request landing on the replica without
// the session is forwarded to the holder and served, streaming both
// ways; a holder that fails is a transport failure the door retries;
// and a forward to a replica that does not hold the session is
// provider_unavailable and is not forwarded again.
func TestForwardingAcrossReplicas(t *testing.T) {
	st := memory.New()
	secrets := []string{secretNew}
	holder := newReplica(t, st, replicaOptions{secrets: secrets, forward: true})
	other := newReplica(t, st, replicaOptions{secrets: secrets, forward: true, iss: holder.iss, verifier: holder.verifier})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := holder.mint("alice", time.Hour)
	_, stop := holder.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	defer stop()
	if other.g.Sessions() != 0 {
		t.Fatal("the other replica holds the session")
	}
	// A plain request, forwarded.
	resp, body := send(t, other.g, p, http.MethodPost, "/chat/completions?via=forward", `{"model":"llama3.1"}`, "Content-Type", "application/json")
	if resp.StatusCode != 200 || resp.Header.Get("X-Runtime") != "fake" || !strings.Contains(body, `"echo":{"model":"llama3.1"}`) || !strings.Contains(body, `"query":"via=forward"`) {
		t.Fatalf("forwarded: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if holder.forwards.get() != 1 || other.forwards.get() != 0 {
		t.Fatalf("forward route hits: holder %d, other %d", holder.forwards.get(), other.forwards.get())
	}
	// A stream, forwarded event by event.
	sresp, err := sendErr(t, other.g, p, http.MethodPost, "/chat/completions", `{"stream":true}`)
	if err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(sresp.Body)
	if first, err := rd.ReadString('\n'); err != nil || first != "data: {\"n\":1}\n" {
		t.Fatalf("first forwarded event %q %v", first, err)
	}
	close(rt.gate)
	if rest, err := io.ReadAll(rd); err != nil || !strings.HasSuffix(string(rest), "data: [DONE]\n\n") {
		t.Fatalf("rest %q %v", rest, err)
	}
	_ = sresp.Body.Close()
	// The holder's runtime failing is a transport failure on the forwarder.
	unreachable := tunnelProvider(t, st, "unreachable", nil)
	_, stopU := holder.attach(t, "unreachable", "http://127.0.0.1:9/v1", func() (string, error) { return alice, nil })
	defer stopU()
	if _, err := sendErr(t, other.g, unreachable, http.MethodGet, "/models", ""); err == nil || !strings.Contains(err.Error(), "could not serve request") {
		t.Fatalf("a failing holder: %v", err)
	}
	// One hop: a row naming a replica that does not hold the session is
	// refused there and forwarded nowhere.
	moved := tunnelProvider(t, st, "moved", nil)
	row := store.Tunnel{ProviderID: moved.Status.ID, Session: "tun_moved", Replica: other.g.o.Replica, Subject: "s"}
	if err := st.Tunnels().Register(t.Context(), row, time.Minute); err != nil {
		t.Fatal(err)
	}
	beforeOther, beforeHolder := other.forwards.get(), holder.forwards.get()
	_, err = sendErr(t, holder.g, moved, http.MethodGet, "/models", "")
	if err == nil || !strings.Contains(err.Error(), "answered 503") || !strings.Contains(err.Error(), "provider_unavailable") {
		t.Fatalf("a forward to a replica without the session: %v", err)
	}
	if other.forwards.get() != beforeOther+1 || holder.forwards.get() != beforeHolder {
		t.Fatalf("the refused forward was forwarded again: holder %d, other %d", holder.forwards.get(), other.forwards.get())
	}
	// A row naming this very replica while it holds nothing is stale.
	stale := tunnelProvider(t, st, "stale", nil)
	if err := st.Tunnels().Register(t.Context(), store.Tunnel{ProviderID: stale.Status.ID, Session: "tun_stale", Replica: holder.g.o.Replica}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := sendErr(t, holder.g, stale, http.MethodGet, "/models", ""); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a stale row naming this replica: %v", err)
	}
	// A holder that is gone is a dial failure.
	gone := tunnelProvider(t, st, "gone", nil)
	if err := st.Tunnels().Register(t.Context(), store.Tunnel{ProviderID: gone.Status.ID, Session: "tun_gone", Replica: "127.0.0.1:9"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := sendErr(t, holder.g, gone, http.MethodGet, "/models", ""); err == nil || !strings.Contains(err.Error(), "forward to http://127.0.0.1:9/internal/tunnel/") {
		t.Fatalf("a holder that is gone: %v", err)
	}
}

// TestForwardSecretRotation: with new,old on one replica and old on
// another, forwards succeed in both directions; with new alone against
// old alone they are refused both ways.
func TestForwardSecretRotation(t *testing.T) {
	st := memory.New()
	rt := newRuntime(t)
	rolling := newReplica(t, st, replicaOptions{secrets: []string{secretNew, secretOld}, forward: true})
	old := newReplica(t, st, replicaOptions{secrets: []string{secretOld}, forward: true, iss: rolling.iss, verifier: rolling.verifier})
	alice := rolling.mint("alice", time.Hour)
	token := func() (string, error) { return alice, nil }

	onOld := tunnelProvider(t, st, "on-old", nil)
	_, stopOld := old.attach(t, "on-old", rt.upstream(), token)
	defer stopOld()
	if resp, body := send(t, rolling.g, onOld, http.MethodPost, "/chat/completions", `{"to":"old"}`); resp.StatusCode != 200 || !strings.Contains(body, `"to":"old"`) {
		t.Fatalf("new,old toward old: %d %s", resp.StatusCode, body)
	}
	if old.forwards.get() != 2 {
		t.Errorf("the old replica saw %d forward attempts, want 2: one refused with new, one accepted with old", old.forwards.get())
	}
	if !strings.Contains(rolling.log.String(), "a forward secret was refused; trying the next") {
		t.Errorf("the retry was not logged:\n%s", rolling.log.String())
	}
	onRolling := tunnelProvider(t, st, "on-rolling", nil)
	_, stopRolling := rolling.attach(t, "on-rolling", rt.upstream(), token)
	defer stopRolling()
	if resp, body := send(t, old.g, onRolling, http.MethodPost, "/chat/completions", `{"to":"rolling"}`); resp.StatusCode != 200 || !strings.Contains(body, `"to":"rolling"`) {
		t.Fatalf("old toward new,old: %d %s", resp.StatusCode, body)
	}
	if rolling.forwards.get() != 1 {
		t.Errorf("the rolling replica saw %d forward attempts, want 1", rolling.forwards.get())
	}

	// new alone against old alone: refused both ways, with the code.
	fresh := newReplica(t, st, replicaOptions{secrets: []string{secretNew}, forward: true, iss: rolling.iss, verifier: rolling.verifier})
	_, err := sendErr(t, fresh.g, onOld, http.MethodGet, "/models", "")
	if err == nil || !strings.Contains(err.Error(), "answered 401") || !strings.Contains(err.Error(), "unauthenticated") {
		t.Fatalf("new toward old: %v", err)
	}
	onFresh := tunnelProvider(t, st, "on-fresh", nil)
	_, stopFresh := fresh.attach(t, "on-fresh", rt.upstream(), token)
	defer stopFresh()
	if _, err := sendErr(t, old.g, onFresh, http.MethodGet, "/models", ""); err == nil || !strings.Contains(err.Error(), "answered 401") {
		t.Fatalf("old toward new: %v", err)
	}
	// A replica with no secret cannot forward at all.
	none := newReplica(t, st, replicaOptions{forward: true, iss: rolling.iss, verifier: rolling.verifier})
	if _, err := sendErr(t, none.g, onOld, http.MethodGet, "/models", ""); !errors.Is(err, ErrNoForward) {
		t.Fatalf("no secret: %v", err)
	}
	// A replica with no forward address cannot reach a session elsewhere.
	unaddressed := newReplica(t, st, replicaOptions{iss: rolling.iss, verifier: rolling.verifier})
	if _, err := sendErr(t, unaddressed.g, onOld, http.MethodGet, "/models", ""); !errors.Is(err, ErrNoForward) || !strings.Contains(err.Error(), "LUX_TUNNEL_FORWARD_ADDR is unset") {
		t.Fatalf("no forward address: %v", err)
	}
}

// TestForwardRouteNeedsTheSecret: /internal/tunnel/{id} without the
// secret, with a wrong secret, over HTTP/1.1, with another method, and on
// the public listener are each refused, and a body with no header line
// is invalid_request.
func TestForwardRouteNeedsTheSecret(t *testing.T) {
	st := memory.New()
	r := newReplica(t, st, replicaOptions{secrets: []string{secretNew}, forward: true})
	p := tunnelProvider(t, st, "laptop", nil)
	rt := newRuntime(t)
	alice := r.mint("alice", time.Hour)
	_, stop := r.attach(t, "laptop", rt.upstream(), func() (string, error) { return alice, nil })
	defer stop()
	h2c := newForwardClient()
	target := r.internal.URL + "/internal/tunnel/" + p.Status.ID
	do := func(client *http.Client, method, target, bearer, body string) (*http.Response, string, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == 200 {
			return resp, "", ""
		}
		code, detail := envelopeCode(t, resp)
		return resp, code, detail
	}
	if resp, code, _ := do(h2c, http.MethodPost, target, "", ""); resp.StatusCode != 401 || code != "unauthenticated" {
		t.Errorf("no secret: %d %s", resp.StatusCode, code)
	}
	if resp, code, detail := do(h2c, http.MethodPost, target, secretOld, ""); resp.StatusCode != 401 || code != "unauthenticated" || !strings.Contains(detail, "LUX_TUNNEL_FORWARD_SECRET") {
		t.Errorf("a wrong secret: %d %s %q", resp.StatusCode, code, detail)
	}
	if resp, code, detail := do(&http.Client{}, http.MethodPost, target, secretNew, ""); resp.StatusCode != 404 || code != "not_found" || !strings.Contains(detail, "the tunnel needs HTTP/2") {
		t.Errorf("over HTTP/1.1: %d %s %q", resp.StatusCode, code, detail)
	}
	if resp, code, _ := do(h2c, http.MethodGet, target, secretNew, ""); resp.StatusCode != 404 || code != "not_found" {
		t.Errorf("GET: %d %s", resp.StatusCode, code)
	}
	if resp, code, detail := do(h2c, http.MethodPost, target, secretNew, "not json\n"); resp.StatusCode != 400 || code != "invalid_request" || !strings.Contains(detail, "no header line") {
		t.Errorf("no header line: %d %s %q", resp.StatusCode, code, detail)
	}
	if resp, code, detail := do(h2c, http.MethodPost, r.internal.URL+"/internal/tunnel/prv_nobody", secretNew, ""); resp.StatusCode != 503 || code != "provider_unavailable" || !strings.Contains(detail, "not forwarded again") {
		t.Errorf("no session: %d %s %q", resp.StatusCode, code, detail)
	}
	// The public listener does not carry the route.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.public.URL+"/internal/tunnel/"+p.Status.ID, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+secretNew)
	resp, err := r.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("the forward route on the public listener: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	// With the secret and a header line the route serves, with the
	// carrier's framing on both bodies.
	var body strings.Builder
	_ = wire.WriteLine(&body, wire.Request{ID: "req_fwd", Method: "GET", Path: "/models", Headers: map[string][]string{}})
	_ = wire.NewBodyWriter(&body).Close()
	fresp, _, _ := do(h2c, http.MethodPost, target, secretNew, body.String())
	if fresp.StatusCode != 200 {
		t.Fatalf("a proper forward: %d", fresp.StatusCode)
	}
	rd := bufio.NewReader(fresp.Body)
	var wresp wire.Response
	if err := wire.ReadLine(rd, &wresp); err != nil || wresp.Status != 200 {
		t.Fatalf("the holder's response line: %+v %v", wresp, err)
	}
	if data, err := io.ReadAll(wire.NewBodyReader(rd)); err != nil || !strings.Contains(string(data), `"id":"llama3.1"`) {
		t.Fatalf("the holder's body: %q %v", data, err)
	}
	_ = fresp.Body.Close()
	// A holder whose session ends while the forwarded request waits for a
	// carrier reports the failure in band, after the 200, as the response
	// line's error.
	idle := tunnelProvider(t, st, "idle", nil)
	raw := openRaw(t, r, "idle", alice)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, r.internal.URL+"/internal/tunnel/"+idle.Status.ID, strings.NewReader(body.String()))
	req.Header.Set("Authorization", "Bearer "+secretNew)
	iresp, err := h2c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = iresp.Body.Close() }()
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = raw.send.Close()
	}()
	var inband wire.Response
	if err := wire.ReadLine(bufio.NewReader(iresp.Body), &inband); err != nil || !strings.Contains(inband.Error, "session closed") {
		t.Fatalf("in-band failure: %+v %v", inband, err)
	}
}
