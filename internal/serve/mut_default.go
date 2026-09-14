// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_default

package serve

import (
	"net/http"
	"strings"
)

// The mutation of spec 018's second row: one default Resolve fills, a
// Model's fallback, is not in the object the server answers with, so the
// read-back's spec is no longer the golden's. Only
// case003DefaultsAreVisible may redden.
func init() {
	mutateHandler = func(h http.Handler) http.Handler {
		return rewriteResponses(h, func(r *http.Request, status int, body map[string]any) bool {
			if status >= 300 || !strings.HasPrefix(r.URL.Path, "/v1/models/") {
				return false
			}
			spec, ok := body["spec"].(map[string]any)
			if !ok {
				return false
			}
			delete(spec, "fallback")
			return true
		})
	}
}
