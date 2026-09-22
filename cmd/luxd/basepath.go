// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
)

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
func mountAt(base string, h http.Handler) http.Handler {
	if base == "" {
		return h
	}
	trim := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := new(http.Request)
		*r2 = *r
		u := *r.URL
		u.Path = rooted(strings.TrimPrefix(r.URL.Path, base))
		if r.URL.RawPath != "" {
			u.RawPath = rooted(strings.TrimPrefix(r.URL.RawPath, base))
		}
		r2.URL = &u
		h.ServeHTTP(w, r2)
	})
	mux := http.NewServeMux()
	mux.Handle(base, trim)
	mux.Handle(base+"/", trim)
	return mux
}

// rooted is the path left after the base is trimmed, "/" when nothing is.
func rooted(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
