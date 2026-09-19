---
title: "Keys and limits: the value, verification, the cache, states, rate windows, spend windows, budgets"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/006-identity.md
  - specs/010-state.md
affects: [gateway/, metering/, internal/serve/, internal/config/, cmd/luxd/, docs/]
effort: medium
created: 2026-09-13
updated: 2026-09-16
author: changkun
---

# Keys and limits

## Overview

A Key is the credential a workload holds: an opaque value minted by the
gateway or supplied by the caller that creates it, shown once at most,
stored as a hash, and resolved to a set of Models it may name, rate
limits, a spend limit, and a Budget it draws from. This spec owns the
value and its verification, the per-replica cache that keeps the hot
path off the store, the Key's states and the `status` fields that
report them, how a selector is matched at request time, the arithmetic
of the rate and spend windows, how a Budget is drawn, and the refusals
that arithmetic produces. The manifest fields are
[[003-manifest-contract]]'s; where in the pipeline the checks run is
[[004-request-path]]'s; the record they settle into is
[[009-usage-and-metering]]'s.

The design choice that shapes everything here: a rate window is a
replica's, a spend window is the store's. Requests per minute is a
protection against a runaway loop and needs no coordination to be
useful; money is a promise and needs one source of truth. So rate
buckets live in memory and overshoot by at most the replica count,
while spend counters live in the store and every replica flushes its
deltas on a short interval, with the overshoot bounded and stated
([[009-usage-and-metering]]).

## Current state

Built, over the memory store and the file mode of [[010-state]] and
beside the door handler of [[004-request-path]]: `metering` holds the
window arithmetic, the counter key scheme, the reservation figures, the
overshoot bound, the marker claim, and a replica's deltas over the
store; `internal/serve` holds the Key value's mint, hash, and prefix,
the Key cache with its journal tail and its `Reset`, the Limiter over
`latere.ai/x/pkg/ratelimit` v0.66.0 and the store's counters, and the
read-time rendering of a Key's and a Budget's status; `internal/config`
the three rows; and `luxd serve` starts the cache's tail beside the two
jobs of [[005-providers]], empties the cache after a file-mode `SIGHUP`,
and resolves a file-mode Key under the two rate defaults. The door
handler is not mounted and the `/v1` routes that create, rotate, and
read a Key are [[011-api]]'s, so the Key cache and the Limiter are
constructed and proven here and wired to a door there. The readings
this pass fixed where the text was open are written into the Design
below, each beside the rule it settles.

## Design

### The value

`lux_` followed by 40 characters drawn from `[A-Za-z0-9_-]` by a
cryptographically secure source: 240 bits, so the value is the secret
and nothing about the Key is derivable from it. The first twelve
characters, `lux_` and eight more, are `status.prefix`, shown in every
list, record, and log line as the Key's handle for a person; the
remaining 32 are never shown again. The value is returned once, in the
`status.value` of the response to the `PUT` that created the Key, and
by `POST /v1/keys/{id}/rotate` ([[011-api]]); every other read returns
the object without it.

The store keeps `SHA-256(value)`, hex, in the hash index of
[[010-state]], never the value. A lookup is by hash; the gateway
computes the hash of the presented credential and asks the store,
comparing nothing itself. The door hashes the exact bytes of the
credential it extracted, trims nothing, parses nothing, and checks no
prefix: a value is whatever string a Key was created with, minted or
supplied, and the doors treat every one as opaque bytes
([[001-architecture]], [[004-request-path]]). The bytes are not
retained past the hash, so no later stage, the pipeline, the record,
the log line, the event, holds anything but the hash and the prefix,
and a value is never logged by construction. The redacting log handler
of [[019-observability]], which truncates a minted value's pattern to
its prefix, is a second line for a value logged by a caller's mistake,
not the first. `TestKeyValueNeverAppearsInLogs` runs a minted and a
supplied canary through every path.

Rotation mints a new value, replaces the hash, keeps the id, the name,
the spec, the owner, and the windows, and invalidates the old hash at
once; a request with the old value is `unauthenticated` within
`LUX_KEY_CACHE` on every replica. There is no grace period: a caller
that needs one creates a second Key and deletes the first.

The three functions of the value are `internal/serve`'s, which the
routes of [[011-api]] call: `MintKeyValue` draws forty bytes from
`crypto/rand` and maps each through its low six bits onto the 64-letter
alphabet, so no letter is favoured; `HashKeyValue` is the SHA-256 of
the exact bytes as 64 lower-case hex characters, the one function the
store indexes under and the door looks up with; `KeyPrefix` renders
`status.prefix` for a minted or a supplied value.

### A supplied value

A `PUT` that creates a Key may carry `spec.value`, a value the caller
chose instead of one the gateway mints. It exists for one composition:
a platform that gives a developer one credential registers that
credential here, so the same string the developer presents to every
control plane as a token opens the doors as a Key, under the models
and the budget the platform attached, and the hot path stays a hash
lookup ([[001-architecture]], invariant 3). The field is
[[003-manifest-contract]]'s: a string, write-once, decoded into a member
the JSON and YAML encoders skip exactly as
`Provider.spec.credential.value` is, so no object carrying it can be
serialized by accident.

The rules are the minted value's with these differences:

- Bounds. A supplied value is 32 to 4096 bytes of the exact string
  the body carried; shorter is `invalid_field` at `spec.value` because
  a shorter secret is guessable, longer is `invalid_field` because a
  header cannot carry it. Nothing is trimmed, decoded, or parsed: a
  value that happens to be a JWT is bytes to the gateway, which reads
  no header, claim, signature, or expiry from it and verifies nothing.
  The only expiry a Key has is its own `ttl` or `expiresAt`; a platform
  whose credential expires on its own schedule sets one or deletes the
  Key.
