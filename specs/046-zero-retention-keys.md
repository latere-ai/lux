---
title: "Zero-retention Keys: a Key that asks for zero retention reaches only Providers that declare how they keep nothing of a request, with the Provider's zero-retention request fields written into every request"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/007-keys-and-limits.md
  - specs/011-api.md
affects: [manifest/, gateway/, internal/api/, internal/store/, api/openapi.yaml, docs/, skills/lux/, deploy/catalog/, test/conformance/, specs/003-manifest-contract.md, specs/004-request-path.md, specs/007-keys-and-limits.md, specs/016-security-and-threat-model.md]
effort: medium
created: 2026-10-09
updated: 2026-10-09
author: changkun
---

# Zero-retention Keys

## Overview

A Provider's `spec.requestFields` (spec 042) writes an upstream option
into every request the gateway sends that Provider. An aggregator such
as OpenRouter takes `{"provider": {"zdr": true}}`, which routes a
request only to endpoints whose terms say they keep nothing of it. That
option holds for every caller of the Provider or for none, while the
choice to keep requests from providers that may keep them belongs to
whoever the requests are made for: one workload asks for it and the
next does not, through the same Provider.

A Key gains `spec.zeroRetention`. A request on a Key that asks for it
reaches only a Provider that declares how it keeps nothing of a
request, `spec.zeroRetention` on the Provider, and carries that
Provider's zero-retention request fields, merged after its
`requestFields` by spec 042's rule, so neither the caller nor the
Provider's general fields can turn them off. A request the gateway
cannot send that way is refused before any upstream reads it.

## Current state

On 2026-10-09:

- `KeySpec` (`manifest/v1/key.go`) holds the Models a Key may name, its
  limits, its Budgets, its expiry, `allowUnpriced`, `passthrough` and
  `disabled`. Nothing on a Key changes what the gateway writes into a
  request.
- `ProviderSpec.RequestFields` (`manifest/v1/provider.go`) is merged
  into every body the gateway sends the Provider by `decorate`
  (`gateway/forward.go`), the one place both request builders,
  `outbound` and `forwardOpaque`, pass through. A body that is not a
  JSON object is sent unchanged.
- `selectTargets` (`gateway/handler.go`) orders the called Model's
  targets; a request falls over from one to the next on the failures
  spec 008 names. On an opaque route the caller names the Provider with
  `Lux-Provider`, among those the Key's selectors reach.

## Design

### The Key

`Key.spec.zeroRetention`, a boolean, false by default, written without
`omitempty` as every boolean of a spec is, mutable. A Key that sets it
asks that no request on it reach an upstream that may keep it. It is
not secret and the authorizer's proposal carries it.

### The Provider

`Provider.spec.zeroRetention`, an object, absent by default, mutable,
with one member:

- `requestFields`, a JSON object under every rule of spec 042's
  `requestFields` (the reserved top-level members, no null value, the
  `manifest.MaxRequestFieldsBytes` bound), each checked at its own path
  under `spec.zeroRetention.requestFields`.

A Provider declares one of three things:

| `spec.zeroRetention` | What a request on a zero-retention Key does |
|---|---|
| absent | is never sent to the Provider |
| `{}` | is sent as any request is: the operator states that the upstream keeps nothing of any request |
| `{"requestFields": {...}}` | is sent with the fields merged after `spec.requestFields`, the zero-retention fields winning on every member both name |

The declaration is the operator's statement about the upstream's
terms. The gateway enforces that a zero-retention Key's requests reach
only Providers that make it and carry the fields they name; it cannot
check the upstream's terms.

### Which Providers a request may reach

For a request on a zero-retention Key:

- **Model routes.** `selectTargets` drops every target whose Provider
  does not declare `spec.zeroRetention`, before ordering, so no
  failover reaches one. With no target left the request is refused
  `zero_retention_unavailable`, 403, and no upstream is called. A
  Model with a declaring target and a non-declaring one serves the Key
  through the first alone.
- **Opaque routes.** A Provider named with `Lux-Provider`, or the only
  one the Key's selectors reach, that does not declare
  `spec.zeroRetention` is refused `zero_retention_unavailable`.
