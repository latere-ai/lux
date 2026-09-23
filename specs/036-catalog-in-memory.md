---
title: "The catalog in memory: Models, Providers, and sealed credentials served from a per-replica snapshot"
status: drafted
track: core
depends_on:
  - specs/004-request-path.md
  - specs/005-providers.md
  - specs/007-keys-and-limits.md
  - specs/009-usage-and-metering.md
  - specs/010-state.md
  - specs/012-request-log-and-events.md
  - specs/019-observability.md
affects: [internal/serve/, internal/config/, cmd/luxd/, docs/configuration.md, docs/observability.md, docs/performance.md, specs/002-repository-scaffold.md, specs/019-observability.md]
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

**Decision 1: discovery's shape updates.** A discovered Model whose
shape changed is updated without a row today.

| | Shape | For | Against |
|---|---|---|---|
| (i) | Discovery journals `model.updated` with reason `discovery` beside the update, in the same transaction | the change reaches every replica within the tail, like every other catalog change; an operator's sink learns that a discovered Model changed | the sink receives a row it did not receive before |
| (ii) | Leave it to the backstop | no change to the events | a changed target or price serves up to `LUX_CATALOG_RELOAD` late |

**Recommendation: (i).** A changed price that bills for thirty seconds
at the old one is a metering error, and the row is true.

**The file mode.** The `SIGHUP` re-read swaps the file mode's snapshot
and `luxd` rebuilds the catalog snapshot from it, beside the Key cache's
`Reset`.

### Staleness

| Change | Seen by every replica within |
|---|---|
| an apply or delete of a Provider or a Model, its credential included, through `/v1` or a bootstrap | the tail interval, one second, plus one reload |
| a discovered Model created or removed | the tail interval plus one reload |
| a discovered Model's shape changed | the tail interval under decision 1 (i); `LUX_CATALOG_RELOAD` under (ii) |
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
snapshot and its age grows. This does not extend how long a replica
serves without its store: the Key cache answers a lookup for at most
`LUX_KEY_CACHE` after its last store read and caches no failure
([[007-keys-and-limits]]), so a request whose Key entry lapsed is
refused with `store_unavailable` as today.

**Decision 2: readiness on an old snapshot.**

| | Shape | For | Against |
|---|---|---|---|
| (i) | Ready once loaded; the age is a metric and an alert, never a readiness failure | a store outage does not also take every replica out of the load balancer; the Key cache already bounds serving without the store | a replica cut off from the store while the others reach it serves a catalog older than its peers, for as long as its Keys stay cached |
| (ii) | Not ready once the snapshot is older than a bound | a partitioned replica leaves the load balancer | in a full store outage every replica fails readiness together, which turns a degraded gateway into none |

**Recommendation: (i).**

### Memory

The snapshot is every Model and Provider manifest as the store returns
it, and every sealed credential row. A Model is a few hundred bytes to
a few kilobytes of JSON; an installation with ten thousand discovered
Models holds tens of megabytes. There is no cap: a cap would make the
catalog incomplete, which is a correctness failure, where the size is
an operator's to see. `lux_catalog_objects` reports it.

### Configuration

One row in [[002-repository-scaffold]]'s table:

| Variable | Spec | Default | Meaning |
|---|---|---|---|
| `LUX_CATALOG_RELOAD` | 036 | `30s` | how often a replica re-reads the whole catalog, the backstop for writes the journal does not name (5s to 10m) |

The tail keeps the Key cache's one-second interval and no variable.

### Metrics

Three rows in [[019-observability]]'s table, owned by this spec:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `lux_catalog_age_seconds` | gauge | none | seconds since the snapshot last matched the store: the later of the last successful full reload and the last tail read that left nothing to reload |
| `lux_catalog_reloads_total` | counter | `trigger` (`start`, `tail`, `backstop`, `sighup`), `result` (`ok`, `error`) | snapshot reloads |
| `lux_catalog_objects` | gauge | `kind` (`Model`, `Provider`, `credential`) | objects the snapshot holds |

And one alert beside `LuxMeteringFlushLagging`: `LuxCatalogStale`,
`max(lux_catalog_age_seconds) > 3 * LUX_CATALOG_RELOAD` held for 5m.

## Not in this spec

- Keys and Budgets. They stay in the Key cache: their number grows with
  callers, not with the catalog, and the cache's bound is the right
  shape for them.
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
| 5 | Under decision 1 (i), a discovered Model whose price changed upstream is journaled `model.updated` with reason `discovery` and priced at the new price on every replica within the tail interval | `TestDiscoveryShapeUpdateIsJournaled`, `internal/serve` |
| 6 | `/readyz` answers not ready until the first full load succeeds, and a store that fails the first load keeps it not ready | `TestCatalogReadiness`, `cmd/luxd` |
| 7 | With the store failing after the load, requests whose Keys are cached are served from the snapshot, `lux_catalog_age_seconds` grows, and a request whose Key entry lapsed is refused `store_unavailable` | `TestCatalogSnapshotStoreDown`, `internal/serve` |
| 8 | A sealed credential that fails to open is re-read from the store once and opened; a second failure is the error the door answers today; no plaintext credential is held in the snapshot | `TestCatalogSnapshotReopensOnce`, and a reflection check that the snapshot's types carry no plaintext field |
| 9 | Lookups racing a stream of swaps each return objects of one whole snapshot, under the race detector | `TestCatalogSnapshotSwapIsAtomic`, `internal/serve` |
| 10 | The three metrics and the alert are in [[019-observability]]'s table and on `/metrics`, and `LUX_CATALOG_RELOAD` is in [[002-repository-scaffold]]'s table and `docs/configuration.md` | `TestMetricsTable`; `TestConfigurationReferenceIsCurrent` and `TestConfigurationReferenceMatchesCode`, `internal/config` |
| 11 | A benchmark of the catalog lookups a request makes, over the snapshot and over the Postgres store, is added beside those of [[023-performance-and-benchmarks]], and its numbers are written into `docs/performance.md` | `BenchmarkCatalogLookups`, `internal/serve`, the Postgres half in the Postgres tier |
