---
title: "Provider request fields: JSON members a Provider sets on every request body the gateway sends it"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/011-api.md
affects: [manifest/, gateway/, internal/api/, internal/store/, api/openapi.yaml, docs/, skills/lux/, deploy/catalog/, specs/003-manifest-contract.md, specs/004-request-path.md, specs/016-security-and-threat-model.md]
effort: small
created: 2026-10-03
updated: 2026-10-03
author: changkun
---

# Provider request fields

## Overview

A Provider sets request headers on every request the gateway sends it,
through `spec.headers`, and nothing in the request body. Some upstream
options are body members. An aggregator such as OpenRouter takes its
provider routing preferences as a `provider` object in the body, and
`{"provider": {"zdr": true, "data_collection": "deny"}}` restricts a
request to endpoints with zero data retention run by providers that do
not store or train on the data. An operator who requires such an
option of every call through a Provider today depends on every caller
sending it, and any caller can turn it off. A Provider gains
`spec.requestFields`, a JSON object the gateway merges into every
request body it sends that Provider, with the Provider's values
winning, so a caller can neither leave the option out nor override it.

## Current state

On 2026-10-03:

- Two functions in `gateway/forward.go` build an upstream request:
  `outbound` for every model route, passthrough and translated, and
  `forwardOpaque` for an opaque route, which streams the caller's body
  through unread. Both call `decorate`, which writes the Provider's
  `headers`, the `User-Agent`, and `Lux-Request-Id`.
- On passthrough the body is the caller's, with the `model` member
  rewritten to the target's upstream name (`bridge.SetModel`) and, on
  an openai chat completion stream, `stream_options.include_usage` set
  to true (`bridge.SetIncludeUsage`), so the stream reports the usage
  the request is metered by. On translation the body is the bridge's
  encoding for the target dialect.
- The gateway reads the called Model and whether the answer is a
  stream off the caller's body (`bridge.Probe`), before any edit.

## Design

### The field

`Provider.spec.requestFields`, a JSON object, absent by default,
mutable. It is not secret: every read returns it and the authorizer's
proposal carries it, so a value that must stay secret belongs in
`credential`, never here.

`headers` is named for the part of the request it writes into. The
body's counterpart is named for what it holds, the fields of the
request body, as the upstream APIs' own documentation calls them.
`body` would read as the whole body, and `extraBody`, the name an SDK
gives to members it adds, reads as a default the caller may override,
which is the opposite of what this field does.

### The merge

The Provider's object is merged into the top-level object of the body
as it leaves the gateway, member by member:

- A member whose Provider value is an object is merged into the body's
  member of the same name by the same rule, recursively. A body member
  that is absent, or is not an object, is replaced by the merge into an
  empty object.
- A member whose Provider value is anything else, an array included,
  replaces the body's member whole. Arrays are not merged element by
  element, because neither position nor value identifies an element
  across the two lists, and a merged list would be one neither side
  wrote.
- A body member the Provider does not name is kept, its bytes as the
  caller or the bridge wrote them.
- A body member whose name differs from a Provider member's name only
  in case, under Unicode simple folding, is removed from that object,
  because some JSON readers match member names case-insensitively and
  would otherwise read the caller's value.
- A body that is not a JSON object, or no body, is sent unchanged.

Each object the merge writes into is written again with its members
sorted by name and a duplicate member collapsed to the last one, which is the one `bridge.Probe` reads, so the upstream cannot read
a duplicate the gateway did not. Every value the merge does not reach
keeps its bytes. A Provider without `requestFields` is sent what it is
sent today, byte for byte.

### Where it applies

`decorate` applies the merge, after the Provider's headers, so both
request builders pass through the one place: every model route of every
door, passthrough and translated, stream and not, a count route that
reaches an upstream, and an opaque route. It is the last edit to the
body, after the model rewrite and the usage member.

- On an opaque route toward a Provider with `requestFields`, the
  caller's body is read whole under `LUX_MAX_BODY_BYTES` instead of
  streamed through, so a JSON object body sent to any path of the
  Provider carries the fields; a body above the limit is
  `body_too_large`. A Provider without the field streams as before.
- A request whose `Content-Encoding` is anything but `identity` is
  `invalid_request` toward a Provider with `requestFields`, on every
  route, because the fields cannot be written into an encoded body and
  an upstream that decodes it would read it without them.
- The outbound request's `GetBody` returns the merged body, so a
  transport that sends the request again on a new connection sends the
  merged body.
- A tunneled Provider is merged like any other: the body crosses the
  tunnel as the gateway built it. Every dialect is merged; on `gemini`
  the model and the stream are in the path, and the reserved members
  below are reserved there too, so one rule holds for every Provider.
- The record, the log line, and the spans carry no body, merged or not.

### Validation

