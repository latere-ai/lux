---
title: "The authorizer vocabulary as a package: the actions, resource shapes, and limits an authorizer is written against"
status: testing
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
  - specs/006-identity.md
affects: [authorizer/, internal/auth/, internal/api/, internal/arch/, test/, docs/plane.md, examples/authorizer/, .gitignore]
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

Ten packages import `internal/auth`. `internal/arch` names the
vocabulary alone, `Actions()` in `plane_test.go`; `internal/api`,
`test/stubs/authorizer`, and `test/e2e` name both halves; `cmd/luxd`,
`internal/events`, `internal/luxcli`, `internal/tunnel`,
`test/conformance`, and `test/stubs/issuer` name none of the moved
symbols, so the move does not touch them. `internal/auth` itself names
the constants in `policy.go` and `lookup.go`, three builders in
`lookup.go`, and `Limits` and `DecodeLimits` in `authorizer.go`. A
platform's authorizer, outside this module, holds a checked-in copy of
the action list and of the `limits` wire names, held to
[[006-identity]]'s table by a test that reads a copy of the table.

One thing is in the way. The module root carries a tracked file named
`authorizer`: a stray executable a bare `go build ./examples/authorizer`
left behind and commit `1d00e2c` recorded, which `.gitignore` does not
cover because its stray-binary rule names `/lux` and `/luxd` alone. The
directory this spec adds cannot exist beside it.

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
today is unexported and carries no `omitempty`, and an authorizer that
renders `limits` wants the type `luxd` decodes rather than six string
literals. Its members carry `omitempty` so that an authorizer setting
two ceilings sends two and the other four stay absent, which grants
nothing and takes nothing away ([[006-identity]]). Five are pointers,
so a member deliberately set to zero is still sent and still decodes
to zero, which is the same grant as absence; `MaxKeyTTL` is a
`v1.Duration`, which is a string, so its absent case is the empty
string, exactly what `omitempty` omits and what `DecodeLimits` already
skips. The two conventions therefore agree: for every member, absent
and zero are one grant. `DecodeLimits` decodes into it and keeps every
rule it has: an
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
| `actions_test.go` but for `TestCodesHaveOneSentence`, which holds `errors.go`'s codes and stays; `TestDecodeLimits` out of `authorizer_test.go` but for its `through Decide` case, which holds the client and stays | the package's tests, holding the same shapes and the same figures |
| `authorizer.go`, `verifier.go`, `policy.go`, `lookup.go`, `startup.go`, `errors.go`, `doc.go` | stay: the client, the verifier, the owner policy, the lookup, the startup checks, and the errors are `luxd`'s |

Every importer of the moved names switches to the package in the
commit that moves them; `internal/auth` keeps no forwarding name, as
the repository's convention says. `internal/auth` imports the package
for `Limits` and `DecodeLimits`, which its `Decision` and its client
carry; for `Known` and the action constants of its owner policy; and
for `ProviderObject`, `BudgetObject`, and `ModelUse`, which its
`Lookup` builds. Nothing in the package imports `internal/auth` back,
and `manifest`, which the package imports, reaches only `manifest/v1`
and the YAML decoder, so neither direction is a cycle.

### The dependency rule

The package imports `latere.ai/x/pkg/authz` for `Resource` and
`Decision`, `manifest` for `Limits`, `manifest/v1` for the kinds, and
the standard library. It imports nothing under `internal/`, no
database driver, and no identity library, and it dials nothing: it
constructs no client and calls none of `authz`'s.

Its build list is another matter, and the rule as written today would
refuse it. `latere.ai/x/pkg/authz` carries the shared authorizer
client, so importing it reaches `net/http`, `crypto/tls`, and
`latere.ai/x/pkg/cache`, none of which `manifest` reaches; and
`internal/arch`'s `rootForbid` names `latere.ai/x/pkg/authz` among the
prefixes no root package may reach whatever its row says. A vocabulary
that cannot name `authz.Resource` is not the seam this spec is for, so
[[001-architecture]] and its tests take four edits, all of them made
here:

- the package table gains the row for `authorizer`;
- the root-package rule, which says three trees at the root and no
  HTTP client outside `gateway`, says four and excepts this one, whose
  HTTP client is a type it never constructs;
