---
title: Disable existing Keys after access has been withdrawn
status: in-progress
track: core
depends_on:
  - 032-key-fence-api.md
affects:
  - internal/api/
  - docs/api.md
effort: small
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Disable existing Keys after access has been withdrawn

## Overview

A controller must be able to disable an existing Key after its model or budget
permission has been withdrawn, its references removed, its plan ceilings lowered,
or its absolute expiry passed. Ordinary resolution currently refuses those
requests before the storage layer can accept a safe reduction.

## Design

Keep ordinary PUT and its authenticated `key.update` authorization, sanitized
proposal, owner check, version precondition, atomic journal and optimistic store
write. After those checks, recognize an exact disable-only proposal: metadata and
every spec field equal the persisted Key except `disabled: true`; credential
input must be absent. Clone the existing object, changing only that flag, without
resolving references or applying new default/ceiling/expiry rules. Preserve the
stored budget identity, expiry, selectors and credential identity. Explicit
status input remains rejected by Decode. This applies to fenced and unfenced Keys
and to an identical disabled replay. No broader policy change uses this path.

An authorizer can independently scope `key.update` to an exact staged disable.
Permission to use a model or draw from a budget is unnecessary for this reduction.
A stale explicit precondition still refuses and a concurrent write still conflicts.

## Acceptance criteria

- Reproducible HTTP tests fail before this change and pass after it for withdrawn
  model/budget permissions, missing references, reduced ceilings and expired Keys.
- Exact disable preserves persisted budget ID and expiry and emits a normal update
  event. Authorization denial and stale preconditions still refuse without writes.
- Any changed selector, limit, TTL, expiry, labels, budget or credential uses normal
  resolution and cannot exploit this path. New logic exceeds 90% coverage.
- The real-process fence lifecycle test disables after its model is deleted.
