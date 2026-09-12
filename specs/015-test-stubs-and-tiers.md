---
title: "Test stubs and tiers: the stub providers, issuer, authorizer, and sink, make run, the tiers, CI jobs"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/005-providers.md
  - specs/006-identity.md
  - specs/012-request-log-and-events.md
affects: [cmd/lux-stubs/, test/stubs/, test/e2e/, deploy/examples/, Makefile, .github/workflows/verify.yml]
effort: medium
created: 2026-09-13
updated: 2026-09-13
author: changkun
---

# Test stubs and tiers

## Overview

Every endpoint `luxd` dials has a stub in the tree: a provider per
dialect, an OIDC issuer, an authorizer, and an event sink. They are
small, they are honest about the contract they implement, and they
record what they received, so a test asserts what left the gateway
rather than what the gateway says it sent. One binary, `lux-stubs`,
serves all of them, which is what lets `make run` give a clean clone a
working gateway in one command and what lets the release pipeline run
the conformance suite against a published image with one sidecar
([[017-release-and-installation]]).

The tiers are selected by build tag and test name prefix, never by a
wall-clock guess about what is installed. A test belongs to exactly one
tier by its name, so a new test lands in a tier by how it is called.

## Current state

Nothing is built. The scaffold's `verify.yml` runs the gate, the tidy
check, and the image build ([[002-repository-scaffold]]), and `make
run` starts a process that serves the probes and nothing else. The
hosted gateway this design is extracted from is tested against the real
providers with a recorded-cassette layer that goes stale whenever an
upstream changes a field, which is the cost this tree avoids by
implementing each dialect's wire shape once, in a stub it owns.

## Design

### The binary

`cmd/lux-stubs` is a `main` over the packages under `test/stubs/`. It
starts every stub on one loopback address each, prints one line per
stub with its URL, and exits on `SIGTERM`. It ships as the test image
of [[017-release-and-installation]] and is never part of an
installation; [[001-architecture]]'s binary table says so.

| Stub | Package | Serves |
|---|---|---|
| provider | `test/stubs/provider` | one instance per dialect: `openai`, `anthropic`, `gemini`, `lux` |
| issuer | `test/stubs/issuer` | OIDC discovery, a key set, and a minting route |
| authorizer | `test/stubs/authorizer` | the contract of [[006-identity]] |
| sink | `test/stubs/sink` | the contract of [[012-request-log-and-events]] |

There is no stub bucket: the archive tier runs against
`latere.ai/x/pkg/s3/s3test`, which is the same package the s3 client's
own tests use, so a bug in the stub cannot hide a bug in the client.

Every stub serves its control routes under a `_` prefix, which no
dialect route and no contract route uses, so a control route can never
shadow a served one.

### The stub provider

One handler, parameterised by dialect, serving that dialect's routes
from [[004-request-path]]'s table: the translated routes, the model
routes, the model list, and a catch-all that records an opaque request
and answers `200 {}`.

The answer is a function of the request, so a test asserts a value
rather than a shape. The assistant content is
`stub:<dialect>:<model>:<first 8 hex of the SHA-256 of the last user
text>`, the usage members of that dialect report `100` input and `20`
output tokens, and a streamed response is five content events followed
by that dialect's final usage event. `?tokens=<in>,<out>` and
`?events=<n>` on the Provider's `baseURL` change the two, which is how
a cost test fixes the numbers it is computing against.

The stub reads the credential from the header and scheme the dialect
defaults to ([[003-manifest-contract]]) and compares it with its
`-credential` flag: a mismatch is `401` and is recorded, so
`TestProviderCredentialNeverLeavesTheGateway` has a positive case as
well as a negative one.

Failure injection is by **upstream model name**, because that is the
one string the gateway rewrites onto the wire on every route and in
both directions ([[004-request-path]]), so one mechanism works on a
passthrough and a translation alike. A Model whose target names one of
these gets that behaviour:

| Upstream model name | The stub does |
|---|---|
| `fail-500` | `500` with that dialect's error body |
| `fail-429` | `429` with `Retry-After: 1` |
| `fail-401` | `401`, the shape a wrong credential produces |
| `slow-<duration>`, as `slow-5s` | waits the Go duration, then answers normally |
| `hang` | writes the status and headers and then nothing, until the caller cancels |
| `fail-stream-mid` | streams two content events, then closes the connection with no usage event |
| `fail-body` | `200` with a body that is not the dialect's shape |
| anything else | the deterministic answer above |

