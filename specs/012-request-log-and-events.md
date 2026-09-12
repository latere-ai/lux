---
title: "Request log and events: one signed event per mutation to the operator's sink, one record per request to an archive"
status: drafted
track: core
depends_on:
  - specs/006-identity.md
  - specs/009-usage-and-metering.md
  - specs/010-state.md
affects: [internal/events/, internal/reqlog/, internal/config/, test/stubs/, docs/]
effort: small
created: 2026-09-13
updated: 2026-09-13
author: changkun
---

# Request log and events

## Overview

Two streams leave the gateway for the operator. Events are one signed
record per mutation and per state change worth knowing about, delivered
to one endpoint the operator names, at least once and in order per
object. The request log is one `metering.Record` per data plane request
([[009-usage-and-metering]]), batched as NDJSON objects into an S3
compatible bucket. What the operator does with either, an audit trail,
an invoice, an activity feed, a warehouse, is the operator's; the
gateway keeps no opinion about it and ships no consumer.

Both streams are off by default and neither is on the hot path. An
event is written to the journal of [[010-state]] inside the same
transaction as the mutation and delivered by a worker; a record is
handed to a bounded buffer with a non-blocking send and written by a
worker. A sink that stops acknowledging and a bucket that stops
answering each cost the operator data, with a metric that says how
much, and cost a caller nothing.

## Current state

Nothing is built. The hosted gateway this design is extracted from
writes an audit row and a request row into the same database it bills
from, and derives its activity feed and its invoices from those tables
by query. The event stream here is the part of that audit row that
describes an object rather than the platform around it, and the request
log is that request row with the platform's columns removed and the
content columns that table never had.

## Design

### The event record

```json
{
  "id": "evt_01J9ZK2P7Q8R9S0T1U2V3W4X62",
  "type": "key.created",
  "time": "2026-09-13T10:41:00.123Z",
  "subject": "https://login.example.com|alice",
  "reason": "request",
  "request_id": "req_01J9ZK2P7Q8R9S0T1U2V3W4X63",
  "object": {
    "kind": "Key",
    "id": "key_01J9ZK2P7Q8R9S0T1U2V3W4X60",
    "name": "run-42",
    "owner": "https://login.example.com|alice",
    "labels": {"run": "r_42"}
  },
  "data": {"prefix": "lux_ab12cd34ef56", "models": ["gpt-5"], "budget": "team-research"}
}
```

`id` is a ULID with the `evt_` prefix. `subject` is the rendered
subject of [[006-identity]] that caused the change, empty for an event
the server raised itself; `reason` says which: `request` for a
mutation through [[011-api]], `discovery` for the job of
[[005-providers]], `probe` for a health transition, `flush` for a
counter crossing a limit. `request_id` is set for `reason: request`
and empty otherwise. `object` is the kind, the id, the name, the owner,
and the labels, and nothing else, so a sink can key on it without
parsing `data`.

### The types

| Type | When | `data` |
|---|---|---|
| `provider.created`, `.updated`, `.deleted` | the API applied or deleted a `Provider` | the changed paths on an update; `{dialect, baseURL}` on a create; empty on a delete |
| `provider.unreachable` | health entered `Unreachable` ([[005-providers]]) | `{since, lastError, targets}`: when, the probe's error, how many targets left selection |
| `provider.healthy` | health returned to `Healthy` from any other state | `{since, wasUnreachableFor}` |
| `model.created`, `.updated`, `.deleted` | the API applied or deleted a declared `Model` | the changed paths on an update; `{targets, priced}` on a create |
| `model.discovered` | discovery declared a Model that did not exist | `{provider, upstreamModel}` |
| `model.removed` | discovery deleted a discovered Model the upstream dropped | `{provider, upstreamModel}` |
| `key.created` | a `Key` was applied for the first time | `{prefix, models, budget, expiresAt}` |
| `key.updated` | a `Key`'s spec changed | the changed paths |
| `key.rotated` | `POST /v1/keys/{id}/rotate` | `{prefix, previousPrefix}` |
| `key.deleted` | a `Key` was deleted | `{prefix}` |
| `key.exhausted` | a flush observed a Key's spend window at or over its amount | `{window, amount, spent, currency, resetsAt}` |
| `budget.created`, `.updated`, `.deleted` | the API applied or deleted a `Budget` | as the Model rows |
| `budget.exhausted` | a flush observed a Budget's window at or over its amount, hard or soft | `{window, amount, spent, currency, resetsAt, hard}` |
| `check.ping` | `luxd check` verifying the sink ([[017-release-and-installation]]); names no object, `reason: check`, never journalled | `{}` |

