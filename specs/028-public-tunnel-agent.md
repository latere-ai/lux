---
title: Public tunnel client for platform CLIs
status: in-progress
track: core
depends_on:
  - 013-tunnelled-runtimes.md
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
