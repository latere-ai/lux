---
title: "Usage and metering: the record, cost, windows, the usage API, the multi-replica rule"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/007-keys-and-limits.md
  - specs/008-routing-and-models.md
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

Nothing is built. The hosted gateway this design is extracted from
prices a call in floating point USD per million tokens from a rate card
compiled into the binary and a live snapshot of one provider's price
list (`internal/rates`), rounds to USD micro-units, and marks a model
the card does not know with `-1` and a `model_unknown` flag while
serving the call; keeps its running totals in Redis, one day bucket
per key and per principal and one calendar-month bucket per funded
principal (`internal/store/redisusage`), and reads a rolling sum of the
day buckets on every request to enforce a cap; writes the per-call
record to a Redis stream drained to an S3 bucket as NDJSON
(`internal/store/reqlog`); and answers its usage routes by folding the
recent stream window. Its earlier Postgres usage table was retired. This
design keeps the micro-unit integer and the archive, moves the price
onto the `Model` an operator declares so the gateway ships no card,
replaces the day buckets with fixed windows every replica computes from
the clock, and takes the per-request store read off the hot path.

## Design

### The record

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
| `cost` | object | `{amount, currency, priced}`: `amount` an `int64` count of micro-units of `currency`, so `1250000` is `1.25`; the money strings of [[003-manifest-contract]] are what people write in a manifest and what `status` renders, and a record and a usage row carry the integer, never the string |
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
refusing without one would hide the most useful case.

### Tokens

| Source | When | Marks |
|---|---|---|
| the upstream's own usage | the response carries the dialect's usage members | `estimated: false` |
| a stream's own usage | the members are read as the last value of each across the stream; an `/openai` chat completion stream carries them because [[004-request-path]] sets `stream_options.include_usage` on the way out | `estimated: false` |
| an estimate | the response carries none, or a stream ended before its usage frame | `estimated: true` |

On a translated route the counts come from `ir.Usage`, which
`llmdialect` fills from the upstream dialect's members. On a
passthrough the gateway reads the same members itself:

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
`llmdialect/tokencount.Estimate(*ir.Request)` over the intermediate
request and `tokens.output` is `0`, because no exported estimator
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
unpriced until an operator declares it, and the hosted plane's compiled
rate card and its provider price snapshot are the platform's to keep
as Models it applies.

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
cannot price is not a limit.

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
| key totals | the three above with window `none` | `none` | the same three over the Key's lifetime | the store |
| budget spend | `budget:<budget id>:spend:<start>` | `Budget.spec.window` | cost in micro-units | the store |
| exhausted marker | `key:<key id>:exhausted:<start>`, `budget:<budget id>:exhausted:<start>` | the object's spend window | `1`, added by a replica that observes the window at or over its amount; the add that returns `1` is the one that emits the event ([[007-keys-and-limits]], [[012-request-log-and-events]]) | the store |

`<start>` is the window start as Unix seconds, so a key names exactly
one window and a finished window's row is prunable by its own name.
The spend counter is what the limit of [[007-keys-and-limits]] reads;
the requests and tokens counters, and the three totals, are what
`status.usage` renders, and the window rows exist only for a Key with
a spend limit. The per-minute rate limits are not counters here at all:
they are that spec's per-replica buckets and never reach the store,
which is the multi-replica rule below.

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
There is no per-request store read: the check a hard limit makes is

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
settle and at flush: `lux_tokens_total{direction}` adds each record's
`input`, `output`, `cachedInput`, and `cacheWrite`;
`lux_spend_microunits_total{currency}` adds each priced record's
`cost.amount`; `lux_metering_flush_lag_seconds` is the seconds since
this replica's last successful `Flush`, so a stalled store shows as a
growing gauge before any limit is wrong by more than the bound.

### Configuration

| Variable | Default | Rule |
|---|---|---|
| `LUX_METERING_FLUSH` | `1s` | at least `100ms`, at most `1m`; the flush interval of every spend counter and of the aggregates; listed with its owner in [[002-repository-scaffold]]'s table |

