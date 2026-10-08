---
title: "A zero-priced request on a spent window: a Model whose every price is zero is served whatever a spend limit or a Budget has left"
status: complete
track: core
depends_on:
  - specs/004-request-path.md
  - specs/007-keys-and-limits.md
  - specs/009-usage-and-metering.md
  - specs/018-conformance-suite.md
affects: [metering/, internal/serve/, examples/plane/, test/conformance/, docs/, specs/003-manifest-contract.md, specs/004-request-path.md, specs/007-keys-and-limits.md, specs/009-usage-and-metering.md]
effort: small
created: 2026-10-08
updated: 2026-10-08
author: changkun
---

# A zero-priced request on a spent window

## Overview

A hard window, a Key's spend limit or a hard Budget, refuses a request
when `known + pending + estimate` would pass its amount
([[007-keys-and-limits]]). A request that lands on the amount runs, and
so does the one whose estimate fit but whose measured cost did not: a
spent window is often past its amount, by the request that crossed.
From then on the comparison is true for every request, an estimate of
zero included, so a Key whose Budget ran dry was refused
`budget_exhausted` for a Model whose every price is zero, and the same
held for `spend_exceeded`. A request that costs nothing cannot move a
money window, and a platform that pairs a funded Budget with free
Models wants exactly those to keep answering when the money is gone.

The spend and Budget windows now admit a request for a Model whose
every price is zero whatever they hold. Every other limit applies to it
as before.

## Current state

On 2026-10-08:

- `serve.Limiter.reserveSpend`, stage 7 of [[004-request-path]], has the
  resolved Model, so its pricing, before any money question. The
  estimate is `metering.Cost` over the reserved input and output tokens;
  the settle replaces it with `metering.Cost` over the measured input,
  output, cached input, and cache write.
- Each of the spend window and every hard Budget refuses on
  `metering.Exceeds(Projected(known, pending, estimate), amount)`, which
  is `projected > amount`. With `known + pending` above the amount and
  an estimate of zero it is true.
- `examples/plane` holds the same windows in its own Limiter, which the
  conformance suite runs against.

## Design

### The rule

`metering.Free(p)` is true when `p` is not nil and every member `Cost`
reads, `input`, `output`, `cachedInput`, and `cacheWrite`, is zero or
unset. Then `Cost(t, p)` is zero for every `t`, so the gate and the
meter read "costs nothing" from the same members. It is decided from
the price alone: an estimate of zero is not the test, because the
estimate prices only input and output, and a Model with a priced cached
input or cache write would be admitted and then billed. A rounding to
zero is not the test either: one member above zero is a priced Model,
however small.

A nil pricing is an unpriced Model, whose cost is unknown rather than
zero. It stays under `model_unpriced` and, under `allowUnpriced`, under
the windows as before.

### The windows

At stage 7, when `metering.Free` holds for the Model's pricing, neither
the Key's spend window nor any hard Budget refuses the request, whatever
`known + pending` is. The request is otherwise admitted as any other:
it is counted under the Key's totals and its spend window, its estimate
of zero enters every window it draws on, and its settle adds zero. It
raises no `key.exhausted` or `budget.exhausted`, since no refusal
observed the window.

Unchanged for it: the request and token rate buckets, `rate_limited`;
`model_unpriced`, which cannot apply to a priced Model; and
`currency_mismatch`, so a zero-priced Model in another currency than a
Budget's is refused as a misconfiguration. The Key's validity, its
expiry, its selectors, and `model_disabled` are earlier stages and do
not move.

A priced Model on a spent window is refused with the code, the detail,
and the `Retry-After` it was refused with before.

### Status

A Key renders `Exhausted` and a Budget `Exhausted` from their counters
as before; a Key in that state is still served a zero-priced Model.

## Not in this spec

- A soft Budget, which never refuses and is unchanged.
- An opaque route, which names no Model and is unpriced by
  construction.
- A request whose estimate rounds to zero under a priced Model: it can
  settle above zero, and the window keeps refusing it.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `Free` is true for a pricing whose every member is zero or unset, false for a nil pricing and for each member alone above zero, and every price member of `v1.Pricing` is a term of it; a free pricing costs zero for any count | `TestFree`, `metering` |
| 2 | A hard Budget past its amount admits a zero-priced request, counts it, adds nothing to the Budget, and refuses the next priced request `budget_exhausted` with the same detail and `Retry-After` | `TestZeroPricedModelOnASpentBudget`, `internal/serve` |
| 3 | A Key's spend window past its amount does the same, refusing the priced request `spend_exceeded` as before | `TestZeroPricedModelPastASpendLimit` |
| 4 | A Model priced at zero for input and output but above zero for cached input or cache write is refused on a spent Budget; an unpriced Model under `allowUnpriced` is too; a zero-priced Model in another currency is `currency_mismatch` | `TestOnlyAModelPricedAtZeroPassesASpentBudget` |
| 5 | The request and token rate buckets refuse a zero-priced request on a spent Budget `rate_limited` | `TestRateLimitsHoldForAZeroPricedModel` |
| 6 | The example plane holds the same rule | `TestAZeroPricedModelPassesASpentWindow`, `examples/plane` |
| 7 | Over HTTP, a hard Budget carried past its amount refuses a priced Model `budget_exhausted`, serves a zero-priced one whose record costs `0` with `priced` true, and keeps `status.spent` at the priced request's cost | `case045ZeroPricedOnASpentBudget` |

## Outcome

Built and verified on 2026-10-08 as designed.

| # | Test |
|---|---|
| 1 | `TestFree` |
| 2 | `TestZeroPricedModelOnASpentBudget` |
| 3 | `TestZeroPricedModelPastASpendLimit` |
| 4 | `TestOnlyAModelPricedAtZeroPassesASpentBudget` |
| 5 | `TestRateLimitsHoldForAZeroPricedModel` |
| 6 | `TestAZeroPricedModelPassesASpentWindow` |
| 7 | `case045ZeroPricedOnASpentBudget`, against the reference server and in the end-to-end tier, both with stubs; the example plane's run has none and skips it, so criterion 6 holds the example to the rule |

Without the rule, criteria 2, 3, 5, 6, and 7 fail: the zero-priced
request is refused `budget_exhausted` or `spend_exceeded` with an
estimate of `0` in the detail. Criterion 4 holds either way and guards
the rule from widening to an estimate of zero.