- Uniqueness. The hash index of [[010-state]] is unique. A supplied
  value whose hash is already registered, to any Key, live or disabled,
  is `invalid_field` at `spec.value` with the developer detail `a Key
  with this value already exists`, naming no Key: the caller holds the
  value already and learns only that it is taken. The store reports
  the collision as `ErrHashTaken` from `Keys.Put` inside the create's
  transaction, so two concurrent creates of one value produce one Key.
- Write-once. `spec.value` present on an update is `immutable_field` at
  `spec.value`, whether or not it equals the stored one, because the
  stored one is a hash and cannot be compared ([[003-manifest-contract]],
  stage 5). `spec.value` with `spec.valueFrom` is `exclusive_fields`;
  `spec.value` in file mode is `invalid_field`, because a directory a
  deployment copies is not a place for a credential ([[010-state]]).
- Never returned. The create response carries no `status.value`, and
  no read, list, event, record, or log line carries the value, so the
  resolved manifest a caller reads back has the field absent and
  re-applies unchanged.
- The prefix. `status.prefix` is `sup_` and the first eight lower-case
  hex characters of the hash, twelve characters like a minted prefix,
  because a supplied value has no prefix of its own to show and its
  first twelve bytes may be the same for every value of its kind. The
  prefix is derived from the hash, so it names one Key and discloses
  nothing of the value ([[001-architecture]] states the rule for a
  minted value and defers to this one for a supplied one).
- Rotation. `POST /v1/keys/{id}/rotate` mints a `lux_` value and
  replaces the hash, so a supplied value stops working at rotate; a
  platform that wants the same string again recreates the Key.
- Revocation. A platform revokes the credential by deleting or
  disabling the Key, and the doors refuse the value within
  `LUX_KEY_CACHE` on every replica, exactly as for a minted value;
  whatever the platform's issuer does with the same string is its own.

The plane boundary holds by verification, not by shape. On a door,
every credential is a hash lookup and a value no Key was created with
is `unauthenticated`, whatever it looks like; a token is a Key only
where an operator registered that exact string as one, which is a
control plane act ([[004-request-path]], [[006-identity]]). On `/v1`,
the same string is a bearer like any other and is accepted only when it
independently verifies under [[006-identity]], a listed issuer and the
gateway's own audience; a platform's developer credential carries the
platform's audience and so never reaches desired state, and a minted
value is not a JWS and never does. Neither plane consults the other's
table.

### A value supplied by its hash

A `PUT` that creates a Key may carry `spec.valueSHA256` instead of
`spec.value`: the SHA-256 of a value the caller holds only as a hash.
It exists for one composition: an importer moving credentials out of a
store that kept hashes alone, so every credential its holders already
present keeps opening the doors after the move, without a re-issue.
The field is [[003-manifest-contract]]'s: 64 lower-case hex characters,
write-once, encoder-skipped as `spec.value` is.

The rules are the supplied value's, with the hash standing in for the
value at every step. The store keeps it as the hash a supplied value
would have produced, so the hash index, its uniqueness (`ErrHashTaken`,
`invalid_field` at `spec.valueSHA256` naming no Key), the door's lookup,
the cache, revocation, and `LUX_KEY_CACHE` are one path for the three
forms of a value. `status.prefix` is `sup_` and the hash's first eight
characters. The create response carries no `status.value`, and the hash
is returned by no read, list, event, record, or log line, because a
hash of a low-entropy value is a guessing target. A rotate mints a
`lux_` value and replaces the hash. The gateway cannot check the
hash's bounds on the value it stands for, so the importer answers for
them: a value under 32 bytes registered this way is the importer's
choice, and the version promise of [release and installation](.archive/017-release-and-installation.md)
does not extend to it.

### Verification and the cache

```mermaid
flowchart LR
  R[request] --> H[hash the credential]
  H --> C{cache}
  C -- hit, fresh --> K[resolved Key]
  C -- miss --> S[(store: key by hash)]
  S -- found --> K
  S -- not found --> N[negative entry]
  N --> U[unauthenticated]
  K --> P[the pipeline of 004]
```

Each replica caches the store's answer per hash for `LUX_KEY_CACHE`
(default `10s`): the resolved Key with its owner, selectors, limits,
Budget reference, and expiry, or the absence of one. A positive entry
is also dropped when the journal reports a change to that Key: every
replica tails `Journal.Since` ([[010-state]]) and evicts the entry of
any Key a `key.updated`, `key.rotated`, or `key.deleted` row names
([[012-request-log-and-events]]), so a delete, a disable, or a rotation
is seen at the next request on a replica that has consumed the row and
within the window on one that has not. The journal row is written for
every mutation whether or not a sink is configured, so the tail works
in every installation with a store; in file mode the `SIGHUP` snapshot
swap empties the cache ([[010-state]]). A negative entry protects the
store from a flood of unknown values; it is bounded to `LUX_KEY_CACHE`
and the cache holds at most 100 000 entries, evicting the least recent.
Unknown values are also subject to the door's unauthenticated rate
`LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE` per client address
([[011-api]]), so guessing costs the guesser first. Every lookup counts
once in `lux_key_cache_hits_total` with `result` `hit`, `miss`, or
`negative` ([[019-observability]]).

