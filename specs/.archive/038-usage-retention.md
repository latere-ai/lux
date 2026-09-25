---
title: "Usage retention: hourly rows rolled up into monthly ones by rules on Key labels, an owner's rows redacted, and the request log partitioned by a Key label"
status: drafted
track: core
depends_on:
  - specs/006-identity.md
  - specs/009-usage-and-metering.md
  - specs/010-state.md
  - specs/011-api.md
  - specs/012-request-log-and-events.md
  - specs/022-authorizer-vocabulary-package.md
affects: [metering/, internal/store/, internal/serve/, internal/api/, internal/config/, internal/reqlog/, authorizer/, cmd/luxd/, docs/, specs/002-repository-scaffold.md, specs/006-identity.md, specs/009-usage-and-metering.md, specs/010-state.md, specs/012-request-log-and-events.md, specs/022-authorizer-vocabulary-package.md]
effort: large
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# Usage retention

## Overview

The core keeps one usage row per hour per dimension tuple, forever
([[009-usage-and-metering]]). An operator that serves several kinds of
caller needs to keep them differently: an hourly breakdown of one
person's calls for a few months and a monthly total after that, a team's
rows under the team's own rule, and the right to take one person's
identity out of the rows when they ask. The request log's objects are
kept by the bucket's lifecycle rules, which act on a key prefix, and
today every record shares one prefix.

This spec adds retention rules matched on the Key's labels, each keeping
hourly rows for a period and then folding them into monthly rows with
chosen dimensions dropped; an operation that redacts one owner from
every row; and a request log partitioned by a Key label so each
partition can have a lifecycle rule of its own. With no rule the core
keeps every hourly row forever, as it does now.

## Current state

On 2026-09-24:

- `usage_hourly` holds one row per hour and per tuple of key, model,
  provider, owner, door, status, and currency, with the Key's labels
  denormalized on the row, summed on conflict
  (`internal/store/postgres/migrations/1000002_usage.up.sql`). Nothing
  deletes a row.
- `GET /v1/usage` reads at most 90 days per query, grouped by up to
  three dimensions and an interval of `none`, `hour`, `day`, or `month`
  (`metering/query.go`).
- The request log writes NDJSON objects under
  `<LUX_S3_PREFIX>/yyyy/mm/dd/hh/<replica>-<ulid>.ndjson`
  (`internal/reqlog/exporter.go:250-253`), and the archive reader lists
  the hours of a query (`internal/reqlog/reader.go:56-110`).
- The store declares a `usage` lease that no job holds
  (`internal/store/store.go:240`).

## Design

### 1. Retention rules

A rule is:

| Field | Type | Default | Meaning |
|---|---|---|---|
| `match` | map of label to value | `{}`, every row | the Key labels a row must carry, every pair |
| `hourly` | duration, at least `24h`, or `forever` | `forever` | how long an hourly row is kept before it is folded into its month |
| `monthly` | duration, at least `720h`, or `forever` | `forever` | how long a monthly row is kept before it is deleted |
| `drop` | list of `owner`, `key`, `labels` | `[]` | the dimensions a monthly row does not keep: `owner` and `key` are emptied, `labels` keeps only the pairs `match` names, so the rule still matches the row |

Rules are ordered, and the first whose `match` a row's labels satisfy
applies to it; a row no rule matches is kept hourly, forever.

| | Where the rules live | For | Against |
|---|---|---|---|
| A | `LUX_USAGE_RETENTION`, a JSON list of rules read at start, a malformed one a start-up failure naming the rule | an operator's policy is configuration, like every other `LUX_` setting; one place, reviewed with the deployment; no new route, action, or store row | a change is a rollout; one rule per team means one rule per team in the configuration |
| B | A document behind `/v1`, `GET` and `PUT /v1/usage/retention`, stored in the store and read by the job at each pass | a platform changes a rule without a rollout, and can keep a rule per team | a new route, a new action, a new store row, and a document that only operators may write |

**Recommendation: A.** A platform with a few classes of caller writes a
rule per class and labels its Keys with the class; a rule per team can
move to B when a platform needs it, with the same rule shape.

### 2. The roll-up

A job on the replica that holds the `usage` lease runs every hour. Per
pass it reads the hourly rows older than the shortest finite `hourly`
of any rule, in batches of 1000 by bucket, applies each row's rule, and
for every row whose rule's `hourly` has passed:

- builds the monthly row: the bucket is the first instant of the row's
  UTC month, the dimensions of `drop` are emptied or narrowed, and the
  sums are the row's;
- adds it into `usage_monthly`, summed on conflict like the hourly
  table;
- deletes the hourly row,

the add and the delete of one batch in one transaction, so a pass that
stops never counts a row twice or loses it. Then it deletes the monthly
rows whose rule's `monthly` has passed, measured from the end of the
row's month.

