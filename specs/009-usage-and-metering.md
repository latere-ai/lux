---
title: "Usage and metering: the record, cost, windows, the usage API, the multi-replica rule"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/007-keys-and-limits.md
  - specs/008-routing-and-models.md
  - specs/010-state.md
affects: [metering/, gateway/, internal/store/, internal/api/, internal/reqlog/]
effort: medium
created: 2026-09-13
updated: 2026-09-14
author: changkun
---

# Usage and metering

## Overview

Every data plane request produces exactly one usage record, whether it
was refused at the gate, failed against an upstream, or succeeded
([[001-architecture]], invariant 7). The record names the key, the
model, the provider, the tokens, the cost, the latency, and the
outcome, and never the content. This spec owns the record's fields,
where its token counts come from, the integer arithmetic that turns
them into money, the counters the limits of [[007-keys-and-limits]]
read, the bound on how far several replicas may overshoot a spend
limit before they agree, the queries the usage API answers, and the
`metering` package that holds all of it.

`metering` computes and dials nothing ([[001-architecture]]): it
imports `manifest/v1` and the standard library, takes counters through
an interface, and is driven by `gateway` on one side and the API on the
other. A cost computed by one build is computed the same by every later
build for the same pricing, which is what makes a record auditable a
year after it was written.

## Current state

The `metering` package holds the record, `Cost`, the aggregate shapes,
`Fold`, the query, and the windows and counters [[007-keys-and-limits]]
built there. `internal/store` declares `Usage()` on the contract with
the memory store's hourly rows and per-Key ring and the suite's cases,
and `internal/store/postgres` answers it from the `usage_hourly` table
of its second migration, held to the same suite under the postgres tag
([[010-state]]). `internal/serve` holds the `Recorder`
over `gateway.Record`, the `Usage` aggregation for the route, the three
metrics, and `LUX_METERING_FLUSH` through the Limiter's and the
Recorder's flush, which `luxd serve` starts with its jobs. The routes
that read the aggregates and the records are [[011-api]]'s and are
mounted there, `GET /v1/usage` over `serve.Usage` and `GET /v1/requests`
over the ring with `source: memory`; the archive is
[[012-request-log-and-events]]'s.

## Design

### The record

The gateway hands its `Recorder` a `gateway.Record` carrying everything
the pipeline knows and no cost ([[004-request-path]]); this package's
`Record` is built from it by adding the cost from the Model's pricing,
and `priced: false` follows from a count or an opaque route.

One `metering.Record` per request, emitted after the response is
finished or refused.

| Field | Type | Value |
|---|---|---|
| `id` | string | the request id, a ULID with the `req_` prefix |
| `at`, `endedAt` | timestamp | when the gateway received the first byte and finished the response, RFC 3339 UTC |
| `key` | object | `{id, prefix}`: the Key's `key_` id and its twelve-character prefix |
| `owner` | string | the Key's owner, the rendered subject `<iss>\|<sub>` of [[006-identity]] |
| `model` | object | `{name, id}` of the resolved Model; empty when the name did not resolve |
| `provider` | object | `{name, id}` of the Provider that answered; empty when none was reached |
| `upstreamModel` | string | the target's `targets[].model` |
| `door` | enum | `openai`, `anthropic`, `gemini`, `lux` |
| `targetDialect` | enum | the same set; empty when no target was chosen |
| `route` | string | the door route, and with it the route class of [[004-request-path]]: translated, model, or opaque |
| `translated` | bool | true when the request crossed dialects |
| `loss` | []string | `ir.Loss.Strings()` of the translation, empty otherwise |
| `attempts` | []object | one per target tried: `{provider, upstreamModel, status, httpStatus, error, durationMs}`, in order |
| `status` | enum | `ok`, `refused`, `failed` |
| `error` | string | the data plane error code of [[004-request-path]] that answered the caller, or `client_closed` when the caller disconnected first, which is that spec's record-only code; empty when `ok` |
| `upstreamStatus` | int | the HTTP status the upstream returned; `0` when none did |
| `latencyMs` | int | `endedAt` minus `at` |
| `ttfbMs` | int | to the first response byte written to the caller; `0` when none was |
| `tokens` | object | `{input, output, cachedInput, cacheWrite, reasoning, estimated}`, every count an `int64` |
| `cost` | object | `{amount, currency, priced}`, the `metering.Charge` struct, so named because `Cost` is the function: `amount` an `int64` count of micro-units of `currency`, so `1250000` is `1.25`; the money strings of [[003-manifest-contract]] are what people write in a manifest and what `status` renders, and a record and a usage row carry the integer, never the string; `currency` is empty and `amount` `0` when `priced` is false |
| `stream` | bool | the caller asked for a stream |
| `labels` | map | a copy of the Key's `metadata.labels` at request time |
| `requestLabels` | map | the accepted pairs of the request's `Lux-Labels` header ([[007-keys-and-limits]]), at most eight; empty when none; never an aggregate dimension |