### The usage API

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
answer's `filter` is intersected with the query: an `owners` list
narrows the rows to those Keys' owners and a `labels` map to Keys
carrying every pair, so a caller outside the filter reads an empty
result and never a 403 ([[011-api]]). Under the owner policy the filter
is the caller's own subject.

A response row is `{bucket, dimensions, requests, ok, refused, failed,
inputTokens, outputTokens, cachedInputTokens, cacheWriteTokens,
cost, currency, unpricedRequests}`. `bucket` is the interval's start,
RFC 3339 UTC, or absent for `interval` `none`; `dimensions` is a map of
each `by` name to its value; `ok`, `refused`, and `failed` sum the
`status` dimension of the aggregate rows. `currency` is a dimension of
every row whether or not it is named in `by`, because two currencies
are never summed. `cost` is an `int64` of micro-units, as in the
record.

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

The aggregate is one row per hour per dimension tuple, upserted by the
same flush that writes the counters:

| Column | Type | Note |
|---|---|---|
| `bucket` | timestamptz | the hour, UTC |
| `key_id`, `model_id`, `provider_id`, `owner`, `door`, `status`, `currency` | text | the dimensions; the primary key is every one of them with `bucket` |
| `labels` | jsonb | the Key's labels, denormalised; a function of `key_id`, so it adds no rows |
| `requests`, `input_tokens`, `output_tokens`, `cached_input_tokens`, `cache_write_tokens`, `cost_micro`, `unpriced` | bigint | summed on conflict |

`by=label:<name>` groups on `labels->><name>`, which costs nothing in
cardinality because `key_id` is already a dimension and a Key's labels
are one value per Key. Coarser intervals are summed from the hours;
`month` is summed from the hours of the calendar month in UTC. The
memory store keeps the same rows in maps and loses them at restart,
which the start-up log says ([[010-state]]).

### The `metering` package

```go
// Cost, Tokens, Record, Attempt as above.

// Window returns the bounds of w containing at. resetsAt is the zero
// time for window none.
func Window(w v1.Window, at, createdAt time.Time) (start, resetsAt time.Time)

type Scope string

const (
	ScopeKeyRequests     Scope = "key:requests"
	ScopeKeyTokens       Scope = "key:tokens"
	ScopeKeySpend        Scope = "key:spend"
	ScopeKeyExhausted    Scope = "key:exhausted"
	ScopeBudgetSpend     Scope = "budget:spend"
	ScopeBudgetExhausted Scope = "budget:exhausted"
)

// CounterKey renders the key of the counter table: one key names one
// window of one object. For ScopeKeySpend under a Key with no spend
// limit, and for the totals, w is WindowNone and start is createdAt.
func CounterKey(s Scope, id string, w v1.Window, at, createdAt time.Time) string

// CounterStore is what the store satisfies (010).
type CounterStore interface {
	AddCounter(ctx context.Context, key string, delta int64, expiresAt time.Time) (total int64, err error)
	ReadCounters(ctx context.Context, keys []string) (map[string]int64, error)
}

// Counters holds one replica's deltas over a CounterStore.
type Counters struct{ ... }

func NewCounters(store CounterStore, flush time.Duration) *Counters
func (c *Counters) Add(key string, n int64)                 // local, lock-free on the hot path
func (c *Counters) Total(key string) int64                  // last store total plus local delta
func (c *Counters) Flush(ctx context.Context) error         // one add per dirty key
func (c *Counters) Run(ctx context.Context) error           // flush on the interval until ctx ends

// Row is one aggregate row; Fold turns records into rows. Query and
// RecordQuery are the parameters of the two usage routes above, which
// the store answers (010).
type Row struct{ ... }
type Dimension string
type Interval string
type Query struct {
	From, To time.Time
	By       []Dimension
	Interval Interval
	Keys, Models, Providers, Owners []string
	Labels   map[string]string
}
type RecordQuery struct {
	Query
	Status, Error string
	Stream        *bool
}

func Fold(rs []Record, by []Dimension, in Interval) []Row

// RecordsPerKey is the memory store's ring size per key.
const RecordsPerKey = 1000
```

