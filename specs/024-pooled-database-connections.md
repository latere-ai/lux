---
title: Separate pooled serving and direct migration connections
status: in-progress
track: core
depends_on:
  - 010-state.md
affects:
  - internal/config/
  - internal/store/postgres/
  - internal/check/check.go
  - cmd/luxd/main.go
  - docs/configuration.md
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Separate pooled serving and direct migration connections

## Overview

Operators may put serving queries through a transaction pooler while schema
migrations use a direct database connection. Replica count then need not
multiply the database's backend connection budget.

## Current state

`LUX_DB_URL` supplies both pgxpool and the migration driver's URL. Migrations
use session advisory locks and must not use a transaction pooler. The pool
also holds one idle connection even when the gateway has no traffic.

## Design

Add optional `LUX_DB_POOL_URL` for serving. `LUX_DB_URL` remains required and
supplies the direct migration endpoint. Both URLs must use the same database
path; pooled hosts and ports may differ. Without the new variable existing
installations keep their direct serving connection. Neither URL is echoed.
The runtime pool has zero minimum connections and uses pgx's extended-query
execution without a prepared statement cache when a pooled URL is configured.
Rewrap and `luxd check` use the same serving selection; migration always uses
the direct endpoint. No defaults mention a particular hosting provider.

## Acceptance criteria

- Configuration refuses a pool URL without a direct URL and rejects invalid
  URLs without printing credentials.
- Store tests prove that pool configuration uses the pooled endpoint while
  its migration URL still names the direct endpoint.
- An integration test migrates and serves through distinct database roles:
  the serving role cannot change the schema, so migrations cannot accidentally
  run on the serving connection.
- Existing direct-mode and Postgres conformance tests remain green.
- The generated environment reference documents the new variable.
