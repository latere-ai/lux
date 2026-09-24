---
title: "Several Budgets per Key: a list of Budgets a Key draws on together, anchored windows, and a restart"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/006-identity.md
  - specs/007-keys-and-limits.md
  - specs/009-usage-and-metering.md
  - specs/010-state.md
  - specs/011-api.md
  - specs/018-conformance-suite.md
affects: [manifest/, metering/, authorizer/, internal/serve/, internal/api/, internal/auth/, internal/store/, internal/bootstrap/, internal/luxcli/, test/conformance/, examples/plane/, docs/, specs/003-manifest-contract.md, specs/007-keys-and-limits.md, specs/009-usage-and-metering.md, specs/010-state.md]
effort: large
created: 2026-09-24
updated: 2026-09-24
author: changkun
---

# Several Budgets per Key

## Overview

A platform that sells model access in layers needs one call to answer
to several limits at once: an organization's funded balance, the limit
an administrator set for the person holding the key, and that limit's
weekly and monthly lines together. A Key names one Budget today, a
Budget's window is fixed to the calendar or to the Unix epoch, and
nothing restarts a window before it resets. This spec lets a Key list
up to four Budgets that every call must fit, lets a Budget's window
start at an instant the operator chooses, and lets an operator restart
a Budget's current window, with every limit still decided at the door
from the core's own counters.

## Current state

On 2026-09-24, at v0.6.0:

- `Key.spec.budget` names one Budget; resolve asks `budget.draw` for it
  as the caller, with the proposed Key as the binding
  (`internal/auth/lookup.go:76-88`); the API writes `status.budget`, its
  name and id (`internal/api/apply.go:223-225`).
- Stage 7 checks the Key's own spend window, then the one Budget: a hard
  Budget refuses `budget_exhausted` when `known + pending + estimate`
  exceeds its amount, with `Retry-After` its reset
  (`internal/serve/limits.go:202-262`). Admission adds the estimate to
  its window's counter and the settle replaces it with the measured
  cost (`limits.go:462-518`).
- A Budget's window is `month` (calendar, UTC, from the 1st), `none`
  (the lifetime, from `createdAt`), or a duration from `1m` to `8760h`
  aligned to the Unix epoch, so `168h` resets on Thursdays at 00:00 UTC
  (`manifest/v1/scalars.go:180-200`). The window is immutable
  (`manifest/resolve.go:396-406`).
- A counter key carries its window's start (`metering/window.go:50-56`)
  and nothing restarts one. The exhaustion marker is keyed the same way,
  so it is claimed once per window (`limits.go:285-303`), and raising a
  Budget's amount inside a window does not re-arm it: a Budget exhausted,
  raised, and exhausted again in one window announces once.
- `RenderBudget` counts `status.keys` by listing every Key in the store
  (`internal/serve/render.go:102-111`), and deleting a Budget checks
  `budget_in_use` the same way (`internal/api/delete.go:75-83`).

## Design

### 1. `Key.spec.budgets`

A Key gains `spec.budgets`, a list of Budget references, by name or
`bud_` id, at most four, no two naming the same Budget, and
`status.budgets`, each entry's name and id in the order written.

| | `spec.budget` | For | Against |
|---|---|---|---|
| A | Kept as the one-Budget form, `exclusive_fields` with `spec.budgets`; `status.budget` stays its rendering, and every reader takes one helper, `v1.KeyBudgets`, that answers the list either way | no manifest, stored Key, golden output, or client breaks; the common case keeps its short form | two spellings of one idea, read through one function |
| B | Deprecated: accepted and rewritten into `spec.budgets` at resolve | one stored form for new writes | every existing golden output of a Key with a Budget changes; Keys stored before the change still carry `status.budget`, so the helper is needed anyway |
| C | Removed | one field | breaks every manifest and client that names a Budget, for no gain the helper does not give |

**Recommendation: A.** Resolve asks `budget.draw` once per entry, as for
the one Budget today, with the proposed Key as the binding, and a
refusal names the entry (`spec.budgets[1]`). The authorizer's
`key.create` resource gains `budgets`, the list as written, beside
`budget`. The `key.created` event's data gains `budgets`, optional,
present when the list is set.

### 2. Stage 7 over every listed Budget

For each Budget the Key lists, in order: a Budget in another currency
than the Model's pricing is `currency_mismatch` naming it, as today.
Then, for each hard Budget `b`,
`projected_b = known_b + pending_b + estimate`, and:

- the request is refused `budget_exhausted` when any `projected_b`
  exceeds `amount_b`;
- the detail names every refusing Budget with its spent and amount;
- each refusing Budget announces its own `budget.exhausted` marker;
- `Retry-After` is the latest reset among the refusing Budgets, and is
  absent when any refusing Budget never resets (`none`), because waiting
  for one window does not reopen another.

A request admitted adds its estimate to the current window of every
listed Budget, soft ones included, and the settle replaces it with the
measured cost in each, into the window the request was admitted in.
The Key's own spend limit is checked before the Budgets and refuses
`spend_exceeded` first, as today. A soft Budget never refuses and
announces its exhaustion at the flush, as today.

### 3. `Budget.spec.anchor`

An RFC 3339 instant, optional, immutable like the window
(`immutable_field` on a change), stored in UTC:

- a duration window aligns to the anchor instead of the epoch: the
  window containing `at` starts at
  `anchor + floor((at - anchor) / d) * d`, for `at` before the anchor as
  well as after it;
- `month` starts on the anchor's day of month at its time of day,
  clamped to the month's last day, so an anchor on the 31st starts
  February's window on the 28th or the 29th and March's on the 31st;
- with `none` it is `invalid_field`: a lifetime has no period to align.

Absent is today's behavior. The anchor is an instant, so a local
midnight is a fixed offset and daylight saving time is not followed.

### 4. `Budget.spec.restartedAt`

An RFC 3339 instant, optional and mutable. When it lies inside the
current window's natural bounds and is not after `now`, the current
window starts at it instead; its reset is unchanged. The counter key and
the exhaustion marker carry the start, so spend restarts from zero and
the marker re-arms. Under `none` the natural window starts at
`createdAt` and never resets; a restart inside it renders the lifetime
counter's key with the instant (`budget:<id>:spend:none@<unix>`), so
the lifetime counts from the restart. An instant before the current
window, from an earlier window, has no effect; an instant in the future
takes effect when the clock reaches it, if it is still inside the
window. A request admitted before the restart settles into the window
it was admitted in.

| | Shape | For | Against |
|---|---|---|---|
| A | A field, `spec.restartedAt`, applied as any update | idempotent under retry, since an applied instant is the same instant twice; no new route and no new authorizer action; the Key cache's `budget.updated` eviction carries it to every replica | the caller names the instant, which a platform takes from its own clock |
| B | A verb, `POST /v1/budgets/{id}/restart` | reads as an action | a new route and action; a retried call restarts twice |

**Recommendation: A.**

### 5. The marker when the amount changes, and counting Keys

- The exhaustion markers of a Budget and of a Key's spend limit carry
  the amount they were claimed at
  (`budget:<id>:exhausted:<start>:<amount>`), so a Budget exhausted,
  raised, and exhausted again in one window announces twice, once per
  amount.
- The store's object filter gains `Budget`, a `bud_` id, that selects
  the live Keys listing it, in either form. `RenderBudget` counts
  `status.keys` and the delete's `budget_in_use` check reads through it,
  so neither lists every Key. On Postgres it is served by two partial
  expression indexes over the Key rows' `status` (one on
  `status->'budget'->>'id'`, one GIN on `status->'budgets'`), which the
  additive migration rule admits; the memory and file stores filter in
  process.

### Where the window is computed

One function, `metering.BudgetWindow(b, at)`, answers a Budget's
current window, its start and reset, from `spec.window`, `spec.anchor`,
`spec.restartedAt`, and `status.createdAt`, and one,
`metering.BudgetCounterKey(scope, b, at)`, its counter keys. The
Limiter, `RenderBudget`, `RenderKey`, and the markers read only these.
A Key's own spend window is unchanged.

## Not in this spec

- Zone-aware windows.
- More than four Budgets per Key.
- An anchor or a restart on a Key's own `limits.spend`.
- Tiers of Budgets, where one Budget spends before another: every
  listed Budget is drawn by every call.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | A Key lists one to four distinct Budgets; five, a duplicate, and both `budget` and `budgets` are refused with `invalid_field` or `exclusive_fields` naming the path; `status.budgets` carries each name and id in order | `manifest` resolve tests and golden corpus entries; `internal/api` apply test |
