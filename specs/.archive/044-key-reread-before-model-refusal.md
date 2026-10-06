---
title: "A Key read past the cache before a model refusal: a model granted through one replica is served through any other at once"
status: complete
track: core
depends_on:
  - specs/004-request-path.md
  - specs/007-keys-and-limits.md
  - specs/018-conformance-suite.md
affects: [gateway/, internal/serve/, test/conformance/, docs/, specs/004-request-path.md, specs/007-keys-and-limits.md, specs/019-observability.md]
effort: small
created: 2026-10-06
updated: 2026-10-06
author: changkun
---

# A Key read past the cache before a model refusal

## Overview

Each replica caches a Key for `LUX_KEY_CACHE`, ten seconds by default,
and drops the entry earlier when its journal tail, once a second, reads
a row naming the Key ([[007-keys-and-limits]]). The replica that applied
a change tails at once, through the API's `Committed` hook; every other
replica serves its cached Key until its own tail or the window. For a
change that takes access away that is the documented bound. For a change
that grants it, a model added to `spec.models`, it is a refusal the
caller cannot explain: a consumer that widens a Key through one replica
and calls the new model through another inside that second of lag is
refused `model_not_allowed` by a replica holding the Key as it was. A
consumer that widens a Key to move a running session to another model
calls within milliseconds of the change, and with two replicas behind a
load balancer the call lands on the other one about half the time.

A door now reads the Key from the store once before it refuses
`model_not_allowed`, and the Key the store holds decides.

## Current state

On 2026-10-06:

- `gateway`'s `lookupModel` resolves the Model, then holds it to the
  request's Key with `allowed`; no match is `model_not_allowed` with the
  Key's selectors in the detail. `authenticate` computed the value's
  hash for `KeyLookup.ByHash` and kept only the Key.
- `serve.KeyCache.lookup` serves a fresh entry from the cache and reads
  the store on a miss: the read starts its window before it runs, and
  `publish` refuses an answer an invalidation overtook while it was in
  flight, so a read never extends authority; three such reads fail
  closed.
- The door's model list, `GET /{door}/v1/models`, filters the catalog
  through the cached Key's selectors; it names no single model.

## Design

### The door

`gateway` gains an optional interface beside `KeyLookup`:

```go
type KeyRefresher interface {
	Refresh(ctx context.Context, hash string) (*v1.Key, error)
}
```

A `KeyLookup` that also implements it is asked once per request, and
only when the cached Key selects none of the requested Model's names,
at stage 5 of [[004-request-path]]. The call keeps the hash
`authenticate` computed. What Refresh answers decides:

| Refresh answers | The door |
|---|---|
| a Key that selects the Model | adopts it, its identity and labels the record's and its limits the reservation's, and goes on |
| a Key that does not | `model_not_allowed`, the detail naming the store's selectors |
| a Key the facts disable or expire | `key_disabled` or `key_expired`, as at stage 3 |
| nil, no Key has the value any longer | `unauthenticated`, the record naming no Key, as a refusal at stage 3 does |
| an error | `model_not_allowed` on the cached Key, the error in the detail: a refusal the cache already decided is not turned into `store_unavailable` by an outage |

A `KeyLookup` without `Refresh`, a platform's own among them, refuses on
the cached Key as before. The model read route, `GET
/{door}/v1/models/{name}`, goes through the same stage and rereads the
same way. The model list and the opaque route's Provider choice do not:
neither names one model, so neither has a refusal to check.

### The cache

`serve.KeyCache.Refresh` reads the Key by hash past the cache and
publishes what it read, replacing the entry. It is `lookup` without the
cache hit: each attempt reads the store and publishes under the
generation the read began in, so an answer an invalidation overtook is
never returned and three such reads fail closed, the bounds `lookup`
already holds. It does not drop the entry first and does not raise the
generation, so a refresh neither fails other Keys' reads in flight nor,
when the store read fails, takes away the entry the grace of
[036-catalog-in-memory](036-catalog-in-memory.md) would serve to the
Key's other requests. A Key the store no longer holds leaves a negative
entry, as a miss would. Each call counts once in
`lux_key_cache_hits_total` with `result` `refresh`.

### The bound

One store read per refused request, two queries: the hash index and the
object. A Key that truly lacks the Model pays it on every such request;
no per-Key cooldown is added, because a cooldown is the window this
spec closes: a request refused, the Key widened, the request sent
again within milliseconds. Concurrent refusals are not coalesced into
one read either, because a request that joins a read begun before the
change it waits for is served the old Key. A Key reaches this stage
only after its value authenticated, so each read is spent by a holder
of a live Key on its own requests.

## Not in this spec

- The model list on a door, which follows the cache: an added model is
  listed within `LUX_KEY_CACHE`. A consumer that needs it listed at once
  rereads the list after the change, or calls the model.
- A change that takes access away, which still reaches a replica that
  has not consumed its row within `LUX_KEY_CACHE`.
- A per-Key ceiling on rereads, should one holder's refused requests
  become a measurable load; `latere.ai/x/pkg/ratelimit`, which the
  per-address bucket of [[011-api]] uses, would be its primitive.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | Two reference servers over one store: a Key widened through the first's API is served the new Model on the second's door within the second's cache window and with its tail an hour away, and refused there without the reread | `TestKeyWidenedOnOneReplicaServesOnTheOther`, `test/conformance` |
| 2 | Two data planes over one store: the same, with exactly one store read, after which the second plane's cache holds the widened Key and the next call reads nothing | `TestKeyWidenedThroughAnotherReplica`, `internal/serve` |
| 3 | A Key the store also refuses the Model is refused on every call with exactly one store read per refusal | `TestKeyLackingTheModelReadsOncePerRefusal`, `TestRereadIsOnePerRefusal` |
| 4 | The door adopts the store's Key for the record and the reservation; a served request rereads nothing; the model read route rereads and the model list does not | `TestRereadBeforeModelNotAllowed`, `TestRereadIsOnePerRefusal` |
| 5 | A failed reread is `model_not_allowed` with the error in the detail; a vanished Key is `unauthenticated` with no Key on the record; a disabled one is `key_disabled`; a `KeyLookup` without `Refresh` refuses on the cached Key | `TestRereadOutcomes` |
| 6 | `Refresh` replaces a fresh entry, counts `refresh`, leaves the entry on a failed read, and leaves a negative entry for a deleted Key | `TestRefresh` |

## Outcome

Built and verified on 2026-10-06 as designed.

| # | Test |
|---|---|
| 1 | `TestKeyWidenedOnOneReplicaServesOnTheOther`, over two reference servers given one memory store through `serverOptions.store`, the second with `keyTail` and `keyCache` of an hour |
| 2 | `TestKeyWidenedThroughAnotherReplica` |
| 3 | `TestKeyLackingTheModelReadsOncePerRefusal`, counting `Keys.ByHash` on an instrumented store; `TestRereadIsOnePerRefusal` at the door |
| 4 | `TestRereadBeforeModelNotAllowed`, `TestRereadIsOnePerRefusal` |
| 5 | `TestRereadOutcomes` |
| 6 | `TestRefresh` |

With the door's reread disabled, criteria 1 to 5 fail: the second
replica answers `model_not_allowed` naming the selectors it cached, and
a refusal reads the store zero times.

`lookup` and `Refresh` share one attempt, `fetch`, the read and the
publish under its generation, and one error for three overtaken reads,
so the two paths cannot drift on the guarantees.
