// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authkit/issuertest"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/serve"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/internal/tunnel"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// fakeTunnel records what the routes hand it and answers with the
// refusal the test sets, or, without one, commits a 200 and nothing
// else.
type fakeTunnel struct {
	mu       sync.Mutex
	sessions []tunnel.SessionRequest
	carriers []tunnel.CarrierRequest
	refuse   *tunnel.Error
}

func (f *fakeTunnel) ServeSession(w http.ResponseWriter, _ *http.Request, s tunnel.SessionRequest) *tunnel.Error {
	f.mu.Lock()
	f.sessions = append(f.sessions, s)
	refuse := f.refuse
	f.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (f *fakeTunnel) ServeCarrier(w http.ResponseWriter, _ *http.Request, c tunnel.CarrierRequest) *tunnel.Error {
	f.mu.Lock()
	f.carriers = append(f.carriers, c)
	refuse := f.refuse
	f.mu.Unlock()
	if refuse != nil {
		return refuse
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// tunnelProviderJSON is a tunnel: true Provider manifest: no baseURL, no
// credential.
const tunnelProviderJSON = `{"spec": {"dialect": "openai", "tunnel": true}}`

// withTunnel turns the tunnel on with the fake behind the routes.
func withTunnel(f *fakeTunnel) func(*Options) {
	return func(o *Options) {
		o.TunnelEnabled = true
		o.Tunnel = f
	}
}

// TestTunnelRoutesAreNotFoundWhenOff: with LUX_TUNNEL_ENABLED unset the
// session route and the carrier route are not_found like any path
// outside the table, whatever the bearer, and a tunnel: true Provider is
// invalid_field at spec.tunnel.
func TestTunnelRoutesAreNotFoundWhenOff(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/v1/providers/laptop/tunnel", "/v1/providers/laptop/tunnel/carry"} {
		rec := h.request(http.MethodPost, path, "")
		details := wantCode(t, rec, CodeNotFound)
		if d, _ := details["detail"].(string); !strings.Contains(d, "LUX_TUNNEL_ENABLED is unset") {
			t.Errorf("%s: detail %q", path, d)
		}
		if rec := h.request(http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
	if h.stub.Requests() != nil {
		t.Error("a route the tunnel does not serve asked the authorizer")
	}
	rec := h.request(http.MethodPut, "/v1/providers/laptop", tunnelProviderJSON)
	details := wantCode(t, rec, CodeInvalidField)
	if p := paths(details); len(p) != 1 || p[0] != "spec.tunnel" {
		t.Errorf("paths %v", p)
	}
}

// TestTunnelOwnerPolicyException: under the owner policy a non-admin
// applies and tunnels a Provider with tunnel: true and is refused one
// without it; another subject may not update, delete, or tunnel it; and
// the owner deletes it.
func TestTunnelOwnerPolicyException(t *testing.T) {
	f := &fakeTunnel{}
	h := newHarness(t, withTunnel(f))
	a, err := auth.New(t.Context(), auth.Options{Issuers: []string{h.iss.URL()}, Audience: audience, AdminSubjects: []string{h.subject()}, HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	h.auth = a
	o := h.h.o
	o.Auth, o.Authorizer = a, a.Authorizer(&serve.ObjectOwners{Objects: h.st.Objects()})
	h.h = New(o)
	carol := h.iss.Mint(issuertest.Claims{Sub: "carol"})

	// bob, no admin, applies a tunnelled Provider and is refused a
	// dialled one.
	rec := h.request(http.MethodPut, "/v1/providers/laptop", tunnelProviderJSON, as(h.bob)...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bob's tunnelled Provider: %d %s", rec.Code, rec.Body.String())
	}
	if st := status(t, rec); st["owner"] != h.iss.URL()+"|bob" {
		t.Errorf("status %v", st)
	}
	rec = h.request(http.MethodPut, "/v1/providers/openai", providerJSON, as(h.bob)...)
	if details := wantCode(t, rec, CodeForbidden); !strings.Contains(details["detail"].(string), auth.ReasonAdminOnly) {
		t.Errorf("bob's dialled Provider: %v", details)
	}
	// bob tunnels it; the session request carries the object, the
	// subject, the bearer's exp, and the agent.
	rec = h.request(http.MethodPost, "/v1/providers/laptop/tunnel", "", "Authorization", "Bearer "+h.bob, "User-Agent", "lux/0.1.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("bob's session: %d %s", rec.Code, rec.Body.String())
	}
	if len(f.sessions) != 1 {
		t.Fatalf("%d sessions handed to the tunnel", len(f.sessions))
	}
	s := f.sessions[0]
	if s.Provider == nil || s.Provider.Metadata.Name != "laptop" || !s.Provider.Spec.Tunnel || s.Subject != h.iss.URL()+"|bob" || s.Agent != "lux/0.1.0" || !strings.HasPrefix(s.RequestID, "req_") {
		t.Errorf("session request %+v", s)
	}
	if until := time.Until(s.ExpiresAt); until < 59*time.Minute || until > 61*time.Minute {
		t.Errorf("the session's expiry is %s away, want the bearer's hour", until)
	}
	// carol may neither tunnel, update, nor delete bob's.
	rec = h.request(http.MethodPost, "/v1/providers/laptop/tunnel", "", as(carol)...)
	if details := wantCode(t, rec, CodeForbidden); !strings.Contains(details["detail"].(string), "not_owner") {
		t.Errorf("carol's session: %v", details)
	}
	rec = h.request(http.MethodPut, "/v1/providers/laptop", `{"metadata": {"labels": {"x": "y"}}, "spec": {"dialect": "openai", "tunnel": true}}`, as(carol)...)
	wantCode(t, rec, CodeForbidden)
	rec = h.request(http.MethodDelete, "/v1/providers/laptop", "", as(carol)...)
	wantCode(t, rec, CodeForbidden)
	if len(f.sessions) != 1 {
		t.Fatal("a refused session reached the tunnel")
	}
	// The admin may tunnel it too, and bob deletes his own.
	if rec := h.request(http.MethodPost, "/v1/providers/laptop/tunnel", ""); rec.Code != http.StatusOK {
		t.Errorf("the admin's session: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.request(http.MethodDelete, "/v1/providers/laptop", "", as(h.bob)...); rec.Code != http.StatusNoContent {
		t.Errorf("bob's delete: %d %s", rec.Code, rec.Body.String())
	}
	// A session for a Provider that is not tunnelled, or that does not
	// exist, is not_found.
	if rec := h.request(http.MethodPut, "/v1/providers/openai", providerJSON); rec.Code != http.StatusCreated {
		t.Fatalf("the admin's dialled Provider: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.request(http.MethodPost, "/v1/providers/openai/tunnel", "")
	if details := wantCode(t, rec, CodeNotFound); !strings.Contains(details["detail"].(string), "has tunnel false") {
		t.Errorf("a dialled Provider's session: %v", details)
	}
	wantCode(t, h.request(http.MethodPost, "/v1/providers/nobody/tunnel", ""), CodeNotFound)
	wantCode(t, h.request(http.MethodPost, "/v1/providers/laptop/tunnel", "", "Authorization", ""), CodeUnauthenticated)
}

// TestTunnelInTheAuthorizerResource: with an authorizer, the resource of
// every provider action carries tunnel, provider.tunnel among them, and
// a deny on provider.tunnel is forbidden with the reason.
func TestTunnelInTheAuthorizerResource(t *testing.T) {
	f := &fakeTunnel{}
	h := newHarness(t, withTunnel(f))
	if rec := h.request(http.MethodPut, "/v1/providers/laptop", tunnelProviderJSON); rec.Code != http.StatusCreated {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	id := status(t, h.request(http.MethodGet, "/v1/providers/laptop", ""))["id"].(string)
	if rec := h.request(http.MethodPost, "/v1/providers/"+id+"/tunnel", ""); rec.Code != http.StatusOK {
		t.Fatalf("session by id: %d %s", rec.Code, rec.Body.String())
	}
	h.request(http.MethodPut, "/v1/providers/laptop", `{"metadata": {"labels": {"x": "y"}}, "spec": {"dialect": "openai", "tunnel": true}}`)
	h.request(http.MethodDelete, "/v1/providers/laptop", "")
	seen := map[string]bool{}
	for _, req := range h.stub.Requests() {
		if !strings.HasPrefix(req.Action, "provider.") {
			continue
		}
		seen[req.Action] = true
		if tun, ok := req.Resource.Fields["tunnel"].(bool); !ok || !tun {
			t.Errorf("%s: resource %v carries no tunnel true", req.Action, req.Resource.Fields)
		}
		if req.Action == auth.ActionProviderTunnel && (req.Resource.ID != id || req.Resource.Fields["owner"] != h.subject() || req.Resource.Fields["baseURL"] != "") {
			t.Errorf("provider.tunnel resource %+v", req.Resource)
		}
	}
	for _, action := range []string{auth.ActionProviderCreate, auth.ActionProviderRead, auth.ActionProviderUpdate, auth.ActionProviderDelete, auth.ActionProviderTunnel} {
		if !seen[action] {
			t.Errorf("%s was not asked", action)
		}
	}
	// A platform that denies provider.tunnel closes the door at connect.
	if rec := h.request(http.MethodPut, "/v1/providers/laptop", tunnelProviderJSON); rec.Code != http.StatusCreated {
		t.Fatalf("apply again: %d %s", rec.Code, rec.Body.String())
	}
	h.stub.Deny(stub.Rule{Action: auth.ActionProviderTunnel}, "no_machines")
	rec := h.request(http.MethodPost, "/v1/providers/laptop/tunnel", "")
	if details := wantCode(t, rec, CodeForbidden); !strings.Contains(details["detail"].(string), "no_machines") {
		t.Errorf("a denied session: %v", details)
	}
	if len(f.sessions) != 1 {
		t.Errorf("%d sessions reached the tunnel, want the allowed one alone", len(f.sessions))
	}
}

// TestTunnelCarrierRoute: a carrier is authenticated against the issuers
// and nothing else: no bearer or no session header is unauthenticated,
// the tunnel gets the path's reference, the header, and the subject, a
// refusal it returns is written in the envelope with its sentence, and
// the subject's bucket is not drawn on.
func TestTunnelCarrierRoute(t *testing.T) {
	f := &fakeTunnel{}
	h := newHarness(t, func(o *Options) {
		withTunnel(f)(o)
		o.RequestsPerMinute = 1
	})
	if rec := h.request(http.MethodPut, "/v1/providers/laptop", tunnelProviderJSON); rec.Code != http.StatusCreated {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body.String())
	}
	wantCode(t, h.request(http.MethodPost, "/v1/providers/laptop/tunnel/carry", "", "Authorization", ""), CodeUnauthenticated)
	rec := h.request(http.MethodPost, "/v1/providers/laptop/tunnel/carry", "")
	if details := wantCode(t, rec, CodeUnauthenticated); !strings.Contains(details["detail"].(string), wire.HeaderSession) {
		t.Errorf("no session header: %v", details)
	}
	// The bucket admits one request a minute; carriers are not in it.
	for range 3 {
		rec := h.request(http.MethodPost, "/v1/providers/laptop/tunnel/carry", "", wire.HeaderSession, "tun_1", "Authorization", "Bearer "+h.bob)
		if rec.Code != http.StatusOK || rec.Header().Get("RateLimit-Limit") != "" {
			t.Fatalf("carrier: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
		}
	}
	if len(f.carriers) != 3 {
		t.Fatalf("%d carriers reached the tunnel", len(f.carriers))
	}
	if c := f.carriers[0]; c.Provider != "laptop" || c.Session != "tun_1" || c.Subject != h.iss.URL()+"|bob" {
		t.Errorf("carrier request %+v", c)
	}
	if n := len(h.stub.Requests()); n != 1 {
		t.Errorf("the carriers asked the authorizer: %d requests, want the apply's alone", n)
	}
	// A refusal from the tunnel is this package's envelope with the same
	// code, and the tunnel's sentences are the table's.
	f.refuse = &tunnel.Error{Code: tunnel.CodeUnauthenticated, Detail: "no live session"}
	rec = h.request(http.MethodPost, "/v1/providers/laptop/tunnel/carry", "", wire.HeaderSession, "tun_9")
	if details := wantCode(t, rec, CodeUnauthenticated); details["detail"] != "no live session" {
		t.Errorf("a refused carrier: %v", details)
	}
	// The session route is in the bucket: alice spent her one request on
	// the apply, so hers is rate_limited and bob's reaches the tunnel.
	f.refuse = &tunnel.Error{Code: tunnel.CodeNotFound, Detail: "the tunnel needs HTTP/2"}
	wantCode(t, h.request(http.MethodPost, "/v1/providers/laptop/tunnel", ""), CodeRateLimited)
	rec = h.request(http.MethodPost, "/v1/providers/laptop/tunnel", "", as(h.bob)...)
	if details := wantCode(t, rec, CodeNotFound); details["detail"] != "the tunnel needs HTTP/2" {
		t.Errorf("a refused session: %v", details)
	}
	for _, c := range tunnel.Codes() {
		if c.Message() != Code(c).Message() || c.Status() != Code(c).Status() {
			t.Errorf("tunnel code %s: %q %d, the table says %q %d", c, c.Message(), c.Status(), Code(c).Message(), Code(c).Status())
		}
	}
	if got := expiryOf(auth.Caller{Claims: map[string]any{"exp": int64(1700000000)}}); !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("expiryOf an int64 exp: %s", got)
	}
	if got := expiryOf(auth.Caller{}); !got.IsZero() {
		t.Errorf("expiryOf no exp: %s", got)
	}
	// The status of the applied Provider renders no tunnel block until
	// the health job writes one, and renders it as written afterwards.
	h.advance(2 * time.Minute) // alice's bucket refills
	st := status(t, h.request(http.MethodGet, "/v1/providers/laptop", ""))
	if st["tunnel"] != nil {
		t.Errorf("status.tunnel before any session: %v", st["tunnel"])
	}
	obj := h.objectOf(v1.KindProvider, "laptop")
	if err := h.st.Objects().PutStatus(bg(), v1.KindProvider, obj.ID(), store.ProviderObserved{Tunnel: &v1.TunnelStatus{State: v1.TunnelConnected, Session: "tun_1", Subject: h.subject(), Agent: "lux/0.1.0"}}); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Minute)
	st = status(t, h.request(http.MethodGet, "/v1/providers/laptop", ""))
	if tun, _ := st["tunnel"].(map[string]any); tun["state"] != "Connected" || tun["session"] != "tun_1" || tun["agent"] != "lux/0.1.0" {
		t.Errorf("status.tunnel: %v", st["tunnel"])
	}
}
