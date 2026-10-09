---
title: "Zero-retention Keys: a Key that asks for zero retention reaches only Providers that declare how they keep nothing of a request, with the Provider's zero-retention request fields written into every request"
status: testing
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/007-keys-and-limits.md
  - specs/011-api.md
affects: [manifest/, gateway/, metering/, authorizer/, internal/api/, internal/serve/, internal/store/, api/openapi.yaml, docs/, skills/lux/, deploy/catalog/, examples/plane/, test/conformance/, specs/003-manifest-contract.md, specs/004-request-path.md, specs/007-keys-and-limits.md, specs/011-api.md, specs/016-security-and-threat-model.md, specs/019-observability.md]
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
  failover reaches one. When no target of the Model's spec names a
  declaring Provider, the request is refused `zero_retention_unavailable`,
  403, and no upstream is called: that answer is permanent for the Key
  and the Model. When declaring targets exist and none can be tried now
  (unhealthy, or its circuit open), the request is answered as any
  request with no target left is, `provider_unavailable`. A Model with a
  declaring target and a non-declaring one serves the Key through the
  first alone.
- **Opaque routes** are refused `zero_retention_unavailable` toward a
  Provider whose `spec.zeroRetention` names `requestFields`: an opaque
  route reaches the upstream's own API, whose storage endpoints (threads,
  files, batches, stored conversations) keep what they are sent whatever
  a routing option says. Toward a Provider that declares `{}`, whose
  upstream keeps nothing of any request, an opaque route is served as
  today. A Provider that declares nothing is refused as on a model
  route.
- **Every route.** `decorate` checks again before it writes, so no
  builder of an upstream request reaches a non-declaring Provider with
  such a Key, and it writes the zero-retention fields whether or not the
  Provider has `spec.requestFields`.

A request on a Key without `zeroRetention` is routed as today, to
declaring and non-declaring Providers alike, and its body never takes
the zero-retention fields.

### What the gateway writes

`decorate` writes `spec.requestFields`, then, for a zero-retention Key,
`spec.zeroRetention.requestFields`, both by spec 042's merge. Where the
zero-retention fields name members, on a model route (opaque routes are
refused above):

- a body that is not a JSON object is refused `invalid_request`, because
  the fields cannot be written into it and an upstream reading it would
  read it without them (spec 042 sends such a body unchanged; a
  zero-retention Key does not);
- a request carrying any `Content-Encoding` value other than `identity`,
  every value of the header read, is `invalid_request`;
- `GetBody` returns the body with both sets of fields, as spec 042's
  does with one.

### When the Key changes

A change to `spec.zeroRetention` is a Key update: it raises
`key.updated` in the journal, which evicts the Key from every replica's
cache at the replica's next journal read (spec 036, one second by
default). Until then a replica may serve the Key as it was cached; past
its window a cached Key is served only while the store does not answer
(`LUX_KEY_CACHE_GRACE`). A platform that turns the flag on states this
bound to whoever relies on it.

### The record

The request record and the request log line carry `zeroRetention`, true
for a request on a zero-retention Key, and the `lux.upstream` span the
attribute `lux.zero_retention`, so whether a given request was sent
with the fields is read from the request itself, not from the Key as
it stands now. A refusal is recorded with its code as any refusal is.

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
| 3 | A request on a zero-retention Key reaches the upstream with the zero-retention fields merged after `requestFields`, winning over the caller's and the Provider's general fields, on passthrough and translation, stream and not, with `GetBody` returning the same body; a Provider with no `requestFields` and only zero-retention fields writes them too | `gateway` forward test against a stub upstream |
| 4 | A request on a Key without the flag reaches the same Provider without the zero-retention fields | the same test |
| 5 | A Model whose spec targets declare nothing refuses a zero-retention Key `zero_retention_unavailable`, 403, with no upstream called; a Model with a declaring and a non-declaring target serves it through the declaring one alone, failover included; declaring targets that cannot be tried now answer `provider_unavailable` | `gateway` routing test |
| 6 | An opaque route on a zero-retention Key is refused toward a Provider with zero-retention fields and toward one that declares nothing, and served toward one that declares `{}`; a model-route body that is not a JSON object and a non-identity encoding in any header value are `invalid_request` | `gateway` opaque and forward tests |
| 7 | A Provider declaring `{}` serves a zero-retention Key with nothing added | `gateway` forward test |
| 7a | The record, the log line and the span say whether a request was on a zero-retention Key | `gateway` record test |
| 8 | A Key update that sets the flag reaches the gateway at the next journal read | `internal/serve` key cache test |
| 9 | The OpenAPI document carries both members and the code | `TestOpenAPIIsCurrent` |
| 10 | The conformance suite holds criteria 3 to 5 over the wire | `test/conformance` |

## Outcome

Built on 2026-10-09 as designed, at `testing`: every criterion has a
passing test in the tree. The Postgres half of criteria 1 and 2 runs in
the Postgres tier, `make test-postgres`, which the gate does not start;
it passed against Postgres 17 for this build.