The header `Lux-Stub-Fail: <name>` does the same for a test driving the
stub directly, without a gateway in front.

`GET /_received` returns every request the instance received, in order:
method, path, query, every header, and the body. `DELETE /_received`
clears it. Those two are what `TestSameDialectSameBytes`,
`TestCallerCredentialsNeverForwarded`, and `TestModelNameRewrite`
([[004-request-path]]) read.

### The stub issuer

`GET /.well-known/openid-configuration` and `GET /jwks` over one
generated RS256 key, and `POST /mint` with `{"sub", "aud", "exp"}`
returning a signed token for any subject asked. Minting anything asked
for is what a stub issuer is for and is why it never runs in an
installation. `luxd` accepts it over `http://` only because the test
sets `LUX_OIDC_INSECURE_ISSUERS` ([[006-identity]]).

### The stub authorizer

Allow everything by default, so a tier that is not testing permission
writes no rules. `POST /_rules` replaces the rule list, each rule
`{subject, action, name, allow, reason, limits, filter}` with empty
fields matching anything and the first match winning; `DELETE /_rules`
empties it. `-deny <action>` is the same as one rule, for a test that
starts the binary rather than driving it.

`-fail-mode` produces each unavailability form [[006-identity]] names,
one per run: `timeout`, `malformed`, `status:<code>`, `no-allow`,
`refuse` (the listener closes the connection). The suite asserts every
one of them is `authorizer_unavailable` and none is an allow.

`GET /_calls` returns the number of requests and every request body
received, and `DELETE /_calls` resets the count.
`TestHotPathDialsNoWebhook` ([[001-architecture]]) is exactly a reset,
a thousand data plane requests, and a read of that number expecting
zero.

### The stub sink

The contract of [[012-request-log-and-events]]: it recomputes the
`Lux-Signature` over `"<t>.<body>"`, compares it in constant time,
refuses a body whose `t` is more than five minutes from its clock, and
stores what verified. `GET /_events` returns them in receipt order with
their delivery attempt counts; `DELETE /_events` clears. `-fail-first
<n>` answers `500` to the first `n` deliveries, which is how the
ordering and resume criteria of 012 are driven. A body that does not
verify is answered `400` and recorded apart at `GET /_events?invalid=1`,
so a signature test asserts a rejection rather than an absence.

### make run

`make run` builds `luxd` and `lux-stubs`, generates a `LUX_SECRETS_KEK`
under `out/` once and reuses it, starts the stubs on loopback ports
derived from the checkout directory's name so two checkouts do not
collide, and starts `luxd serve` with the memory store, the stub issuer
in `LUX_OIDC_ISSUERS` and `LUX_OIDC_INSECURE_ISSUERS`, the stub
authorizer, and the stub sink. It then mints a token for subject `dev`,
applies every manifest under `deploy/examples/` through `/v1`, and
prints

```
export LUX_URL=http://127.0.0.1:8080
export LUX_TOKEN=<a dev token>
export LUX_KEY=lux_...
```

with a line showing an `openai` SDK base URL and a `curl` against the
`/openai` door. The examples are one Provider per dialect pointed at
the matching stub, one Model per Provider, one Budget, and one Key that
names them, so the first request a clean clone sends reaches a provider
and produces a usage record and an event at the sink.

`make run-file` starts the same stubs with `LUX_MANIFEST_DIR` on the
same `deploy/examples/` directory and a generated `LUX_DEV_KEY` that
the example Key names through `spec.valueFrom.env` ([[010-state]]), so
the read-only mode is also one command. `make run-down` stops
everything either started.

### Tiers

| Tier | Tag | Prefix | Needs | Runs |
|---|---|---|---|---|
| unit | none | any | the Go toolchain | every push, inside the gate |
| integration | `integration` | `TestE2E` | the Go toolchain | every push, `make test-e2e` |
| postgres | `postgres` | `TestPostgres` | a Postgres | every push, against a service container |
| conformance | none | `TestContract` | a server URL | against the integration tier's server, and against the published images in the release pipeline ([[018-conformance-suite]]) |

The integration tier lives in `test/e2e` and starts `luxd` and
`lux-stubs` as processes on loopback, so it exercises the real HTTP
clients, the real listeners, and the real shutdown path rather than a
handler under `httptest`. Every test binds port 0 and reads the address
back, so the tier is parallel-safe and needs no fixed port.

