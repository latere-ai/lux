---
title: "Request-label filter: GET /v1/requests reads the records whose request labels match, under the read the caller already has"
status: drafted
track: core
depends_on:
  - specs/007-keys-and-limits.md
  - specs/009-usage-and-metering.md
  - specs/011-api.md
  - specs/012-request-log-and-events.md
affects: [metering/, internal/store/, internal/reqlog/, internal/api/, internal/luxcli/, gateway/, api/openapi.yaml, docs/, skills/lux/, test/conformance/, specs/009-usage-and-metering.md, specs/011-api.md]
effort: small
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Request-label filter

## Overview

A caller labels its own requests with the `Lux-Labels` header, and the
gateway keeps the accepted pairs as the record's `requestLabels`
([[007-keys-and-limits]], [[009-usage-and-metering]]). Nothing reads them
back by value: `GET /v1/requests` filters by the Key's labels, the
`label` parameter, and not by a request's. A harness that runs many
trials under one Key, and labels each call with the trial it belongs to,
can find a trial's records only by keeping every request id or by
reading every record of the Key and filtering on its side.

`GET /v1/requests` gains `request_label`, a filter over the record's
`requestLabels`. It is a second filter beside `label`, never a new name
for it, and it narrows what the caller may already read: the
authorization, the paging, and the sources of the route are unchanged.

## Current state

On 2026-10-18:

- The gateway parses `Lux-Labels` in `requestLabels`
  (`gateway/key.go`): at most eight pairs, each name 1 to 64 characters
  of `[A-Za-z0-9._-]` and each value 1 to 128 of `[A-Za-z0-9._:/-]`, a
  pair outside the rule dropped. The record carries the pairs as
  `requestLabels`; no aggregate row does.
- `metering.RecordQuery` (`metering/query.go`) is the range, the Key,
  Model, Provider, owner, and Key-label filters of `metering.Query`, and
  the record's status, error, and stream flag. `RecordQuery.Matches` is
  what both record sources apply: the replica's ring
  (`internal/store/memory/usage.go`, which the Postgres store delegates
  to) and the archive reader (`internal/reqlog/reader.go`).
- `parseUsageQuery` (`internal/api/usage.go`) refuses a parameter the
  route does not define with `invalid_field` at its name.
- A record cursor carries a digest of the query (`recordDigest`,
  `internal/store/usage.go`), so a cursor is valid only for the query
  that produced it.

## Design

### The parameter

`request_label`, on `GET /v1/requests` alone, repeatable, each value
`<name>=<value>` split at the first `=`, written
`request_label=trial%3Dt-42` or `request_label=trial=t-42`; both reach
the server as the pair `trial`, `t-42`.

| Rule | Answer |
|---|---|
| a value without `=`, or with an empty name | `invalid_field` at `request_label` |
| a name or a value outside the `Lux-Labels` syntax of [[007-keys-and-limits]] | `invalid_field` at `request_label`: the gateway keeps no such pair, so the filter could select nothing |
| more distinct names than `gateway.MaxRequestLabels`, eight | `invalid_field` at `request_label`: a record keeps at most that many pairs, so a query naming more selects nothing |
| one name with one value twice | one pair |
| one name with two values | an empty `items`, read from no source, as two values for one `label` name are ([[011-api]]) |

The syntax check and the bound are the gateway's own, exported as
`gateway.ValidRequestLabel` and `gateway.MaxRequestLabels`, so the
filter and the header parser cannot drift apart.

`GET /v1/usage` does not define the parameter: a request label is never
an aggregate dimension ([[009-usage-and-metering]]), so it answers
`request_label` as it answers any parameter it does not define,
`invalid_field` at that name.

### What a pair selects

`RecordQuery.RequestLabels` holds the pairs. A record matches when its
`requestLabels` holds every pair, name and value compared byte for byte,
and it passes every other filter of the query, so the parameter is a
conjunction with the rest. `label` reads the Key's labels, the record's
`labels`; `request_label` reads `requestLabels`. One name in both is two
filters, one over each map.

### Authorization

Unchanged. The route asks `usage.read` with the resolved Key ids and the
owners of the query, the resource of [[006-identity]], and intersects
the decision's `filter` with the query as before. The request-label
pairs are applied on top, so they narrow what the caller may already
read and never widen it: a record outside the authorizer's filter is
not returned, whatever its request labels, and the answer is an empty
`items`, never a 403.

The resource does not carry the pairs, so an authorizer cannot grant a
read by request label. Whoever holds a Key writes its requests' labels,
and the gateway trusts them for nothing but reporting
([[007-keys-and-limits]]): a record that carries a label proves nothing
about who sent it, so the read stays scoped by owner, Key, and Key
label.

### Paging and the sources

The record cursor's digest covers the pairs, as it covers every other
filter, so a cursor from a query with other pairs is `invalid_field` at
`cursor`. A query without the parameter digests to the bytes it did
before, so a cursor issued by an earlier release keeps working.

Both sources apply the pairs while reading, through
`RecordQuery.Matches`. The archive's objects are partitioned by a Key
label (`LUX_REQUESTLOG_PARTITION_LABEL`, [usage
retention](.archive/038-usage-retention.md)), never by a request label,
so a query that names request labels alone walks every partition of
the range's hours. A page ends after `reqlog.PageReads` bucket calls, as
every archive page does ([request history
paging](.archive/041-request-history-paging.md)), and may then hold
fewer records than `limit`, or none, while `next_cursor` is present; a
caller reads a whole set by following `next_cursor` until it is absent,
and reads less by naming the Key, the owner, or the partition label as
well. Without an archive the source is the replica's own ring, which
`source: memory` already says.

### The command and the document

`lux requests` gains `-request-label k=v`, repeatable, sent as
`request_label`; a value without `=` is refused before any request, as
`-label`'s is. The OpenAPI document lists the parameter on
`listRequests`, with the syntax and the bound in its description.

## Not in this spec

- A request label as an aggregate dimension of `GET /v1/usage`: request
  labels are unbounded in cardinality, which is what keeps them out of
  the aggregate rows.
- An authorizer decision by request label, above.
- A prefix or pattern match on a value: an exact pair is what a caller
  that wrote the label holds.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `RecordQuery.Matches` selects a record whose `requestLabels` hold every pair and no record missing one or holding another value, and a query without pairs selects as before | `metering` test |
| 2 | `Records` filtered by request labels answers the matching records newest first, pages by cursor, and answers none for another value, over the memory store and the Postgres store | `storetest` |
| 3 | A record cursor is bound to the pairs, and a query without pairs digests as it did before the member existed | `internal/store` cursor test |
| 4 | `GET /v1/requests?request_label=` filters the records; a value without `=`, a pair outside the syntax, and a ninth distinct name are `invalid_field` at `request_label`; one name with two values is an empty `items`; the same pair twice is one; `GET /v1/usage?request_label=` is `invalid_field` | `internal/api` test |
| 5 | Under an authorizer whose filter names an owner, a record of another owner carrying the pair is not returned, and the `usage.read` resource carries `keys` and `owners` alone | `internal/api` test |
| 6 | The archive reader answers the records carrying the pairs across partitions, a page by page walk included | `internal/reqlog` test |
| 7 | `lux requests -request-label k=v` sends `request_label`, and a value without `=` is refused before any request | `internal/luxcli` test |
| 8 | The OpenAPI document lists the parameter | `TestOpenAPIIsCurrent` |
| 9 | Over the wire, two requests on one Key with different `Lux-Labels` are told apart by `request_label` | `test/conformance` |
