// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build mut_keyvalue

package serve

import (
	"net/http"
	"strings"
)

// The mutation of spec 018's seventh row: a read or a list of Keys
// answers a status.value. Only case011SecretsNeverInResponses may
// redden.
func init() {
	mutateHandler = func(h http.Handler) http.Handler {
		return rewriteResponses(h, func(r *http.Request, status int, body map[string]any) bool {
			if r.Method != http.MethodGet || status != http.StatusOK || !strings.HasPrefix(r.URL.Path, "/v1/keys") {
				return false
			}
			leaked := false
			if items, ok := body["items"].([]any); ok {
				for _, it := range items {
					if m, ok := it.(map[string]any); ok && leakValue(m) {
						leaked = true
					}
				}
				return leaked
			}
			return leakValue(body)
		})
	}
}

// leakValue writes a value into a Key's status.
func leakValue(key map[string]any) bool {
	status, ok := key["status"].(map[string]any)
	if !ok {
		return false
	}
	status["value"] = "lux_" + strings.Repeat("leak", 10)
	return true
}
