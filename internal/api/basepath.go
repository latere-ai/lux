// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// versionSegment is the segment every control plane route sits under at
// the root.
const versionSegment = "/v1"

// Route is the address a route of the public listener answers at under
// base (spec 040). route is written as a server at the root serves it:
// /v1/... for the control plane, and /.well-known/lux, /version, or a
// door's path for the rest. An empty base is the root, where a route is
// its own path. Otherwise the base is put in front of the route, except
// that with replacesV1, LUX_BASE_PATH_MODE=replace, the base stands in
// the place of a control plane route's /v1: under /v1/models the route
// /v1/keys is /v1/models/keys and /.well-known/lux is
// /v1/models/.well-known/lux. The listener, the served document, and the
// tests read addresses by this one rule.
func Route(base string, replacesV1 bool, route string) string {
	if base == "" {
		return route
	}
	if replacesV1 {
		if rest, ok := strings.CutPrefix(route, versionSegment); ok && (rest == "" || rest[0] == '/') {
			return base + rest
		}
	}
	return base + route
}
