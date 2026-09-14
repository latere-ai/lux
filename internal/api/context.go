// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
)

// callKey carries the call through the request's context, from
// ServeHTTP to the route the mux picks.
type callKey struct{}

func withCall(ctx context.Context, c *call) context.Context {
	return context.WithValue(ctx, callKey{}, c)
}

// callOf is the call ServeHTTP built for the request.
func callOf(r *http.Request) *call {
	c, _ := r.Context().Value(callKey{}).(*call)
	return c
}