- **Every route.** `decorate` checks again before it writes, so no
  builder of an upstream request reaches a non-declaring Provider with
  such a Key.

A request on a Key without `zeroRetention` is routed as today, to
declaring and non-declaring Providers alike, and its body never takes
the zero-retention fields.

### What the gateway writes

`decorate` writes `spec.requestFields`, then, for a zero-retention Key,
`spec.zeroRetention.requestFields`, both by spec 042's merge. Where the
zero-retention fields name members:

- a body that is not a JSON object and is not empty is refused
  `invalid_request`, because the fields cannot be written into it and
  an upstream reading it would read it without them (spec 042 sends
  such a body unchanged; a zero-retention Key does not). A multipart
  upload and a form body on an opaque route are such bodies;
- a body under a `Content-Encoding` other than `identity` is
  `invalid_request`, as spec 042 already answers;
- an empty body, a `GET` among them, is sent as it is: it carries
  nothing an upstream could keep.

### When the Key changes

A change to `spec.zeroRetention` is a Key update: it raises
`key.updated` in the journal, which evicts the Key from every replica's
cache at the replica's next journal read (spec 036, one second by
default). Until then a replica may serve the Key as it was cached; past
its window a cached Key is served only while the store does not answer
(`LUX_KEY_CACHE_GRACE`). A platform that turns the flag on states this
bound to whoever relies on it.

### The record

The request record, the request log line and the `lux.upstream` span
carry nothing new: which Provider served a request is already
recorded, and the Key's spec is readable at the API. A refusal is
recorded with its code as any refusal is.

### Validation

| Rule | Code | Path |
|---|---|---|
| `Key.spec.zeroRetention` is a boolean | `invalid_field` from the schema walk | `spec.zeroRetention` |
| `Provider.spec.zeroRetention` is an object with no member but `requestFields` | `unknown_field` | `spec.zeroRetention.<name>` |
| `spec.zeroRetention.requestFields` under spec 042's rules | spec 042's codes | under `spec.zeroRetention.requestFields` |

## Not in this spec

- Zero retention per Model, per Budget or per request header: a
  workload holds a Key, and the Key is the one object a caller cannot
  leave out of a request.
- Hiding from a zero-retention Key's door model list the Models it
  cannot reach: the list answers what the Key's selectors match, and a
  request to a Model with no declaring target is refused at once.
- Checking an upstream's terms: the declaration is the operator's.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `Key.spec.zeroRetention` decodes, defaults to false, renders, resolves to a fixed point, is mutable, and survives a store round trip | corpus entries; `internal/api` apply test; `storetest` |
| 2 | `Provider.spec.zeroRetention` decodes absent, `{}` and with `requestFields`; its fields are refused under spec 042's rules at their own paths; an unknown member is refused | corpus entries under `accepted/` and `refused/`; `manifest` validation test |
| 3 | A request on a zero-retention Key reaches the upstream with the zero-retention fields merged after `requestFields`, winning over the caller's and the Provider's general fields, on passthrough and translation, stream and not | `gateway` forward test against a stub upstream |
| 4 | A request on a Key without the flag reaches the same Provider without the zero-retention fields | the same test |
| 5 | A Model whose targets declare nothing refuses a zero-retention Key `zero_retention_unavailable`, 403, with no upstream called; a Model with a declaring and a non-declaring target serves it through the declaring one alone, failover included | `gateway` routing test |
| 6 | On an opaque route a non-declaring Provider is refused; a non-empty body that is not a JSON object is `invalid_request` where fields are to be written; an empty body is sent | `gateway` opaque test |
| 7 | A Provider declaring `{}` serves a zero-retention Key with nothing added | `gateway` forward test |
| 8 | A Key update that sets the flag reaches the gateway at the next journal read | `internal/serve` key cache test |
| 9 | The OpenAPI document carries both members and the code | `TestOpenAPIIsCurrent` |
| 10 | The conformance suite holds criteria 3 to 5 over the wire | `test/conformance` |
