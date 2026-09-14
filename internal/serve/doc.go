// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package serve is the serve role of luxd: what runs beside the
// listeners over the store, the identity, and the upstream clients. This
// phase holds spec 005's two background jobs and the two seams the data
// plane of spec 004 takes from it. Discovery reads every auto-mode
// Provider's model list under the discovery lease, on the interval and
// at once when the journal reports a Provider created or re-addressed,
// and resolves each name into a discovered Model through the same
// manifest.Resolve every other surface uses. Health probes every
// probe-mode Provider's models route under the health lease, folds in the
// data plane's own outcomes, publishes status.health and every Model's
// availability through PutStatus, keeps a per-replica view that only
// ever downgrades the published state, and raises provider.unreachable
// and provider.healthy once per transition. StoreCredentials and
// FileCredentials open a Provider's credential for one request, from the
// sealed row and the key encryption keys in server mode and from the
// process environment in file mode. KeyCache and Limiter are spec 007's
// Key lookup and windows on one replica, Catalog is spec 008's view of
// desired state for the doors, and RenderKey and RenderBudget fill the
// read-time status the API returns. For spec 011, ClientAddress and
// LimitUnauthenticated are the client address rule and the per-address
// bucket in front of both planes, ObjectOwners answers the owner policy
// over the store, AppendEvent writes an event of spec 012's shape, and
// DiscardRecorder is the doors' recorder until spec 009's replaces it.
// The /v1 handlers are internal/api's; cmd/luxd wires and nothing more.
package serve
