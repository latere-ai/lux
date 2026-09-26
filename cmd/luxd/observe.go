// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"path"
	"strings"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/api"
	"latere.ai/x/lux/internal/serve"
)

// observePublic is the public listener's handler inside
// latere.ai/x/pkg/otel's Handler: for every request but the two probes,
// one SERVER span named after the route and one
// http.server.request.duration point labeled with it as http.route. It
// sits outside the mount, the per-address bucket, and each plane's
// authentication, so a refused or rate-limited request is recorded like
// a served one. lux.request and lux.api open under its span as INTERNAL
// children.
//
// otelhttp copies the peer address, the X-Forwarded-For client, and the
// User-Agent onto the server span, and spec 019 puts no caller address
// on any span, so the handler sees the request with those three removed
// and the listener behind it gets them back.
func observePublic(mounted http.Handler, routes listenerRoutes) http.Handler {
	return conceal(otel.Handler(reveal(mounted), serve.ServiceName,
		otel.WithSkip(routes.probe),
		otel.WithRouteTemplate(routes.template),
	))
}

// listenerRoutes names the route a request to the public listener takes.
// Every name is a pattern of the listener's mux, a row of the doors'
// table (gateway.RouteTemplate), or a pattern of the control plane's
// route table, so the set is bounded by the build and no model name,
// object name, or path segment a caller sent becomes a label.
type listenerRoutes struct {
	base    string         // LUX_BASE_PATH, "" at the root
	mounted http.Handler   // mountAt's handler for base over public
	public  *http.ServeMux // the listener's routes, rooted
	control *api.Handler
	// controlOnPublic is false in the file mode, where the public
	// listener answers not_found under /v1 and the control plane is the
	// internal listener's.
	controlOnPublic bool
}

// probe reports a request /livez or /readyz answers on this listener,
// which otel.Handler skips: no span and no request metric.
func (l listenerRoutes) probe(r *http.Request) bool {
	rr, ok := rootedRequest(l.base, l.mounted, r)
	if !ok {
		return false
	}
	_, pattern := l.public.Handler(rr)
	return pattern == "GET /livez" || pattern == "GET /readyz"
}

// template is the request's http.route, rooted as the handlers behind
// the mount read it (/openai/v1/chat/completions, /v1/keys/{name}), or
// "" for a request no route answers: a path outside the base, one the
// mux redirects, a method the route does not take, and a path under a
// door or /v1 that is not in its table.
func (l listenerRoutes) template(r *http.Request) string {
	rr, ok := rootedRequest(l.base, l.mounted, r)
	if !ok || !canonical(rr.URL.Path) {
		return ""
	}
	_, pattern := l.public.Handler(rr)
	switch pattern {
	case "":
		return ""
	case "GET /{$}":
		return "/"
	case "GET /version":
		return "/version"
	case "/.well-known/lux", "/.well-known/lux/":
		return l.control.RouteTemplate(rr)
	case "/v1", "/v1/":
		if !l.controlOnPublic {
			return ""
		}
		return l.control.RouteTemplate(rr)
	}
	return gateway.RouteTemplate(rr.Method, rr.URL.Path)
}

// canonical reports a path the mux serves rather than redirects: the
// cleaned path, keeping one trailing slash, as net/http's ServeMux
// cleans it.
func canonical(p string) bool {
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c == p
}

// concealedKey carries the caller fields conceal removed.
type concealedKey struct{}

// caller is what conceal removes from the request otelhttp reads.
type caller struct {
	remoteAddr string
	header     http.Header
}

// conceal hands next a copy of r without RemoteAddr, User-Agent, and
// X-Forwarded-For, the fields otelhttp turns into network.peer.address,
// user_agent.original, and client.address on the server span, and keeps
// the originals in the context for reveal. The original header is not
// changed; the copy's is a clone.
func conceal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.WithContext(context.WithValue(r.Context(), concealedKey{}, caller{remoteAddr: r.RemoteAddr, header: r.Header}))
		r2.RemoteAddr = ""
		r2.Header = r.Header.Clone()
		r2.Header.Del("User-Agent")
		r2.Header.Del("X-Forwarded-For")
		next.ServeHTTP(w, r2)
	})
}

// reveal hands next a copy of r with the address and header conceal
// removed, so the per-address bucket and the authorizer's request.ip read
// the caller as the listener received it. The body stays r's, which
// otelhttp wraps to count what is read. The copy also keeps the listener's
// muxes from writing their pattern onto the request otelhttp labels the
// metrics from: otelhttp prefers a matched pattern over the route
// template, and the listener's patterns (/openai/, /v1/) are coarser than
// the template.
func reveal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.WithContext(r.Context())
		if c, ok := r.Context().Value(concealedKey{}).(caller); ok {
			r2.RemoteAddr = c.remoteAddr
			r2.Header = c.header
		}
		next.ServeHTTP(w, r2)
	})
}
