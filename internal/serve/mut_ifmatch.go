// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_ifmatch

package serve

import "net/http"

// The mutation of spec 018's first row: If-Match is ignored, so a stale
// precondition updates instead of answering conflict. Only
// case011Preconditions may redden.
func init() {
	mutateHandler = func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("If-Match")
			h.ServeHTTP(w, r)
		})
	}
}