| Rule | Code | Path |
|---|---|---|
| a JSON object whose values are objects, arrays, strings, numbers, and booleans | `invalid_field` from the schema walk | `spec.requestFields` |
| `model`, `stream`, and `stream_options` at the top level, compared case-insensitively, are the gateway's: `model` names the target's upstream model, `stream` decides how the answer is read, and `stream_options` carries the usage a stream is metered by | `reserved_prefix`, as a reserved header in `headers` is | `spec.requestFields["model"]` |
| a member whose value is null is refused, so null keeps no meaning a later change would have to preserve; removing the caller's member, as a JSON merge patch does with null, is left open | `invalid_field` | the member's path, `spec.requestFields["provider"]["zdr"]` |
| the compact JSON encoding is at most `manifest.MaxRequestFieldsBytes`, 16384 bytes, because every request to the Provider carries it | `invalid_field` | `spec.requestFields` |

A number is decoded as a 64-bit float wherever the object is read back,
the stores and the API among them, so an integer above 2^53 loses
precision. Routing options are booleans, strings, short lists, and
prices, none of which reach that range.

## Not in this spec

- Removing a caller's member, which a null could mean in a later
  change.
- Request fields per Model or per Key: a Provider is the unit an
  operator declares an upstream's terms on.
- Options carried where the gateway does not read a JSON object: a
  multipart upload on an opaque route, or a request inside an uploaded
  batch input file.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `spec.requestFields` decodes from YAML and JSON, is absent by default, renders when set, resolves to a fixed point, and is mutable | the corpus entry `accepted/provider/aggregator`; `internal/api` apply test |
| 2 | A reserved top-level member, a null member value, and an encoding above the bound are refused with the codes and paths of the table | corpus entries under `refused/`; `manifest` validation test |
| 3 | The merge sets nested members, overrides a caller's leaf, keeps a caller's siblings, replaces arrays and non-object members whole, removes case variants, collapses duplicates, and leaves a non-object body unchanged | `gateway` merge unit test |
| 4 | The upstream receives the merged body on passthrough and on translation, streaming and not, and a Provider without the field receives the caller's bytes | `gateway` forward test; `TestSameDialectSameBytes` |
| 5 | An opaque route toward a Provider with the field sends a merged JSON object body and an unchanged non-JSON body; an encoded body is `invalid_request` | `gateway` opaque test |
| 6 | `GetBody` of the outbound request returns the merged body | `gateway` forward test |
| 7 | The field survives a store round trip, memory and Postgres | `storetest` |
| 8 | The OpenAPI document carries the field | `TestOpenAPIIsCurrent` |

## Outcome

Built and verified on 2026-10-03 as designed.

| # | Test |
|---|---|
| 1 | the corpus entry `accepted/provider/aggregator`, whose golden decodes and resolves again to itself; `TestRequestFieldsInGoForm`, which resolves Go-typed values to their JSON form; `TestProviderRequestFieldsRoundTrip`, `internal/api`, which applies, reads, changes, and removes the field through `/v1`; `TestMutationProposalExcludesSecretsAndIncludesPolicy`, which holds the authorizer's proposal to carrying it |
| 2 | `TestRequestFieldsRules`, and the corpus entries `refused/reserved_prefix/request-field-model` and `refused/invalid_field/request-field-null`, the second a YAML member left without a value |
| 3 | `TestMergeRequestFields` |
| 4 | `TestRequestFieldsReachTheUpstream`, over passthrough on all four dialects and translation in both directions between `openai` and `anthropic`, stream and not; `TestSameDialectSameBytes` for a Provider without the field |
| 5 | `TestRequestFieldsOnOpaqueRoutes` |
| 6 | `TestRequestFieldsReachTheUpstream`, which reads `GetBody` from the request the transport is handed and compares it with what the upstream read |
| 7 | `TestStoreConformance/TestOptimisticConcurrency`, against the memory store and Postgres |
| 8 | `TestOpenAPIIsCurrent`; the schema's description is formatted from `manifest.ReservedRequestFields` and `manifest.MaxRequestFieldsBytes` |

Two details the design left to the build. The merge writes an object
it touches by hand, member by member, rather than through
`encoding/json`, so a value it does not reach keeps its bytes, the
insignificant whitespace inside it included, where an encoder would
compact it. And a merge that cannot encode a value, which `Resolve`
refuses but a Provider a platform builds in Go without `Resolve` could
carry, fails the attempt `provider_unavailable` rather than send the
body without the fields.

What the gateway cannot hold: an option inside an uploaded file, a
batch input for one, is not a body the gateway reads, and an upstream
that accepts the same option in another encoding, a form body for one,
on an opaque route is reached by any Key with `passthrough`. An
operator who relies on the field keeps `passthrough` off on the Keys
that reach the Provider.