The cache is what makes invariant 3 of [[001-architecture]] cheap: a
hot path that touches the store once per Key per ten seconds, and a
counter per request, dials nothing else.

The cache is `serve.KeyCache`, [[004-request-path]]'s `KeyLookup`. It
also holds the Budget a Key draws from, by id, under the same window
and the same bound, dropped by a `budget.updated` or `budget.deleted`
row, so the Limiter's read of a hard Budget's amount is a cache read
and never a store round trip per request. A hash whose row is gone,
which a delete in flight leaves for a moment, is a negative entry like
an unknown value. The tail is `Run`, started by `luxd serve` beside the
jobs of [[005-providers]]; its first read walks the journal from the
start, which evicts nothing from a cache that is empty at start and
places the tail at the end. A store failure on a lookup is returned as
the store's error, which the door answers `store_unavailable`, and is
cached nowhere. The file-mode re-read writes no journal row, so `luxd`
calls `Reset` after every successful `SIGHUP` reload instead.

### States

```mermaid
stateDiagram-v2
  [*] --> Active: created
  Active --> Disabled: spec.disabled true
  Disabled --> Active: spec.disabled false
  Active --> Expired: expiresAt passes
  Active --> Exhausted: a spend or budget window fills
  Exhausted --> Active: the window resets, or the limit is raised
  Disabled --> Expired: expiresAt passes
  Expired --> [*]: deleted
  Active --> [*]: deleted
```

`status.state` is what a reader sees; the gateway decides from the
underlying facts, not from the cached word. `Disabled` is
`spec.disabled`, refused `key_disabled`. `Expired` is `Now` past
`status.expiresAt`, refused `key_expired`, decided at the request from
the clock, so a Key expires on every replica at the same instant
without a write. `Exhausted` is the Key's spend window at or over its
amount, refused `spend_exceeded`, or the hard Budget it draws from at
or over its amount, refused `budget_exhausted`; it is decided at stage
7 of the pipeline from the current counters, so a window that has reset
serves at once. A refusal can precede the state: a window is refused
when the request's estimate would carry it over the amount, so a Key
whose window holds a little less than its amount is refused
`spend_exceeded` while it renders `Active` with the remainder in
`status.usage.window`, and the refusal's `Retry-After` names the reset
either way. The codes and their statuses are [[004-request-path]]'s
table. An `Expired` Key is kept until deleted, so its usage stays
readable; an operator's sweep of expired Keys is a platform's job or a
`lux keys prune` later.

The `status` fields that report the facts are rendered at read time,
not stored, so no flush writes a row per Key per interval and every
replica renders the same answer from the store:

| Field | Rendered from |
|---|---|
| `status.state` | the rules above, from `spec.disabled`, `status.expiresAt`, and the counters of the current window |
| `status.usage.window` | `{requests, tokens, spend, resetsAt}` of the current spend window, from the three counters [[009-usage-and-metering]] keeps under `limits.spend.window`; absent when the Key has no spend limit |
| `status.usage.total` | `{requests, tokens, spend}` over the Key's lifetime, from the same three counters under window `none` |
| `status.lastUsedAt` | the one field written: by the Limiter's flush, through `PutStatus` ([[010-state]]), for every Key whose request was admitted at stage 7 in a minute not yet written, rounded down to the minute, so a busy Key costs one status write a minute and not one a request; a Key deleted between its use and the flush is no error |

`spend` in `status.usage` renders the counter's micro-units as a money
string of [[003-manifest-contract]], the form a person reads; the
record and the usage API carry the same number as an integer
([[009-usage-and-metering]]). The rendering is `serve.RenderKey` and
`serve.RenderBudget`, which fill the read-time members of one object
from one `Counters.Read` and, for a Key that draws from a Budget, one
read of the Budget; the routes of [[011-api]] call them on every read
and list. A Key's `usage.window` is absent without a spend limit, and
under a `none` spend window it is the totals themselves, because the
`none` window is the lifetime counter.

### Selectors

`spec.models` is a list of selectors under the glob rule of
[[003-manifest-contract]]: `*` matches any run of characters including
`/`, every other character matches itself, and a selector without `*`
is an exact name. What a selector matches is a Model's `metadata.name`,
declared or discovered alike, so `anthropic/*` matches every Model
discovered from the Provider named `anthropic` and `gpt-5` matches the
one Model of that name. At resolve, each selector is one `model.use`
decision through `Lookup.Models` ([[006-identity]]), which binds the
selector and records what it matched then in `status.selectors[]`; a
selector that matched nothing is a warning, not a refusal.

At request time the gateway matches again, against the catalog as it
is now, with `manifest.Match(selector, name)`, the same function that
validated the selector: the resolved Model's name is held to each
selector in order and the first match admits it. A Model that appears
after the Key was resolved, a discovered one or a newly declared one,
is admitted by a selector that matches it without a re-resolve; a
Model that was deleted stops matching because its name is no longer in
the catalog. The refusal is `model_not_allowed`, 403, at stage 5 of
[[004-request-path]], after `model_not_found`, so a caller learns
whether the name exists before whether this Key may use it. The same
match is what `GET /v1/models` on a door lists ([[004-request-path]])
and what admits an opaque route's Provider: one whose Models some
selector reaches ([[004-request-path]]). The selectors are the whole
of what a Key may name; there is no per-Key scope beyond them, and the
control plane is closed to a Key whatever its selectors say
([[006-identity]]).

### Rate windows

