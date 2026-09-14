// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package tunnel is the gateway side of spec 013: a local model server
// attached as a Provider through one outbound HTTP/2 connection its
// agent opens. The Gateway holds the sessions this replica has
// accepted, parks the carriers each agent keeps open, and hands a
// proxied request to a parked one; it satisfies spec 004's
// ClientSource, answering a tunnel: true Provider with a client whose
// transport writes onto a carrier and delegating every other Provider
// to spec 005's clients, so the doors, discovery, and health reach a
// tunnelled Provider through the seam they already have. The registry,
// store.Tunnels, says which replica holds a session; a replica that
// does not hold one forwards to the one that does over the internal
// route Forward serves, with LUX_TUNNEL_FORWARD_SECRET as the bearer.
//
// The two public routes are mounted by internal/api, which
// authenticates the bearer, asks provider.tunnel at connect, and then
// calls ServeSession and ServeCarrier; this package writes no envelope
// on them. The wire format is internal/tunnel/wire; the agent is
// internal/tunnel/agent.
package tunnel
