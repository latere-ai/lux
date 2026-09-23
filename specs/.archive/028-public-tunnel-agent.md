---
title: Public tunnel client for platform CLIs
status: complete
track: core
depends_on:
  - 013-tunneled-runtimes.md
  - 014-agent-client.md
affects:
  - client/tunnel/
  - internal/tunnel/
  - internal/luxcli/
  - internal/arch/
  - test/e2e/
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Public tunnel client for platform CLIs

## Overview

A platform CLI must attach a local runtime to a core Provider using the same
HTTP/2 protocol as lux serve. It needs a public client, without copying the
protocol or importing internal server packages.

## Design

Move the existing agent implementation into client/tunnel. Run serves one
session and joins its goroutines before returning. Options, typed close/refusal
errors and carrier constants remain the transport API. Export the four close
reason constants. Use client.StaticToken and client.FileToken instead of adding
a second token-file helper. The caller owns reconnect policy.

Keep internal/tunnel/wire private and shared with the server. The architecture
check permits client to reach that exact package only, preserving every other
internal-package prohibition. No identity or database dependency is introduced.

Document concurrent token calls, fresh heartbeat tokens, HTTP/2-capable custom
clients without whole-session timeouts, and local runtime proxy bypass.

## Acceptance criteria

- lux serve and server tests use the public client without protocol changes.
- Cancellation, streaming, refreshed tokens, refusal and close reasons retain
  their existing tested behavior; Run leaves no goroutines behind.
- A public-import-only integration test applies a Provider, attaches the client,
  sends inference through a real luxd and cancels cleanly.
- The dependency gate permits only the shared private wire codec.

## Outcome

Moved the existing agent to client/tunnel, exported close reasons, and switched
all in-tree consumers. The duplicate token-file helper was removed; public
client token sources already provide it. The private wire codec remains shared,
with an exact-path architecture exception and a regression rejecting broader
internal access. No protocol or reconnect behavior changed.

The public-import-only real-process test applies a Provider, forwards streamed
and nonstreamed inference, refreshes the bearer beyond the original expiry,
and cancels cleanly. Existing lifecycle, reconnect and refusal tests pass.
Race coverage: public client 98.2%, tunnel client 91.7%, server tunnel 91.0%,
wire 94.3%, CLI 93.1%. Full tests, architecture, lint, vet and build pass.
No deviations or deferred criteria.