The postgres tier is the same tree of cases with `LUX_DB_URL` set,
plus the cases that only exist with a shared store: two `luxd`
processes against one database, a lease held by one of them, a spend
counter that both see within one flush, a Key cache invalidated through
the journal, and a restart that resumes an unacknowledged event
([[010-state]], [[012-request-log-and-events]]). It is a tag rather
than an environment check because a tier that silently skips is a tier
nobody notices is not running.

### CI

`verify.yml` keeps its `gate`, `tidy`, and `image` jobs and gains two,
each on hosted runners, each with every third-party action pinned by
commit with its version in a comment, as the file already does.

| Job | Runs | Does |
|---|---|---|
| `e2e` | every push and pull request | `go test -tags=integration -run '^TestE2E' -v ./test/...` |
| `postgres` | every push and pull request | the same with `-tags=postgres -run '^TestPostgres'`, with `LUX_DB_URL` pointed at a service container |

```yaml
  postgres:
    name: the postgres tier
    if: "!startsWith(github.ref, 'refs/tags/')"
    runs-on: ubuntu-latest
    timeout-minutes: 20
    services:
      postgres:
        # postgres:17-alpine, pinned by digest as every image reference is
        image: postgres@sha256:<digest>
        env:
          POSTGRES_PASSWORD: lux
        options: >-
          --health-cmd pg_isready --health-interval 5s
          --health-timeout 5s --health-retries 10
        ports: ['5432:5432']
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version-file: go.mod
      - run: go test -tags=postgres -run '^TestPostgres' -v ./test/...
        env:
          LUX_DB_URL: postgres://postgres:lux@127.0.0.1:5432/postgres?sslmode=disable
```

Both jobs pass `-v`, so a log names the tests that ran and a tier that
silently matched nothing is visible as an empty list rather than a
green check.

## Not in this spec

The conformance suite the stubs are wired into
([[018-conformance-suite]]); the release pipeline and the test image
([[017-release-and-installation]]); the s3 archive's own assertions,
which run against `s3test` ([[012-request-log-and-events]]); what each
stub's contract is, which is its owning spec's.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each stub package's own test drives every route and every behaviour flag it offers | one test package per stub, `TestProviderStub`, `TestIssuerStub`, `TestAuthorizerStub`, `TestSinkStub` | not built |
| The stub provider answers deterministically: one request twice yields byte-identical bodies, and the content names the dialect, the model, and the digest of the last user text | `TestProviderStubIsDeterministic` | not built |
| Every row of the failure injection table produces its behaviour on each of the four dialects, by upstream model name and by header | `TestFailureInjection`, table-driven over rows and dialects | not built |
| The stub provider refuses a request whose credential is not the configured one and records it | `TestProviderStubChecksTheCredential` | not built |
| `GET /_received` returns every request in order with its headers and body, and `DELETE /_received` clears it | `TestReceivedRecording` | not built |
| The stub issuer's discovery document and key set verify a minted token through the same verifier `luxd` uses | `TestIssuerStubMintsVerifiableTokens` | not built |
| Each `-fail-mode` of the stub authorizer is `authorizer_unavailable` at the gateway and none is an allow | `TestE2EAuthorizerUnavailability` with [[006-identity]]'s `TestAuthorizerUnavailability` | not built |
| The stub sink refuses a body whose signature does not verify and records it apart | `TestSinkVerifiesSignature` | not built |
| `make run` on a clean clone prints the three exports, and a request through the `/openai` door with the printed Key returns a stub answer, produces one usage record, and delivers one event to the sink | `TestE2EMakeRun` | not built |
| `make run-file` serves the same door from the manifest directory and refuses a `PUT` with `read_only` | `TestE2EMakeRunFileMode` | not built |
| The integration tier starts `luxd` as a process on port 0 and covers every door, the four kinds' grammar, and a clean shutdown | `TestE2ELifecycle` | not built |
| The postgres tier runs two `luxd` processes against one database and proves the shared lease, the shared spend counter, the journal-driven cache invalidation, and the resumed event | `TestPostgresTwoReplicas` | not built |
| Every test in `test/` matches exactly one tier's prefix, and a test outside the prefixes fails the check | `TestEveryTestIsInATier` | not built |
| The `e2e` and `postgres` jobs pass on the first push to `main`, with every action pinned by commit | the `verify` workflow run | not built |