Those are the four state changes worth an event: `key.exhausted`,
`budget.exhausted`, `provider.unreachable`, `provider.healthy`. Each is
raised once per transition by the replica that observed it, under the
lease that makes one replica the observer ([[005-providers]],
[[010-state]]), so an installation of six replicas emits one event and
not six. There is no `key.expired` event, and no event for `Degraded`
or `Unknown` health: an expiry is a fact every replica computes from
the clock at the instant `status.expiresAt` passes, with no write to
hang an event on and no observer to raise it once, and `Degraded` is by
construction a state a single failed probe enters and a single success
leaves ([[005-providers]]), so an event per transition would be noise.
A sink that wants expiries reads `status.expiresAt` from
`key.created`.

### What an event never carries

No event body contains a Key value, a Provider credential value or its
ciphertext, a request or response body, a prompt, a completion, a tool
argument, a caller's address, or any header the caller or the upstream
sent. A Key is named by its id, its name, and its `status.prefix`; a
Provider's credential appears in an event only as
`status.credential.version` under a `provider.updated` changed path.
`TestEventsCarryNoSecrets` runs a canary value through every mutation
and asserts it reaches the sink nowhere.

### Delivery

```mermaid
sequenceDiagram
  participant A as luxd /v1
  participant S as store
  participant J as journal
  participant W as delivery worker
  participant K as the operator's sink
  A->>S: write desired state and the journal row, one transaction
  S-->>A: committed
  A-->>A: 200 to the caller; delivery is not on the response path
  W->>J: claim the oldest unacknowledged row per object
  W->>K: POST body, Lux-Signature
  K-->>W: 2xx
  W->>J: acknowledge
  Note over W,K: a non-2xx or a transport error backs off and retries the same row
```

`LUX_EVENTS_URL` receives `POST` with `Content-Type: application/json`
and a body of one event. The header is

```
Lux-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>" under LUX_EVENTS_SECRET>
```

over the exact bytes sent. A 2xx acknowledges; anything else, including
a redirect, is a failure. A failure is retried with exponential backoff
from 1 second to 5 minutes with full jitter for 24 hours, after which
the event is dropped with an `ERROR` line naming its id and type
([[019-observability]]). `lux_events_pending` is the count of
unacknowledged rows.

Delivery is ordered per object id: a row that is failing holds the rows
behind it for that object and no other, so a sink sees
`key.created` before `key.rotated` for one Key while another Key's
events continue. Across objects there is no order. Delivery is at least
once: an acknowledgement lost after the sink committed produces a
second `POST` of the same `id`, so a sink deduplicates on `id`, which
the documentation says in those words.

The worker runs from the journal of [[010-state]], claiming rows under
a lease, so one replica delivers a given object's events and a restart
resumes where it stopped. Without a store the journal is the process's
memory and a restart loses what was not yet acknowledged, which the
start-up log says. `LUX_EVENTS_URL` unset is events off, and the
journal row is still written, so an installation that names a sink
later delivers nothing that happened before. An `http://` sink URL is
refused at start unless it is on a loopback address, and
`LUX_EVENTS_URL` without `LUX_EVENTS_SECRET` is a start-up failure
([[002-repository-scaffold]]).

A sink should reject a body whose `t` is more than five minutes from
its own clock and should compare the signature in constant time; the
stub sink of [[015-test-stubs-and-tiers]] does both, so a sink author
has a reference to read.

### The request log

One `metering.Record` per data plane request, the same value the
counters and the aggregates are folded from ([[009-usage-and-metering]]),
serialised as one JSON object per line with
`latere.ai/x/pkg/ndjson`. `LUX_REQUESTLOG_EXPORTER` selects where it
goes.

| Value | Does |
|---|---|
| `none` (default) | nothing leaves the process; the record reaches the store's aggregates and the replica's own ring, which is what `GET /v1/requests` serves with `source: memory` ([[009-usage-and-metering]]) |
| `s3` | the record is also appended to the buffer below and written to the bucket in NDJSON batches |

The archive object key is

```
<LUX_S3_PREFIX>/<yyyy>/<mm>/<dd>/<hh>/<replica>-<ulid>.ndjson
```

`<yyyy>/<mm>/<dd>/<hh>` is the UTC hour of the batch's **first**
record, so a record never moves between hours after the fact and a
reader of one hour reads every object under one prefix. `<replica>` is
the process's hostname lowered and reduced to `[a-z0-9-]`, which is the
Pod name in a Deployment, so two replicas never collide; `<ulid>` makes
the key unique and sorts the objects of one replica in write order.
The layout is by hour because `GET /v1/requests` answers a `from` and
`to` range: a range lists one prefix per hour it covers and nothing
else.

Batching and the buffer:

| Value | Setting | Why |
|---|---|---|
| flush size | 5 000 records | an object of a few megabytes, large enough that the per-object cost is noise |
| flush interval | 30s | a record is readable in the archive within half a minute of its request |
| buffer cap | 50 000 records | ten flushes of headroom; about 25 MiB at the record's median size |
| object write deadline | 30s | one `PutObject` through `latere.ai/x/pkg/s3` |

The record reaches the buffer through a non-blocking send from the
request path. When the buffer is at its cap the **oldest** record is
dropped to make room, `lux_requestlog_dropped_total` counts it, and one
`WARN` line per minute says how many. The hot path never blocks on the
archive, never fails a request because the archive failed, and never
grows without bound; an unreachable bucket costs the operator the
oldest records first, which are the ones already summarised in the
aggregates.

A failed `PutObject` is retried with `latere.ai/x/pkg/retry` from 1
second to 60 seconds for five attempts; a batch that still fails is
returned to the head of the buffer and retried at the next flush, which
is what makes a bucket outage of a few minutes lossless and one of an
hour lossy by the cap. At shutdown the drain of
[[002-repository-scaffold]] writes what the buffer holds before the
process exits.

The archive carries no more than the record does, which is no content
at all ([[009-usage-and-metering]]): `Record` has no `any` member and
no body field, so the canary test is over the same struct as the
metering one and the two run together.

### Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `LUX_EVENTS_URL` | no | unset | the operator's event sink; unset is events off |
| `LUX_EVENTS_SECRET` | with the URL | unset | the HMAC-SHA256 key of `Lux-Signature`; the URL without it is a start-up failure |
| `LUX_REQUESTLOG_EXPORTER` | no | `none` | `none` or `s3` |
| `LUX_S3_BUCKET` | with `s3` | unset | the archive bucket |
| `LUX_S3_ENDPOINT` | no | unset | an S3 compatible endpoint; unset is AWS |
| `LUX_S3_REGION` | no | `us-east-1` | the region |
| `LUX_S3_ACCESS_KEY`, `LUX_S3_SECRET_KEY` | no | unset | static credentials; unset uses the environment's default chain |
| `LUX_S3_PREFIX` | no | `lux/` | the key prefix of every archive object |

## Not in this spec

The record's fields, the aggregates, and the usage API
([[009-usage-and-metering]]); the journal table, the leases, and what a
restart resumes ([[010-state]]); the routes that raise the mutation
events ([[011-api]]); the stub sink and its behaviour flags
([[015-test-stubs-and-tiers]]); `lux_events_pending` and
`lux_requestlog_dropped_total` as metrics ([[019-observability]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every type in the table is emitted by the action in its row, once, with the `data` members named and the `reason` of its source | `TestEventTable`, table-driven over every type | not built |
| No event body and no archived record contains a canary Key value, a canary Provider credential, a canary prompt, or a canary completion, over a run that exercises every type and every door | `TestEventsCarryNoSecrets`, `TestArchiveCarriesNoContent` | not built |
| The signature verifies under the documented formula, a body changed by one byte does not, and a wrong secret does not | `TestSignature` | not built |
| A sink that fails three times receives the event on the fourth attempt, and that object's later events after it in order, while another object's events are delivered meanwhile | `TestDeliveryIsOrderedPerObject` | not built |
| A restart with a store resumes delivery of an unacknowledged event, and a second delivery of one `id` happens when an acknowledgement is lost | `TestDeliveryResumesFromTheJournal`, `TestDeliveryIsAtLeastOnce` | not built |
| An event that fails for 24 hours is dropped with a log line naming its id, and `lux_events_pending` returns to zero | `TestDeliveryGivesUp`, with a fake clock | not built |
| Six replicas observing one Budget crossing its amount emit exactly one `budget.exhausted` | `TestStateChangeEventIsRaisedOnce` | not built |
| A batch of 5 000 records is written at the size, a partial batch at the interval, and the key is the UTC hour of the batch's first record with the replica and a ULID | `TestArchiveBatching`, `TestArchiveObjectKey` | not built |
| Every line of an archived object parses as one record, and the records of one hour's objects equal the records the gateway produced in that hour | `TestArchiveRoundTrip` against the stub bucket | not built |
| With the bucket unreachable, no request is slowed by more than a millisecond, the buffer stops at its cap, the oldest records are dropped, and `lux_requestlog_dropped_total` counts exactly the drops | `TestArchiveOutageNeverBlocksTheHotPath`, `TestBufferDropsOldest` | not built |
| A bucket outage shorter than the buffer's headroom loses nothing once it recovers | `TestArchiveRecoversWithoutLoss` | not built |
| Shutdown writes what the buffer holds before the process exits | `TestDrainWritesTheBuffer` | not built |
| `LUX_EVENTS_URL` without `LUX_EVENTS_SECRET`, and an `http://` sink off loopback, are each start-up failures naming the variable | `TestEventsConfigurationRefusals` | not built |