`Record` is a struct of scalars, slices of scalars, and two string
maps. It has no `any` member and no field that could carry a body,
which is what makes invariant 7's second half checkable rather than
promised: never a prompt, a completion, a tool argument, a system text,
a request or response header, a provider credential, a Key value, or
the caller's address. `requestLabels` is bounded by its syntax to eight
pairs of at most 64 and 128 characters, so it cannot smuggle a body
either.

A refused request has a record: `status` `refused`, `error` set,
`attempts` empty, zero tokens, zero cost. This is the record an
operator reads to find out why a workload is getting nothing, so
refusing without one would hide the most useful case. A request refused
before its Key was known carries an empty `key` and `owner`.

The Recorder of `internal/serve` builds the record after the response
is finished, so the one read it makes adds nothing to the caller's
latency: the resolved Model's `pricing` is read through the catalog by
the name the record carries, bounded by five seconds, and a catalog
that does not answer leaves the record unpriced and logs the read. A
count route is told by its template in the door table of
[[004-request-path]], `/anthropic/v1/messages/count_tokens`,
`/gemini/v1beta/models/{model}:countTokens`, and `/lux/v1/count_tokens`,
and an opaque route by its class; both are unpriced whatever the Model
says, because no completion was made from the count. `gateway.Record`
could carry a count flag and make the template list unnecessary, which
is that spec's edit to make.

### Tokens

| Source | When | Marks |
|---|---|---|
| the upstream's own usage | the response carries the dialect's usage members | `estimated: false` |
| a stream's own usage | the members are read as the last value of each across the stream; an `/openai` chat completion stream carries them because [[004-request-path]] sets `stream_options.include_usage` on the way out | `estimated: false` |
| an estimate | the response carries none, or a stream ended before its usage frame | `estimated: true` |

On a translated route the counts come from the `bridge.Usage` that
`llmdialect/bridge`'s `Response` and `Stream` return, filled from
`ir.Usage` and the upstream dialect's members. On a passthrough the
gateway reads the same members through `bridge.UsageOf` and
`bridge.NewUsageScanner`:

| Dialect | Input | Output | Cached input | Cache write |
|---|---|---|---|---|
| `openai` | `usage.prompt_tokens` less `usage.prompt_tokens_details.cached_tokens` | `usage.completion_tokens` | `usage.prompt_tokens_details.cached_tokens` | none |
| `anthropic` | `usage.input_tokens` | `usage.output_tokens` | `usage.cache_read_input_tokens` | `usage.cache_creation_input_tokens` |
| `gemini` | `usageMetadata.promptTokenCount` less `usageMetadata.cachedContentTokenCount` | `usageMetadata.candidatesTokenCount` | `usageMetadata.cachedContentTokenCount` | none |
| `lux` | the `ir.Usage` members by name | | | |

`tokens.input` is the count billed at the input price and excludes
cached input, on every dialect. OpenAI and Gemini report a prompt total
that includes their cache reads and Anthropic reports them apart, so
the subtraction above, floored at zero, is the one place that
difference is handled; `llmdialect`'s `openaichat` and `openairesp`
codecs apply the same floored subtraction when they fill
`ir.Usage.InputTokens` on a translated route, so the two paths agree.
`tokens.reasoning` is `ir.Usage.ReasoningTokens`, which the upstream
reports inside its output count, and is carried for reporting only; it
is not a term in the cost.