- `rootDirs` and `TestRootPackagesAreTheThree` accept `authorizer` at
  the module root, and the test's message names four trees;
- `rootAllow` gains an `authorizer` row, the module prefixes
  `latere.ai/x/lux/manifest` and `latere.ai/x/lux/authorizer`, the
  external prefixes `latere.ai/x/pkg/authz`, `latere.ai/x/pkg/cache`,
  and `github.com/goccy/go-yaml`, and the `noStd` entries
  `database/sql` and `os/exec`; `rootForbid` excepts `authorizer` from
  its `latere.ai/x/pkg/authz` prefix, and refuses it everything else,
  `internal/`, `cmd/`, `authkit`, and the store drivers included.

The alternative, splitting the envelope out of `latere.ai/x/pkg/authz`
so the types come without the client, is a change to another module
and is not in this spec's reach. [[006-identity]]'s sentence naming
`internal/auth` as the home of the builders names the package instead.

The directory needs the tracked file of the same name deleted first,
and `.gitignore`'s stray-binary rule gains `/authorizer`.

### Who imports it

- `internal/auth`, `internal/api`, `internal/arch`, and the two test
  tiers that name the vocabulary, `test/stubs/authorizer` and
  `test/e2e`, in place of the moved names. `test/stubs/authorizer` is
  itself `package authorizer`, which is legal beside this import and
  reads badly, so it takes the import under the alias `vocabulary`.
- `examples/authorizer` ([[020-building-a-plane]]) switches on
  `authorizer.Kind` where `decide` tests a `provider.` or `model.`
  prefix today, and names `authorizer.ActionModelUse` where it spells
  `"model.use"`, so the program a platform team copies uses the
  vocabulary by name and not by string. The example imports nothing of
  this module today, by design: it is the whole program `docs/plane.md`
  prints. It gains one import, and the document's sentence that
  introduces the minimal authorizer names the package and says a reader
  who copies the block runs `go get latere.ai/x/lux` first. The Go
  block stays the file word for word, which is what
  [[020-building-a-plane]]'s `TestPlaneDocIsCurrent` holds.
- A platform's authorizer outside this module imports it for the
  action constants, `ResourceFor` in its tests, and `WireLimits` for
  its answers, and deletes its copies.

## Not in this spec

Any change to an action, a resource shape, a `limits` member, or the
envelope: [[006-identity]] owns their meaning and the family contract
owns the envelope. The verifier, the client with its cache rules, the
owner policy, and the startup checks, which stay in `internal/auth`.
A vocabulary for another core.

### What the build changed

Each row is a departure from the design above, with the reason.