`requestsPerMinute` and `tokensPerMinute` are token buckets, one pair
per Key per replica, evicted when idle for ten minutes. A bucket's
capacity is the per-minute value and its refill is that value per
sixty seconds, so a Key may burst its full minute at once and then
proceeds at the rate; that is the shape SDK retry loops expect. Zero
is no bucket. The buckets are `latere.ai/x/pkg/ratelimit.Buckets`,
keyed by Key id, with a per-Key rate set through `SetRate`; that
package today admits one token per `Allow` and has no way to charge
several or to give any back, so this spec needs three additions to it,
named here as the pkg change it depends on:

```go
// AllowN consumes n tokens for key at once, or consumes nothing and
// reports the wait until n are available. n of 1 is Allow.
func (b *Buckets) AllowN(key string, n int) Allowance
// Adjust adds delta tokens to key's bucket: a negative delta debits
// past zero, so the next Allow waits; a positive one refunds up to the
// burst. It is how a reservation is settled.
func (b *Buckets) Adjust(key string, delta int)
// Config.PerMinute of zero with SetRate per key means no default
// bucket and per-key rates only, rather than a disabled limiter.
```

The three landed in `latere.ai/x/pkg` v0.66.0, which the module pins.
The Limiter runs one `Buckets` in the per-key mode with two keys per
Key id, `requests:<id>` and `tokens:<id>`, and sets each rate before
each admission, because an evicted key's rate goes with it and the rate
is the Key's spec, or `LUX_DEFAULT_REQUESTS_PER_MINUTE` and
`LUX_DEFAULT_TOKENS_PER_MINUTE` where the spec names none.

A request costs one from the request bucket at stage 7. The token
bucket is charged a reservation before the request and settled once
the measured tokens are known, which for a whole answer is before its
body is written to the caller, so the next request a caller sends the
moment it has the answer meets the settled ledger, and a Budget that
answer exhausted refuses it (`TestWholeResponseSettlesBeforeItsBody`);
a stream settles when it ends:

```
reserve  = estimate(input tokens) + requested max output tokens, or 1024 when unrequested
settle   = measured input + measured output tokens
adjust   = reserve - settle        (a refund when positive, a further debit when negative)
```

`estimate` is `latere.ai/x/pkg/llmdialect/tokencount.Estimate` over
the decoded request on translated routes, and, on a passthrough model
route, the body's length in bytes divided by four, because no upstream
reports a count before it answers; an opaque route reserves one request
and no tokens. The settle uses the measured count either way. A
reservation is bounded by the rate, which is the bucket's burst: a
request whose estimate is larger than a whole minute's tokens is
charged the minute, admitted when the bucket is full, and settled to
its measured count, which the refill covers as a deficit before the
next token, rather than refused on every call with a wait that never
ends. A bucket that cannot cover the reservation refuses with
`rate_limited` and `Retry-After` of the seconds until it can,
`Allowance.Retry` rounded up and at least `1`; nothing is debited on a
refusal, the request token included when the token bucket refuses
after the request bucket admitted, and a request refused at a later
stage or failed before it was sent is refunded whole, which is a settle
with zero tokens. Because the buckets are per replica, an installation
with `n` replicas admits at most `n` times the configured rate, which
the documentation says in those words. The bucket and the symmetric
settle are deliberate, because a fixed window admits two minutes' worth
at a boundary and an unsettled under-estimate lets a Key exceed its
tokens per minute by the estimate's error every minute.

### Spend windows

A Key that names no spend limit and draws from no Budget spends without
bound: the core has no default cap, because a default that is money
belongs to the operator's manifest or to the authorizer's `limits`.

A Key's `limits.spend` and a Budget are both a spend window: an
amount, a currency, and a window under the window rule of
[[003-manifest-contract]]. Their counters are the store's, in
micro-units of the currency, keyed by `(object id, window number)`
([[010-state]]); each replica keeps a delta per key per window and
flushes every `LUX_METERING_FLUSH` (default `1s`).

The arithmetic is `metering`'s, as [[001-architecture]] places it:
`Window` is the bounds of a window at an instant; `CounterKey` renders
`<kind>:<id>:<counter>:<start>` with the start as Unix seconds, and the
word `none` in place of a start for the `none` window, because the
`none` window is the lifetime counter and a duration window that begins
at the object's creation instant must not share its key, which
[[009-usage-and-metering]]'s table, keying the totals by `createdAt`,
would have let it; `TotalKey` is that key; `Reserved`, `Settled`,
`Adjustment`, `Projected`, and `Exceeds` are the figures above and
below; `Overshoot` is the bound; `Claim` is the marker; and `Counters`
is one replica's deltas over the store's `Counters()`, with `Add`
carrying the window's expiry the store writes when it first sees the
key, `Known` and `Pending` the two terms of the check, `Flush` one add
per dirty key whose returned total replaces the known value and whose
failure keeps the delta, and `Run` the flush on the interval and once
more at stop. The Limiter of `internal/serve` composes the buckets
with `metering.Counters` over the store, and it feeds the three
counters of a Key: one request at admission, the estimate at admission
and the measured cost less the estimate at the settle, and the measured
tokens at the settle, each under the lifetime totals and, for a Key
with a spend limit, under the current window, once when the two are one
key. A Budget's spend counter takes the same two adds. The Recorder of
[[009-usage-and-metering]] folds the aggregates from the records and
adds to none of these.

At stage 7, for each of the Key's spend window and the Budget's:

```
known     = the store's counter as of the last flush
pending   = this replica's unflushed delta
projected = known + pending + estimated cost of this request
refuse when projected > amount
```

