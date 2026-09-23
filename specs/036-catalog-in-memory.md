---
title: "The catalog in memory: Models, Providers, and sealed credentials served from a per-replica snapshot, and serving through a store outage"
status: in-progress
track: core
depends_on:
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/007-keys-and-limits.md
  - specs/009-usage-and-metering.md
  - specs/010-state.md
  - specs/012-request-log-and-events.md
  - specs/019-observability.md
affects: [internal/serve/, internal/config/, cmd/luxd/, docs/configuration.md, docs/observability.md, docs/performance.md, docs/security.md, specs/002-repository-scaffold.md, specs/007-keys-and-limits.md, specs/019-observability.md]
effort: medium
created: 2026-09-23
updated: 2026-09-23
author: changkun
---

# The catalog in memory

## Overview

On the Postgres store, every data plane request reads the catalog from
the database: the Model it names, the Provider of every target, and the
Provider's sealed credential, and the Recorder reads the Model again to
price the record. The Key and the Budget already come from a
per-replica cache ([[007-keys-and-limits]]); the catalog does not. The
catalog is small, changes rarely, and every change to it that matters on
the data plane is already written to the journal, so each replica can
hold all of it in memory and serve the doors with no store read at all.

This spec replaces the per-request catalog reads with one snapshot per
replica, kept current by the journal tail the Key cache already runs and
by a periodic full reload for the few writes the journal does not name.
It also lets a replica keep serving through a store outage for a bounded
grace, and states how the spend made during the outage reaches the
store once it answers again.

## Current state

What one request reads from the store on 2026-09-23, on the Postgres
store:

| Read | Where | When |
|---|---|---|
| the Model by exact name | `gateway/handler.go:327` through `serve.Catalog.Model` (`internal/serve/catalog.go`), `Objects.ByName` | every request |
| the Provider of every target of that Model, before one is chosen | `gateway/router.go:139`, `serve.Catalog.Provider` | every request, once per target |
| the Provider's sealed credential, then two AES-GCM opens | `gateway/forward.go:188` through `serve.StoreCredentials.Credential` (`internal/serve/credentials.go:31`) | every attempt |
| the Model again, for its pricing | `internal/serve/recorder.go:208` | every record, after the response |
| every live Model, then the Provider of every reachable target | `gateway/forward.go:520-534` | every request on a passthrough route |
| every live Model, for the door's model list | `serve.Catalog.Models`; the list keeps a Model whose `status.available` is true (`gateway/handler.go:439`) | every model-list request |

A request to a Model with two targets is therefore four store round
trips before the first byte and one after the response. Nothing caches
them: the only caches on the data plane are the Key cache and the
Limiter's counters. The memory store and the file mode hold the catalog
in process already, so the cost is the Postgres store's, where each read
also holds one of the pool's connections, a budget an installation often
shares with other services.

What already exists to build on:

- The journal is written for every mutation whether or not a sink is
  configured, in the mutation's own transaction: an API apply or delete
  (`internal/api/events.go:41` through `internal/events/event.go:108`),
  a bootstrap write (`internal/bootstrap/bootstrap.go:535-560`, the
  object, the credential, and the event in one `Transact`), and
  discovery's creates and removals (`internal/serve/discovery.go:387`,
  `:405`).
- Every replica tails `Journal.Since` once a second for the Key cache
  (`internal/serve/keys.go:372-398`), and the discovery lease holder
  tails it for Provider changes (`internal/serve/discovery.go:174`).
- The router's health is a per-replica view (`gateway/router.go:50-63`,
  `serve.Health.View`), refreshed by the health job's tick on every
  replica and never read from the store per request.
- The file mode's `SIGHUP` re-read writes no journal row, and `luxd`
  calls the Key cache's `Reset` after it.

Writes that change what the data plane reads and are not journaled:

| Write | Where | What changes on the data plane |
|---|---|---|
| discovery updates a discovered Model whose shape changed | `internal/serve/discovery.go:388-392`, `Objects.Put` with no event | the Model's targets and pricing |
| the health job fills `status.available` of a Model that has none, a Model declared or discovered since the last tick | `internal/serve/health.go` `fillModels`, `PutStatus` | whether the model list shows it |
| the health job resets a Provider whose probe mode is `none` to Unknown and refreshes its Models | `internal/serve/health.go` `resetToUnknown` | `status.available` of those Models |
| `luxd rewrap` re-wraps every data key under the first key of `LUX_SECRETS_KEK` | `cmd/luxd/main.go` `rewrapCmd`, `Credentials.Rewrap` | the sealed row, not the plaintext |