| Where | The design said | The build does | Why |
|---|---|---|---|
| the moved tests | `actions_test.go` moves but for `TestCodesHaveOneSentence` | the fixtures of that file, which four of `internal/auth`'s own tests build from, stay behind in `internal/auth/fixtures_test.go` and are rendered through the package; `TestCodesHaveOneSentence` moves to `internal/auth/errors_test.go`, the file its subject is in | the fixtures are a test helper and not the vocabulary, and `actions_test.go` is no name for a file in a package with no `actions.go` |
| `TestDecodeLimits`'s `through Decide` case | it stays in `internal/auth` | it stays as `TestDecodeLimitsThroughDecide`, a test of its own | two tests of one name in one package is not possible, and the case is about `Decide` reading an answer it cannot decode, not about the decoding |
| `TestVocabularyHasOneHome` | no package outside `authorizer/` declares an action constant or its own copy of the six `limits` wire names | so it holds, and `examples/plane` switched to the package: its twenty-three action constants, its `kindUsage`, and its own four-name reading of the `limits` object are gone | `examples/plane` is a platform's front built from the root packages, which is exactly the reader this spec is for; the design named the importers inside `luxd` and did not look at the example |
| `TestVocabularyHasOneHome` | the six wire names are declared in one place | [[011-api]]'s `SelfLimits` is excepted by name in the test, with its reason | `GET /v1/self` renders what the last allow granted in those same names, as a response schema of the `/v1` API and its OpenAPI document; it reads no answer and is not a second reading of the contract |
| `examples/authorizer` | switches on `authorizer.Kind` and names `authorizer.ActionModelUse` | that, and denies an action `Kind` does not know | the prefix test it replaces refused an unknown `provider.` or `model.` string, and the policy should not grow a hole where the vocabulary ends |
| the importers | `internal/api` and `examples/authorizer` import the package where they name it | `internal/api`'s one `provider.tunnel` ask moved to `auth.go` beside the other asks, so `tunnel.go` names the vocabulary through it, and `examples/authorizer` reads the reserved probe id from `authz.ProbeID` instead of a copy of the value | the shared gate's identity rule reads a string literal carrying `authorize` and a slash in a file that posts as an authorizer asked by hand, and the import path `latere.ai/x/lux/authorizer` is such a literal; both changes are ones the tree is better for, and the rule's reading of an import path is reported to the gate |
| `.gitignore` | the stray-binary rule gains `/authorizer` | it did, on `main`, and this build adds `!/authorizer/` beside it | a rule that names a path ignores the directory of that name whole, and the package directory is tracked; with the directory there, a bare `go build ./examples/authorizer` now refuses to write rather than leaving a stray |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `authorizer.Actions()` is [[006-identity]]'s twenty-four actions, each distinct and handed out as a copy, `Kind` answers the kind of every one, and both `Kind` and `Known` refuse `""`, `key`, `key.rotate`, `usage.write`, and `Provider.read` | `TestActionsAndKinds`, moved with the code and unchanged in what it holds | passing |
| Every builder renders exactly the members [[006-identity]]'s resource table names for its row, a create carries no id, lists and maps are present and never null, and `ResourceFor` picks the row for an action on a manifest object and refuses a wrong kind, a nil object, `model.use`, `usage.read`, and a string outside the vocabulary | `TestResourceShapes`, `TestResourceListsAreNeverNull`, `TestResourceForRefusesTheWrongKind`, moved and unchanged | passing |
| `DecodeLimits` reads the six wire names into the six figures, refuses a negative figure, a spend that is not money, and a ttl that is not a duration, and a `WireLimits` with two members set renders exactly those two names and decodes back to those two figures with the other four zero | `TestDecodeLimits`, moved but for its `through Decide` case; `TestWireLimitsRoundTrip` | passing; the case that stayed is `internal/auth`'s `TestDecodeLimitsThroughDecide` |
| The package's direct imports are `latere.ai/x/pkg/authz`, `manifest`, `manifest/v1`, and the standard library; its build list adds `latere.ai/x/pkg/cache` and the standard library `authz`'s client reaches, and reaches no package under `internal/` or `cmd/`, no `authkit`, no database driver, and no store driver | [[001-architecture]]'s `TestRootPackagesDialNothing`, in its `authorizer` sub-test against the new `rootAllow` row, with `TestRootPackagesAreTheThree` accepting the fourth tree | passing |
| `internal/auth` exports none of the moved names, and no package outside `authorizer/` declares an action constant or its own copy of the six `limits` wire names; the action strings that remain in the tree are JSON fixtures and subtest names inside tests | `TestVocabularyHasOneHome`, over the tree's declarations | passing, with `examples/plane` switched to the package and one exception recorded below |
| `examples/authorizer` reaches every action it decides on through the package rather than through a string literal, and `docs/plane.md` names the package where it introduces the minimal authorizer | `TestExampleAuthorizerUsesTheVocabulary`; [[020-building-a-plane]]'s `TestPlaneDocIsCurrent` and `TestPlaneDocAuthorizerConforms`, unedited, over the edited program | passing |
| The module root holds the `authorizer` package directory and no file of that name, and a stray binary at the root from any `go build` of this module is ignored | the gate's build, which cannot produce the directory while the file is tracked, and `git status` clean after `go build ./examples/authorizer` | passing; the file was deleted before this spec was built, and a bare build of the example now refuses rather than writing a stray |
| `internal/auth` and `authorizer` each clear the coverage floor after the move | the coverage gate | passing: 100% of `authorizer` and 99.1% of `internal/auth` |
| Every acceptance row of [[006-identity]] and [[011-api]] still passes | their suites, unedited but for the import paths | passing |
