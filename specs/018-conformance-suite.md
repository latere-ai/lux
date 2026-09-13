---
title: "Conformance suite: the contract, the doors, and the API as executable tests, against any server"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/011-api.md
affects: [test/conformance/, internal/serve/, docs/]
effort: large
created: 2026-09-13
updated: 2026-09-14
author: changkun
---

# Conformance suite

## Overview

The contract is a suite, not a document. `test/conformance` is an
importable Go test package that, given a server URL and a token, runs
the acceptance criteria of the manifest contract, the dialect doors,
and the `/v1` API against whatever is listening, and reports which
hold. `luxd` passes it in every tier ([[015-test-stubs-and-tiers]]);
the release pipeline passes it against the published images before a
tag publishes ([[017-release-and-installation]]); a platform that
composes the packages behind its own front runs it against that front
to prove its edge did not change what a manifest means or what a door
answers ([[020-building-a-plane]]).

The suite is the reason the invariants of [[001-architecture]] are
checkable by someone outside Latere. A fork, a platform, or a
reimplementation is conformant when this package is green against it,
and nothing else in the tree makes that claim.

## Current state

Nothing is built. The hosted gateway this design is extracted from uses
the word "conformance" nowhere. It has a handful of live tests that take
a base URL from the environment and skip without a key, which is the
closest thing to this idea in the family, but they sit inside the same
package as its unit tests, cannot be imported, run against real
upstreams so they assert shapes rather than values, and are not run by
its release pipeline. Its release smoke is a shell script that fetches
three paths. Nothing there states a contract a third party could
implement, so none of this is a port.

## Design

### Shape

```go
// Run executes every case the configuration admits against one
// server. It is the whole public surface: a caller supplies where the
// server is and how to get a token, and gets a subtest per case.
func Run(t *testing.T, cfg Config)

type Config struct {
	// URL is the base the doors and /v1 hang off, the value of
	// LUX_PUBLIC_URL on the server under test.
	URL string
	// Token returns an issuer token for a subject, and false when this
	// deployment cannot mint one; a case that needs a subject it
	// cannot get skips with the subject named. A caller that can mint
	// returns a token per subject; a caller holding one static token
	// returns it for Subject and false for every other name.
	Token func(subject string) (string, bool)
	// Subject is who Token's default token speaks for, the value the
	// isolation labels and the owner assertions expect.
	Subject string
	// StubsURL is a lux-stubs instance the server's Providers point
	// at. Empty skips the cases in the stub table below.
	StubsURL string
}
```

`Run` fetches `GET /.well-known/lux` ([[011-api]]) once before the
first case and reads the served `doors`, `dialects`, `mode`, and
`apiVersion` from it, so the suite adapts to a server that serves three
doors instead of four and refuses to guess at anything else. It is not
a `Config` field: a caller that could tell the suite what the server
serves could tell it wrong, and the document exists so nobody has to.

`TestContract` in `test/conformance` is the entry point. It builds
`Config` from the environment and calls `Run`:

| Variable | Required | Means |
|---|---|---|
| `LUX_TEST_URL` | yes | the server under test; unset skips the whole suite with one line saying so |
| `LUX_TEST_TOKEN` | with `LUX_TEST_URL` | an issuer token the server accepts on `/v1`; unset leaves only the cases that need no bearer, and the suite prints which groups it dropped |
| `LUX_TEST_SUBJECT` | no | the rendered subject `LUX_TEST_TOKEN` speaks for, `issuer` and `sub` joined by a bar ([[006-identity]]); unset is read from the token's own `iss` and `sub` |
| `LUX_TEST_ISSUER_URL` | no | a `latere.ai/x/pkg/authkit/issuertest` the server lists; set, `Token` mints per subject through its `POST /mint` and the multi-subject cases run; unset, `Token` answers `LUX_TEST_SUBJECT` alone and they skip by name |
| `LUX_TEST_STUBS_URL` | no | a `lux-stubs` instance; unset skips the stub cases and prints the list |

These five are the suite's and are read by no server;
[[002-repository-scaffold]]'s table says so and this is their table.

A `file` mode server runs the read half of the api group and skips the
write half, which the group's own table says.

### The two credentials

The suite needs both of [[006-identity]]'s: an issuer token for `/v1`
and a Key for the doors. It is given the first and mints the second.

