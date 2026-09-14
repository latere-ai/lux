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
// process environment in file mode. The /v1 routes mount here with spec
// 011; cmd/luxd wires and nothing more.
package serve
