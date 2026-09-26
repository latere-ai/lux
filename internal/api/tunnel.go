// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"time"

	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/tunnel"
	"latere.ai/x/lux/internal/tunnel/wire"
	v1 "latere.ai/x/lux/manifest/v1"
)

// TunnelRoutes is the gateway side of spec 013 as the two routes under
// /v1/providers/{id-or-name}/tunnel hand a request to it once this
// package has authenticated and authorized the caller: *tunnel.Gateway.
// The refusal either method returns before its stream is committed is
// written in this package's envelope; after the commit the stream
// carries the outcome and the method returns nil.
type TunnelRoutes interface {
	ServeSession(w http.ResponseWriter, r *http.Request, s tunnel.SessionRequest) *tunnel.Error
	ServeCarrier(w http.ResponseWriter, r *http.Request, c tunnel.CarrierRequest) *tunnel.Error
}

// tunnelRoutes mounts the session route and the carrier route beside the
// Provider routes. Both are POST alone; another method is this package's
// not_found like any path outside the table.
func (h *Handler) tunnelRoutes() {
	h.handle("/v1/providers/{name}/tunnel", h.route(map[string]handlerFunc{
		http.MethodPost: func(c *call, ctx context.Context) *Error { return c.tunnelSession(ctx, c.r.PathValue("name")) },
	}))
	h.handle("/v1/providers/{name}/tunnel/carry", h.route(map[string]handlerFunc{
		http.MethodPost: func(c *call, ctx context.Context) *Error { return c.tunnelCarrier(ctx, c.r.PathValue("name")) },
	}))
}

// tunnelSession is POST /v1/providers/{id-or-name}/tunnel: a /v1 route
// in every respect, the bearer and the subject's bucket, the Provider
// loaded, provider.tunnel asked with the resource carrying tunnel, then
// the session for as long as it lives. A Provider that is not tunneled
// has no such route, as an installation with the tunnel off has none.
func (c *call) tunnelSession(ctx context.Context, ref string) *Error {
	if c.h.o.Tunnel == nil {
		return refuse(CodeNotFound, "POST "+c.r.URL.Path+" is not in the route table: LUX_TUNNEL_ENABLED is unset")
	}
	if err := c.authenticate(); err != nil {
		return err
	}
	p, err := c.loadTunneled(ctx, ref)
	if err != nil {
		return err
	}
	if err := c.authorizeTunnel(ctx, p); err != nil {
		return err
	}
	if e := c.h.o.Tunnel.ServeSession(c.w, c.r, tunnel.SessionRequest{
		Provider: p, Subject: c.caller.Subject, ExpiresAt: expiryOf(c.caller), Agent: c.r.UserAgent(), RequestID: c.id,
	}); e != nil {
		return tunnelError(e)
	}
	return nil
}

// tunnelCarrier is POST /v1/providers/{id-or-name}/tunnel/carry: the
// bearer verified against the issuers and nothing asked of the
// authorizer or the subject's bucket, because the session it joins was
// authorized at connect and one carrier is spent per proxied request;
// the session named in Lux-Tunnel-Session must be live on this replica
// under the bearer's subject, which the tunnel checks.
func (c *call) tunnelCarrier(_ context.Context, ref string) *Error {
	if c.h.o.Tunnel == nil {
		return refuse(CodeNotFound, "POST "+c.r.URL.Path+" is not in the route table: LUX_TUNNEL_ENABLED is unset")
	}
	caller, err := c.h.o.Auth.Verifier.Authenticate(c.r)
	if err != nil {
		return mapError(err)
	}
	c.caller = caller
	session := c.r.Header.Get(wire.HeaderSession)
	if session == "" {
		return refuse(CodeUnauthenticated, "a carrier names its session in "+wire.HeaderSession+", and this one carries none")
	}
	if e := c.h.o.Tunnel.ServeCarrier(c.w, c.r, tunnel.CarrierRequest{Provider: ref, Session: session, Subject: caller.Subject}); e != nil {
		return tunnelError(e)
	}
	return nil
}

// loadTunneled is the Provider a tunnel route names, which must be a
// tunnel: true one.
func (c *call) loadTunneled(ctx context.Context, ref string) (*v1.Provider, *Error) {
	obj, _, err := c.load(ctx, kindOf(v1.KindProvider), ref)
	if err != nil {
		return nil, err
	}
	p, ok := obj.(*v1.Provider)
	if !ok {
		return nil, refuse(CodeInternal, "")
	}
	if !p.Spec.Tunnel {
		return nil, refuse(CodeNotFound, "Provider "+p.Metadata.Name+" has tunnel false; the tunnel routes are a tunneled Provider's")
	}
	return p, nil
}

// expiryOf is the exp of the caller's bearer, which the session lives
// as long as.
func expiryOf(c auth.Caller) time.Time {
	switch exp := c.Claims["exp"].(type) {
	case float64:
		return time.Unix(int64(exp), 0)
	case int64:
		return time.Unix(exp, 0)
	}
	return time.Time{}
}

// tunnelError is a tunnel refusal in this package's envelope: the same
// code, the same detail.
func tunnelError(e *tunnel.Error) *Error {
	return refuse(Code(e.Code), e.Detail)
}
