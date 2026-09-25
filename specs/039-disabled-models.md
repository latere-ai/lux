---
title: "Disabled Models: a Model can be disabled, and a disabled Model is refused at call time"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/011-api.md
  - specs/018-conformance-suite.md
  - specs/.archive/036-catalog-in-memory.md
affects: [manifest/, gateway/, internal/serve/, internal/api/, test/conformance/, examples/plane/, skills/lux/, docs/, specs/003-manifest-contract.md, specs/004-request-path.md, specs/011-api.md]
effort: small
created: 2026-09-25
updated: 2026-09-25
author: changkun
---

# Disabled Models

## Overview

An operator who wants to stop a Model from being called today deletes
it, and every Key whose selectors named it keeps a selector that matches
nothing until the Model is declared again. A Model gains `spec.disabled`:
while it is true every door refuses a call to the Model and leaves it
out of every model list, and setting it back to false restores both,
with no Key changed.

## Current state

On 2026-09-25:

- A door resolves the Model a call names from the in-memory catalog of
  [036-catalog-in-memory](.archive/036-catalog-in-memory.md), then holds
  it to the Key's selectors: `model_not_found`, 404, for a name the
  catalog lacks, `model_not_allowed`, 403, for one no selector matches
  (`gateway/handler.go` `lookupModel`).
- A door's model list is the Models the Key's selectors match whose
  `status.available` is true (`gateway/handler.go` `listModels`); the
  one-entry read answers a Model the list leaves out, because it exists
  and the Key may name it.
- A passthrough route reaches the Providers of every Model the Key may
  name (`gateway/forward.go`).
- A Model has no field that stops it being called; an apply, a delete,
  and discovery's create and remove are the only ways its presence
  changes.

## Design

### The field

`Model.spec.disabled`, a bool, default false, mutable. It is written
only by an apply: discovery keeps the value of the Model it updates, and
the health job writes `status`, never `spec`. It is omitted from a
rendered Model while false, so a Model that never set it renders as it
did.

### At the door

A call that names a disabled Model, literally or through a selector's
glob, is refused after `model_not_found` and `model_not_allowed`:

| | Code and status | For | Against |
|---|---|---|---|
| A | `model_disabled`, 403 | the Model exists and the Key may name it, so the refusal says what the caller can act on; it is a standing refusal by policy, as `key_disabled` is, and a retry does not help until an operator re-enables it | a caller learns the name exists, which the Key's selectors already let it name |
| B | `model_not_found`, 404 | hides the Model entirely | a caller cannot tell a mistyped name from a Model an operator stopped, and the answer changes when it is re-enabled without the name having changed |

**Recommendation: A.** The order stays the one the caller can act on:
a name that does not exist, then a name the Key may not use, then a
Model that is stopped. The refusal is decided from the snapshot, so it
holds on the replica that took the apply at once and on every other
within its journal tail, with no authorizer call per request.

### Lists and reads

A door's model list leaves out a disabled Model, as it leaves out one
whose `status.available` is false. The one-entry read of a disabled
Model is `model_disabled`, so the list and the read agree that the Key
cannot call it. A passthrough route does not reach a Provider through a
disabled Model.

### Keys

A Key whose selectors name a disabled Model is unchanged: its selectors
resolve as before, `model.use` is not asked again, and when the Model is
enabled the next call through it is served.

## Not in this spec

- A disabled Provider, which a Model's targets and health already route
  around.
- Disabling a Model for some Keys and not others, which is the Key's own
  selectors.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `spec.disabled` decodes, defaults to false, renders only when true, and is mutable | `manifest` corpus entries; `internal/api` apply test |
| 2 | A call naming a disabled Model, literally and through a glob, is `model_disabled`, 403, on every door, after `model_not_found` and `model_not_allowed`, and the call is recorded refused | `gateway` handler test; `internal/serve` door test over the snapshot; conformance `case039DisabledModel` |
| 3 | The replica that took the apply refuses at once and another replica within its journal tail, with no authorizer request per call | `internal/serve` snapshot test with a recording authorizer |
| 4 | Every door's model list leaves out a disabled Model and its one-entry read is `model_disabled`; a passthrough route reaches no Provider through it | `gateway` handler tests; conformance `case039DisabledModel` |
| 5 | A Key naming a disabled Model keeps its selectors, and after `spec.disabled` returns to false its next call is served | `internal/serve` test; conformance `case039DisabledModel` |
| 6 | Discovery's update of a discovered Model keeps the value it finds, and the health job never writes it | `internal/serve` discovery test |
| 7 | The code is in the door table of [[004-request-path]], the API table of [[011-api]], the OpenAPI document, and the agent skill | `TestOpenAPIIsCurrent`; the error table tests |