`LUX_TEST_TOKEN` is the issuer token. A remote server has no way to
hand one out, so the operator running the suite supplies it: a person's
token from their own login, or a service token from the
`client_credentials` grant their issuer serves. The suite never mints an
issuer token itself and holds no issuer key, because a suite that could
would be a suite an operator could not safely point at a serving
installation.

The Key is the suite's own. Before the `doors` and `keys` groups run,
the suite applies a `Key` named `conf-<run ulid>-key` through `PUT
/v1/keys/{name}` with the selectors the group needs, reads
`status.value` from the one response that carries it ([[007-keys-and-limits]]),
and deletes it at teardown. That is why those two groups depend on the
`api` group passing, and why a `file` mode server skips them: nothing
can mint a Key there ([[010-state]]).

Cases are functions named `case<NNN><Name>(t *testing.T, c *client)`
after the spec whose criterion they prove, registered in one table with
their group and whether they need stubs. A failing case therefore names
its spec in its own name, which is what makes a red suite a bug report
rather than a search.

### Groups

| Group | Proves | From | Stubs |
|---|---|---|---|
| `manifest` | each kind's example applies, resolves, and reads back with a spec byte-identical to what `Resolve` produces; every default is visible in the read-back; every refusal code of the decode and resolve tables is reachable through the API with its path | [[003-manifest-contract]] | no |
| `api` | the four routes per kind; apply as create then update; the preconditions and `conflict`; percent-encoded and two-segment Model names; rotate; `budget_in_use`; pagination and the list filters; the error envelope and `request_id`; the rate limit headers; `read_only` in file mode; every response validating against `GET /v1/openapi.json` | [[011-api]] | no |
| `doors` | every row of the door route table through its door; same-dialect same-bytes; a translated request's `Lux-Loss`; the error envelope in each dialect's own shape; a streamed response's event sequence and final usage; `GET /v1/models` as the Key's view | [[004-request-path]] | yes |
| `keys` | rotate invalidates the old value; each Key state refuses with its code; a rate refusal carries `Retry-After`; a spend refusal and a hard Budget refusal carry theirs; an unpriced Model under a Budget is `model_unpriced` | [[007-keys-and-limits]] | yes |
| `identity` | a Key on `/v1` and an issuer token on a door are each `unauthenticated`; an unauthenticated control plane request is 401; with the authorizer down every control plane request is `authorizer_unavailable` and every data plane request with a valid Key is served | [[006-identity]] | yes |
| `usage` | one request through a door produces exactly one record at `GET /v1/requests` with the tokens, the cost, the model, the provider, and no content; `GET /v1/usage` aggregates it under every dimension and window the parameters admit; a refused request has a record too | [[009-usage-and-metering]] | yes |
| `fixture` | the previous release's manifests and records still read back, below | [[003-manifest-contract]], [[009-usage-and-metering]] | no |

The `manifest` group's inputs are [[003-manifest-contract]]'s golden
corpus and not a second set written here, so one corpus proves `Resolve`
in process and over HTTP and the two cannot drift. That corpus is
`manifest/testdata/v1/` today, which `//go:embed` cannot reach from
another package; this spec needs it exported as an `embed.FS` from a
package of its own, and [[003-manifest-contract]] owns where. Until it
is, the `manifest` group reads the directory by relative path and is
skipped when the suite runs outside a checkout.

Every case cleans up what it created, and the suite runs in under five
minutes against a local `luxd`, which is a `-timeout 5m` on the run
rather than a hope.

### What a front has to pass

`luxd` passes every group. A platform running the suite against its own
front ([[020-building-a-plane]]) is conformant on what it claims:

| Group | Required of any front | Why |
|---|---|---|
| `manifest`, `api` | yes | the contract is the schema and the grammar; a front that admits less admits a different contract |
| `doors` | yes for each dialect its `.well-known/lux` lists | a served door that answers differently is the one thing a caller cannot work around |
| `identity` | yes | the plane boundary of [[006-identity]] is what keeps a Key off `/v1` and a token off a door |
| `usage` | yes | [[020-building-a-plane]] promises a platform's users the same usage surface |
| `keys` | only where the front serves limits | a front that declares no limits has nothing to refuse |
| `fixture` | no | it is this repository's release history and means nothing against another tree |