`usage_monthly` has the hourly table's columns and primary key, with
one row per month and per tuple; it is a new table, which the additive
migration rule admits, rather than a flag on `usage_hourly`, whose
primary key a migration may not change.

`GET /v1/usage` reads both tables: an hourly row inside the range as
today, and a monthly row whose month overlaps the range. A monthly row
lands in the bucket of its month's first instant whatever the interval,
since it has no finer time; a query over a range the rule has rolled up
answers at month granularity, and one that groups by a dropped
dimension finds it empty. The 90-day bound on a query is unchanged.

The hourly rows the flush writes are never older than the current hour,
so the job and the flush never touch the same row.

### 3. Redacting an owner

`POST /v1/usage/redact` with `{"owner": "<rendered subject>"}` empties
the owner of every hourly and monthly row that carries it, adding each
into the row that has the same other dimensions and no owner, in one
transaction, and removes the owner from the records of the process's
ring. It answers `{"rows": n}`, the rows rewritten. It is a new action,
`usage.redact`, whose resource is `{"kind": "Usage", "owner"}`, asked as
the caller like every control plane action; with the built-in owner
policy only an administrator may take it. Nothing about the objects
changes: a Key keeps its owner until the operator deletes or reassigns
it. The request log's archive is the operator's bucket, which the
partition below lets them expire or rewrite by prefix.

### 4. The request log partitioned by a Key label

`LUX_REQUESTLOG_PARTITION_LABEL` names one Key label. With it set, a
record's object key gains the label's value after the prefix,
`<LUX_S3_PREFIX>/<value>/yyyy/mm/dd/hh/<replica>-<ulid>.ndjson`, and `_`
for a Key without the label, which no label value can be, since a value
begins and ends alphanumeric. A batch writes one object per partition.
The archive reader lists the partitions of each hour, or reads only
one when the query's label filter names the partition label. Unset is
today's layout.

A bucket's lifecycle rule acts on a prefix, so an operator expires each
partition by its own rule, `lux/personal/` at 90 days and `lux/team/`
at a year, without the core reading a record.

### Configuration

Two rows in [[002-repository-scaffold]]'s table:

| Variable | Spec | Default | Meaning |
|---|---|---|---|
| `LUX_USAGE_RETENTION` | 038 | unset, every hourly row kept | a JSON list of retention rules, applied first match first |
| `LUX_REQUESTLOG_PARTITION_LABEL` | 038 | unset | a Key label whose value partitions the request log's object keys |

### Metrics

One row in [[019-observability]]'s table:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `lux_usage_rolled_up_total` | counter | `result` (`folded`, `deleted`, `error`) | hourly rows folded into monthly ones, monthly rows deleted, and passes that failed |

## Not in this spec

- Rules behind `/v1` (option B).
- An event for a redaction; the control plane's access log records it.
- Reading more than 90 days in one usage query.
- Rewriting objects already in the request log archive.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `LUX_USAGE_RETENTION` parses to ordered rules; an unknown field, a `drop` outside the three, an `hourly` under `24h`, or a `monthly` under `720h` refuses to start naming the rule; unset is no rule | `internal/config` table test |
| 2 | A row matching a rule whose `hourly` has passed is folded into its month's row with the rule's dimensions dropped and deleted from the hourly table, in one transaction; a row no rule matches, or whose rule keeps it, stays | `internal/serve` job test over the memory store; the store suite's case over memory and Postgres |
| 3 | A monthly row whose rule's `monthly` has passed since the end of its month is deleted; `forever` keeps it | `internal/serve` job test |
| 4 | `GET /v1/usage` over a range the job has folded answers the same totals as before the fold, at month granularity, and a query grouped by a dropped dimension finds it empty | `internal/serve` test over `Usage`; `internal/api` test |
| 5 | A pass interrupted between batches, or run twice, counts every row once | the store suite's case with a failing second batch |
| 6 | `POST /v1/usage/redact` empties the owner of every hourly and monthly row carrying it, sums each into the row without an owner, removes it from the ring's records, answers the rows rewritten, and is `usage.redact` as the caller | `internal/api` test with a recording authorizer; the store suite's case |
| 7 | With `LUX_REQUESTLOG_PARTITION_LABEL` set, a batch spanning two label values writes one object under each value's prefix and a Key without the label writes under `_`; the reader reads one partition when the query names it and every partition otherwise | `internal/reqlog` exporter and reader tests over the bucket stub |
| 8 | The job runs on the replica holding the `usage` lease alone | `internal/serve` test with two jobs over one store |
| 9 | `usage_monthly` and its indexes are served by the queries the job and the usage API run | the Postgres index test |
