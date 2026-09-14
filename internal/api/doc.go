// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package api is the control plane of spec 011: one HTTP surface under
// /v1 with one grammar for every kind. Apply is PUT by name and runs
// Decode and Resolve of the manifest package against the object the
// store holds, then writes the object, its journal row, and a Key's hash
// or a Provider's sealed credential in one transaction; read is GET by
// id or name; list pages with limit and cursor under the authorizer's
// filter; delete is DELETE; a Key has one verb, rotate. Every request
// carries a bearer through internal/auth and asks the authorizer the
// action of its route before it acts, and every refusal is one envelope
// with one code from the error table, which is both planes' table in
// one place. /v1/self reports the caller, /.well-known/lux the server,
// and /v1/openapi.json the document generated from the Go types and
// the route table. In the file mode the surface is read-only and needs
// no bearer, and cmd/luxd mounts it on the internal listener alone.
package api