The mutation check below is `luxd`'s own and is no part of a front's
claim: it proves the suite is specific, not that a server is
conformant.

### The cases that need stubs

`LUX_TEST_STUBS_URL` unset skips exactly these, and the suite prints
the list, so a partial run is never mistaken for a full one:

| Case | Needs |
|---|---|
| `case004SameDialectSameBytes` | the stub's `GET /_received` to compare the forwarded body byte for byte |
| `case004CallerCredentialsNeverForwarded` | the same, to assert the caller's headers are absent |
| `case004TranslationLoss` | a Provider of a dialect other than the door's |
| `case004Streaming` | the stub's fixed event count |
| `case004ModelNameRewrite` | the upstream name the stub received |
| `case004UpstreamError`, `case004UpstreamTimeout` | the `fail-500`, `hang`, and `slow-<d>` upstream model names |
| `case008Fallback` | two targets, the first of them `fail-500` |
| `case007SpendWindow`, `case007BudgetExhausted` | the fixed token counts a stub answers with, so a cost is a known number |
| `case006AuthorizerUnavailable` | the stub authorizer's `-fail-mode` |
| `case012EventReachesTheSink` | the stub sink's `GET /_events` |
| `case009RecordHasTheStubsTokens` | the fixed token counts, so a record's numbers are a known value |

A case in the table asserts against what the provider received, not
against what the gateway reports, which is the whole reason the doors
and keys groups cannot run against an arbitrary front with real
upstreams behind it.

### Isolation

Every object the suite creates is named `conf-<run ulid>-<case>` and
carries the label `conformance=<run ulid>`. Teardown deletes by that
label and nothing else, so the suite never touches an object it did not
create and two runs against one server do not collide. A server that
refuses a create the suite needs fails the case with the refusal, which
is the honest answer for a front that admits less than the contract.

### The previous-release fixture

`test/conformance/testdata/previous/<version>/` holds what the last
tagged release produced: one resolved manifest per kind, and a bucket
of archived records as NDJSON ([[012-request-log-and-events]]). The
fixture group applies each manifest and asserts the read-back's `spec`
equals the fixture's, and decodes each record with the current
`metering.Record` and asserts every field the fixture carries is still
present with the same value.

This is the schema evolution promise of [[003-manifest-contract]] and
the record additivity promise of [[009-usage-and-metering]] executed
rather than asserted: a field that changed type, a default that
changed, or a record member that was removed fails here and nowhere
else. The release pipeline writes the next fixture directory at each
tag, so the set grows by one per release and old ones are kept
([[017-release-and-installation]]).

### The authorizer's own conformance

This suite proves what `luxd` does with a decision. It does not prove
that an operator's authorizer decides correctly, and it cannot: the
authorizer is the operator's program and its answers are the
operator's policy. The test an authorizer passes belongs with the
contract it implements, `latere.ai/x/pkg/authz`, which
[[006-identity]] names as its home. That package ships the envelope,
the client, the cache, the owner policy, and the stub today, and no
conformance test; until one exists, an operator's only check that its
endpoint is wired correctly is `luxd check`'s authorizer row
([[017-release-and-installation]]) and the `identity` group here, and
this spec says so rather than implying the suite covers it.

### The mutation check

A suite that never fails proves nothing. `TestSuiteCatchesADroppedCapability`
builds the server with one capability removed, runs the suite against
it, and asserts that it fails and that the failing cases are exactly
the ones in the row. Each mutation is a build tag over one shim in
`internal/serve`, compiled only under that tag, so no mutation is in a
released binary.

| Mutation | Tag | Must fail, and only |
|---|---|---|
| `If-Match` is ignored | `mut_ifmatch` | `case011Preconditions` |
| one default is not applied in `Resolve` | `mut_default` | `case003DefaultsAreVisible` |
| the translation loss report is dropped | `mut_loss` | `case004TranslationLoss` |
| an unknown manifest field is accepted | `mut_unknownfield` | `case003UnknownField` |
| an unpriced Model is served under a hard Budget | `mut_unpriced` | `case007UnpricedUnderABudget` |
| `Retry-After` is omitted on a rate refusal | `mut_retryafter` | `case007RateLimited` |
| a read returns a Key's `status.value` | `mut_keyvalue` | `case011SecretsNeverInResponses` |
| the caller's `Authorization` is forwarded upstream | `mut_forwardcred` | `case004CallerCredentialsNeverForwarded` |

