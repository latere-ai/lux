---
title: "Conformance suite: the contract, the doors, and the API as executable tests, against any server"
status: complete
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

Built: `test/conformance` is the package below, with `Run`, `Config`,
`TestContract`, fifty-eight cases in seven groups and the Key mint before the doors, the OpenAPI validation of
every `/v1` answer, the previous-release fixture group, and the
mutation check over eight shims in `internal/serve`. Its own tests
assemble `luxd`'s serve role in process from the packages `cmd/luxd`
wires, with the stub issuer and authorizer of `latere.ai/x/pkg` and
stub providers speaking the contract of [[015-test-stubs-and-tiers]],
and prove the suite green against it, red against a server that
answers everything with a plausible success, and specific under each
mutation. The stubs binary and the tiers that start `luxd` as a
process are [[015-test-stubs-and-tiers]]'s and are not built; the
fixture group skips until [[017-release-and-installation]] writes the
first release's directory. Against `luxd` as this tree wires it the
suite finds one drift, named in the Outcome, which [[011-api]] owns.

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
	// Token returns an issuer token for a rendered subject, and false
	// when this deployment cannot mint one; a case that needs a subject
	// it cannot get skips with the subject named. A caller that can mint
	// returns a token per subject; a caller holding one static token
	// returns it for Subject and false for every other name. Nil is a
	// run without a bearer, which keeps the cases that need none.
	Token func(subject string) (string, bool)
	// Subject is who Token's default token speaks for, the value the
	// owner assertions expect.
	Subject string
	// StubsURL is the address of a lux-stubs instance the server's
	// Providers can reach, whose GET / names each stub's URL. Empty
	// skips the cases in the stub table below and prints the list.
	StubsURL string
	// InternalURL is the server's internal listener, read in the file
	// mode alone, where /v1 is mounted there and not under URL.
	InternalURL string
}
```

`Run` fetches `GET /.well-known/lux` ([[011-api]]) once before the
first case and reads the served `doors`, `dialects`, `mode`, `api`,
and `apiVersion` from it, so the suite adapts to a server that serves
three doors instead of four and refuses to guess at anything else. It
is not a `Config` field: a caller that could tell the suite what the
server serves could tell it wrong, and the document exists so nobody
has to. It then fetches `GET /v1/openapi.json` and holds every `/v1`
answer of the run to it, below.

`TestContract` in `test/conformance` is the entry point. It builds
`Config` from the environment and calls `Run`:

| Variable | Required | Means |
|---|---|---|
| `LUX_TEST_URL` | yes | the server under test; unset skips the whole suite with one line saying so |
| `LUX_TEST_TOKEN` | with `LUX_TEST_URL` | an issuer token the server accepts on `/v1`; unset leaves only the cases that need no bearer, and the suite prints which groups it dropped |
| `LUX_TEST_SUBJECT` | no | the rendered subject `LUX_TEST_TOKEN` speaks for, `issuer` and `sub` joined by a bar ([[006-identity]]); unset is read from the token's own `iss` and `sub` through the shared reader `authkit/jwt.DecodePayload`, which is the one place a token is taken apart |
| `LUX_TEST_ISSUER_URL` | no | a `latere.ai/x/pkg/authkit/issuertest` the server lists; set, `Token` mints per subject of that issuer through its `POST /mint`, with the audience `LUX_TEST_TOKEN` carries, and the multi-subject cases run; unset, `Token` answers `LUX_TEST_SUBJECT` alone and they skip by name |
| `LUX_TEST_STUBS_URL` | no | a `lux-stubs` instance's document, below; unset skips the stub cases and prints the list |
| `LUX_TEST_INTERNAL_URL` | in the file mode | the internal listener, where a file mode server mounts `/v1` ([[011-api]]); unset there drops the api group naming the variable |

These six are the suite's and are read by no server;
[[002-repository-scaffold]]'s table says so and this is their table.
The sixth exists because a file mode server's `/.well-known/lux` names
an `api` under the public address that answers `not_found` there, so a
suite with the public URL alone cannot reach the read half it is meant
to run.

A `file` mode server runs the read half of the api group on the
internal listener and skips the write half naming `read_only`; the
manifest, doors, keys, identity, and usage groups are dropped whole,
each naming `read_only`, because every one of them writes desired state
or drives a door with a Key the suite minted.

### The stubs document

`LUX_TEST_STUBS_URL` names one address, and the binary of
[[015-test-stubs-and-tiers]] gives every stub a listener of its own, so
the suite reads one document at `GET {LUX_TEST_STUBS_URL}/` and never
guesses a port:

```json
{
  "providers": {"openai": "http://127.0.0.1:41001", "anthropic": "http://127.0.0.1:41002",
                "gemini": "http://127.0.0.1:41003", "lux": "http://127.0.0.1:41004"},
  "authorizer": "http://127.0.0.1:41005",
  "issuer": "http://127.0.0.1:41006",
  "sink": "",
  "credential": "the value every stub provider checks its dialect's credential header for"
}
```

`providers` names one stub per dialect the server lists, and a document
missing one fails the run; `authorizer` is the stub authorizer whose
`PUT /fail` the identity group drives; `credential` is what the suite
writes into the Providers it applies, so a request the gateway forwards
without the Provider's credential is a 401 the stub records. A set
address that does not answer, or answers something else, fails the run
before any case: a partial run must never look like a full one. This
document is what `lux-stubs` has to serve, and that binary is
[[015-test-stubs-and-tiers]]'s.

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

The Key is the suite's own. Before the `doors` group runs, the suite
applies a `Key` named `conf-<run>-key` through `PUT /v1/keys/{name}`
with the one selector `conf-<run>-*`, which every Model the suite
applies matches, reads `status.value` from the one response that
carries it ([[007-keys-and-limits]]), and deletes it at teardown; that
is `case007SuiteMintsItsKey`, the first subtest of the doors group. It
then applies its Providers and Models, below. A Key that could not be
minted skips every door and key case naming why, and a `file` mode
server skips them naming `read_only`: nothing can mint a Key there
([[010-state]]).

Cases are functions named `case<NNN><Name>(t testing.TB, c *client)`
after the spec whose criterion they prove, registered in one table with
their group and whether they need a bearer, the Key, the stubs, or one
mode. A failing case therefore names its spec in its own name, which is
what makes a red suite a bug report rather than a search. The parameter
is `testing.TB` rather than `*testing.T` so the package's own tests can
hand a case a recording fake and prove it fails when it should.

### Groups

| Group | Proves | From | Stubs |
|---|---|---|---|
| `manifest` | each kind's example applies, resolves, and reads back with a spec byte-identical to what `Resolve` produces; every default is visible in the read-back; every refusal code of the decode and resolve tables is reachable through the API with its path | [[003-manifest-contract]] | no |
| `api` | the four routes per kind; apply as create then update; the preconditions and `conflict`; percent-encoded and two-segment Model names; rotate; `budget_in_use`; pagination and the list filters; the error envelope and `request_id`; the rate limit headers; `read_only` in file mode; a body of spec alone, YAML and JSON alike; every response validating against `GET /v1/openapi.json` | [[011-api]] | no |
| `doors` | every row of the door route table through its door; same-dialect same-bytes; a translated request's `Lux-Loss`; the error envelope in each dialect's own shape; a streamed response's event sequence and final usage; `GET /v1/models` as the Key's view; every credential form; the count emulation | [[004-request-path]], [[008-routing-and-models]] | some |
| `keys` | rotate invalidates the old value; each Key state refuses with its code; a rate refusal carries `Retry-After`; a spend refusal and a hard Budget refusal carry theirs; an unpriced Model under a Budget is `model_unpriced`; a supplied value opens a door by its exact bytes | [[007-keys-and-limits]] | some |
| `identity` | a Key on `/v1` is `unauthenticated`, and so is an issuer token on a door; an unauthenticated control plane request is 401; an object's owner is the token's subject; with the authorizer down every control plane decision is `authorizer_unavailable` and every data plane request with a valid Key is served | [[006-identity]] | some |
| `usage` | one request through a door produces exactly one record at `GET /v1/requests` with the tokens, the cost, the model, the provider, and no content; `GET /v1/usage` aggregates it under every dimension and window the parameters admit; a refused request has a record too | [[009-usage-and-metering]] | some |
| `fixture` | the previous release's manifests and records still read back, below | [[003-manifest-contract]], [[009-usage-and-metering]] | no |

The cases, by group:

| Group | Cases |
|---|---|
| `manifest` | `case003AcceptedCorpus`, `case003DefaultsAreVisible`, `case003RefusedCorpus`, `case003UnknownField`, `case003AcceptedCorpusDeletes` |
| `api` | `case011WellKnown`, `case011NoCORS`, `case011ListReads`, `case011Self`, `case011Envelope`, `case011RequestIdOnEveryResponse`, `case011ReadOnlyInFileMode`, `case011GrammarPerKind`, `case011ApplyWithoutTheEnvelope`, `case011BodiesAndTypes`, `case011ApplyIsCreateThenUpdate`, `case011Preconditions`, `case011AddressByIdOrName`, `case011ModelNamesWithSlashes`, `case011Rotate`, `case011BudgetInUse`, `case011Pagination`, `case011RateLimitHeaders`, `case011SecretsNeverInResponses`, `case011OpenAPIValidatesEveryResponse` |
| `doors` | `case007SuiteMintsItsKey`, `case004RouteTable`, `case004ErrorEnvelopePerDialect`, `case004ModelsListIsTheKeysView`, `case004DialectBridging`, `case004CredentialForms`, `case004CountTokens`, `case004SameDialectSameBytes`, `case004CallerCredentialsNeverForwarded`, `case004TranslationLoss`, `case004Streaming`, `case004ModelNameRewrite`, `case004UpstreamError`, `case004UpstreamTimeout`, `case008Fallback`, `case004OpaqueRoute` |
| `keys` | `case007RotateInvalidatesTheOldValue`, `case007KeyStates`, `case007RateLimited`, `case007UnpricedUnderABudget`, `case007SuppliedValue`, `case007HashSuppliedValue`, `case007SpendWindow`, `case007BudgetExhausted` |
| `identity` | `case006UnauthenticatedControlPlane`, `case006KeyOnControlPlaneIsUnauthenticated`, `case006TokenOnDoorIsUnauthenticated`, `case006OwnerIsTheTokensSubject`, `case006AuthorizerUnavailable` |
| `usage` | `case009RefusedRequestHasARecord`, `case009RecordHasTheStubsTokens`, `case009UsageAggregates` |
| `fixture` | `case003PreviousReleaseManifests`, `case009PreviousReleaseRecords` |

`TestEveryCriterionHasACase` holds the table to the acceptance tables
of [[003-manifest-contract]], [[004-request-path]], and [[011-api]]:
every test a row of those names is either covered by a case here or
stated unreachable over HTTP with why, in two tables the test reads, so
a row of those specs is a case or a reason and never an omission.

The `manifest` group's inputs are [[003-manifest-contract]]'s golden
corpus and not a second set written here, so one corpus proves `Resolve`
in process and over HTTP and the two cannot drift. That corpus is
`manifest.Corpus`, the `fs.FS` the `manifest` package exports over
`manifest/testdata/v1/` ([[003-manifest-contract]]), so the group reads
it through the import from any module and never by a relative path.
The suite applies the accepted cases in kind order, Provider, Budget,
Model, Key, so every reference resolves; the nameless Key case is `PUT`
to the corpus's `NewName`, `fixed-name-0000`, under the run's prefix;
and the `platform-dev` Key carries a supplied value, so its response
carries no `status.value`.

Every name in the corpus is applied under the run's prefix,
`conf-<run>-`, on the manifest and on the golden alike: `metadata.name`,
a Model's `targets[].provider`, a Key's `budget` and every selector but
`*`, a target's `model` when it equals the Model's own name, since that
equality is what such a case says, and a supplied `spec.value` of
valid length, since the hash index keeps one across the installation.
The read-back's `metadata` and `spec` are then compared with the
renamed golden byte for byte. The prefix is not a convenience: a suite
that applied a Provider named `openai` against a serving installation
would replace the operator's own with the corpus's and delete it at
teardown, and two runs against one server would meet in one object.

The refused half is sent through the route of the case's kind, the
body renamed the same way where it parses as one YAML mapping, and as
the file is for the parser's refusals, `malformed_body` and
`multi_document`, and for a document with an alias, whose expansion is
the case. Five cases the API cannot reach as their code skip by name
with the reason: the two `missing_field` cases that omit `apiVersion`
or `kind`, which the route supplies ([[003-manifest-contract]]'s
hint); the four `reserved_prefix` name cases, whose id-shaped path
segment is `invalid_field` at the route before the manifest is decoded
([[011-api]]); and the Model name with a repeated slash, a path that
is not clean, which is answered before the manifest is decoded. The
own-host case names the server's host in place of the corpus's
`lux.example.com`, since the loop is this server's. A server that
admits private upstreams answers the private, loopback, link-local,
plaintext, single-label, `.local`, and `.internal` host cases with a
create and a warning, which the rule allows, and the suite reads the
warning as the server's answer and deletes what it created. A tunnelled
Provider the server refuses at `spec.tunnel`, and the Models that
target it, skip by name: the tunnel is [[013-tunnelled-runtimes]]'s and
not built.

Every case cleans up what it created, and the suite runs in under five
minutes against a local `luxd`, which is a five minute deadline on the
context every request carries rather than a hope.

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
| `case004Streaming` | the stub's fixed event count, on a passthrough and on a translation |
| `case004ModelNameRewrite` | the upstream name the stub received |
| `case004UpstreamError`, `case004UpstreamTimeout` | the `fail-500`, `fail-400`, and `hang` upstream model names |
| `case008Fallback` | two targets, the first of them `fail-500` |
| `case004OpaqueRoute` | the stub's catch-all, which answers an opaque route whole |
| `case007SpendWindow`, `case007BudgetExhausted` | the fixed token counts a stub answers with, so a cost is a known number |
| `case006AuthorizerUnavailable` | the stub authorizer's `PUT /fail` |
| `case009RecordHasTheStubsTokens` | the fixed token counts, so a record's numbers are a known value |

A case in the table asserts against what the provider received, not
against what the gateway reports, which is the whole reason the doors
and keys groups cannot run against an arbitrary front with real
upstreams behind it. The event sink case the first draft listed here
is [[012-request-log-and-events]]'s and is not in the table, since
neither the sink nor its stub exists; that spec adds it with them.

The cases outside the table drive a door without any provider being
reached: `case004RouteTable` proves every row dispatches to its class
and reads the model from where the table says by the refusal each row
answers a Model no Key has, `model_not_found`, `route_not_allowed`
without `passthrough`, or `not_found` off the table, and a body with no
model or not an object by `invalid_request`; `case007RateLimited`'s
first request is admitted at the windows and refused at the codec
before the forward, so the second is `rate_limited`; and every Provider
the suite applies without stubs names a host under `example.com` with
discovery and health off, which nothing dials.

### Isolation

Every object the suite creates is named `conf-<run>-<case>` and carries
the label `conformance=<run>`, the run being one lower-case ULID.
Teardown deletes by that label and nothing else, Keys first and
Providers last, so the suite never touches an object it did not create
and two runs against one server do not collide. A server that refuses a
create the suite needs fails the case with the refusal, which is the
honest answer for a front that admits less than the contract. Two
concurrent runs share one thing they cannot: the authorizer outage of
`case006AuthorizerUnavailable` takes the stub authorizer down for every
caller, so `TestConcurrentRuns` runs its two without stubs.

A door's model list carries a Model once the server's health tick has
filled its availability ([[005-providers]]), which is `LUX_HEALTH_INTERVAL`
away on a server that has just applied it, so
`case004ModelsListIsTheKeysView` waits for the list rather than reading
it once.

### The previous-release fixture

`test/conformance/testdata/previous/<version>/` holds what the last
tagged release produced: one resolved manifest per kind as
`provider.json`, `budget.json`, `model.json`, and `key.json`, and a
bucket of archived records as `records.ndjson`
([[012-request-log-and-events]]). The fixture group applies each
manifest under the run's prefix and asserts the read-back's `spec`
equals the fixture's, and decodes each record with the current
`metering.Record` and asserts every member the fixture carries is still
present with the same value after a round trip.

This is the schema evolution promise of [[003-manifest-contract]] and
the record additivity promise of [[009-usage-and-metering]] executed
rather than asserted: a field that changed type, a default that
changed, or a record member that was removed fails here and nowhere
else. The release pipeline writes the next fixture directory at each
tag, so the set grows by one per release and old ones are kept
([[017-release-and-installation]]). Until the first tag the directory
holds a placeholder alone and the group skips saying so;
`TestFixtureGroupReadsAPreviousRelease` drives it over a synthetic
release built from the golden corpus and proves it passes a release
that reads back and fails one whose record lost a member.

### The authorizer's own conformance

This suite proves what `luxd` does with a decision. It does not prove
that an operator's authorizer decides correctly, and it cannot: the
authorizer is the operator's program and its answers are the
operator's policy. The test an authorizer passes belongs with the
contract it implements, `latere.ai/x/pkg/authz`, which
[[006-identity]] names as its home. That package ships the envelope,
the client, the cache, the owner policy, the stub, and
`authz/conformance`, the test every authorizer passes, and this suite
does not repeat it; an operator's check that its endpoint is wired
correctly is `luxd check`'s authorizer row
([[017-release-and-installation]]) and the `identity` group here.

### The mutation check

A suite that never fails proves nothing. `TestSuiteCatchesADroppedCapability`
builds the reference server's run with one capability removed, runs the
suite against it, and asserts that exactly the cases in the row fail
and no others. Each mutation is a build tag over one shim in
`internal/serve`, compiled only under that tag, so no mutation is in a
released binary.

| Mutation | Tag | Must fail, and only |
|---|---|---|
| `If-Match` is ignored | `mut_ifmatch` | `case011Preconditions` |
| one default is not in the answer: a Model's `fallback` | `mut_default` | `case003DefaultsAreVisible` |
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

The shims are three seams in `internal/serve`, `Mutate` over the
server's whole handler, `MutateLimiter` over the doors' Limiter, and
`MutateClients` over the upstream clients, each the identity in every
build; the file of a tag sets one of the three hooks in its `init`.
`mut_ifmatch` strips the header on the way in; `mut_default` and
`mut_keyvalue` rewrite a `/v1` answer's body; `mut_loss` drops the
header as the answer is committed; `mut_unknownfield` decodes a `PUT`
body as the API would and deletes every path an `unknown_field`
refusal names before the API sees it; `mut_unpriced` and
`mut_retryafter` wrap the Limiter's refusal; `mut_forwardcred` keeps
the caller's header on the request's context and writes it on the
outbound request. The reference server of the package's own tests
passes its handler, Limiter, and clients through the three seams and
`luxd` calls none of them, so the check runs where the shims can act
and a released binary carries no hook that does anything.

Each mutation is built by `go test -tags=<tag> -json -run
^TestReferenceServerConforms$` from the test itself, whose events name
the failing cases, so the shims are compiled nowhere else. The test
therefore needs the Go toolchain on `PATH`, which is what the gate
already guarantees ([[015-test-stubs-and-tiers]]).

### The reference server

The package's own tests drive `luxd`'s serve role assembled in process:
the memory store, `auth.New` over the stub issuer and the stub
authorizer, `serve.NewKeyCache`, `NewLimiter`, `NewRecorder`,
`NewHealth`, `gateway.NewClientSource`, `gateway.New`, and `api.New`,
mounted as `cmd/luxd` mounts them behind `serve.LimitUnauthenticated`,
in the order `serveCmd` has them. `cmd/luxd`'s `run` is package main's
and cannot be imported, so the wiring is repeated; a drift between the
two is what the reference run and `cmd/luxd`'s own tests would show
from either side. Its upstream clients resolve no name and dial
loopback alone, so a corpus Provider under `example.com` fails before
any network, and it names itself `localhost` rather than by its
address, because the loop check of [[003-manifest-contract]] compares a
Provider's host with the public URL's host alone and every stub listens
on the loopback address the listener does.

`TestReferenceServerConforms` runs the whole suite with stubs against
it and holds the run to no dropped group, the three skips a server mode
run has, and no failure but the OpenAPI drifts a ledger names by
finding, each with the spec that owes the fix; a ledger entry that
stops appearing fails the test, so the ledger shrinks with the fixes and
never hides a finding that came back. `Run` tolerates nothing: the
ledger is an unexported option of the package's own tests.
`TestSuiteIsRedAgainstAHollowServer` runs every case against a server
that answers every request with a plausible success and holds the suite
to failing all but the three cases whose every assertion is a success,
which is the other half of the mutation check.

### Where it runs

| Runner | Server | Stubs |
|---|---|---|
| the package's own tests | `luxd`'s serve role in process, above | in process |
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

The conformance test an authorizer passes, which is
`latere.ai/x/pkg/authz/conformance`; the stubs the doors and keys
groups need, the document they serve, and the tiers that start them
([[015-test-stubs-and-tiers]]); the release pipeline that runs the
suite against the published images and writes the next fixture
([[017-release-and-installation]]); the criteria themselves, which
belong to the specs the cases are named after; the `lux` command,
whose own scenario is [[014-agent-client]]'s.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every acceptance criterion of [[003-manifest-contract]], [[004-request-path]], and [[011-api]] that is reachable over HTTP has a case, and every case names an existing spec | `TestEveryCriterionHasACase`, reading `specs/*.md` and the case table | passing: every test the three tables name is a case or a stated reason |
| `TestContract` against `luxd` on the memory store with stubs is green in under five minutes | the integration tier | not built, [[015-test-stubs-and-tiers]]'s; `TestReferenceServerConforms` runs the same suite against `luxd`'s wiring in process in forty seconds, green but for the drift the ledger names |
| `TestContract` against two `luxd` processes on one Postgres is green | the postgres tier | not built, [[015-test-stubs-and-tiers]]'s |
| Without `LUX_TEST_STUBS_URL` the suite is green and skips exactly the cases in the stub table, printing them | `TestSkipListIsExactlyTheStubCases` | passing |
| Without `LUX_TEST_URL` the suite skips with one line and exits zero | `TestContractSkipsWithoutAURL` | passing |
| Each mutation reddens exactly the cases its row names and no others | `TestSuiteCatchesADroppedCapability`, table-driven over the mutation table | passing, eight rows |
| Every object the suite creates carries the run label and is gone after the run, and no object created before the run is touched | `TestSuiteCleansUpExactlyItsOwn` | passing |
| The previous release's manifests read back with an equal `spec` and its archived records decode with every field preserved | `case003PreviousReleaseManifests`, `case009PreviousReleaseRecords` | passing over a synthetic release in `TestFixtureGroupReadsAPreviousRelease`; the two cases skip in a run until [[017-release-and-installation]] writes the first directory |
| Every group but `fixture` is green against a server that is not `luxd` and holds none of this repository's state | `TestExamplePlaneConforms` ([[020-building-a-plane]]) | not built, [[020-building-a-plane]]'s |
| The suite mints its own Key through `/v1` before the door cases, deletes it at teardown, and skips the `doors` and `keys` groups naming `read_only` against a `file` mode server | `case007SuiteMintsItsKey`, `TestFileModeSkipList` | passing |
| Every response the suite receives validates against the server's own `GET /v1/openapi.json` | `case011OpenAPIValidatesEveryResponse` | passing as a check on every `/v1` answer; against `luxd` it finds the one drift the Outcome names, owed by [[011-api]] |
| The suite runs against an external URL with the documented command, reading no file the caller wrote and no state the server did not serve; the only files it reads are the module's own embedded corpus and fixtures | `TestExternalInvocation` | passing: the command runs every group, and the package's source imports nothing that reads a file |
| `LUX_TEST_URL` set and `LUX_TEST_TOKEN` unset leaves the suite green on the bearer-free cases and prints the groups it dropped | `TestSkipListWithoutAToken` | passing |
| Two concurrent runs against one server both pass | `TestConcurrentRuns` | passing |

## Outcome

Built on 2026-09-14 in five commits on a branch: the mutation seams and
the eight shims in `internal/serve`, then `test/conformance` in four,
the package, its reference server and acceptance tests, and the lint
and identity gates' findings. It is proven by the whole gate and by
coverage under the race detector of 91.7% for `test/conformance` and
97.1% for `internal/serve`; the package runs in forty seconds and
starts no process but the mutation check's `go test`.

What was built: `Run`, `Config`, and `TestContract` over the six
variables of the table; fifty-seven cases in seven groups and the Key
mint, each named after the spec whose criterion it proves; the
validation of every `/v1` answer against the served OpenAPI document;
the run prefix and label with teardown by label; the stubs document
reader; the fixture group over an embedded release directory; the
recording `testing.TB` the package's own tests hand a case; the
reference server assembled in process from the packages `cmd/luxd`
wires, with the stub issuer and authorizer of `latere.ai/x/pkg` and a
stub provider per dialect speaking [[015-test-stubs-and-tiers]]'s
contract; the hollow server every case is red against; and the
mutation check over the eight tags. The Design above was rewritten to
what was built; what diverged from the text as dispatched:

- `LUX_TEST_STUBS_URL` names one document, `GET /` with the URL of each
  stub and the credential they check, rather than one stub; the binary
  of [[015-test-stubs-and-tiers]] has a listener per stub and the suite
  could not guess seven ports from one address.
- `LUX_TEST_INTERNAL_URL` and `Config.InternalURL` exist, because a
  `file` mode server mounts `/v1` on the internal listener and its
  `/.well-known/lux` names an `api` under the public address that
  answers `not_found`; [[002-repository-scaffold]]'s table of the
  suite's variables gains the row.
- Every corpus name and every reference to one is prefixed with the
  run, the golden alike, including a target's `model` equal to its
  Model's name and a supplied `spec.value` of valid length; without the
  prefix the suite would replace an operator's `openai` Provider and
  two runs would collide on the value's hash index.
- Seven refused cases are unreachable as their code over HTTP and skip
  by name: `missing_field` without `apiVersion` or `kind`, which the
  route supplies; the four `reserved_prefix` names, `invalid_field` at
  the route's id-shaped path segment; and the Model name with a
  repeated slash, answered by a redirect before `/v1` is reached. The
  redirect is `cmd/luxd`'s outer mux and not the table of [[011-api]],
  which says any path outside it is `not_found`; that spec owns the
  answer.
- A tunnelled Provider and the Models over it skip, since `spec.tunnel`
  is refused until [[013-tunnelled-runtimes]] enables it.
- The mutation check is `go test -tags=<tag> -json` over the reference
  server's own test rather than a build of `luxd` with one capability
  removed, and the shims act through three seams `luxd` never calls;
  `cmd/luxd`'s `run` is package main's and cannot be imported, so the
  reference server repeats the wiring.
- `case004RouteTable` proves dispatch by each row's refusal without a
  Key that can see the Model, and `case007RateLimited`'s first request
  is refused at the codec after the windows admitted it, so neither
  needs a stub.
- `Run` tolerates no drift; the package's own tests carry an
  unexported ledger of the OpenAPI findings against `luxd`, each with
  the owner, and fail when a finding stops appearing. It holds one
  entry: `GET /v1/requests` serves `targetDialect: ""` on a refused
  record, as [[009-usage-and-metering]]'s table says, and the served
  enum lacks the empty value; [[011-api]]'s generator owns it.
- A case's signature is `testing.TB`, so `TestSuiteIsRedAgainstAHollowServer`
  and the mutation table can hold a case to failing.
- `TestConcurrentRuns` runs without stubs, since the authorizer outage
  of `case006AuthorizerUnavailable` is one outage for every caller.
- The event sink case left the stub table: the sink and its stub are
  [[012-request-log-and-events]]'s and neither exists.

What the neighbouring specs own from here. [[015-test-stubs-and-tiers]]
serves the stubs document, starts `luxd` for the integration and
postgres rows above, and corrects its `lux` dialect stub's usage
members, which the gateway and `llmdialect` read as `input_tokens` and
`output_tokens` and its table names `usage.input` and `usage.output`;
its `make run` on a loopback public URL will also meet the loop check
of [[003-manifest-contract]], which compares a Provider's host with
the public URL's host and not its port, so every loopback stub reads
as this gateway's own host until the check compares the authority.
[[011-api]] owns the `targetDialect` enum and the redirect. [[002-repository-scaffold]]
lists `LUX_TEST_INTERNAL_URL`. [[017-release-and-installation]] writes
the first fixture directory and runs the suite against the images.
[[020-building-a-plane]] runs it against the example plane. A door's
model list carries a Model only after the health tick of
[[005-providers]] has run, so a caller reading the list right after an
apply waits `LUX_HEALTH_INTERVAL`; the suite does, and that spec may
want to say so.
