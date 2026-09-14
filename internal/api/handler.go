// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/metrics"
	"latere.ai/x/pkg/ratelimit"

	"latere.ai/x/lux/gateway"
	"latere.ai/x/lux/internal/auth"
	"latere.ai/x/lux/internal/secrets"
	"latere.ai/x/lux/internal/store"
	"latere.ai/x/lux/manifest"
	v1 "latere.ai/x/lux/manifest/v1"
)

// MetricRefusals counts every error envelope written by code, spec
// 019's lux_refusals_total.
const MetricRefusals = "lux_refusals_total"

// ClientRevoker forgets the upstream client of a deleted Provider: spec
// 005's *gateway.Clients.
type ClientRevoker interface {
	Revoke(providerID string)
}

// Options is what New builds the surface from.
type Options struct {
	// Store holds desired state; in the file mode it is the read-only
	// store over the directory. Required.
	Store store.Store
	// Auth is the identity of spec 006: its Verifier authenticates every
	// bearer and its Policy is what /v1/self reports; PolicyFile is the
	// file mode, where no bearer is asked and nothing is authorized.
	// Required.
	Auth *auth.Auth
	// Authorizer decides every action and answers the Lookup of Resolve;
	// nil in the file mode alone.
	Authorizer *auth.Authorizer
	// PublicURL is LUX_PUBLIC_URL, the base of every URL in a response
	// and Resolve's loop check. Required.
	PublicURL *url.URL
	// Version is the build's, for /.well-known/lux.
	Version string
	// RequestsPerMinute is LUX_REQUESTS_PER_MINUTE, which an authorizer's
	// limits.requests_per_minute overrides per subject; 0 is no limit.
	RequestsPerMinute int
	// TrustedProxies is LUX_TRUSTED_PROXIES, for the authorizer's
	// request.ip.
	TrustedProxies []netip.Prefix
	// MaxManifestBytes is LUX_MAX_MANIFEST_BYTES; 0 is 65536.
	MaxManifestBytes int64
	// Defaults, AllowPrivateUpstreams, and TunnelEnabled are Resolve's
	// options from the configuration.
	Defaults              manifest.Defaults
	AllowPrivateUpstreams bool
	TunnelEnabled         bool
	// Keys seals a Provider's credential value at apply; nil in the
	// file mode alone, where no apply reaches it.
	Keys *secrets.Keyring
	// Clients is told of a deleted Provider; nil tells nothing.
	Clients ClientRevoker
	// ReadOnlyDir is the directory the file mode reads, named in the
	// developer detail of read_only.
	ReadOnlyDir string
	// Metrics receives MetricRefusals; nil records none.
	Metrics *metrics.Registry
	// Logger receives the developer's lines, a handler panic among
	// them; nil is slog.Default.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// NewID mints a prefixed ULID; nil uses v1.NewID.
	NewID func(prefix string) string
	// NewName names an object applied without one, <adjective>-<noun>-<4
	// hex>; nil leaves an absent name missing_field.
	NewName func() string
	// MintKeyValue mints a Key's value; nil uses serve.MintKeyValue.
	MintKeyValue func() (string, error)
}

// Handler is the /v1 surface and /.well-known/lux as one http.Handler.
// It is mounted at /v1/ and at /.well-known/lux; a path under /v1 it
// does not route is not_found in its own envelope.
type Handler struct {
	o        Options
	mux      *http.ServeMux
	subjects *ratelimit.Buckets
	grants   *grants
	refusals *metrics.Counter
	openapi  []byte
	logger   *slog.Logger
}

// DefaultMaxManifestBytes is LUX_MAX_MANIFEST_BYTES's default.
const DefaultMaxManifestBytes int64 = 64 << 10