| # | Test |
|---|---|
| 1 | the corpus entry `accepted/key/zero-retention`, which renders `zeroRetention: true`, and every other accepted Key, which renders `false`; `refused/invalid_field/key-zero-retention`; `TestZeroRetentionRules`, `manifest`, for the default and the rendering; `TestZeroRetentionRoundTrip`, `internal/api`, which sets, reads, and clears the flag through `/v1`; `TestPutKeepsNoValue` of `storetest` against the memory store and in the Postgres tier; `TestFencedKeyCleanup`, which holds a fenced Key's flag as a policy write; `TestMutationProposalExcludesSecretsAndIncludesPolicy`, which holds the authorizer's proposal to carrying it |
| 2 | the corpus entries `accepted/provider/zero-retention` and `accepted/provider/keeps-nothing`, `refused/unknown_field/provider-zero-retention-member`, `refused/reserved_prefix/zero-retention-field-model`, and `refused/invalid_field/zero-retention-field-null`; `TestZeroRetentionRules`, every rule of `requestFields` at its path under `spec.zeroRetention.requestFields`, the two encodings at the bound side by side, and the Go form; `TestZeroRetentionRoundTrip`; `TestOptimisticConcurrency` and `TestPutKeepsNoValue` of `storetest`, a declaration with fields and `{}` |
| 3 | `TestZeroRetentionFieldsReachTheUpstream`, over passthrough on all four dialects and translation in both directions between `openai` and `anthropic`, stream and not, with `GetBody` compared with what the upstream read, a caller's `provider` member sent as `{"zdr": false}`, `{"ZDR": false}`, `null`, a string, under `Provider`, and twice, and a Provider with zero-retention fields and no `requestFields`; `case047ZeroRetention` over the wire |
| 4 | `TestZeroRetentionFieldsReachTheUpstream`, every case sent again on a Key without the flag; `case047ZeroRetention` |
| 5 | `TestZeroRetentionRouting`: the refusal with no upstream, limit, or Router reached, a Provider the catalog does not hold, failover between two declaring targets past a non-declaring one, both failing without reaching it, and both excluded by the Router answering `provider_unavailable`; `case047ZeroRetention`, where an open circuit is the outage |
| 6 | `TestZeroRetentionOnOpaqueRoutes`, with no reservation taken by a refusal; `TestZeroRetentionRefusesWhatCannotCarryTheFields`; `TestZeroRetentionDecorateChecksAgain`, which hands the handler a Router that ignores the Model it is given |
| 7 | `TestZeroRetentionDeclaringNothingAdded`; `TestZeroRetentionRouting`, whose `{}` target reads the caller's bytes |
| 7a | `TestZeroRetentionIsRecorded`, the record, the log line, and the span, a refused request included; `TestZeroRetentionRereadClearsTheFlag`; `TestRecordSaysZeroRetention`, `internal/serve`, the record in the request history; `TestSpansCarryNoIdentity` and `TestLogFieldsAreTheTable`, `cmd/luxd`, against spec 019's rows; `case047ZeroRetention` reads `zeroRetention` from `GET /v1/requests` |
| 8 | `TestZeroRetentionReachesTheDoorAtTheNextTail`, two replicas over one store: the one that tails at the commit refuses at once, the other serves the Key as cached until its own tail |
| 9 | `TestOpenAPIIsCurrent`; the members carry descriptions, and `zero_retention_unavailable` is a row of `x-lux-errors` |
| 10 | `case047ZeroRetention`, against the reference server with stubs; the example plane's run has none and skips it |

Decided while building:

- The spec was drafted as 046 while 046, six Budgets per Key, shipped in
  v0.15.0, so it is 047.
- `selectTargets` hands the Router a copy of the Model whose targets are
  the declaring ones, so the order, the weights, and the circuits are
  the Router's as for any Model. Whether any target declares is read
  from the Model's spec targets through the catalog, so a refusal never
  depends on health.
- An opaque route is refused after its Provider is chosen and before the
  reservation, so a refused request takes no rate slot; the choice
  itself is unchanged, `provider_required` included.
- `{"requestFields": {}}` resolves to `{}` and is treated as one.
- A body that is not a JSON object cannot reach the merge through a
  door today, because `bridge.Probe` refuses it `invalid_request` at
  stage 5; the rule in `writeRequestFields` holds a builder that would
  produce one, and is tested on the function.
- Every value of `Content-Encoding`, and every coding a value lists, is
  read for `spec.requestFields` too; spec 042 read the first line only.
- `spec.zeroRetention` on a Key renders `false`, without `omitempty`, as
  the design says. A Key from an earlier release therefore reads back
  with one member more: the conformance suite's previous-release group
  compares it with the member at its default, which spec 003's promise
  admits, the same object defaults aside, and the release promise
  counts the six changed Key goldens as major rows, which before
  v1.0.0 a minor release carries. Spec 039 made the other choice for a
  Model's `spec.disabled`, rendered only when true.
- The record's flag is the Key's as the request was admitted, the Key a
  reread before `model_not_allowed` adopted included, so a refused
  request carries it. A request `decorate` refuses on a model route,
  which only a Router that ignores the Model it is handed can produce,
  is recorded `failed` with its attempt, as spec 042's refusal of an
  encoded body is.
- The catalog's `openrouter` Provider declares
  `provider: {zdr: true}`; no other catalog Provider declares anything,
  because each vendor's terms are the operator's to state.
- Specs 003, 004, 007, 011, 016, and 019 name the members, the code, the
  key cache schedule, the threat, and the two observability fields.

What the gateway cannot hold: the declaration is the operator's
statement about the upstream's terms, and the gateway does not check
them; and a change to the flag reaches a replica at its next journal
read, or after `LUX_KEY_CACHE_GRACE` while the store does not answer.

