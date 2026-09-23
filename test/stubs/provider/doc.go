// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package provider is the stub provider of spec 015: one http.Handler
// parameterised by dialect, serving the upstream paths of spec 005's
// dialect table, recording every request it receives, and answering as
// a function of the request, so a test asserts a value rather than a
// shape.
//
// The assistant content is stub:<dialect>:<model>:<first 8 hex of the
// SHA-256 of the last user text>, the usage members are the route's and
// report 100 input and 20 output tokens unless the upstream model name
// says otherwise, and a streamed answer is five content events followed
// by the route's final usage event. Failure is injected by upstream
// model name, the one string the gateway rewrites onto the wire on every
// route, or by the Lux-Stub-Fail header for a test that drives the stub
// directly; Behaviors lists the table.
//
// The control routes sit under /_, which no dialect route uses: GET
// /_received returns every request the instance received, in order, and
// DELETE /_received clears the record.
package provider
