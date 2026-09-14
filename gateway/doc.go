// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package gateway is the data plane of spec 004 as one http.Handler: the
// four dialect doors, /openai, /anthropic, /gemini, and /lux, each
// serving its dialect's own API under its prefix. A request presents a
// Key, names a model, and is authenticated, resolved to a Model and a
// target, checked against the Key's limits, and forwarded to the target's
// provider with that provider's credential, translated through
// latere.ai/x/pkg/llmdialect/bridge when the door's dialect and the
// target's differ and passed through byte for byte when they are the
// same, the bridge also writing every door's error envelope, model list,
// count answer, and stream error frame and reading every usage. Every
// refusal is a fixed code in the door's own error shape, raised before a
// byte reaches a provider, and every request ends in one Record.
//
// The handler computes and drives. It holds no store, no identity, and
// no HTTP server: the Key lookup, the catalog, the credentials, the
// target order, the windows, the record sink, and the upstream clients
// are the interfaces of Options, satisfied by luxd's serve role or by a
// platform that mounts the handler in its own binary. It dials nothing
// but the providers, through the ClientSource the importer constructs.
package gateway