| 2 | Resolve asks `budget.draw` once per entry as the caller with the proposed Key as the binding, and a refusal names `spec.budgets[i]` | `internal/auth` lookup test with a recording authorizer |
| 3 | A request under a Key with two hard Budgets is refused `budget_exhausted` when either would exceed its amount, the detail naming the refusing Budget, `Retry-After` the latest reset among refusing Budgets and absent when one is `none`; an admitted request adds its estimate and its settled cost to both | `internal/serve` Limiter tests; conformance case `case037SeveralBudgets` |
| 4 | Each refusing Budget raises its own `budget.exhausted`, once per window and amount | `internal/serve` Limiter test |
| 5 | A duration window with an anchor starts at the anchor plus whole periods, before and after the anchor; `month` with an anchor starts on its day and time, clamped to the month's last day; an anchor under `none` is `invalid_field`; a changed anchor is `immutable_field` | `metering` window tests, table-driven; `manifest` resolve tests; conformance case `case037AnchoredWindow` reading `status.resetsAt` |
| 6 | `restartedAt` inside the current window and not in the future restarts `status.spent` at zero with `status.resetsAt` unchanged, reopens a Budget that was `Exhausted`, and re-arms its marker; one before the window or in the future has no effect until it is inside and past; a request admitted before the restart settles into the old window; under `none` the lifetime restarts | `metering` and `internal/serve` tests; conformance case `case037Restart` |
| 7 | A Budget raised inside a window and exhausted again announces a second time | `internal/serve` Limiter test |
| 8 | `RenderBudget` and `budget_in_use` read the Keys of one Budget through the filter, on every store, both forms of reference counted | `internal/store/storetest` case for `Filter.Budget`; `internal/serve` render test over a counting store; Postgres tier |
| 9 | The manifest tables of [[003-manifest-contract]], the rules of [[007-keys-and-limits]], and the counter keys of [[009-usage-and-metering]] carry the new fields and keys, and the API reference and the `lux` command show `budgets` | spec-lint and the docs freshness checks; `internal/luxcli` test |

## Outcome

Built and verified on 2026-09-24, with recommendation A of each choice:
`spec.budget` kept as the one-Budget form beside `spec.budgets`, and the
restart a field.

| # | Test |
|---|---|
| 1 | the corpus entries `accepted/key/org-member`, `refused/exclusive_fields/key-budget-and-budgets`, `refused/invalid_field/key-budgets-duplicate` and `key-budgets-five`; `TestKeyListsBudgets`, `internal/api` |
| 2 | `TestLookupBudgetsAskEachEntry`, `internal/auth` |
| 3 | `TestSeveralBudgets`, `TestSeveralBudgetsRetryAfter`, `internal/serve`; `case037SeveralBudgets` |
| 4 | `TestSeveralBudgetsRetryAfter` |
| 5 | `TestAnchoredBounds`, `manifest/v1`; `TestBudgetWindow`, `metering`; `TestAnchoredBudget`, `internal/serve`; `refused/invalid_field/budget-anchor-under-none`; the immutable anchor in `TestKeyListsBudgets`; `case037AnchoredWindow` |
| 6 | `TestBudgetRestart`, `internal/serve`; `TestBudgetWindow`; `case037Restart` |
| 7 | `TestBudgetAmountRaisedRearms` |
| 8 | `TestFilterByBudget` in the store suite, memory and Postgres; `TestPostgresQueriesUseIndexes` over a table seeded with Keys in both forms; `status.keys` and `budget_in_use` through the API in `TestKeyListsBudgets` |
| 9 | this change's edits to specs 003, 006, 007, 009, 010, and 012; `api/openapi.yaml` regenerated; `TestFlagFormsBuildTheManifest` and `TestCLIDocIsCurrent`, `internal/luxcli` |

Notes on the build:

- The example plane (`examples/plane`) draws on every listed Budget and
  renders anchored and restarted windows too, since it runs the same
  conformance suite as the core.
- The exhaustion marker of a Key's own spend limit carries the amount
  as well, by the same function as a Budget's, so a raised spend limit
  re-arms it too. A window that announced before the upgrade announces
  once more after it, which the changelog says.
- Postgres plans the Budget filter through the two partial indexes when
  the list is read whole, as `RenderBudget` and `budget_in_use` read it;
  a limited page over few Keys may scan the name index instead, which
  costs nothing at that size.
- The refusal's detail, which names every refusing Budget, is on the
  error the doors log and record; the doors' envelope carries the fixed
  message, so the conformance cases hold `Retry-After` and the Budgets'
  status instead.

