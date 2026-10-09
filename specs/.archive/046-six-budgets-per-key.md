---
title: "Six Budgets per Key: the bound on spec.budgets raised from four"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/007-keys-and-limits.md
  - specs/018-conformance-suite.md
  - specs/.archive/037-several-budgets-per-key.md
affects: [manifest/, internal/serve/, internal/api/, test/conformance/, docs/, README.md, specs/003-manifest-contract.md, specs/007-keys-and-limits.md]
effort: small
created: 2026-10-09
updated: 2026-10-09
author: changkun
---

# Six Budgets per Key

## Overview

A Key lists in `spec.budgets` the Budgets every one of its calls must
fit, and [037-several-budgets-per-key](037-several-budgets-per-key.md)
held the list to four. Its own case needed three, an organization's
balance and a member's weekly and monthly limits; four was chosen with
no technical reason behind it, and a longer list was left out of scope.

A platform that layers its limits one level deeper needs five. It gives
each API key it issues a Budget of its own, so one count holds
everything the key causes, and that Budget is listed by the Key serving
the API key's own calls and by the Key of every agent session the API
key starts. A session of an organization's agent started that way
answers to the organization's balance, the agent's two limit lines, the
session's own Budget, and the API key's. With the bound at four its
Key is refused `invalid_field`, so the session gets no Key at all.

A Key now lists up to six. Five are needed; the sixth is spare, so the
next layer of this kind is a manifest and not a release of the core and
its rollout.

## Current state

On 2026-10-09:

- `v1.MaxKeyBudgets` is 4 and has one reader: the structural validation
  of `Resolve` refuses a longer `spec.budgets` with `invalid_field` at
  `spec.budgets` (`manifest/validate.go`), before any entry is looked
  up.
- Nothing else knows the number. `v1.KeyBudgets` answers a slice, and
  stage 7, `RenderKey`, the store's Budget filter with its two Postgres
  indexes, the authorizer's `key.create` resource, and the `key.created`
  event each take the list at the length it has. The OpenAPI document
  puts no `maxItems` on `budgets`.
- The bound is held when a Key is written, through the API, a bootstrap
  directory, or the file mode's load, and never when a stored Key is
  read.

## Design

### The bound

`v1.MaxKeyBudgets` is 6. A `spec.budgets` of one to six distinct entries
is accepted and a seventh is `invalid_field` at `spec.budgets`, the
detail naming the bound and the count given. `spec.budget`, the
duplicate rule, the `not_found` of one entry, and all that stage 7 does
with the list stay as
[037-several-budgets-per-key](037-several-budgets-per-key.md) left
them.

Six and not five, because a bound that fits one consumer exactly makes
that consumer's next limit a release of the core. Six and not unbounded,
because every entry is paid for on each of the paths below: a bound
keeps what a Key costs a request, a write, and a read a constant of the
contract, and refuses a generated list that grew by mistake at the
write, where its author reads the refusal, and not as a slow door.

### What a listed Budget costs

| Where | For each listed Budget |
|---|---|
| A request, stage 7 of [[004-request-path]] | one read of the Budget through the Key cache, from memory inside `LUX_KEY_CACHE` and from the store once it has passed; one comparison of its currency with the Model's pricing; one read each of its counter's known and pending totals; one add of the estimate at admission and one of the measured cost less the estimate at the settle. The counter is this replica's own, in memory, so an admitted request writes nothing to the store for it |
| A refused request | the same reads and no add; for each Budget that refuses, and for no other, one marker claim in the store, the first of a window and amount raising `budget.exhausted` |
| A flush | one counter row written when the Budget's window took spend since the last flush, whichever Keys spent it |
| A write of the Key | one lookup of the Budget and one `budget.draw` question to the authorizer, with the proposed Key as the binding |
| A read of the Key through the API | one object read of the Budget, and one more key in the single counter read `RenderKey` makes |

No request asks `budget.draw`: the authorizer answers for an entry when
the Key is written, and the door reads the stored list. So two more
entries add to an admitted request, inside the cache window, ten
operations on maps in this replica's memory, and to a write two lookups
and two authorizer questions.

### A rollout

The number is read in one place, so during a rollout a replica still on
the release before this one refuses to write a Key listing five or six,
and serves one that another replica wrote. A consumer that needs five
writes its Keys after every replica runs the new release, or takes the
refusal: the Key is not created, and nothing is served outside a Budget.

## Not in this spec

- A bound an operator configures. One number in the contract keeps a
  manifest valid on every installation.
- More than six Budgets per Key.
- Tiers of Budgets, where one spends before another: every listed
  Budget is drawn by every call, as before.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | A Key listing six distinct Budgets resolves with `spec.budgets` in the order written, and one listing seven is `invalid_field` at `spec.budgets` | the corpus entries `accepted/key/agent-session` and `refused/invalid_field/key-budgets-seven`; `TestFieldSyntax`, `manifest` |
| 2 | Through the API, a Key listing six Budgets is created with `budget.draw` asked once per entry, `status.budgets` carries each name and id in order, and each Budget counts the Key in `status.keys`; a create or an update listing seven is `invalid_field` at `spec.budgets`, asks no `budget.draw`, and writes no Key | `TestKeyListsSixBudgets`, `internal/api` |
| 3 | A request under a Key listing six hard Budgets adds its estimate and then its settled cost to all six; with the sixth alone at its amount the next is refused `budget_exhausted`, the detail and the one `budget.exhausted` naming the sixth alone, `Retry-After` its reset, and nothing is added to the five with room | `TestSixBudgets`, `internal/serve` |
| 4 | Over HTTP, a Key listing six Budgets with the sixth tight is served its first request and refused the second `budget_exhausted` with the sixth's `Retry-After`, and each of the six holds the one request in `status.spent` and counts the Key in `status.keys` | `case046SixBudgets` |
| 5 | The field table of [[003-manifest-contract]], the stage 7 rule of [[007-keys-and-limits]], the README, and `docs/plane.md` state six | this change's edits |

## Outcome

Built and verified on 2026-10-09 as designed.

| # | Test |
|---|---|
| 1 | `TestGoldenCorpus` over `accepted/key/agent-session` and `refused/invalid_field/key-budgets-seven`; `TestFieldSyntax`, six among the values the rules admit and seven among the refused |
| 2 | `TestKeyListsSixBudgets` |
| 3 | `TestSixBudgets` |
| 4 | `case046SixBudgets`, against the reference server and in the end-to-end tier, both with stubs; the example plane's run has none and skips it, and `case003AcceptedCorpus` applies the six-Budget Key to the example plane too |
| 5 | the edits to specs 003 and 007, `README.md`, and `docs/plane.md` |

With the bound at four, criteria 1, 2, and 4 fail, each on
`at most 4 Budgets; 6 given`. Criterion 3 holds either way: stage 7
never read the bound, and its test stores the Key directly, so it holds
the door to a list of the new length and not the bound itself.

Notes on the build:

- The corpus's Lookup answers a Budget reference from the accepted
  Budgets alone, so the six-Budget Key brought four accepted Budgets
  with it: `agent-daily`, `agent-monthly`, `session-run`, and
  `caller-key`. Two of them are forms the corpus did not hold,
  `agent-monthly` a `month` window with an anchor and `session-run` a
  `none` window.
- `refused/invalid_field/key-budgets-five` is now
  `key-budgets-seven`, since five is accepted.
- The OpenAPI document is unchanged, having stated no bound.