A health state change does refresh the Provider's Models before it
journals `provider.unreachable` or `provider.healthy`
(`internal/serve/health.go` `publish`), so a reader that reloads the
Provider's Models on that row sees the new `status.available`. The
Provider's own observed members (health detail, tunnel, discovery
count) are written by `PutStatus` without a row, and no door reads them.

## Design

### Options

| | Shape | For | Against |
|---|---|---|---|
| A | A read-through cache per object, by name and by id, for a window like `LUX_KEY_CACHE`, evicted by the journal tail | bounded memory whatever the catalog's size; the Key cache's shape | a miss still reads the store, once per object per window per replica; the model list and the passthrough route list every Model, which a per-object cache does not serve; an unknown model name needs negative entries to stay off the store |
| B | A full snapshot per replica: every live Model and Provider and every sealed credential row, loaded at start, updated by the journal tail, and replaced by a full reload on an interval as the backstop for unjournaled writes | zero store reads per request in steady state; the list reads are served too; one source of truth per replica, swapped whole, so a request never sees half a change | memory grows with the catalog; the backstop re-reads the whole catalog on every replica on its interval |
| C | A shared external cache, such as Redis, in front of the store | one copy for every replica | a network round trip per read, which is the cost being removed; a new stateful dependency the store contract of [[010-state]] does not have; its own consistency with the journal |

**Recommendation: B.** The catalog is a few hundred objects in a large
installation, and a full reload is two lists and one credential read
per Provider (`Credentials.List` returns ids, `Credentials.Get` one row,
[[010-state]]), while A keeps store traffic proportional to request
rate and C keeps the round trip. C is not offered further.

### The snapshot

`serve.CatalogSnapshot` holds, behind one `atomic.Pointer`:

- every live Model, by exact name, and in name order for the lists;
- every live Provider, by name and by `prv_` id;
- every Provider's sealed credential row, by Provider id, as stored:
  the wrapped data key and the ciphertext, never the plaintext.

It satisfies the `gateway.Catalog` and the credential source the doors
take today, so the gateway package does not change. A credential is
opened per attempt from the sealed row in memory, two AES-GCM opens of a
few dozen bytes, so the plaintext exists only for the life of one
outbound request, as it does now (invariant 2 of [[001-architecture]]).
An open that fails re-reads that one row from the store once and opens
it again, which covers a row the snapshot loaded before `luxd rewrap`
re-wrapped it, on a replica whose key list no longer holds the key it
was wrapped under; a second failure is the error the door answers
today.

A change never edits the snapshot in place: the tail or the reload
builds the next snapshot from the current one and swaps the pointer, so
no lookup sees half a change. Separate lookups of one request, the Model
and later its pricing, may straddle a swap, as they straddle a store
write today.

The snapshot serves the data plane only. The `/v1` control plane keeps
reading the store, so an apply and the read after it agree, and the
authorizer's reference lookups at apply time ([[006-identity]]) see
current state.

### Keeping it current

**The tail.** The Key cache's tail becomes one reader per replica that
hands each row to both the Key cache and the snapshot, so the store sees
one `Journal.Since` per second per replica, as today. On a row the
snapshot reloads what it names, from the store, and swaps:

| Row | Reload |
|---|---|
| `provider.created`, `provider.updated` | the Provider, its credential row, and every Model with a target on it |
| `provider.deleted` | drop the Provider and its credential row; reload every Model with a target on it |
| `provider.unreachable`, `provider.healthy` | every Model with a target on the Provider, for `status.available` |
| `model.created`, `model.updated`, `model.discovered` | the Model |
| `model.deleted`, `model.removed` | drop the Model |

A reload that fails leaves the snapshot as it was, counts the failure,
and marks the snapshot for a full reload at the next tick of the tail,
so a lost row costs at most one interval and never a stale object until
the backstop.

**The backstop.** Every `LUX_CATALOG_RELOAD` (default `30s`) each
replica reads every live Model and Provider and every credential row,
builds a new snapshot, and swaps it. This is what brings in the writes
of the table in "Current state" that no row names.