The second half of each row, and only, is what keeps the suite
specific: a mutation that reddens six cases means six cases are
asserting the same thing, and a mutation that reddens none means the
capability is not covered at all.

Each mutation is built by `go build -tags=<tag>` from the test itself,
so the shims are compiled nowhere else and a released binary carries
none of them. The test therefore needs the Go toolchain on `PATH`,
which is what the gate already guarantees ([[015-test-stubs-and-tiers]]).

### Where it runs

| Runner | Server | Stubs |
|---|---|---|
| the integration tier | `luxd` on loopback with the memory store, through `conformance.Run` in process | yes |
| the postgres tier | two `luxd` processes against one database, the same way | yes |
| the release pipeline | the published `luxd` image | the published `lux-stubs` image |
| a platform's own CI | its front | its own, or none, with the stub cases skipped |

A platform runs it as

```
go test latere.ai/x/lux/test/conformance -run TestContract -v
```

with `LUX_TEST_URL`, `LUX_TEST_TOKEN`, and optionally
`LUX_TEST_STUBS_URL` in the environment, or imports the package and
calls `Run` with a `Config` whose `Token` mints through its own issuer.
That is the command [[020-building-a-plane]] tells a platform to put in
its own pipeline, and it is the same command this repository runs.

## Not in this spec

The conformance test an authorizer passes, which belongs to
`latere.ai/x/pkg/authz` and is described above as a gap rather than
filled here; the stubs the doors and keys groups need and the tiers
that start them ([[015-test-stubs-and-tiers]]); the release pipeline that runs the
suite against the published images and writes the next fixture
([[017-release-and-installation]]); the criteria themselves, which
belong to the specs the cases are named after; the `lux` command,
whose own scenario is [[014-agent-client]]'s.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every acceptance criterion of [[003-manifest-contract]], [[004-request-path]], and [[011-api]] that is reachable over HTTP has a case, and every case names an existing spec | `TestEveryCriterionHasACase`, reading `specs/*.md` and the case table | not built |
| `TestContract` against `luxd` on the memory store with stubs is green in under five minutes | the integration tier | not built |
| `TestContract` against two `luxd` processes on one Postgres is green | the postgres tier | not built |
| Without `LUX_TEST_STUBS_URL` the suite is green and skips exactly the cases in the stub table, printing them | `TestSkipListIsExactlyTheStubCases` | not built |
| Without `LUX_TEST_URL` the suite skips with one line and exits zero | `TestContractSkipsWithoutAURL` | not built |
| Each mutation reddens exactly the cases its row names and no others | `TestSuiteCatchesADroppedCapability`, table-driven over the mutation table | not built |
| Every object the suite creates carries the run label and is gone after the run, and no object created before the run is touched | `TestSuiteCleansUpExactlyItsOwn` | not built |
| The previous release's manifests read back with an equal `spec` and its archived records decode with every field preserved | `case003PreviousReleaseManifests`, `case009PreviousReleaseRecords` | not built |
| Every group but `fixture` is green against a server that is not `luxd` and holds none of this repository's state | `TestExamplePlaneConforms` ([[020-building-a-plane]]) | not built |
| The suite mints its own Key through `/v1` before the door cases, deletes it at teardown, and skips the `doors` and `keys` groups naming `read_only` against a `file` mode server | `case007SuiteMintsItsKey`, `TestFileModeSkipList` | not built |
| Every response the suite receives validates against the server's own `GET /v1/openapi.json` | `case011OpenAPIValidatesEveryResponse` | not built |
| The suite runs against an external URL with the documented command, reading no file the caller wrote and no state the server did not serve; the only files it reads are the module's own embedded corpus and fixtures | `TestExternalInvocation` | not built |
| `LUX_TEST_URL` set and `LUX_TEST_TOKEN` unset leaves the suite green on the bearer-free cases and prints the groups it dropped | `TestSkipListWithoutAToken` | not built |
| Two concurrent runs against one server both pass | `TestConcurrentRuns` | not built |