When the upstream reports nothing, `tokens.input` is
`llmdialect/bridge.CountTokensFor` over the request in the route's
dialect, which is `tokencount.Estimate` over the decoded request, and
`tokens.output` is `0`, because no exported estimator
counts a response and inventing one would put a number nobody can
audit into a bill. `tokencount`'s own documentation says metering uses
the upstream's reported usage and never the estimate, which is why the
record marks the whole token block `estimated` rather than pretending
the two are the same number. An opaque route names no Model and has no
intermediate request: it records zero tokens with `estimated: true` and
`cost.priced` false, which is what [[004-request-path]] means by a
request it cannot price.

### Cost

The core ships no price card. Every price is the operator's
`Model.spec.pricing` ([[003-manifest-contract]]), a discovered Model is
unpriced until an operator declares it, and a compiled-in rate card or a
provider's price snapshot is a platform's to keep as Models it applies.

```go
// Pricing quotes money per Per tokens, with Per one of 1, 1000,
// 1000000 (003). Money is an integer count of micro-units of the
// currency, so no float touches a price at any point. One rounding,
// half up, over the whole sum: rounding each term would let a price
// change of zero move a bill.
func Cost(t Tokens, p *v1.Pricing) (v1.Money, bool) {
	if p == nil {
		return 0, false // unpriced
	}
	per := int64(p.Per)
	n := t.Input*int64(p.Input) +
		t.Output*int64(p.Output) +
		t.CachedInput*int64(p.CachedInput) +
		t.CacheWrite*int64(p.CacheWrite)
	return v1.Money((n + per/2) / per), true
}
```

`cost.currency` is `Model.spec.pricing.currency` and the record carries
it beside the amount. Nothing in Lux converts a currency: an amount in
one currency is never added to an amount in another, in a record, in a
counter, in an aggregate, or in an API response. A Key that draws from
a Budget in one currency and names a Model priced in another is refused
`currency_mismatch` ([[007-keys-and-limits]]).

`cost.priced` is false when the Model has no pricing, and the amount is
then `0`. Under a spend limit or a Budget an unpriced Model is refused
`model_unpriced` before any bytes reach a provider unless the Key sets
`allowUnpriced`, because a spend limit that silently admits requests it
cannot price is not a limit. `Charged(t, p)` is the record's cost block
for `Cost(t, p)`: the amount and the Pricing's currency when priced, an
empty block otherwise. A price the Pricing does not name is `0`, and a
`per` of `0`, which the resolver never leaves but a caller might, reads
as `1`.

### Windows and counters

A window is the arithmetic of [[003-manifest-contract]], restated as
the function the counters key on.

```
duration window d:  n = floor(unixSeconds(at) / seconds(d))
                    start = n * d, resetsAt = (n+1) * d, UTC
month:              start = first of at's month, 00:00:00Z
                    resetsAt = first of the next month, 00:00:00Z
none:               start = the object's createdAt, resetsAt = never
```

A duration window is fixed rather than rolling, so every replica agrees
on the boundary from the clock alone and no replica has to be told when
a window began.

| Counter | Key | Window | Counts | Kept |
|---|---|---|---|---|
| key spend | `key:<key id>:spend:<start>` | `limits.spend.window` | cost in micro-units | the store |
| key requests | `key:<key id>:requests:<start>` | `limits.spend.window` | one per admitted request | the store |
| key tokens | `key:<key id>:tokens:<start>` | `limits.spend.window` | input plus output | the store |
| key totals | the three above with the word `none` in place of `<start>`, `key:<key id>:spend:none` | `none` | the same three over the Key's lifetime | the store |
| budget spend | `budget:<budget id>:spend:<start>`, with `<start>` the window's start after `spec.anchor` and `spec.restartedAt`, and `none@<unix>` for a lifetime restarted ([[037-several-budgets-per-key]]) | `Budget.spec.window` | cost in micro-units | the store |
| exhausted marker | `key:<key id>:exhausted:<start>:<amount>`, `budget:<budget id>:exhausted:<start>:<amount>`, the amount in micro-units so a raised amount re-arms it ([[037-several-budgets-per-key]]) | the object's spend window | `1`, added by a replica that observes the window at or over its amount; the add that returns `1` is the one that emits the event ([[007-keys-and-limits]], [[012-request-log-and-events]]) | the store |