**Discovery's shape updates are journaled** (decided on 2026-09-23). A
discovered Model whose shape changed is updated without a row today
(`internal/serve/discovery.go:388-392`). Discovery now journals
`model.updated` with reason `discovery` beside the update, in the same
transaction, so the change reaches every replica within the tail like
every other catalog change, and an operator's sink learns that a
discovered Model changed. What discovery changes on a Model it
declared is its labels, which follow the Provider's, and its modalities;
leaving that to the backstop was the alternative, with no new row at the
sink but a Model served under its old labels, which an authorizer's
selectors read, for up to `LUX_CATALOG_RELOAD`. The row's data is the
changed paths, as an API update's is, from one `events.ChangedPaths`
that the API, bootstrap, and discovery share.

**The replica that took the write.** An apply, a delete, or a rotation
through `/v1` runs the Key cache's tail on its own replica as soon as its
transaction commits (`api.Options.Committed`), so a caller that applies
a Model and calls it through the same replica is served at once, and a
Key deleted there is refused there at once; every other replica sees
either within its tail. Tails are serialized, so the commit's and the
loop's never interleave.

**The file mode.** The `SIGHUP` re-read swaps the file mode's snapshot
and `luxd` rebuilds the catalog snapshot from it, beside the Key cache's
`Reset`.

### Staleness

| Change | Seen by every replica within |
|---|---|
| an apply or delete of a Provider or a Model, its credential included, through `/v1` or a bootstrap | the tail interval, one second, plus one reload |
| a discovered Model created or removed | the tail interval plus one reload |
| a discovered Model's shape changed | the tail interval plus one reload |
| a Provider's health state change, and the `status.available` of its Models | the tail interval plus one reload |
| `status.available` of a Model the health job filled or reset | `LUX_CATALOG_RELOAD` |
| a credential re-wrapped by `luxd rewrap` | `LUX_CATALOG_RELOAD`; until then the replica opens the old wrap with the key list it started with, or re-reads the row on a failed open |
| the file mode's `SIGHUP` | at once |

### Start and a store that does not answer

`luxd serve` loads the snapshot before `/readyz` answers ready: a new
readiness check, `catalog`, beside `draining` and `store`
(`cmd/luxd/main.go:331-335`), passes once the first full load has
succeeded and stays passed. A failed first load is retried every
tail interval, and the replica stays not ready until one succeeds.

When the store stops answering, the replica keeps serving from the last
snapshot and its age grows. The age is a metric and an alert and never a
readiness failure (decided on 2026-09-23): failing readiness on age
would take every replica out of the load balancer together in a full
store outage, turning a degraded gateway into none. The cost is that a
replica cut off from the store while its peers reach it serves an older
catalog than they do, for as long as the section below lets it serve.

### Serving through a store outage, and the late correction

The catalog snapshot alone does not keep a replica serving. The Key
cache drops an entry once its window has passed
(`internal/serve/keys.go:278-291`) and returns the store's error on a
failed read (`:189-201`), which the door answers `store_unavailable`.
So today every call is refused about `LUX_KEY_CACHE` (default `10s`)
into an outage, whatever the snapshot holds.

**The stale grace.** `LUX_KEY_CACHE_GRACE` (default `5m`, decided by
the maintainer on 2026-09-23; `0` disables it) lets the Key cache serve
an entry past its window when, and only when, the store read that would
replace it fails:

- A positive Key or Budget entry whose window has passed is kept, not
  dropped, until its window plus the grace. A lookup that finds it
  reads the store as today; on a store answer the entry is replaced, and
  on a store error the expired entry is served and counted as `stale`
  in `lux_key_cache_hits_total` ([[007-keys-and-limits]] owns the
  metric; this spec adds the value).
- Past the window plus the grace, a failed read is `store_unavailable`,
  as today.
- A negative entry, an unknown value, is never served stale: a Key
  created just before the outage is refused `store_unavailable`, not
  `unauthenticated`.
- A Key first seen during the outage has no entry and is refused
  `store_unavailable`.
- The door's own checks still run on the cached Key: a disabled Key is
  refused, and a Key past its `expiresAt` is `key_expired`
  (`gateway/key.go:55`), so the grace never extends a Key past its own
  expiry.
- A failure is still cached nowhere: the grace serves the last good
  answer, and every lookup past the window tries the store first.

Together with the catalog snapshot, the Limiter's local counters and
the recorder's local rows, a replica serves a Key it has seen for up to
`LUX_KEY_CACHE + LUX_KEY_CACHE_GRACE` after its last successful read of
that Key.

What the grace costs:

| Cost | Bound |
|---|---|
| A revocation, disable, or rotation this replica has not consumed from the journal is not seen until the store answers again | at most the window plus the grace; a revocation cannot commit while the store is down for everyone, so this is the replica that is cut off while its peers are not, or a row written just before the outage |
| A hard Budget is passed | each replica sees only its own spend since its last successful flush, so the overshoot is the other replicas' spend over that time: `(R - 1) × D × T × C + C`, the bound of [[009-usage-and-metering]] with the flush interval replaced by `D`, the time since the last successful flush, at most `LUX_KEY_CACHE + LUX_KEY_CACHE_GRACE`; and never more than `(R - 1)` times what the Budget had left at that flush, plus one request per replica |

An operator who needs a revocation to hold within `LUX_KEY_CACHE`
whatever the store does sets the grace to `0`.

**The late correction.** It already exists, and the grace relies on it:

- The Limiter's counters keep an unflushed delta in `pending` when the
  store's add fails, and the next flush retries it
  (`metering/counters.go:140-147`, `:163-166`). A replica's own `Total`
  counts that delta throughout, so its own hard checks stay right.
- The Recorder puts its hourly rows back when `AddRows` fails, merged
  with any rows recorded since (`internal/serve/recorder.go:269-276`),
  and `lux_metering_flush_lag_seconds` grows meanwhile.

So the spend made during the outage lands on the Budgets' counters and
in the usage rows at the first flush after the store returns. A replica
learns the combined total from its own flush of a delta, whose add
returns it, so the replica that flushed last refuses at once and every
other replica refuses after at most one more request, the one-request
term of [[009-usage-and-metering]]'s bound. `budget.exhausted` is raised
by the first refusal: the marker claim during the outage fails and is logged
(`internal/serve/limits.go:285-303`). The overshoot is therefore
visible, not lost: `GET /v1/usage` and the counters carry the true
spend, and the operator's ledger, reading usage, sees the true spend
and reconciles it.

What the correction does not cover is a replica that stops during the
outage: its last flush on stop (`metering/counters.go` `Run`) fails as
well, and its unflushed deltas and rows are lost with the process.

| | Shape | For | Against |
|---|---|---|---|
| A | Accept the loss; the metric and the alert say how long the flush lagged | nothing new | the spend of a replica that crashes or is rolled during an outage is never recorded |
| B | A local disk spool: a replica appends each unflushed delta and row to a file on its volume and replays it at start | no loss across a restart on the same volume | a writable volume per replica, a replay format and its tests; a replica rescheduled onto another node still loses the spool unless the volume follows it |

**Recommendation: A** as the default. B is an option for an operator
whose outages are long enough, and whose replicas restart often enough
during them, for the loss to matter; it is not built by this spec.

### Memory

The snapshot is every Model and Provider manifest as the store returns
it, and every sealed credential row. A Model is a few hundred bytes to
a few kilobytes of JSON; an installation with ten thousand discovered
Models holds tens of megabytes. There is no cap: a cap would make the
catalog incomplete, which is a correctness failure, where the size is
an operator's to see. `lux_catalog_objects` reports it.

### Configuration

Two rows in [[002-repository-scaffold]]'s table:

| Variable | Spec | Default | Meaning |
|---|---|---|---|
| `LUX_CATALOG_RELOAD` | 036 | `30s` | how often a replica re-reads the whole catalog, the backstop for writes the journal does not name (5s to 10m) |
| `LUX_KEY_CACHE_GRACE` | 036 | `5m` | how long past its window a cached Key or Budget is served while the store does not answer; `0` refuses at the window as before (0 to 1h) |

The tail keeps the Key cache's one-second interval and no variable.

### Metrics

Three rows in [[019-observability]]'s table, owned by this spec:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `lux_catalog_age_seconds` | gauge | none | seconds since the snapshot last matched the store: the later of the last successful full reload and the last tail read that left nothing to reload |
| `lux_catalog_reloads_total` | counter | `trigger` (`start`, `tail`, `backstop`, `sighup`), `result` (`ok`, `error`) | snapshot reloads |
| `lux_catalog_objects` | gauge | `kind` (`Model`, `Provider`, `credential`) | objects the snapshot holds |

`lux_key_cache_hits_total` ([[007-keys-and-limits]]) gains the
`result` value `stale`, a lookup served past its window under the grace.

Two alerts beside `LuxMeteringFlushLagging`: `LuxCatalogStale`,
`max(lux_catalog_age_seconds) > 90`, three default backstop intervals,
held for 5m; and
`LuxKeyCacheServingStale`,
`sum(rate(lux_key_cache_hits_total{result="stale"}[5m])) > 0` held for
1m, a replica serving through a store outage.

## Not in this spec

