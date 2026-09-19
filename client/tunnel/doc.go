// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package tunnel attaches a local model runtime to a Lux Provider over HTTP/2.
//
// Apply a tunnelled Provider with client.Client, then call Run with its name and
// a token source such as client.StaticToken or client.FileToken. The source may
// be called concurrently and must be safe for that use. New bearer values are
// used on subsequent requests and sent in the next session heartbeat.
//
// Run owns one session and joins all its goroutines before returning. The caller
// owns reconnect policy: ReasonDraining permits a retry, while other close reasons
// normally require a changed token, Provider, or attachment. Cancellation is a
// clean stop. RefusedError describes an HTTP refusal before a session opened.
//
// The default gateway client supports TLS HTTP/2 and plaintext h2c. A custom
// Options.Client must support full-duplex HTTP/2 and must not impose a timeout
// on the whole session. The default runtime client bypasses proxy environment
// variables; its address stays on the attaching machine.
package tunnel

import "latere.ai/x/lux/internal/tunnel/wire"

const (
	// ReasonSuperseded means another agent attached the same Provider.
	ReasonSuperseded = wire.ReasonSuperseded
	// ReasonTokenExpired means no fresh bearer arrived before expiry.
	ReasonTokenExpired = wire.ReasonTokenExpired
	// ReasonProviderDeleted means the Provider no longer exists.
	ReasonProviderDeleted = wire.ReasonProviderDeleted
	// ReasonDraining means the replica is shutting down; reconnect elsewhere.
	ReasonDraining = wire.ReasonDraining
)