`<start>` is the window start as Unix seconds, so a key names exactly
one window and a finished window's row is prunable by its own name.
The totals' key carries the word `none`, not the Key's `createdAt`,
as [[007-keys-and-limits]] settled: a duration window that begins at
the creation instant, which the first window of every Key created on
an aligned boundary does, would otherwise render the totals' key and
count every request twice. The spend counter is what the limit of
[[007-keys-and-limits]] reads; the requests and tokens counters, and
the three totals, are what `status.usage` renders, and the window rows
exist only for a Key with a spend limit. The Limiter feeds all of them,
one request at admission, the measured tokens at the settle, and the
estimate then the measured cost less it; the Recorder folds the
aggregates from the records and adds to none of these, or every
admitted request would count twice. The per-minute rate limits are not
counters here at all: they are that spec's per-replica buckets and
never reach the store, which is the multi-replica rule below.

### The multi-replica rule

Rate windows are per replica, the buckets of [[007-keys-and-limits]]
over `latere.ai/x/pkg/ratelimit` keyed by the Key's id. A shared rate
counter would put a store round trip on the hot path, which is the
latency the gateway exists not to add, and a rate limit is a bound on
abuse rather than an account. The overshoot is therefore exact and
documented:

```
effective rate ceiling = limits.requestsPerMinute * replicas
                         limits.tokensPerMinute   * replicas
```

Spend windows are the store's. Each replica keeps a local delta per
counter key and flushes it every `LUX_METERING_FLUSH`, and at shutdown,
through one add that returns the counter's new total; the returned
total replaces the replica's cached store value and the delta resets.
A replica that has never added to a key holds no view of it and admits
its first request under that key on an empty view, which is the one
request of the bound's last term; it learns the store's total at its
own first flush of that key. There is no per-request store read: the
check a hard limit makes is

```
spent = lastStoreTotal + localDelta
refuse when spent + costOfThisRequest > limit
```

which reads a replica's own spending immediately and every other
replica's within one flush. The worst case is one flush interval of
every other replica's spending arriving at once, plus the one request
of this replica's that crossed:

```
overshoot <= (R - 1) * F * T * C + C

R = replicas
F = LUX_METERING_FLUSH in seconds
T = requests per second per replica against that counter
C = the greatest cost of one request under that Key's models
```

This is an upper bound, not a tight one: a replica refuses on its own
view as soon as it crosses, so only the spending the other replicas had
not yet flushed, and the request that crossed, can exceed the limit.
With the default flush of one second, three replicas, ten requests a
second each, and a one-cent request, the bound is twenty-one cents;
with one replica it is one request. [[007-keys-and-limits]] states the
same formula in the same symbols. An operator who needs it smaller
lowers `LUX_METERING_FLUSH` and pays one store write per counter per
interval for it.

A soft Budget, `hard: false`, never refuses for its amount: it records,
emits `budget.exhausted` once per window through the marker counter
above ([[007-keys-and-limits]], [[012-request-log-and-events]]), and
continues.

Three metrics of [[019-observability]] are this spec's, recorded at
the record and at the flush, under `serve.MetricTokens`,
`MetricSpend`, and `MetricFlushLag`: `lux_tokens_total{direction}` adds
each record's `input`, `output`, `cached_input`, and `cache_write`;
`lux_spend_microunits_total{currency}` adds each priced record's
`cost.amount`; `lux_metering_flush_lag_seconds` is the seconds since
this replica's last successful flush, the older of the Limiter's
counter flush and the Recorder's aggregate flush, so a stalled store
shows as a growing gauge before any limit is wrong by more than the
bound. [[019-observability]]'s table also lists
`lux_output_tokens_per_second` under this spec; it is not recorded
here and is that spec's to place.

### Configuration

| Variable | Default | Rule |
|---|---|---|
| `LUX_METERING_FLUSH` | `1s` | at least `100ms`, at most `1m`; the flush interval of every spend counter, through the Limiter, and of the aggregates, through the Recorder, each a loop `luxd serve` starts with its jobs and flushes once more at stop; listed with its owner in [[002-repository-scaffold]]'s table |

### The usage API

Every aggregate this API answers is read through `Store.Usage()`, the
collection [[010-state]] declares with this spec's types, so the memory
store and the Postgres store answer one query the same way. As built,
`AddRows` takes `[]metering.Aggregate`, the hourly row with every
dimension a member, because a row keyed by a `dimensions` map cannot be
upserted on its primary key; `QueryRows` answers `[]metering.Row`, the
response rows, grouped as `metering.Group` groups; `AppendRecord` and
`Records` are the ring, with `store.EncodeRecordCursor` binding a
cursor to the record query and the record's `at` and `id`. That spec's
code block names the element type of `AddRows` as `Row` and is its
edit to make. `serve.Usage(ctx, store, query, now)` is what the route
calls: it fills an open range with the last day, validates, and reads
the rows, never nil.