The estimated cost is the reservation's tokens priced by the Model's
`pricing` through `metering.Cost` ([[009-usage-and-metering]]), whose
arithmetic the Limiter holds as `costOf` until that package lands and
replaces it; after the response the measured cost replaces it in the
delta. The Budget a Key draws from is read through the Key cache, and
a Key whose Budget is gone, which `budget_in_use` forbids and a delete
in flight can leave for a moment, draws from none. The bound this
gives, stated once here and once in the metering spec in the same
symbols: with `R` replicas, a flush interval `F` in seconds, `T`
requests per second per replica against the counter, and `C` the
greatest cost of one request under the Key's Models,

```
overshoot <= (R - 1) × F × T × C + C
```

because the first term is what the other replicas spent inside one
flush interval that this one has not yet seen, and the last is this
replica's own request that crossed. With one replica the bound is one
request. An installation that needs a tighter bound lowers the flush
interval.

The order of refusals is the pipeline's: `rate_limited` before any
money question, then `model_unpriced`, `currency_mismatch`,
`spend_exceeded`, `budget_exhausted`. `Retry-After` on the last two is
the seconds until the window resets, or absent for a `none` window,
which never does.

Exhaustion is announced once per window. The first replica to observe
a window at or over its amount, at the gate for a hard limit and at a
flush for a soft Budget, adds `1` to the marker counter
`key:<id>:exhausted:<start>` or `budget:<id>:exhausted:<start>`
([[009-usage-and-metering]]); the one replica whose add returns `1`
emits `key.exhausted` or `budget.exhausted`
([[012-request-log-and-events]]), and every other replica's returns
more and emits nothing. No lease is needed, because the store's add is
atomic, and the marker resets with the window because it is keyed by
the window's start. For a hard window the observation is the first
`spend_exceeded` or `budget_exhausted` refusal, as
[[012-request-log-and-events]]'s table says, so the event's `spent` is
the window's total as the refusing replica saw it, which the estimate
of the refused request would have carried over the amount. For a soft
Budget it is the flush whose returned total is at or over the amount,
so the announcing replica is one whose own delta crossed, and its
`spent` is the total that flush returned; a replica whose last flush
left the window under and that has nothing more to flush for it does
not look again, because another replica's crossing is that replica's
to announce. The event is `reason: limit` with the object block of the
Key or the Budget and `data` of `{window, amount, spent, currency,
resetsAt}`, `resetsAt` absent for a `none` window, and `hard` added on
a Budget's; `amount` and `spent` are money strings. A marker or a
journal the store cannot answer is logged and the refusal stands, and
that window's event is the one at-least-once does not cover
([[012-request-log-and-events]]).

Pricing rules at this stage:

- A Model with no `pricing`, and every opaque route, is unpriced. A Key
  with a spend limit or a Budget, hard or soft, refuses an unpriced
  request with `model_unpriced` unless `allowUnpriced` is true; a Key
  with neither serves it and the record says `priced: false`. This is
  the rule that keeps a budget meaning what it says: money that cannot
  be counted is not spent under one by default.
- The currency of the Key's spend limit, the Budget, and the Model's
  pricing must agree; the first disagreement is `currency_mismatch`.
  Nothing converts currencies.
- A soft Budget (`hard: false`) never refuses for its amount. When its
  window first reaches `amount`, the flush that observes it emits one
  `budget.exhausted` event as above, `status.state` renders `Exhausted`
  until the reset, and requests continue. A soft Budget's Keys are
  never `Exhausted` by it; `model_unpriced` and `currency_mismatch` are
  the Key's refusals and hold under a soft Budget as under a hard one.

### Budgets

A Budget is drawn by every Key that names it. `budget.draw` is decided
once, at the Key's resolve ([[006-identity]]); after that the draw is
arithmetic. Its `status` is rendered at read time like a Key's, by
`serve.RenderBudget`: `status.keys` counts the live Keys whose
`status.budget` names it now; `status.spent` is the
current window's counter as a money string, `status.remaining` is
`amount` less that, floored at zero, and `status.resetsAt` is the
window's reset, or absent for `none`; `status.state` is `Exhausted`
while `spent` is at or over `amount` and `Open` otherwise. Deleting a
Budget that Keys still name is refused with `budget_in_use`, 409
([[011-api]]), so a Key never points at nothing; an operator disables
the Keys or moves them first. Raising `amount` on an `Exhausted` Budget
returns it to `Open` at the next request. Changing `window` or
`currency` is immutable by [[003-manifest-contract]], because a counter
keyed by window number under one rule cannot be reinterpreted under
another.

### Attribution

Every record carries the Key's id and prefix and the Key's `owner`,
the subject that applied it, and the Key's labels; a platform that
wants to charge a team puts the team in a label or gives the team a
Budget, and reads `GET /v1/usage` by either
([[009-usage-and-metering]]). A caller may add request labels through
the `Lux-Labels` header: comma separated `k=v` pairs, at most eight,
each key 1 to 64 characters of `[A-Za-z0-9._-]` and each value 1 to
128 characters of `[A-Za-z0-9._:/-]`, no duplicate keys. They are
recorded apart from the Key's labels as the record's `requestLabels`,
the request's own, readable through `GET /v1/requests` and the
archive, never an aggregate dimension, and never trusted for anything
but reporting. A pair that fails the rule, a duplicate, and every pair
past the eighth are dropped and the rest kept; the header is never
forwarded to a provider ([[004-request-path]] strips every `Lux-*`
request header). Dropping a bad pair rather than refusing the request is
deliberate: a data plane refusal is a code in the error table, and
attribution is not worth one.

### Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `LUX_KEY_CACHE` | no | `10s` | how long a Key lookup, positive or negative, is cached per replica; at least `1s`, at most `10m` |
| `LUX_DEFAULT_REQUESTS_PER_MINUTE`, `LUX_DEFAULT_TOKENS_PER_MINUTE` | no | `0`, `0` | the `Defaults` a Key without `limits` gets; `0` is none |

[[002-repository-scaffold]] lists both in the one table every `LUX_*`
variable is in, with this spec as the owner.

## Not in this spec

The record, cost arithmetic, the counters, and the flush
([[009-usage-and-metering]]); the counter and hash storage
([[010-state]]); the `spec.value` field's row in the schema and the
glob rule ([[003-manifest-contract]]); the routes that create, rotate,
disable, and delete a Key and their status codes ([[011-api]]); where
the checks sit in the pipeline and the codes' HTTP statuses
([[004-request-path]]); who may create a Key and how a selector's
`model.use` is decided ([[006-identity]]); the events' delivery
([[012-request-log-and-events]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A minted value matches `^lux_[A-Za-z0-9_-]{40}$`, ten thousand mints are distinct, and `status.prefix` is its first twelve characters | `TestKeyValueShape`, `TestKeyValuesAreDistinct` | passing, `internal/serve` |
| A minted value and a supplied value each appear in the create response (the minted one only) and the rotate response and in no other read, list, event, record, or log line, with a canary of each run through every path | `TestKeyValueShownOnce`, `TestKeyValueNeverAppearsInLogs` | not built; the responses are [[011-api]]'s and the run through every path is [[015-test-stubs-and-tiers]]'s; the cache and the Limiter are held to logging no value in `TestKeyCacheRunTailsAndSurvivesFailures` and `TestLimiterRunAndFailures` |
| A rotated Key keeps its id, name, spec, owner, and windows; the old value is `unauthenticated` on a second replica within `LUX_KEY_CACHE` | `TestRevocationPropagates` with [[011-api]]'s `TestRotate` | passing: the cache half in `TestRevocationPropagates`, the old hash refused on the replica that consumed the row at once and on the other within the window; the route half in [[011-api]]'s `TestRotate`, which keeps the id, name, spec, owner, and windows and drops the old hash |
| A Key created with a 32-byte `spec.value` authenticates by that exact value on every door and credential form, the create response carries no `status.value`, `status.prefix` is `sup_` and the first eight hex characters of the value's SHA-256, and the supplied value is refused at rotate while the minted one is accepted | `TestSuppliedKeyValue` | passing at the store and the cache, `internal/serve`; the door half is [[004-request-path]]'s `TestSuppliedValueOpensTheDoor`; the response half passes in [[011-api]]'s `TestSuppliedValueIsNotEchoed`, which reads the `sup_` prefix and neither value |
| A supplied value of 31 bytes and one of 4097 bytes are each `invalid_field` at `spec.value`; one with leading whitespace authenticates only with that whitespace; a value that is a well-formed JWT with a past `exp` and a bad signature authenticates, because the gateway parses nothing | [[003-manifest-contract]]'s `TestSuppliedValueSchema`, `TestSuppliedValueIsOpaqueBytes` | passing: the bounds as [[003-manifest-contract]]'s `TestSuppliedValueSchema`, the bytes in `internal/serve` |
| A second create with a value already registered, to a live or a disabled Key, is `invalid_field` at `spec.value` whose detail names no Key; two concurrent creates of one value yield one 201 and one `invalid_field` | `TestSuppliedValueMustBeUnique` | passing at the store, `internal/serve`: `ErrHashTaken` inside the transaction, the refused Key stored nowhere, one Key from a concurrent pair; the `invalid_field` mapping is [[011-api]]'s |
| A Key created with `spec.valueSHA256` equal to the SHA-256 of a string authenticates by that string on every door, `status.prefix` is `sup_` and the hash's first eight characters, the create response carries neither the hash nor a value, a second create with the same hash is `invalid_field` at `spec.valueSHA256` and one with a `spec.value` of that string is `invalid_field` at `spec.value`, each naming no Key, and a rotate mints a `lux_` value after which the string is `unauthenticated` | `TestHashSuppliedKeyOpensTheDoor` | passing at the store and the cache, `internal/serve`, where the row the hash wrote and the row the value would have written are one row and one handle; the response half passes in [[011-api]]'s `TestHashSuppliedValueIsNotEchoed`, which reads the `sup_` prefix, neither the hash nor a value, both refusals, and the rotate; the door half is [[018-conformance-suite]]'s `case007HashSuppliedValue`, which opens a door with the string over HTTP and reads `unauthenticated` after the rotate |
| An update carrying `spec.value`, equal to the stored one or not, is `immutable_field` at `spec.value`; `spec.value` with `spec.valueFrom` is `exclusive_fields`; `spec.value` in file mode is `invalid_field` | [[003-manifest-contract]]'s `TestSuppliedValueSchema` and `TestImmutableFields` with [[010-state]]'s `TestFileModeRefusesServerOnlyFields` | passing as [[003-manifest-contract]]'s `TestSuppliedValueSchema` and `TestImmutableFields`, and [[010-state]]'s `TestFileModeRefusesServerOnlyFields` for the file mode |
| A supplied value that is a token for a listed issuer with another audience is a Key on a door and `unauthenticated` on `/v1`; a minted value is `unauthenticated` on `/v1`; an issuer token no Key was created with is `unauthenticated` on a door | `TestPlaneBoundaryHoldsByVerification` with [[006-identity]]'s `TestPlanesRefuseEachOthersCredential` and [[004-request-path]]'s `TestDoorsTakeKeysOnly` | not built; the door half passes as [[004-request-path]]'s `TestDoorsTakeKeysOnly`, the `/v1` half waits for [[011-api]] |
| A Key looked up once is served from the cache for the window with one store call; a negative entry holds an unknown value to one store call per window; the cache evicts at 100 000 entries; every lookup increments `lux_key_cache_hits_total` with the matching `result` | `TestKeyCache`, `TestNegativeCache`, `TestCacheBound` | passing, `internal/serve` |
| A deleted, disabled, or rotated Key is refused at the next request on a replica that has consumed the journal row and within the window on one that has not; a `SIGHUP` in file mode empties the cache | `TestRevocationPropagates`, `TestFileModeReloadEmptiesTheCache` | passing, `internal/serve`; `luxd serve` calls `Reset` after the reload in `cmd/luxd`'s `TestFileModeServesAndReloadsOnSIGHUP` |
| Each state is decided from the facts: `Disabled` from the spec, `Expired` from the clock on every replica at once, `Exhausted` from the current window and cleared by its reset without a write; each refuses with its code | `TestKeyStates`, table-driven with a fake clock | passing, `internal/serve`, over `RenderKey` and the Limiter; `key_disabled` and `key_expired` at the door are [[004-request-path]]'s `TestKeyState` |
| `status.usage.window` and `.total` render the counters' requests, tokens, and spend with `spend` as a money string, `window` is absent for a Key without a spend limit, and `status.lastUsedAt` is written at most once per minute per used Key | `TestKeyStatusRendersFromCounters`, `TestLastUsedIsCoalesced` | passing, `internal/serve` |
| A request naming a Model whose name matches no selector is `model_not_allowed` after `model_not_found`; a Model declared or discovered after the Key was resolved is admitted when a selector matches its name; a glob matches across `/`; the match is `manifest.Match` | `TestSelectorsMatchAtRequestTime`, table-driven | passing, `gateway` |
| `ratelimit.Buckets` offers `AllowN`, `Adjust`, and per-key rates without a default bucket, and the gateway's buckets use them | `TestRateLimitPackageShape`, compile-time against `latere.ai/x/pkg/ratelimit` | passing, `internal/serve`; the pkg change is v0.66.0 |
| A Key with `requestsPerMinute` 60 admits 60 at once, refuses the 61st with `Retry-After` 1, and admits one more after a second; zero is no limit | `TestRequestBucket` | passing, `internal/serve` |
| The token bucket is charged the reservation before the request and settled to the measured count after, refunding an over-estimate and debiting an under-estimate; a refusal at this stage debits nothing, and a refusal at a later stage refunds the whole reservation | `TestTokenReservationSettles`, `TestRefusalDebitsNothing` | passing, `internal/serve` |
| Two replicas each admit the configured rate, so the installation admits twice it and no more | `TestRateIsPerReplica` | passing, `internal/serve` |
| A spend window refuses when `known + pending + estimate` exceeds the amount, serves after the window number changes, and `Retry-After` names the reset; a `none` window carries no `Retry-After` | `TestSpendWindow`, `TestWindowReset` | passing, `internal/serve` |
| With three replicas, a flush interval of one second, ten requests a second per replica, and a one-cent request, the overshoot of a hard limit never exceeds `(R − 1) × F × T × C + C`, twenty-one cents, over a hundred runs, and one replica never overshoots by more than one request | `TestOvershootBound` | passing, `internal/serve`: three Limiters over one store, the flush driven by the simulated clock, a hundred seeded runs; the worst run reaches twenty cents |
| Six replicas driving one Key's spend window and one Budget's window past their amounts emit exactly one `key.exhausted` and one `budget.exhausted` per window, through the marker counter, for a hard limit at the first refusal and for a soft Budget at the first flush that observes it | `TestExhaustionIsAnnouncedOnce`, with [[012-request-log-and-events]]'s `TestStateChangeEventIsRaisedOnce` | passing here, `internal/serve`, six Limiters over one store; the delivery half is [[012-request-log-and-events]]'s |
| An unpriced Model and an opaque route are `model_unpriced` under a spend limit or a Budget, hard or soft, without `allowUnpriced`, served with it, and served with `priced: false` under neither | `TestUnpricedRule`, [[001-architecture]]'s `TestUnpricedModelRefusedUnderABudget` | passing here, `internal/serve`; the e2e row is [[009-usage-and-metering]]'s |
| A Budget in another currency than the Model's pricing is `currency_mismatch`; the refusal order across all five money and rate codes is the pipeline's | `TestCurrencyMismatch`, `TestRefusalOrderAmongLimits` | passing, `internal/serve` |
| A soft Budget never refuses for its amount, emits `budget.exhausted` exactly once per window when reached, and renders `Exhausted` until the reset | `TestSoftBudget` | passing, `internal/serve` |
| Deleting a Budget a Key names is `budget_in_use`; raising an exhausted Budget's amount serves at the next request; `status.keys`, `spent`, `remaining`, `resetsAt`, and `state` render from the live Keys and the current window's counter | `TestBudgetLifecycle` | passing: the raise and the rendering in `internal/serve`, and `budget_in_use` with the 409 and the delete after the Key in [[011-api]]'s `TestBudgetInUse` |
| Every record carries the Key's id, prefix, owner, and labels, and the valid `Lux-Labels` pairs as `requestLabels`; a ninth pair, a duplicate key, and a pair outside the syntax are dropped while the rest are kept; the header reaches no provider | [[004-request-path]]'s `TestRequestLabels`, `TestRecordFields`, and `TestCallerCredentialsNeverForwarded` | the syntax passing as [[004-request-path]]'s `TestRequestLabels`, which drops the ninth pair, a duplicate, and a pair outside the alphabet and keeps the rest; the record's fields are [[004-request-path]]'s `TestRecordFields` and the header's absence upstream its `TestCallerCredentialsNeverForwarded`; the e2e run is [[009-usage-and-metering]]'s |

## Outcome

Built on 2026-09-14 on `main`, in the commits from `specs: 007 in
progress` to `specs: 007 complete`, over the memory store and the file
mode of [[010-state]] and beside the door handler of
[[004-request-path]], and proven by the whole gate, fifteen gates, and
per-package coverage of 97.8% for `metering`, 97.0% for
`internal/serve`, 99.4% for `internal/config`, 96.6% for `gateway`, and
94.6% for `cmd/luxd`. What diverged from the text as dispatched, each
fixed in the Design above beside the rule it settles:

- The counter key of the `none` window renders the word `none` in place
  of a start, not the object's `createdAt` as
  [[009-usage-and-metering]]'s table has it: a duration window that
  begins at the creation instant, which the first window of every Key
  created on an aligned boundary does, rendered the same key as the
  lifetime totals and counted every request twice. That spec's table
  is its edit to make.
- `metering.Counters`, the replica's deltas over the store, is built
  here to [[009-usage-and-metering]]'s stated shape because the Limiter
  cannot run without it, with three changes that spec should take:
  `Add` carries the window's expiry, which the store writes when it
  first sees the key; `CounterStore` uses the store's own method names,
  `Add` and `Read`, so `Store.Counters()` satisfies it without an
  adapter; `Known` and `Pending` expose the two terms of the check, and
  `Claim` is the marker. `Cost` stays that spec's, and the Limiter
  holds its arithmetic as `costOf` until it lands.
- The Limiter feeds a Key's three counters, one request at admission,
  the measured tokens at the settle, and the estimate then the
  measured cost less it, under the totals and the current window once
  when the two are one key; [[009-usage-and-metering]]'s Recorder folds
  the aggregates and adds to none of them, or every admitted request
  would count twice.
- A token reservation is bounded by the Key's rate, the bucket's burst,
  so a request larger than a whole minute's tokens is admitted when the
  bucket is full and settles into a deficit, rather than refused on
  every call with a `Retry-After` that never comes true.
- The request token is given back when the token bucket refuses after
  the request bucket admitted, so a refusal at this stage debits
  nothing in either bucket.
- `status.lastUsedAt` is written by the Limiter's flush for every Key
  whose request was admitted at stage 7, not by
  [[009-usage-and-metering]]'s, which does not exist yet and would
  otherwise have to learn which Keys were used.
- The Key cache also holds the Budget a Key draws from, dropped by
  `budget.updated` and `budget.deleted`, because a hard Budget's amount
  is read on every request and a store round trip there is what the
  cache exists to remove; a hash whose row is gone is a negative entry;
  `Reset` is what a file-mode `SIGHUP` calls, since the swap writes no
  journal row.
- `Exhausted` renders from the window's spend at or over its amount,
  and a refusal can precede it when the request's estimate would carry
  the window over; the marker counter is not read at render time,
  because a raised amount has to open the object at the next read.
- The soft Budget's announcement is made by a replica whose flush
  returned a total at or over the amount, and a replica whose last
  flush left the window under does not look again, because the replica
  whose delta crossed always flushes it.
- The exhaustion events carry `amount` and `spent` as money strings,
  `resetsAt` only where the window resets, and `hard` on a Budget's;
  their shape is written in `internal/serve/events.go` until
  [[012-request-log-and-events]]'s package lands.
- The `Lux-Labels` syntax row is proven by [[004-request-path]]'s
  `TestRequestLabels`, which the gateway built with the parser; the row
  names it rather than a second test of the same function.
- The value's mint, hash, and prefix are `internal/serve`'s
  `MintKeyValue`, `HashKeyValue`, and `KeyPrefix`; the cache is
  `serve.KeyCache`, the Limiter `serve.Limiter`, the renderers
  `serve.RenderKey` and `serve.RenderBudget`, each for [[011-api]] to
  call; `luxd serve` starts the cache's tail beside the jobs, empties
  the cache after a reload, and resolves a file-mode Key under the two
  rate defaults, and mounts no door.

Left `not built`, owned elsewhere: `TestKeyValueShownOnce` and
`TestKeyValueNeverAppearsInLogs`, whose responses are [[011-api]]'s and
whose run through every path is [[015-test-stubs-and-tiers]]'s; the
route halves of `TestRevocationPropagates` and `TestSuppliedKeyValue`,
the `invalid_field` mapping of `TestSuppliedValueMustBeUnique`, the
`/v1` half of `TestPlaneBoundaryHoldsByVerification`, and
`budget_in_use` in `TestBudgetLifecycle` ([[011-api]]); the delivery
half of `TestExhaustionIsAnnouncedOnce`
([[012-request-log-and-events]]); `TestUnpricedModelRefusedUnderABudget`
and `TestAttribution` ([[009-usage-and-metering]]); and the exposition
of `lux_key_cache_hits_total` on `/metrics`, whose registry
[[019-observability]] constructs.
