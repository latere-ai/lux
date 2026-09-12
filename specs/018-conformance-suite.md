---
title: "Conformance suite: the contract, the doors, and the API as executable tests, against any server"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/004-request-path.md
  - specs/011-api.md
affects: [test/conformance/, test/e2e/, .github/workflows/verify.yml, docs/]
effort: large
created: 2026-09-13
updated: 2026-09-13
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

Nothing is built. The hosted gateway this design is extracted from has
integration tests bound to its own process, its own database fixtures,
and its own identity, so none of them can be pointed at another server
and none of them states a contract a third party could implement.

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
	// cannot get skips with the subject named.
	Token func(subject string) (string, bool)
	// StubsURL is a lux-stubs instance the server's Providers point
	// at. Empty skips the cases in the stub table below.
	StubsURL string
	// Server is GET /.well-known/lux, fetched once; the suite reads
	// the dialects served and the mode from it rather than being told.
	Server ServerInfo
}
```

`TestContract` in `test/conformance` is the entry point. It builds
`Config` from the environment and calls `Run`:

| Variable | Required | Means |
|---|---|---|
| `LUX_TEST_URL` | yes | the server under test; unset skips the whole suite with one line saying so |
| `LUX_TEST_TOKEN` | yes | an issuer token the server accepts; `Token` returns it for every subject and reports false for none, so a case that needs two distinct subjects skips |
| `LUX_TEST_STUBS_URL` | no | a `lux-stubs` instance; unset skips the stub cases and prints the list |

The suite fetches `GET /.well-known/lux` first ([[011-api]]) and reads
the served dialects and the mode from it, so it adapts to a server that
serves three doors instead of four and refuses to guess at anything
else. A `file` mode server runs the read half of the api group and
skips the write half, which the group's own table says.

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
| `fixture` | the previous release's manifests and records still read back, below | [[003-manifest-contract]], [[009-usage-and-metering]] | no |

Every case cleans up what it created, and the suite runs in under five
minutes against a local `luxd`.

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

### Where it runs

| Runner | Server | Stubs |
|---|---|---|
| the integration tier | `luxd` on loopback with the memory store | yes |
| the postgres tier | two `luxd` processes against one database | yes |
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

The stubs the doors and keys groups need and the tiers that start them
([[015-test-stubs-and-tiers]]); the release pipeline that runs the
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
| The previous release's manifests read back with an equal `spec` and its archived records decode with every field preserved | `case018PreviousReleaseManifests`, `case018PreviousReleaseRecords` | not built |
| Every response the suite receives validates against the server's own `GET /v1/openapi.json` | `case011OpenAPIValidatesEveryResponse` | not built |
| The suite runs against an external URL from a clean checkout with the documented command and no repository-local state | `TestExternalInvocation` | not built |
| Two concurrent runs against one server both pass | `TestConcurrentRuns` | not built |