The routes are [[011-api]]'s; the parameters and the response fields
are this spec's.

`GET /v1/usage` answers aggregates.

| Parameter | Type | Default | Rule |
|---|---|---|---|
| `from`, `to` | RFC 3339 | `to` now, `from` 24 hours before | `to` after `from`; the range at most 90 days |
| `by` | csv | none, one total row | `key`, `model`, `provider`, `owner`, `door`, `status`, `label:<name>`; at most three |
| `interval` | enum | `none` | `hour`, `day`, `month`, `none` |
| `key`, `model`, `provider`, `owner` | repeatable | none | names or ids; a filter, not a grouping; a name is resolved to the ids it names now, and a Key's `key_` id keeps working after the Key is deleted, which is how a run's ledger line stays readable ([[020-building-a-plane]]) |
| `label` | repeatable | none | `<name>=<value>` over the Key's labels |

The API asks the authorizer `usage.read` with the resolved `keys` and
the `owners` of the query in the `resource` ([[006-identity]]), and the
answer's `filter` is intersected with the query through
`metering.Intersect(q, owners, labels)`: an `owners` list narrows
`Owners` to the subjects both name, or to the filter's when the query
named none, and a `labels` map adds every pair, so a caller outside the
filter reads an empty result and never a 403 ([[011-api]]); an
intersection that selects nothing, owners with nothing in common or a
label the query names with another value, makes `Intersect` report
`ok` as false, and the route writes an empty `items` without a query. Under the owner policy
the filter is the caller's own subject. A parameter outside the table
is a `*metering.QueryError` naming the parameter in `Field`, which the
route answers `invalid_field` at that name.

A response row, `metering.Row`, is `{bucket, dimensions, requests, ok,
refused, failed, inputTokens, outputTokens, cachedInputTokens,
cacheWriteTokens, cost, currency, unpricedRequests}`. `bucket` is the
interval's start, RFC 3339 UTC, or absent for `interval` `none`;
`dimensions` is a map of each `by` name to its value, the `key_`,
`mdl_`, or `prv_` id for `key`, `model`, and `provider`, never the
name, because a Key's id keeps working after the Key is deleted and a
Model may be renamed under a running query; `ok`, `refused`, and
`failed` sum the `status` dimension of the aggregate rows. `currency`
is a dimension of every row whether or not it is named in `by`, because
two currencies are never summed; the unpriced and the refused requests
of a bucket are a row with an empty `currency`. `cost` is an `int64` of
micro-units, as in the record. The rows are ordered by bucket, then
each `by` value, then currency.

`GET /v1/requests` answers records, newest first, with `from`, `to`,
the same filters, plus `status`, `error`, and `stream`, a `cursor`, and
a `limit` of at most 1000. The response carries the records and a
`source` of `archive` or `memory`: `archive` when the request log
exporter of [[012-request-log-and-events]] is configured, which is the
durable and installation-wide answer, and `memory` otherwise, which is
the replica's own ring of the last `metering.RecordsPerKey` records per
key. A multi-replica installation without an archive is told by
`source` that it is reading one replica. The archive is read by
`internal/reqlog`'s reader ([[012-request-log-and-events]]): the range
names the hour prefixes, the objects under them are listed and decoded
newest first, and the filters are applied while reading, with the
`cursor` naming the object and offset to resume from.

### Retention and aggregates

A record is not a store row. The durable record set is the request log
archive of [[012-request-log-and-events]], one record per request in an
S3 compatible bucket, which is where an installation keeps them for as
long as it keeps anything. The store holds aggregates only, so no
variable configures how long the store keeps records and the store's
size is a function of the dimensions rather than of the traffic.

The aggregate, `metering.Aggregate`, is one row per hour per dimension
tuple, upserted by the Recorder's flush on the same interval as the
counters:

| Column | Type | Note |
|---|---|---|
| `bucket` | timestamptz | the hour, UTC |
| `key_id`, `model_id`, `provider_id`, `owner`, `door`, `status`, `currency` | text | the dimensions; the primary key is every one of them with `bucket` |
| `labels` | jsonb | the Key's labels, denormalised; a function of `key_id`, so it adds no rows |
| `requests`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `cache_write_tokens`, `cost_micro`, `unpriced` | bigint | summed on conflict |