// New builds the handler. A nil Store, Auth, or PublicURL is a panic,
// and so is a nil Authorizer outside the file mode, because a surface
// that cannot decide anything should not start.
func New(o Options) *Handler {
	switch {
	case o.Store == nil:
		panic("api.New: Options.Store is nil")
	case o.Auth == nil:
		panic("api.New: Options.Auth is nil")
	case o.PublicURL == nil:
		panic("api.New: Options.PublicURL is nil")
	case o.Authorizer == nil && o.Auth.Policy != auth.PolicyFile:
		panic("api.New: Options.Authorizer is nil outside the file mode")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.NewID == nil {
		o.NewID = func(prefix string) string { return v1.NewID(prefix, o.Now(), nil) }
	}
	if o.MaxManifestBytes <= 0 {
		o.MaxManifestBytes = DefaultMaxManifestBytes
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	h := &Handler{o: o, mux: http.NewServeMux(), grants: newGrants(o.Now), logger: o.Logger}
	h.subjects = ratelimit.New(ratelimit.Config{Idle: 10 * time.Minute, Now: o.Now})
	if o.Metrics != nil {
		h.refusals = o.Metrics.Counter(MetricRefusals, "Control plane refusals by code.")
	}
	h.openapi = openAPIJSON()
	h.routes()
	return h
}

// fileMode reports whether the surface is the file mode's.
func (h *Handler) fileMode() bool { return h.o.Auth.Policy == auth.PolicyFile }

// call is one request through the surface.
type call struct {
	h      *Handler
	w      *responseWriter
	r      *http.Request
	id     string
	start  time.Time
	caller auth.Caller // set once authenticated
	addr   string      // the client address of spec 011
}

// handlerFunc is one route: it answers or returns the refusal, under the
// request's context.
type handlerFunc func(c *call, ctx context.Context) *Error

// ServeHTTP mints the request id, echoes the caller's X-Request-Id under
// spec 011's rule, refuses the file mode's writes before any body is
// read, dispatches, and turns a handler panic into internal.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &call{h: h, r: r, id: h.o.NewID(v1.PrefixRequest), start: h.o.Now()}
	c.w = &responseWriter{ResponseWriter: w}
	hdr := w.Header()
	hdr.Set(gateway.HeaderRequestID, c.id)
	hdr.Set("X-Content-Type-Options", "nosniff")
	if xid := r.Header.Get("X-Request-Id"); echoable(xid) {
		hdr.Set("X-Request-Id", xid)
	}
	ctx := r.Context()
	defer func() {
		if p := recover(); p != nil {
			h.logger.ErrorContext(ctx, "api: handler panic", "request_id", c.id, "method", r.Method, "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
			if !c.w.committed {
				c.fail(refuse(CodeInternal, ""))
			}
		}
	}()
	if h.fileMode() && isWrite(r.Method) && underAKind(r.URL.Path) {
		c.fail(refuse(CodeReadOnly, "desired state is the directory "+h.o.ReadOnlyDir+"; "+r.Method+" "+r.URL.Path+" was refused before the body was read"))
		return
	}
	// The mux would answer a path with a repeated slash, a dot segment,
	// or a trailing slash with its own redirect; none of those is a
	// route, and the answer of a path that is not a route is not_found.
	if p := r.URL.Path; p != path.Clean(p) {
		c.fail(refuse(CodeNotFound, r.Method+" "+p+" is not in the route table"))
		return
	}
	h.mux.ServeHTTP(c.w, r.WithContext(withCall(ctx, c)))
}

// Unmounted is the handler the public listener serves under /v1 in the
// file mode, where the surface is the internal listener's alone: every
// path is not_found in the envelope, with a request id.
func (h *Handler) Unmounted() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := h.o.NewID(v1.PrefixRequest)
		w.Header().Set(gateway.HeaderRequestID, id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if xid := r.Header.Get("X-Request-Id"); echoable(xid) {
			w.Header().Set("X-Request-Id", xid)
		}
		h.count(CodeNotFound)
		writeError(w, id, refuse(CodeNotFound, "the control plane is on the internal listener in the file mode; "+r.URL.Path+" is not mounted here"))
	})
}