`Counters.Add` is called once per request per counter on the hot path
and never blocks on the store; `Flush` is the only method that touches
it. `Fold` is pure, which is what lets the aggregates be recomputed
from an archive and compared against the store's.

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
| Every data plane request in the e2e tier, refused, failed, or successful, has exactly one record | `TestEveryRequestHasOneUsageRecord` | not built |
| A Key under a hard Budget naming an unpriced Model is refused `model_unpriced` before any bytes reach a provider, and the refusal has a record | `TestUnpricedModelRefusedUnderABudget` | not built |
| A canary prompt, completion, header value, credential, and Key value appear in no record of an e2e run; `Record` has no `any` member | `TestRecordCarriesNoContent` | not built |
| A corpus of pricings and token counts produces the golden costs, including the half-up boundary and `per` of 1, 1000, and 1000000 | `TestCostGolden`, table-driven | not built |
| Cached input tokens are billed once, at the cached price, on each of the four dialects and on both a translated route and a passthrough | `TestCachedInputIsNotBilledTwice` | not built |
| Reasoning tokens are recorded and are not a term in the cost | `TestReasoningIsNotBilled` | not built |
| An upstream that reports no usage yields `estimated: true`, an input count from the estimator, and an output count of zero; an opaque route yields zero tokens, `estimated: true`, and `priced: false` | `TestEstimatedTokensAreMarked`, `TestOpaqueRouteIsUnpriced` | not built |
| A duration window's key is the same for every instant inside it and differs across the boundary; `month` resets on the first UTC; `none` never resets; the totals' key is the Key's `createdAt` | `TestWindowBoundaries` | not built |
| Three replicas spending against one hard limit at ten requests a second each with a one-cent request and a one-second flush overshoot by no more than `(R − 1) × F × T × C + C`, twenty-one cents, over a hundred runs; one replica by no more than one request | `TestTwoReplicaOvershootIsBounded` | not built |
| A rate limit admits `replicas` times its value across replicas and its value on one | `TestRateWindowIsPerReplica` | not built |
| A replica's local delta refuses before any flush, and a flush makes it visible to the other replica within one interval | `TestLocalDeltaRefusesImmediately` | not built |
| A soft Budget past its amount continues and emits `budget.exhausted` once per window; the marker counter's first add returns `1` on exactly one of six replicas | `TestSoftBudgetContinues`, `TestExhaustedMarkerIsClaimedOnce` | not built |
| The three counters under a Key's spend window and under `none` carry the requests, tokens, and spend the Key's records sum to, and no window row exists for a Key without a spend limit | `TestKeyCountersFollowTheRecords` | not built |
| Aggregates folded from the records equal the store's rows for every grouping; no row sums two currencies; `requestLabels` is no dimension and appears in no aggregate row | `TestAggregatesMatchTheRecords`, `TestNoCurrencyIsSummed`, `TestRequestLabelsAreNotAggregated` | not built |
| `GET /v1/usage` rejects a range past 90 days, more than three `by` dimensions, and an unknown dimension; a `cost` in a row is an integer; the authorizer's `filter` of one owner leaves a query naming another owner's Key an empty result | `TestUsageQueryValidation`, `TestUsageFilterNarrows` | not built |
| `lux_tokens_total`, `lux_spend_microunits_total`, and `lux_metering_flush_lag_seconds` follow a run's records and a stalled store | `TestMeteringMetrics` with [[019-observability]]'s `TestMetricsTable` | not built |
| `GET /v1/requests` reports `source` `archive` with the exporter configured and `memory` without it | `TestRequestsSource` | not built |
| `metering` imports `manifest/v1` and the standard library and nothing else | [[001-architecture]]'s `TestRootPackagesDialNothing` | not built |