`by=label:<name>` groups on `labels->><name>`, which costs nothing in
cardinality because `key_id` is already a dimension and a Key's labels
are one value per Key; a Key relabelled between two flushes reads with
its newest labels, because the upsert replaces them. Coarser intervals
are summed from the hours; `month` is summed from the hours of the
calendar month in UTC; an hour that overlaps the query's range is read
whole. The memory store keeps the same rows in a map by
`metering.AggregateKey` and the rings in a map by Key id, and loses
both at restart, which the start-up log says ([[010-state]]).

### The `metering` package

The windows, the counter keys, `Counters`, and `Claim` are as
[[007-keys-and-limits]] built them there; this spec adds the rest.

```go
// Status, Tokens, Charge, Ref, KeyRef, Attempt, Record as the table
// above; Record's JSON names are the table's.

// Cost prices t under p per Per tokens, one rounding half up over the
// whole sum; nil p is unpriced. Charged is the record's cost block.
func Cost(t Tokens, p *v1.Pricing) (v1.Money, bool)
func Charged(t Tokens, p *v1.Pricing) Charge

// Dimension is key, model, provider, owner, door, status, or
// label:<name>; Interval is none, hour, day, or month, and Bucket is
// the start of the bucket holding an instant, in UTC.
type Dimension string
type Interval string
func (d Dimension) Valid() bool
func (d Dimension) Label() (name string, ok bool)
func (i Interval) Valid() bool
func (i Interval) Bucket(at time.Time) time.Time

// Aggregate is the store's hourly row; Sums are its summed columns;
// AggregateKey is its primary key. Row is the response row.
type Aggregate struct { Bucket; KeyID, ModelID, ProviderID, Owner; Door; Status; Currency; Labels; Sums }
type Sums struct { Requests, InputTokens, OutputTokens, CachedInputTokens, CacheWriteTokens, Cost, Unpriced int64 }
type AggregateKey struct { ... }
type Row struct { Bucket; Dimensions; Requests, OK, Refused, Failed; ...Tokens; Cost int64; Currency; UnpricedRequests }

// AggregateOf is one record's hourly row; Aggregates folds records
// into hourly rows; Group sums hourly rows into response rows; Fold is
// Group over Aggregates. All four are pure.
func AggregateOf(r Record) Aggregate
func Aggregates(rs []Record) []Aggregate
func Group(as []Aggregate, by []Dimension, in Interval) []Row
func Fold(rs []Record, by []Dimension, in Interval) []Row

// Query and RecordQuery are the two routes' parameters, ids resolved.
const MaxRange, MaxBy, DefaultRange = 90 days, 3, 24 hours
type Query struct {
	From, To time.Time
	By       []Dimension
	Interval Interval
	Keys, Models, Providers, Owners []string
	Labels   map[string]string
}
type RecordQuery struct {
	Query
	Status Status
	Error  string
	Stream *bool
}
type QueryError struct{ Field, Detail string }
func (q Query) WithDefaults(now time.Time) Query
func (q Query) Validate() error               // a *QueryError at its parameter
func (q Query) Matches(a Aggregate) bool
func (q RecordQuery) Matches(r Record) bool
func Intersect(q Query, owners []string, labels map[string]string) (Query, bool)

// RecordsPerKey is the ring size per Key.
const RecordsPerKey = 1000
```

`Fold` is pure, which is what lets the aggregates be recomputed from
an archive and compared against the store's, and `Group` is the one
grouping the memory store runs, so the store and the fold cannot
disagree. The package imports `manifest/v1` and the standard library
and nothing else; `Status` is its own type with the gateway's three
values, because the gateway imports this package and not the reverse.

## Not in this spec

