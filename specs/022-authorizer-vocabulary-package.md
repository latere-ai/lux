---
title: "The authorizer vocabulary as a package: the actions, resource shapes, and limits an authorizer is written against"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
  - specs/006-identity.md
affects: [authorizer/, internal/auth/, internal/api/, internal/tunnel/, internal/events/, internal/luxcli/, internal/arch/, cmd/luxd/, test/, docs/plane.md, examples/authorizer/]
effort: small
created: 2026-09-14
updated: 2026-09-14
author: changkun
---

# The authorizer vocabulary as a package

## Overview

[[006-identity]] gives an installation's authorizer one contract: the
envelope of `latere.ai/x/pkg/authz`, the twenty-four actions `luxd`
asks, the resource it sends per action, and the `limits` object an
allow may carry. The envelope is importable; the rest is not. The
action names, their table, the resource builders, and the decoder of
`limits` live in `internal/auth`, which no other module can import,
so a platform that writes the authorizer keeps its own copy of the
twenty-four strings and its own copy of the six wire names of
`limits`, and a test that reads this repository's spec table to hold
the copy to it. Two copies of one vocabulary drift, and a drift here
is a request refused or allowed wrongly.

This spec moves what an authorizer is written against into a root
package, `latere.ai/x/lux/authorizer`, with the promise the root
packages make: additive, and the same on every build. `internal/auth`
keeps what only `luxd` runs: the verifier, the client, the owner
policy, the lookup, and the startup checks. Nothing changes on the
wire.

## Current state

`internal/auth/actions.go` holds the twenty-four action constants,
`Actions()`, `Kind()`, `Known()`, `KindUsage`, the resource builders
(`ProviderCreate`, `ProviderObject`, `ProviderList`, `ModelCreate`,
`ModelObject`, `ModelList`, `ModelUse`, `KeyCreate`, `KeyObject`,
`KeyList`, `BudgetCreate`, `BudgetObject`, `BudgetList`, `UsageRead`),
and `ResourceFor(action, object)`. `internal/auth/limits.go` holds
`Limits`, the wire struct of the six `limits` members, and
`DecodeLimits(authz.Decision)`. Both files import only
`latere.ai/x/pkg/authz`, `manifest`, `manifest/v1`, and the standard
library; neither reaches the verifier, the client, or the store.

Eleven packages import `internal/auth`; those that use only the
vocabulary are `internal/tunnel`, `internal/events`,
`internal/luxcli`, `test/stubs/authorizer`, `test/stubs/issuer`,
`test/conformance`, and `test/e2e`. `internal/api` uses both halves.
A platform's authorizer, outside this module, holds a checked-in copy
of the action list and of the `limits` wire names, held to
[[006-identity]]'s table by a test that reads a copy of the table.

## Design

### The package

`latere.ai/x/lux/authorizer` is the seam between `luxd` and its
authorizer, as an import. Its surface is exactly what moves, under the
same names:

```go
package authorizer

// The actions, one constant per row of spec 006's table.
const (
	ActionProviderCreate = "provider.create"
	// … the twenty-four, in the table's order …
	ActionUsageRead = "usage.read"
)

// KindUsage is the resource kind of usage.read.
const KindUsage = "Usage"

func Actions() []string          // every action, in the table's order
func Kind(action string) string  // the resource kind an action acts on; "" outside the vocabulary
func Known(action string) bool

// The resource per action, exactly the fields of spec 006's table.
func ProviderCreate(p *v1.Provider) authz.Resource
func ProviderObject(p *v1.Provider) authz.Resource
func ProviderList() authz.Resource
func ModelCreate(m *v1.Model) authz.Resource
func ModelObject(m *v1.Model) authz.Resource
func ModelList() authz.Resource
func ModelUse(selector string, matched []v1.ModelRef) authz.Resource
func KeyCreate(k *v1.Key) authz.Resource
func KeyObject(k *v1.Key) authz.Resource
func KeyList() authz.Resource
func BudgetCreate(b *v1.Budget) authz.Resource
func BudgetObject(b *v1.Budget) authz.Resource
func BudgetList() authz.Resource
func UsageRead(keys, owners []string) authz.Resource
func ResourceFor(action string, obj v1.Object) (authz.Resource, bool)

// Limits is what an allow granted, decoded one wire name to one figure.
type Limits struct {
	RequestsPerMinute int
	Key               manifest.Limits
	MaxKeys           int
}
func DecodeLimits(d authz.Decision) (Limits, error)

// WireLimits is the limits object as an answer carries it, exported so
// an authorizer renders its answer through the type luxd decodes.
type WireLimits struct {
	RequestsPerMinute       *int        `json:"requests_per_minute,omitempty"`
	MaxKeyRequestsPerMinute *int        `json:"max_key_requests_per_minute,omitempty"`
	MaxKeyTokensPerMinute   *int        `json:"max_key_tokens_per_minute,omitempty"`
	MaxKeySpend             *v1.Money   `json:"max_key_spend,omitempty"`
	MaxKeyTTL               v1.Duration `json:"max_key_ttl,omitempty"`
	MaxKeys                 *int        `json:"max_keys,omitempty"`
}
```

