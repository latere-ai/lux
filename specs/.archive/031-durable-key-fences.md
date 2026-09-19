---
title: Durable Key write fences
status: complete
track: core
depends_on:
  - 010-state.md
affects:
  - internal/store/
  - docs/
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Durable Key write fences

## Overview

A control plane must stop delayed Key mutations before reporting revocation
complete. A request timeout does not prove that its transaction will not commit.
A durable fence closes a Key name to future credential creation, rotation and
policy expansion, including writes authorized before the fence was installed.

## Current state

`Objects.Put` and `Keys.Put` commit through `Store.Transact`. Memory serializes
transactions; Postgres uses transaction-scoped stores and per-operation savepoints.
Neither retains a prohibition after a Key is deleted. Inference uses the hash
index and a bounded cache, independently of management authorization.

## Design

Add `Store.KeyFences()` with `Put(ctx, KeyFence)` and `Get(ctx, name)`.
A fence contains name, expected owner, exact expected labels and creation time.
The store owns creation time. Put is idempotent for identical name/owner/labels;
a different assertion or an existing live Key with different owner or labels
returns `ErrFenceConflict`. An absent name can be fenced. Labels and owner are
assertions checked inside the same transaction, not a source of authorization.
The following HTTP slice supplies explicit authorization before calling Put.

Fences cannot be removed and survive Key deletion and pruning. They deny creates
(including disabled creates), credential hash writes and renames into or out of
the fenced name. A current Key may be updated only to `disabled: true`, preserving
its metadata, owner, credential prefix, resolved expiry, budget reference and all
other spec fields. Reads, observed-status writes and deletion remain available.
Denials return `ErrKeyFenced`; unfenced behavior remains unchanged.

Memory stores fences in its rollback snapshot. Postgres adds one table and uses
one transaction advisory lock shared by fence installation, Key object writes
credential writes and Key deletion. The lock is held until the enclosing transaction commits,
including when an operation uses a savepoint. A global control-plane lock avoids
absent-name and rename lock-order races; inference never acquires it. Queries
checking fences run after acquiring the lock under READ COMMITTED isolation.
Other isolation levels are rejected for these operations. A successful Put
therefore waits for earlier credential transactions and prevents later ones.
File mode returns `ErrReadOnly` for fence writes.

The schema addition is additive. Every writable replica must support fences
before operators use them; rolling back to an older writer while fences exist
is unsupported. This slice does not expose HTTP, declare inference revoked,
disable existing Keys, cancel admitted streams, or implement tenant policy.
A later slice combines this primitive with exact disable, cache expiry and
control-plane reconciliation before reporting enforcement complete.

## Acceptance criteria

- Shared memory/Postgres conformance covers absent and occupied names, identical
  replay, conflicting owner/labels, create/update/rotate, deletion/recreation,
  rename in both directions, exact disabled-only updates and rollback.
- Two independent Postgres connections demonstrate both fence/write lock orders:
  a fence waits for an earlier transaction; a delayed writer cannot cross an
  acknowledged fence, including across a new connection.
- Fence state and input/output maps cannot be changed by caller aliasing.
- File mode refuses mutation. Existing store tests continue passing. New logic
  targets greater than 90% coverage; the HTTP slice owns end-to-end API tests.

## Outcome

Implemented the immutable fence collection, memory snapshots, additive Postgres
migration, transaction-scoped serialization, exact disable-only comparison,
read-only wrappers and instrumentation. Key deletion shares the writer lock to
keep its lock order consistent with mixed credential transactions.

The shared suite covers occupied/absent names, map isolation, conflicting replay,
all preserved policy fields, rename in both directions, rollback, savepoints,
pruning and recreation. Real Postgres tests use independent connections in both
transaction orders, reconnect after installation, reject stale isolation and fail
closed for missing/corrupt storage. All store race tests and the full Lux suite
pass. Store/memory coverage is 98.8%; Postgres is 93.1%; new memory fence and shared
assertion/disable helpers are 100%. Vet and ordinary lint pass. Tagged lint of the
change passes; a full tagged lint also reports 20 pre-existing findings elsewhere
in rows_test.go (context propagation and literal modernization).

HTTP exposure and an inference revocation completion claim remain outside this
slice. Operators must upgrade every writer before activating fences.