// isWrite reports the three methods the file mode refuses.
func isWrite(method string) bool {
	return method == http.MethodPut || method == http.MethodPost || method == http.MethodDelete
}

// underAKind reports a path under /v1/{kind}s, the tunnel routes
// included.
func underAKind(path string) bool {
	for _, k := range kinds {
		prefix := "/v1/" + k.plural
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// echoable is spec 011's rule for X-Request-Id: at most 128 bytes of
// printable ASCII, or the header is dropped rather than echoed.
func echoable(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// routes registers every pattern without a method, so a method the
// table does not list for a path, PATCH and OPTIONS among them, is this
// package's not_found and never the mux's own 405.
func (h *Handler) routes() {
	for _, k := range kinds {
		h.mux.Handle("/v1/"+k.plural, h.route(map[string]handlerFunc{
			http.MethodGet: func(c *call, ctx context.Context) *Error { return c.list(ctx, k) },
		}))
		item := "/v1/" + k.plural + "/{name}"
		if k.name == v1.KindModel {
			item = "/v1/" + k.plural + "/{name...}"
		}
		methods := map[string]handlerFunc{
			http.MethodPut:    func(c *call, ctx context.Context) *Error { return c.apply(ctx, k, c.r.PathValue("name")) },
			http.MethodGet:    func(c *call, ctx context.Context) *Error { return c.read(ctx, k, c.r.PathValue("name")) },
			http.MethodDelete: func(c *call, ctx context.Context) *Error { return c.delete(ctx, k, c.r.PathValue("name")) },
		}
		h.mux.Handle(item, h.route(methods))
		if k.name == v1.KindKey {
			h.mux.Handle(item+"/rotate", h.route(map[string]handlerFunc{
				http.MethodPost: func(c *call, ctx context.Context) *Error { return c.rotate(ctx, c.r.PathValue("name")) },
			}))
		}
	}
	h.mux.Handle("/v1/usage", h.route(map[string]handlerFunc{http.MethodGet: (*call).usage}))
	h.mux.Handle("/v1/requests", h.route(map[string]handlerFunc{http.MethodGet: (*call).requests}))
	h.mux.Handle("/v1/self", h.route(map[string]handlerFunc{http.MethodGet: (*call).self}))
	h.mux.Handle("/v1/openapi.json", h.route(map[string]handlerFunc{http.MethodGet: (*call).openAPI}))
	h.mux.Handle("/.well-known/lux", h.route(map[string]handlerFunc{http.MethodGet: (*call).wellKnown}))
	h.mux.Handle("/", h.route(nil))
}

// route dispatches on the method and writes the refusal a handler
// returns.
func (h *Handler) route(methods map[string]handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := callOf(r)
		// The mux hands the route its own copy of the request, the one
		// that carries the path values.
		c.r = r
		fn, ok := methods[r.Method]
		if !ok {
			c.fail(refuse(CodeNotFound, r.Method+" "+r.URL.Path+" is not in the route table"))
			return
		}
		if err := fn(c, r.Context()); err != nil {
			c.fail(err)
		}
	})
}

// fail writes the refusal and counts it.
func (c *call) fail(e *Error) {
	c.h.count(e.Code)
	writeError(c.w, c.id, e)
}

func (h *Handler) count(code Code) {
	if h.refusals != nil {
		h.refusals.Inc(map[string]string{"code": string(code)})
	}
}

// writeJSON answers with one JSON body, in the Go struct order the
// OpenAPI document records; version above zero is the ETag.
func (c *call) writeJSON(status int, body any, version int64) *Error {
	data, err := json.Marshal(body)
	if err != nil {
		return refuse(CodeInternal, "")
	}
	data = append(data, '\n')
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	if version > 0 {
		h.Set("ETag", etag(version))
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	c.w.WriteHeader(status)
	_, _ = c.w.Write(data)
	return nil
}

// etag renders a version as the strong validator spec 011 names.
func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

// responseWriter notes whether the response was committed, so a panic
// after the first byte is not answered twice.
type responseWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.committed {
		return
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