`WireLimits` is the one addition: the wire struct `DecodeLimits` reads
today is unexported, and an authorizer that renders `limits` wants the
type `luxd` decodes rather than six string literals. Its members carry
`omitempty`, because an absent member grants nothing and takes nothing
away ([[006-identity]]), so an authorizer that sets two ceilings sends
two. `DecodeLimits` decodes into it and keeps every rule it has: an
object that does not parse, a negative figure, a spend that is not a
money string, or a ttl that is not a duration is an error, and the
caller treats the answer as no decision.

### What the package promises

The root packages' promise ([[001-architecture]]): additive within a
module major, and the same on every build. For this package that
means an action string never changes and never disappears, a resource
shape only gains members, and a `limits` member keeps its wire name
and its meaning. A new action is a new row in [[006-identity]]'s table
first, then a constant here. The package's own documentation says
this, and says it is written for an authorizer's author.

### What moves and what stays

| Today in `internal/auth` | After |
|---|---|
| `actions.go`: the constants, `KindUsage`, `actions`, `Actions`, `Kind`, `Known`, `verb`, the fourteen builders, `ResourceFor` | `authorizer/actions.go`, unchanged but for the package clause and its documentation |
| `limits.go`: `Limits`, `wireLimits`, `DecodeLimits` | `authorizer/limits.go`, with `wireLimits` exported as `WireLimits` |
| `actions_test.go` and the limits half of the tests | the package's tests, unchanged in what they hold |
| `authorizer.go`, `verifier.go`, `policy.go`, `lookup.go`, `startup.go`, `errors.go`, `doc.go` | stay: the client, the verifier, the owner policy, the lookup, the startup checks, and the errors are `luxd`'s |

Every importer of the moved names switches to the package in the
commit that moves them; `internal/auth` keeps no forwarding name, as
the repository's convention says. `internal/auth` imports the package
for `DecodeLimits` and `Known`, which its client and policy use.

### The dependency rule

The package imports `latere.ai/x/pkg/authz` for `Resource` and
`Decision`, `manifest` for `Limits`, `manifest/v1` for the kinds, and
the standard library. It imports nothing under `internal/`, no HTTP
client of its own, no database driver, and no identity library, and it
dials nothing: the `authz` package it imports carries a client, but
this package calls none of it, and [[001-architecture]]'s dependency
test holds the package's own imports to the list above and its closure
to what `manifest` already reaches plus `latere.ai/x/pkg/authz`.
[[001-architecture]]'s package table gains the row; [[006-identity]]'s
sentence naming `internal/auth` as the home of the builders names the
package instead. Both edits are made by this spec.

### Who imports it

- `internal/api`, `internal/tunnel`, `internal/events`,
  `internal/luxcli`, `cmd/luxd`, and the test tiers, in place of the
  moved names.
- `examples/authorizer` ([[020-building-a-plane]]) decides over
  `authorizer.Actions()` and answers the probe with the package's
  action names, so the document a platform team copies uses the
  vocabulary by name and not by string; `docs/plane.md` names the
  package in the sentence that introduces the minimal authorizer.
- A platform's authorizer outside this module imports it for the
  action constants, `ResourceFor` in its tests, and `WireLimits` for
  its answers, and deletes its copies.

## Not in this spec

Any change to an action, a resource shape, a `limits` member, or the
envelope: [[006-identity]] owns their meaning and the family contract
owns the envelope. The verifier, the client with its cache rules, the
owner policy, and the startup checks, which stay in `internal/auth`.
A vocabulary for another core.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `authorizer.Actions()` is [[006-identity]]'s table, in its order, and `Kind` and `Known` answer for every row and refuse a string outside it | `TestActionsAreTheTable`, `TestKindPerAction`, moved with the code and unchanged in what they hold | not built |
| Every builder renders exactly the members [[006-identity]]'s resource table names for its row, lists and maps present and never null, and `ResourceFor` picks the row for an action on a manifest object and refuses the rest | `TestResourceShapes`, `TestResourceFor`, moved and unchanged | not built |
| `DecodeLimits` reads the six wire names into the six figures, refuses a negative figure, a spend that is not money, and a ttl that is not a duration, and a `WireLimits` rendered with two members decodes to those two and zero elsewhere | `TestDecodeLimits`, `TestWireLimitsRoundTrip` | not built |
| The package's direct imports are `latere.ai/x/pkg/authz`, `manifest`, `manifest/v1`, and the standard library, and its closure adds `latere.ai/x/pkg/authz` and nothing else to what `manifest` reaches | `TestAuthorizerPackageImports` in `internal/arch`, an explicit list against `go list` | not built |
| No file outside `authorizer/` and its tests spells an action as a string literal, and `internal/auth` exports none of the moved names | `TestVocabularyHasOneHome`, reading the tree | not built |
| `examples/authorizer` imports the package and `docs/plane.md` names it where it introduces the minimal authorizer | [[020-building-a-plane]]'s `TestPlaneDocIsCurrent`, unchanged, over the edited block; `TestExampleAuthorizerUsesTheVocabulary` | not built |
| `internal/auth` and `authorizer` each clear the coverage floor after the move | the coverage gate | not built |
| Every acceptance row of [[006-identity]] and [[011-api]] still passes | their suites, unedited but for the import paths | not built |