Where a limit refuses and with which code ([[007-keys-and-limits]]);
the routes and the OpenAPI document ([[011-api]]); the archive's
format, its batching, and the event sink
([[012-request-log-and-events]]); the counter and aggregate tables'
DDL ([[010-state]]); which target a request reached
([[008-routing-and-models]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every data plane request in the e2e tier, refused, failed, or successful, has exactly one record | `TestEveryRequestHasOneUsageRecord` | the package half passing in `internal/serve`, through `gateway.New` with this spec's Recorder and the real Key cache, catalog, Limiter, and router, over served, failed, and four kinds of refused request; the e2e tier's half is [[015-test-stubs-and-tiers]]'s |
| A Key under a hard Budget naming an unpriced Model is refused `model_unpriced` before any bytes reach a provider, and the refusal has a record | `TestUnpricedModelRefusedUnderABudget` | passing, `internal/serve` |
| A canary prompt, completion, header value, credential, and Key value appear in no record of an e2e run; `Record` has no `any` member | `TestRecordCarriesNoContent` | passing: the type half in `metering` by reflection over every member, the run half in `internal/serve` over every door with the five canaries and the aggregate rows beside the records; the e2e run is [[015-test-stubs-and-tiers]]'s |
| A corpus of pricings and token counts produces the golden costs, including the half-up boundary and `per` of 1, 1000, and 1000000 | `TestCostGolden`, table-driven | passing, `metering` |
| Cached input tokens are billed once, at the cached price, on each of the four dialects and on both a translated route and a passthrough | `TestCachedInputIsNotBilledTwice` | passing: the arithmetic in `metering`, the four dialects as passthroughs and four translations through `gateway.New` in `internal/serve` |
| Reasoning tokens are recorded and are not a term in the cost | `TestReasoningIsNotBilled` | passing, `metering`, and on the `lux` upstream's record in `internal/serve` |
| An upstream that reports no usage yields `estimated: true`, an input count from the estimator, and an output count of zero; an opaque route yields zero tokens, `estimated: true`, and `priced: false` | `TestEstimatedTokensAreMarked`, `TestOpaqueRouteIsUnpriced` | passing, `internal/serve`; the count route's unpriced record is in the first |
| A duration window's key is the same for every instant inside it and differs across the boundary; `month` resets on the first UTC; `none` never resets; the totals' key carries the word `none` | `TestWindowBoundaries` | passing, `metering`, beside [[007-keys-and-limits]]'s `TestWindowEpochs` and `TestCounterKeyScheme` |
| Three replicas spending against one hard limit at ten requests a second each with a one-cent request and a one-second flush overshoot by no more than `(R − 1) × F × T × C + C`, twenty-one cents, over a hundred runs; one replica by no more than one request | `TestTwoReplicaOvershootIsBounded` | passing, `internal/serve`: the twenty-one cents and the one request as figures, two replicas over twenty runs held to the bound; the hundred runs of three replicas are [[007-keys-and-limits]]'s `TestOvershootBound` |
| A rate limit admits `replicas` times its value across replicas and its value on one | `TestRateWindowIsPerReplica` | passing, `internal/serve` |
| A replica's local delta refuses before any flush, and a flush makes it visible to the other replica within one interval | `TestLocalDeltaRefusesImmediately` | passing, `internal/serve` |
| A soft Budget past its amount continues and emits `budget.exhausted` once per window; the marker counter's first add returns `1` on exactly one of six replicas | `TestSoftBudgetContinues`, `TestExhaustedMarkerIsClaimedOnce` | passing, `internal/serve` |
| The three counters under a Key's spend window and under `none` carry the requests, tokens, and spend the Key's admitted records sum to, the Recorder adding to none of them, and no window row exists for a Key without a spend limit | `TestKeyCountersFollowTheRecords` | passing, `internal/serve`, through `gateway.New` with the Limiter and the Recorder together |
| Aggregates folded from the records equal the store's rows for every grouping; no row sums two currencies; `requestLabels` is no dimension and appears in no aggregate row | `TestAggregatesMatchTheRecords`, `TestNoCurrencyIsSummed`, `TestRequestLabelsAreNotAggregated` | passing: the fold against an independent sum in `metering`, the store's rows against the fold in `storetest` over the memory store and in `internal/serve` after the Recorder's flush; the labels row in `metering` |
| `GET /v1/usage` rejects a range past 90 days, more than three `by` dimensions, and an unknown dimension; a `cost` in a row is an integer; the authorizer's `filter` of one owner leaves a query naming another owner's Key an empty result | `TestUsageQueryValidation`, `TestUsageFilterNarrows` | passing: the function half in `metering`, `Query.Validate` and `Intersect`, and `serve.Usage` in `internal/serve`; the route's half in `internal/api`, where `TestUsageQueryValidation` refuses each parameter and `TestUsageFilterNarrows` reads an empty result under another owner's filter |
| `lux_tokens_total`, `lux_spend_microunits_total`, and `lux_metering_flush_lag_seconds` follow a run's records and a stalled store | `TestMeteringMetrics` with [[019-observability]]'s `TestMetricsTable` | `TestMeteringMetrics` passing, `internal/serve`; `TestMetricsTable` is [[019-observability]]'s |
| `GET /v1/requests` reports `source` `archive` with the exporter configured and `memory` without it | `TestRequestsSource`, `TestRequestsArchiveSource` | passing: the `memory` half over the route and `storetest`'s `TestRecordsRingIsBounded` over the ring, the `archive` half over the reader of [[012-request-log-and-events]] |
| `metering` imports `manifest/v1` and the standard library and nothing else | [[001-architecture]]'s `TestRootPackagesDialNothing` | passing, `internal/arch` |

## Outcome

Built on 2026-09-13 and 2026-09-14 in eight commits on `main`,
`e2c9c1d` through `856e08e`, with the two routes landing in `63049e6`
beside [[011-api]]. It is proven by the whole gate and by per-package
coverage under the race detector of 98.9% for `metering`, 97.2% for
`internal/serve`, 98.9% for `internal/store` with 98.7% for the memory
store, and 93.7% for `internal/api`.

What was built: `metering/record.go`, the record and its blocks, and
`metering/cost.go`, `Cost` and `Charged`, the integer micro-unit
arithmetic with one half-up rounding over the whole sum;
`metering/fold.go`, the dimensions, the intervals, the hourly
`Aggregate`, and `AggregateOf`, `Aggregates`, `Group`, and `Fold` over
them; `metering/query.go`, the two queries, `WithDefaults`, `Validate`,
`Matches`, and `Intersect`; `internal/store`'s `Usage()` on the
contract with the memory store's hourly rows and per-Key ring, the
record cursor, and `storetest`'s cases; `internal/serve/recorder.go`,
the `Recorder` over `gateway.Record` with the three metrics and the
flush, and `internal/serve/usage.go`, `serve.Usage`;
`internal/config`'s `LUX_METERING_FLUSH`; `cmd/luxd`'s two flush loops
started and stopped with the jobs; and `internal/api/usage.go`, the
two routes over all of it. The Design above was rewritten to what was
built as this spec reached `testing`; what diverged from the text as
dispatched, each already fixed in the Design beside the rule it
settles:

- `Store.Usage().AddRows` takes `[]metering.Aggregate`, the hourly row
  with every dimension a member, because a row keyed by a `dimensions`
  map cannot be upserted on its primary key. [[010-state]]'s code block
  names that element type as built.
- The Postgres half of `Usage()` is [[010-state]]'s and landed with
  that spec's Postgres phase: the aggregates are a table of its schema,
  the records a ring the Postgres store borrows from the memory store,
  and both are held to the same contract suite the memory store runs.
- `GET /v1/requests` answers `source: memory` alone. The `archive`
  answer, its reader, and the hour-prefixed objects it lists are
  [[012-request-log-and-events]]'s, which closes the other half of the
  `TestRequestsSource` row above.
- The overshoot bound is asserted here with two replicas over twenty
  runs rather than the three over a hundred the criterion names,
  because [[007-keys-and-limits]]'s `TestOvershootBound` already runs
  that shape with the same figures and a bound is worth proving once.
- The e2e halves of the first and the third criteria, one record per
  request and the five canaries over a run, are
  [[015-test-stubs-and-tiers]]'s tier; the package halves pass through
  `gateway.New` with the real Key cache, catalog, Limiter, and router.
- `TestMetricsTable`, the other half of the metrics criterion, is
  [[019-observability]]'s.

What the neighboring specs own from here:
[[012-request-log-and-events]] adds the archive, the exporter, and the
reader that makes `source` read `archive`, and with it the durable
record set this spec's retention rule assumes;
[[015-test-stubs-and-tiers]] runs the two e2e halves;
[[019-observability]] holds the metric names in its table;
[[010-state]] added the Postgres aggregates and counters and the
`AddRows` signature; [[011-api]] owns the two routes' parsing,
refusals, and OpenAPI operations, which this spec's parameter tables
define.
