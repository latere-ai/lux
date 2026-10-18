---
title: "Key doors: a Key may name the doors it opens, and a request on any other door is refused at the Key, without a store read"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/007-keys-and-limits.md
  - specs/011-api.md
affects: [manifest/, gateway/, authorizer/, internal/api/, internal/luxcli/, internal/store/, api/openapi.yaml, docs/, skills/lux/, test/conformance/, specs/003-manifest-contract.md, specs/004-request-path.md, specs/007-keys-and-limits.md, specs/011-api.md]
effort: small
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Key doors

## Overview

The data plane is four doors, `/openai`, `/anthropic`, `/gemini`, and
`/lux`, and every Key opens all four ([[004-request-path]]). A platform
that hands a Key to a program it does not run, and knows which API the
program speaks, has no way to say so: the Key reaches every Model its
selectors match through any door, so a program can take it to a dialect
its author never declared, and nothing in the request history says the
Key was meant for one door.

A Key gains `spec.doors`, the doors it opens. A request on a door the
list does not name is refused `door_not_allowed` at the Key, before any
Model, Provider, window, or upstream is read, and recorded as every Key
refusal is. A Key without the member opens every door, as today.

## Current state

On 2026-10-18:

- `KeySpec` (`manifest/v1/key.go`) holds the Models a Key may name, its
  limits, its Budgets, its expiry, `allowUnpriced`, `passthrough`,
  `zeroRetention`, and `disabled`. Nothing on a Key names a door.
- The handler (`gateway/handler.go`) reads the door from the path at
  stage 1, sets the record's `door`, and decides the Key at stage 3 in
  `admit`: `keyState` refuses `key_disabled` and `key_expired`. `admit`
  is also what a Key reread before `model_not_allowed` passes through
  ([044-key-reread-before-model-refusal](.archive/044-key-reread-before-model-refusal.md)).
- A door is spelled as its dialect, `v1.Dialect`: the record's `door`,
  the usage dimension `door`, and the span attribute `lux.door` carry
  `openai`, `anthropic`, `gemini`, or `lux`.

## Design

### The Key

`Key.spec.doors`, a list of door names, absent by default, mutable, not
secret. Each item is a door spelled as the record's `door` is: `openai`,
`anthropic`, `gemini`, or `lux`. Absent, the Key opens every door. The
member is written only when set, the rule of spec 047's amendment of
2026-10-10 ([[047-zero-retention-keys]]): a Key without it is the same on
the wire as before the member existed, so an authorizer or a client that
holds a Key to the members it knows still reads every Key that does not
use it. The authorizer's proposal carries the list, so a platform can
admit or refuse a Key by the doors it names.

| Rule | Code | Path |
|---|---|---|
| an item that is not a door name | `invalid_field` | `spec.doors[i]` |
| an item named twice | `invalid_field` | `spec.doors[i]` |
| an empty list | `invalid_field` | `spec.doors` |

An empty list is refused rather than read as every door: a writer that
sends one most likely means no door, and reading it as every door would
widen the Key without a word. A Key meant to open every door leaves the
member out. The list is kept in the order written.

### The door

At stage 3, after the Key's state, a request on a door the Key's
`spec.doors` does not name is `door_not_allowed`, 403, in that door's
error shape, with `Lux-Error` and the developer detail naming the Key
and its doors. The order of the Key refusals is `unauthenticated`,
`key_disabled`, `key_expired`, `door_not_allowed`, then stage 4's
`route_not_allowed`, so a Key refused here is refused on every route of
the door: model routes, opaque routes, and the door's own model list and
model read.

The decision reads the Key the cache holds and the path the request
came in on: no store read, no authorizer call, no upstream. A Key reread
before `model_not_allowed` passes through `admit` again, so the reread
Key's doors decide.

### When the Key changes

A change to `spec.doors` is a Key update: it raises `key.updated` in the
journal, which evicts the Key from every replica's cache at the
replica's next journal read, one second by default
([036-catalog-in-memory](.archive/036-catalog-in-memory.md)). Until
then a replica may decide the Key as it was cached; past its window a
cached Key is served only while the store does not answer
(`LUX_KEY_CACHE_GRACE`). A refusal here does not reread the Key past
the cache, as spec 044 does for a model: a Key taken to a door it does
not open repeats the request, and each repeat would cost a store read.
A platform that adds a door to a live Key and calls through it at once
may meet the old list for that window.

### The record

A refused request is recorded as every Key refusal is: `status`
`refused`, `error` `door_not_allowed`, the record's `door`, the Key's
id, prefix, owner, and labels, the request's labels, no attempt, zero
tokens, and zero cost. `lux_requests_total` counts it with its door,
`status="refused"`, and `code="door_not_allowed"`, and the request log
line carries it, as for every code ([[019-observability]]).

### The command and the document

`lux keys create` gains `-doors`, the door names, comma separated. The
OpenAPI document describes `spec.doors` with its four values, and
`door_not_allowed` is a row of `x-lux-errors`.

## Not in this spec

- A restriction narrower than a door, by route or by operation:
  `passthrough` is the switch between the translated and the opaque
  routes ([[004-request-path]]), and a door's routes are its dialect's
  API.
- Doors on a Model or a Budget: a workload holds a Key, and the Key is
  the one object a caller cannot leave out of a request.
- Hiding from a door's model list the Models of a Key that may not use
  that door: the list is refused on that door with the rest.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `Key.spec.doors` decodes, is absent by default and rendered only when set, resolves to a fixed point, is mutable, and survives a store round trip over the memory store and the Postgres store | corpus entries; `manifest` test; `internal/api` apply test; `storetest` |
| 2 | An item that is not a door name, an item named twice, and an empty list are `invalid_field` at their paths | corpus entries under `refused/`; `manifest` test |
| 3 | The authorizer's proposal of a Key carries `spec.doors` when set and no such member when not | `authorizer` test |
| 4 | A Key naming one door is served on it and refused `door_not_allowed`, 403, in each other door's error shape, with no upstream called; the door's model list, its model read, and an opaque route are refused alike, the door's refusal before `route_not_allowed`; a disabled or expired Key answers its state first; a Key without the member is served on every door | `gateway` test |
| 5 | The refusal's record says `refused` with the code, the door, the Key, and the request's labels, with no attempt, and `lux_requests_total` counts it under its door and code | `gateway` test |
| 6 | A Key reread before `model_not_allowed` whose doors no longer name the door is refused `door_not_allowed` | `gateway` test |
| 7 | `lux keys create -doors` writes the list, and `-dry-run` prints it | `internal/luxcli` test |
| 8 | The OpenAPI document carries the member and the code, and the code is one row of spec 011's table | `TestOpenAPIIsCurrent`, `TestErrorTable` |
| 9 | Over the wire, a Key naming one door is served there and refused on another, and the request history carries the refusal | `test/conformance` |