- Keys and Budgets as snapshot contents. They stay in the Key cache:
  their number grows with callers, not with the catalog, and the
  cache's bound is the right shape for them. This spec adds only the
  stale grace to it.
- The disk spool of option B under the late correction.
- The control plane's reads, which stay on the store.
- The router's health, which is already per replica.
- A shared cache across replicas (option C).
- The request log and its archive ([[012-request-log-and-events]]), and
  `GET /v1/requests` across replicas.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | With a store that counts its calls, a request to a Model with two targets, its record priced, performs zero `Objects` and `Credentials` reads once the snapshot is loaded; so do a model-list request and a passthrough request | `TestCatalogSnapshotServesWithoutStoreReads`, `internal/serve`, over a counting wrapper of the memory store |
| 2 | An apply of a Model through `/v1` on one replica is served by a second replica sharing the store within two tail intervals, and a delete is refused there as `model_not_found` in the same bound | `TestCatalogSnapshotFollowsTheJournal`, two servers over one Postgres store in the Postgres tier |
| 3 | A Provider credential replaced through `/v1` is the one injected on the second replica's next request after the tail consumes the row | `TestCatalogSnapshotRotatedCredential`, stub provider asserting the header |
| 4 | A `status.available` written by `PutStatus` alone appears in the model list within `LUX_CATALOG_RELOAD` and not before the backstop runs | `TestCatalogSnapshotBackstop`, fake clock |
| 5 | A discovered Model whose shape changed, its Provider's labels here, is journaled `model.updated` with reason `discovery` and its changed paths in the update's transaction, and is served in its new shape by a replica after one tail | `TestDiscoveryShapeUpdateIsJournaled`, `internal/serve` |
| 6 | `/readyz` answers not ready until the first full load succeeds, and a store that fails the first load keeps it not ready | `TestCatalogReadiness`, `cmd/luxd` |
| 7 | With the store failing after the load, requests whose Keys are cached are served from the snapshot, `lux_catalog_age_seconds` grows, and `/readyz` stays ready | `TestCatalogSnapshotStoreDown`, `internal/serve`; readiness in `cmd/luxd` |
| 8 | A sealed credential that fails to open is re-read from the store once and opened; a second failure is the error the door answers today; no plaintext credential is held in the snapshot | `TestCatalogSnapshotReopensOnce`, and a reflection check that the snapshot's types carry no plaintext field |
| 9 | Lookups racing a stream of swaps each return objects of one whole snapshot, under the race detector | `TestCatalogSnapshotSwapIsAtomic`, `internal/serve` |
| 10 | The three metrics, the `stale` result, and the two alerts are in [[019-observability]]'s table and on `/metrics`, and `LUX_CATALOG_RELOAD` and `LUX_KEY_CACHE_GRACE` are in [[002-repository-scaffold]]'s table and `docs/configuration.md` | `TestMetricsTable`; `TestConfigurationReferenceIsCurrent` and `TestConfigurationReferenceMatchesCode`, `internal/config` |
| 11 | A benchmark of the catalog lookups a request makes, over the snapshot and over the Postgres store, is added beside those of [[023-performance-and-benchmarks]], and its numbers are written into `docs/performance.md` | `BenchmarkCatalogLookups`, `internal/serve`, the Postgres half in the Postgres tier |
| 12 | With a store that fails every read after a Key was cached, the Key is served past `LUX_KEY_CACHE` and until `LUX_KEY_CACHE + LUX_KEY_CACHE_GRACE`, each such lookup counted `stale`, and refused `store_unavailable` after it; with the grace `0` it is refused at `LUX_KEY_CACHE` as today | `TestKeyCacheStaleGrace`, `internal/serve`, fake clock over a failing store wrapper |
| 13 | Under the grace, a negative entry is not served stale, a Key never cached is refused `store_unavailable`, a disabled cached Key is refused, and a cached Key past its `expiresAt` is `key_expired` | `TestKeyCacheStaleGraceLimits`, `internal/serve` |
| 14 | Under the grace, a lookup past the window reads the store first, and a store that answers again replaces the stale entry at once | `TestKeyCacheStaleGraceRecovers`, `internal/serve` |
| 15 | Spend counter deltas and hourly usage rows made on two replicas while the store fails every write land in the store at the first flush after it answers, summed exactly; the replica that flushed last then refuses a hard Budget the combined spend has passed, the other after at most one more request, and `budget.exhausted` is raised once | `TestLateCorrectionAfterOutage`, two Limiters and Recorders over one failing-then-healthy store in `internal/serve` |
