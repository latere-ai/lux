// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
)

// doorNames are the four doors of spec 004, each the first segment of the
// paths it serves on the public listener.
var doorNames = []string{"openai", "anthropic", "gemini", "lux"}

// mountAt serves h under base, the LUX_BASE_PATH of spec 034, and h itself
// when base is empty, so an installation on its own hostname runs the
// listener it always ran. Under a base the doors, the control plane,
// /.well-known/lux, the probes, and the build identity move together:
// the prefix is a property of the listener, not of any route, so nothing
// behind the wrapper learns it, and the paths the handlers, the request
// log, the metric labels, and the authorizer read are the rooted ones.
//
// The base itself answers what / answers, the build identity. A path
// outside the base reaches no pattern and takes the mux's bare 404:
// outside the surface there is no surface to protect. http.StripPrefix
// is not used because it leaves an empty path for the base itself, which
// the inner mux would redirect rather than serve.
//
// With replacesV1, LUX_BASE_PATH_MODE=replace, the mount is spec 040's:
// see mountReplacingV1.
func mountAt(base string, replacesV1 bool, h http.Handler) http.Handler {
	if base == "" {
		return h
	}
	if replacesV1 {
		return mountReplacingV1(base, h)
	}
	trim := rebase(base, "", h)
	mux := http.NewServeMux()
	mux.Handle(base, trim)
	mux.Handle(base+"/", trim)
	return mux
}

// mountReplacingV1 serves h under base with the base in the place of the
// control plane's /v1 (spec 040), so every public address carries one
// version segment. The routes that live outside /v1 keep their own path
// under the base: the four doors, /.well-known/lux, /version, and the
// build identity at the base itself. Every other path under the base is
// a control plane route and reaches h with /v1 in the place of the base.
//
// The probes are not public here: /livez and /readyz under the base reach
// the control plane as /v1/livez and /v1/readyz, which it does not route,
// because the orchestrator reads them on the internal listener. A path
// outside the base takes the mux's bare 404, and <base>/v1/... reaches the
// control plane as /v1/v1/..., which it does not route either.
func mountReplacingV1(base string, h http.Handler) http.Handler {
	trim := rebase(base, "", h)
	mux := http.NewServeMux()
	mux.Handle("GET "+base, trim)
	mux.Handle("GET "+base+"/{$}", trim)
	mux.Handle("GET "+base+"/version", trim)
	mux.Handle(base+"/.well-known/lux", trim)
	for _, d := range doorNames {
		mux.Handle(base+"/"+d, trim)
		mux.Handle(base+"/"+d+"/", trim)
	}
	mux.Handle(base+"/", rebase(base, "/v1", h))
	return mux
}

// rebase serves h with base replaced by to at the head of the request
// path, in the decoded and in the escaped form; an empty remainder with no
// replacement is "/". An escaped path that does not carry base literally
// is dropped, so the two forms never disagree and the decoded one rules.
func rebase(base, to string, h http.Handler) http.Handler {
	return rebased{base: base, to: to, h: h}
}

// rebased is the handler rebase returns, a type of its own so that
// rootedRequest can ask a mount which rewrite a request takes.
type rebased struct {
	base, to string
	h        http.Handler
}

func (b rebased) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.h.ServeHTTP(w, b.request(r))
}

// request is a copy of r with the rewritten path, the request the handler
// behind the mount reads.
func (b rebased) request(r *http.Request) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	u := *r.URL
	u.Path = rooted(b.to + strings.TrimPrefix(r.URL.Path, b.base))
	u.RawPath = ""
	if raw, ok := strings.CutPrefix(r.URL.RawPath, b.base); ok {
		u.RawPath = rooted(b.to + raw)
	}
	r2.URL = &u
	return r2
}

// rootedRequest is r as the handler behind mounted, mountAt's result for
// base, reads it: r itself at the root, and under a base the copy the
// rewrite the mount's mux picks for r produces. It is false for a request
// no pattern of the mount takes, a path outside the base or one the mux
// redirects, which reaches no route. The mount's own mux decides, so the
// answer follows mountAt and mountReplacingV1 without a second copy of
// their table.
func rootedRequest(base string, mounted http.Handler, r *http.Request) (*http.Request, bool) {
	if base == "" {
		return r, true
	}
	mux, ok := mounted.(*http.ServeMux)
	if !ok {
		return nil, false
	}
	h, _ := mux.Handler(r)
	b, ok := h.(rebased)
	if !ok {
		return nil, false
	}
	return b.request(r), true
}

// rooted is the path left after the base is trimmed, "/" when nothing is.
func rooted(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
