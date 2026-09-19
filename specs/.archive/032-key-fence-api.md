---
title: Authorized Key fence endpoints
status: complete
track: core
depends_on:
  - .archive/031-durable-key-fences.md
  - 022-authorizer-vocabulary-package.md
affects:
  - authorizer/
  - internal/api/
  - internal/auth/
  - api/
  - docs/
effort: medium
created: 2026-09-19
updated: 2026-09-19
author: changkun
---

# Authorized Key fence endpoints

## Overview

A controller needs an idempotent HTTP operation that stops delayed credential
writes, including creates for a name that does not yet exist. Expose the durable
storage fence with explicit authorization, a bounded assertion body and readback.

## Design

`POST /v1/keys/{name}/fence` accepts only JSON `{ "owner": "issuer|subject",
"labels": { ... } }`. Labels default to an empty object. Duplicate/unknown fields,
non-string labels, malformed subjects and Key names are rejected; the normal
manifest body limit applies. A fence is addressed only by name. Conditional Key
headers are invalid because the assertion is immutable and installation includes
its own transactional occupant check.

Authorization uses new action `key.fence` and resource kind `KeyFence`, with the
name as resource id and fields name, owner and labels. These fields are the
requested assertion. An authorizer must independently establish permission for
that assertion; storage then validates the actual occupant under its write lock.
The built-in owner policy reserves fencing for configured administrators.

`GET /v1/keys/{name}/fence` authorizes `key.fence.read` against the same resource
kind and name/id before looking up the fence. It returns 404 when no fence exists.
The read resource contains no owner or labels. Ordinary Key permissions grant
neither action. Existing authorizers that do not recognize the actions fail closed.

First installation appends one `key.fenced` event atomically with the fence.
Its object kind is `KeyFence`, stable journal id is `key-fence/<name>`, and its
object contains the asserted name, owner and labels; data is `{}`. The store Put
result adds an inserted boolean decided under its write lock. Identical replay
never appends another event, even after acknowledgment/pruning. Journal failure
rolls back installation. This preserves spec 012's mutation audit invariant;
`key.fenced` does not invalidate inference caches or claim a disabled credential.

Both successful endpoints return 200 with `{name,owner,labels,createdAt}`. POST
replay preserves creation time; conflicts return `fence_conflict` (409). A fenced
credential or unsafe policy write returns `key_fenced` (409), independently of a
previous authorization allow. No secret/hash/policy appears in fence responses.
File mode refuses POST and has no fence records. Requests retain normal identity,
rate limits, request IDs and error envelopes. OpenAPI and authorizer vocabulary
include the new endpoints, schemas, resources and errors.

Success means the name is closed to later writes. Existing enabled Keys still
require exact conditional disable and cache drainage. This API does not claim
revocation completion, mutate tenant policy or terminate admitted streams. Every
writer must support fences before the feature is used; older writers cannot be
part of the deployment or a rollback while fences exist.

## Acceptance criteria

- HTTP tests create a Key, install/read/replay a fence, reject rotation and
  re-enablement, allow exact conditional disable and deletion, and reject recreate.
- An absent name can be fenced; conflicting occupant/assertion fails without
  fencing another owner. Authorization denial installs nothing and read denial
  occurs before existence lookup.
- Validation covers malformed/duplicate/unknown input, body limits, name/id
  addressing, conditional headers, missing identity and read-only mode.
- HTTP authorization resource tests expose exactly the requested assertion and
  no credential. Owner policy admits configured admins only for both actions.
- Concurrent replay, replay after journal pruning, journal rollback and signed
  sink delivery verify exactly one journal insertion (delivery remains at least once).
- A real-process integration test proves the lifecycle end to end. New logic
  targets greater than 90% coverage; generated API documentation stays current.

## Outcome

Implemented on 2026-09-19. POST/GET expose name-addressed immutable fences with
explicit authorization, strict bounded input and no credential output. First
installation and its signed `key.fenced` journal record commit atomically;
identical retries append nothing, including after journal pruning.

HTTP race tests prove already-authorized create/update/rotation cannot commit
through a later fence. Store conformance and real PostgreSQL tests cover one
insertion under concurrent replay. The real-process lifecycle verifies fencing,
conditional disable, inference refusal, delete and denied recreation. Endpoint
coverage is 100%; strict assertion decoding exceeds 90%. API/events race tests,
PostgreSQL tests, lint, vet and generated documentation checks pass. Full-suite
checks also identified and corrected vocabulary table ordering and an audit
fixture lacking a stable object ID.

A follow-up in spec 033 lets exact disable succeed after reference permissions,
expiry or issuance ceilings have changed. Fence success alone still makes no
inference-revocation claim.
