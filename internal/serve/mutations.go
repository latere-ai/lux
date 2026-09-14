// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package serve

import (
	"net/http"

	"latere.ai/x/lux/gateway"
)

// The mutation seams of spec 018: three functions the conformance
// suite's reference server passes its handler, its Limiter, and its
// upstream clients through. In every build but one they are the
// identity. Under one of the build tags mut_ifmatch, mut_default,
// mut_loss, mut_unknownfield, mut_unpriced, mut_retryafter, mut_keyvalue,
// or mut_forwardcred, the file of that tag sets one of the hooks below
// in its init and drops one capability, so the suite's mutation check
// can prove that exactly one case reddens. luxd calls none of the three,
// and no tagged file is compiled without its tag, so a released binary
// carries no mutation.
var (
	mutateHandler func(http.Handler) http.Handler
	mutateLimiter func(gateway.Limiter) gateway.Limiter
	mutateClients func(gateway.ClientSource) gateway.ClientSource
)

// Mutate wraps the server's whole handler, both planes, with the tagged
// mutation when one is compiled in, and returns h otherwise.
func Mutate(h http.Handler) http.Handler {
	if mutateHandler == nil {
		return h
	}
	return mutateHandler(h)
}

// MutateLimiter wraps the doors' Limiter the same way.
func MutateLimiter(l gateway.Limiter) gateway.Limiter {
	if mutateLimiter == nil {
		return l
	}
	return mutateLimiter(l)
}

// MutateClients wraps the doors' upstream clients the same way.
func MutateClients(c gateway.ClientSource) gateway.ClientSource {
	if mutateClients == nil {
		return c
	}
	return mutateClients(c)
}
