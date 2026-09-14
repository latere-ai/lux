---
title: "Test stubs and tiers: the stub providers, issuer, authorizer, and sink, make run, the tiers, CI jobs"
status: testing
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/005-providers.md
  - specs/006-identity.md
  - specs/012-request-log-and-events.md
affects: [cmd/lux-stubs/, test/stubs/, test/e2e/, deploy/examples/, Makefile, .lateregate.yaml, .github/workflows/verify.yml]
effort: medium
created: 2026-09-13
updated: 2026-09-14
author: changkun
---

# Test stubs and tiers

## Overview

Every endpoint `luxd` dials has a stub behind it: a provider per
dialect, an OIDC issuer, an authorizer, and an event sink. They are
small, they are honest about the contract they implement, and they
record what they received, so a test asserts what left the gateway
rather than what the gateway says it sent. Two of the four already
exist and are not rewritten here: the issuer is
`latere.ai/x/pkg/authkit/issuertest` and the authorizer is
`latere.ai/x/pkg/authz/stub`, the stubs of the contracts
[[006-identity]] adopts. This spec mounts those two, writes the
provider and the sink, and adds Lux's own vocabulary around all four.
One binary, `lux-stubs`, serves them, which is what lets `make run`
give a clean clone a working gateway in one command and what lets the
release pipeline run the conformance suite against a published image
with one sidecar ([[017-release-and-installation]]).

The tiers that need something beside the toolchain are selected by
build tag and test name prefix, never by an environment check that
turns a missing dependency into a silent skip. A test under `test/`
belongs to exactly one tier by its file's tag and its own name, so a
new test lands in a tier by how it is written rather than by where it
happens to run.

## Current state

Built and at `testing`. `test/stubs/provider` and `test/stubs/sink` are
the two stubs written here, `test/stubs/issuer` and
`test/stubs/authorizer` mount `pkg`'s two with Lux's vocabulary,
`test/stubs/index` is the document naming them all, `cmd/lux-stubs`
serves the eight listeners, `make run` and `make run-file` give a clean
clone a working gateway over them, and `test/e2e` is the integration
tier with the postgres tier's skeleton beside it.
`TestE2EConformance` runs [[018-conformance-suite]]'s `Run` against the
stubs, with no case skipped for want of them. What keeps the spec from
`complete`: one suite case, `case004Streaming`, asserts that the string
of the upstream model name is absent from a translated stream, which the
deterministic assistant content of this spec carries by construction, so
it waits on a change to [[018-conformance-suite]];
`TestPostgresTwoReplicas` waits on [[010-state]]'s Postgres store; and
the `e2e` and `postgres` jobs have not yet run on a push to `main`. The build's departures from
the design below are listed under "What the build changed".

## Design

### The binary

`cmd/lux-stubs` is a `main` over the packages under `test/stubs/`. It
starts every stub on one loopback address each, prints one line per
stub with its URL, and exits on `SIGTERM`. It ships as the test image
of [[017-release-and-installation]] and is never part of an
installation; [[001-architecture]]'s binary table says so.

| Stub | Comes from | Serves |
|---|---|---|
| provider | `test/stubs/provider`, written here | one instance per dialect: `openai`, `anthropic`, `gemini`, `lux` |
| issuer | `latere.ai/x/pkg/authkit/issuertest`, mounted | OIDC discovery, a key set, and the package's minting routes |
| authorizer | `latere.ai/x/pkg/authz/stub`, mounted | the contract of `latere.ai/x/pkg/authz`, which is [[006-identity]]'s |
| sink | `test/stubs/sink`, written here | the contract of [[012-request-log-and-events]] |
| index | `test/stubs/index`, written here | `GET /`, the document naming every stub above |

Every stub has a listener of its own, so a caller handed one address
finds none of the others. The index is the address that names the rest:
`GET /` answers `{"providers": {"<dialect>": "<url>"}, "issuer": "<url>",
"authorizer": "<url>", "sink": "<url>", "credential": "<value>"}`, the
document [[018-conformance-suite]] reads from `LUX_TEST_STUBS_URL` before
its stub cases run, so a suite driving a `luxd` over these stubs reads a
port rather than guessing one. It serves that one route and answers `404`
to every other, and the credential it reports is the `-credential` flag's,
which a caller writes into the Providers it applies so a request forwarded
without one is the `401` the stub provider records.

A core that writes its own stub issuer and its own stub authorizer
writes a second reading of two contracts it does not own, and the two
readings drift. Both packages expose a `NewHandler` that returns an
`http.Handler` for a binary serving its own address, which is exactly
what `cmd/lux-stubs` needs. Each of the four gets a listener of its
own: the two mounted handlers both serve `POST /hang` and `POST
/resume`, so one mux could not carry both.

There is no stub bucket: the archive tier runs against
`latere.ai/x/pkg/s3/s3test`, which is the same package the s3 client's
own tests use, so a bug in the stub cannot hide a bug in the client.

The two stubs written here serve their control routes under a `_`
prefix, which no dialect route and no contract route uses, so a control
route can never shadow a served one. The two that are mounted serve
theirs on the paths `pkg` already fixed, listed in their rows below.

### The stub provider

One handler, parameterised by dialect, serving the upstream paths of
[[005-providers]]'s dialect table: the translated routes, the model
routes, the model list discovery and the health probe read, and a
catch-all that records an opaque request and answers `200 {}`. The
`openai` instance therefore serves `/v1/chat/completions`,
`/v1/responses`, `/v1/embeddings`, and `/v1/models`, because
[[004-request-path]] translates the first two through two different
codecs. A door's own `GET /v1/models` never reaches a stub: the gateway
answers it from the catalogue ([[004-request-path]]), so the stub's
model list belongs to discovery and the probe alone.

The answer is a function of the request, so a test asserts a value
rather than a shape. The assistant content is
`stub:<dialect>:<model>:<first 8 hex of the SHA-256 of the last user
text>`, the usage members report `100` input and `20` output tokens,
and a streamed response is five content events followed by that route's
final usage event.

The usage members are the route's, not the dialect's, because one
dialect has two of them:

| Route | Input member | Output member |
|---|---|---|
| `openai` `/v1/chat/completions` | `usage.prompt_tokens` | `usage.completion_tokens` |
| `openai` `/v1/embeddings` | `usage.prompt_tokens` | none: the API reports `usage.total_tokens` and no output member |
| `openai` `/v1/responses` | `usage.input_tokens` | `usage.output_tokens` |
| `anthropic` `/v1/messages` | `usage.input_tokens` | `usage.output_tokens` |
| `anthropic` `/v1/messages/count_tokens` | `input_tokens`, at the top level as the API answers | none |
| `gemini` `:generateContent`, `:streamGenerateContent` | `usageMetadata.promptTokenCount` | `usageMetadata.candidatesTokenCount` |
| `gemini` `:countTokens` | `totalTokens` | none |
| `lux` `/v1/generate` | `usage.input_tokens` | `usage.output_tokens`, the `ir.Usage` wire names |
| `lux` `/v1/count_tokens` | `input_tokens` | none |

Those are the members [[009-usage-and-metering]]'s extraction table and
`latere.ai/x/pkg/llmdialect` read, so a stub that reports them is what
turns a metered number into a checkable one. The cached-input and
cache-write members of that table are reported as `0` unless the
upstream model name asks otherwise.

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
| `redirect` | `302` to another host, which the upstream client of [[005-providers]] must not follow |
| `fail-400` | `400` with that dialect's error body, the `upstream_rejected` case of [[004-request-path]] |
| `fail-529` | `529` with an overloaded body, the status [[008-routing-and-models]] retries |
| `fail-html` | `200` with `Content-Type: text/html` and an HTML body, which [[004-request-path]] relays as an octet stream |
| `tokens-<in>-<out>`, as `tokens-1000-500` | the deterministic answer with those two usage numbers, which is how a cost test fixes the figures it is computing against |
| `events-<n>` | a streamed answer of `n` content events instead of five |
| anything else | the deterministic answer above |

The header `Lux-Stub-Fail: <name>` does the same for a test driving the
stub directly, without a gateway in front. There is no query parameter
form: a `Provider.spec.baseURL` carries no query
([[003-manifest-contract]]), so the upstream model name is the only
channel a manifest has into the stub.

`GET /_received` returns every request the instance received, in order,
as a list of `{"method", "path", "query", "headers", "body"}`.
`DELETE /_received` clears it. Those two are what `TestSameDialectSameBytes`,
`TestCallerCredentialsNeverForwarded`, and `TestModelNameRewrite`
([[004-request-path]]) read.

### The stub issuer

`latere.ai/x/pkg/authkit/issuertest`, mounted through `NewHandler` with
`WithIssuer` set to the URL the other processes reach it at. This spec
adds nothing to it. Its routes, and what each is here for:

| Route | Used by |
|---|---|
| `GET /.well-known/openid-configuration`, `GET /jwks` | the verifier of [[006-identity]], and `luxd check`'s `issuers` row ([[017-release-and-installation]]) |
| `POST /mint` | every tier that needs a token for a subject; the body is `issuertest.Claims`, and a field the struct does not name is minted as an extra claim, so one `POST` produces each row of [[006-identity]]'s verification table |
| `POST /token` | the `client_credentials` grant, the service token a platform's unattended work reaches `/v1` with ([[020-building-a-plane]]) |
| `POST /actor-tokens` | the one-hop actor token a platform mints for a person acting in its console ([[020-building-a-plane]]) |
| `POST /rotate` | a token signed before the rotation stops verifying |
| `POST /hang`, `POST /resume` | discovery and the key set block and then answer again, which is the unreachable-issuer case |

It signs RS256 by default and ES256 under `WithES256`, the two
algorithms `luxd check`'s `issuers` row accepts, so one flag on
`lux-stubs` drives both rows of that table. Minting anything asked for
is what a stub issuer is for and is why it never runs in an
installation. `luxd` accepts it over `http://` only because the test
sets `LUX_OIDC_INSECURE_ISSUERS` ([[006-identity]]).

The package serves `GET /requests` and `DELETE /requests`, returning
and clearing the `[]string` of `"METHOD /path"` that `Requests()`
returns, since `latere.ai/x/pkg` v0.66.0, so
`TestE2EHotPathDialsNoWebhook` ([[001-architecture]]) reads the record
across the process boundary. Neither route records itself.

### The stub authorizer

`latere.ai/x/pkg/authz/stub`, mounted through `NewHandler`. It speaks
the contract of `latere.ai/x/pkg/authz`, which is the contract
[[006-identity]] adopts, so a rule written against it is a rule written
against the endpoint an operator writes.

| Route | Does |
|---|---|
| `POST /` | one decision, from the rule table |
| `PUT /rules` | replaces the table with a list of `stub.Rule`, `{subject, action, resource, allow, reason, ttl, limits, filter}`, an empty field matching anything and the **last** matching rule winning |
| `GET /requests`, `DELETE /requests` | every `authz.Request` received, in order, and a reset |
| `PUT /fail` | `{"status": <int>}`; `0` restores the table |
| `POST /hang`, `POST /resume` | the endpoint never answers, and then answers again |

It allows every subject when no rule matches, so a tier that is not
testing permission writes no rules, and it denies `authz.ProbeID` for
every subject and action whatever the table says, which is what makes
`luxd check`'s authorizer row mean anything
([[017-release-and-installation]]). The bearer it requires is
`stub.DefaultToken` unless `WithToken` sets another, and that is the
value `LUX_AUTHORIZER_TOKEN` takes in every tier.

What Lux adds is its vocabulary and nothing else, as [[006-identity]]
says: that spec's action names and `resource` shapes, and one option so
a rule names an object the way an operator does rather than by a ULID
the test cannot know in advance.

| Lux's addition | Shape |
|---|---|
| resource naming | `stub.WithResourceName(func(r authz.Resource) string { return r.Kind + "/" + r.String("name") })`, so `{"action": "model.use", "resource": "Model/gpt-4o"}` is a rule a test writes before the object exists |
| the flags | `lux-stubs -authorizer-deny <action>` and `-authorizer-fail <mode>`, which set the table and the outage through those same methods, for a test that starts the binary rather than driving it |

`GET /requests` is what `TestHotPathDialsNoWebhook`
([[001-architecture]]) reads: a `DELETE`, a thousand data plane
requests, and a read expecting an empty list.

[[006-identity]] names six forms of unavailability. The package covers
four; the tier drives the other two with a `luxd` of its own, because
they are properties of the connection and not of an answer:

| [[006-identity]]'s form | Driven by |
|---|---|
| a timeout | `POST /hang`, with `LUX_AUTHORIZER_TIMEOUT` short |
| a non-200 status | `PUT /fail` with `{"status": 500}` |
| a body that does not parse | `PUT /fail` with `{"body": "malformed"}`, in `pkg` since v0.66.0 |
| a body without `allow` | `PUT /fail` with `{"body": "no-allow"}`, the same |
| a refused connection | a `luxd` of the tier's own whose `LUX_AUTHORIZER_URL` names a port nothing listens at; the stub's `Close` is an in-process affair |
| a TLS failure | outside the stub: it serves plain HTTP, so the tier starts a listener of its own that answers the handshake with bytes that are no TLS record, and a `luxd` whose authorizer URL is `https://` at it |

The suite asserts every one of the six is `authorizer_unavailable` at
the gateway and none of them is an allow.

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
same `deploy/examples/` directory and a generated `STUB_DEV_KEY` that
the example Key names through `spec.valueFrom.env` ([[010-state]]), so
the read-only mode is also one command. The variable is not spelled
`LUX_*`: that prefix is the server's configuration namespace
([[002-repository-scaffold]]) and a manifest's `valueFrom.env` names a
variable of the operator's choosing, not one of the server's.

These are the targets this spec adds to the `Makefile`, beside the
`check`, `build`, `run`, `fmt`, `hooks`, and `clean`
[[002-repository-scaffold]] left:

| Target | Does |
|---|---|
| `run` | replaces the scaffold's: builds both binaries, starts the stubs and `luxd serve`, applies the examples, prints the exports |
| `run-file` | the same in file mode |
| `run-down` | stops whatever `run` or `run-file` started, by the pid file each wrote under `out/` |
| `test-e2e` | `go test -tags=integration -run '^TestE2E' -v ./...` |
| `test-postgres` | `go test -tags=postgres -run '^TestPostgres' -v ./...`, and fails with the one-line instruction below when `LUX_DB_URL` is unset |

### Tiers

| Tier | Where | Tag | Test name | Needs besides the toolchain | Run by |
|---|---|---|---|---|---|
| unit | every package outside `test/` | none | any | nothing | the gate, on every push |
| stub | `test/stubs/...` | none | any | nothing | the same run |
| integration | `test/e2e` | `integration` | `TestE2E*` | nothing | `make test-e2e`, the `e2e` job |
| postgres | `test/e2e`, `internal/store` | `postgres` | `TestPostgres*` | `LUX_DB_URL` | `make test-postgres`, the `postgres` job |
| conformance | `test/conformance` | none | `TestContract` | `LUX_TEST_URL` | the release pipeline and a platform ([[018-conformance-suite]]) |

The integration tier lives in `test/e2e` and starts `luxd` and
`lux-stubs` as processes on loopback, so it exercises the real HTTP
clients, the real listeners, and the real shutdown path rather than a
handler under `httptest`. `TestMain` builds the two binaries with `go
build -o` into a directory it removes at the end, and every test binds
port 0 and reads the address back, so the tier is parallel-safe, needs
no fixed port, and needs nothing on `PATH` but the Go toolchain.

The tier also runs the conformance suite in process: `TestE2EConformance`
calls `conformance.Run(t, cfg)` with the server it just started
([[018-conformance-suite]] exports `Run` for exactly this), rather than
shelling out to `TestContract`. `TestContract` is the entry point that
builds its configuration from the environment, for a server this tree
did not start.

The postgres tier is the same tree of cases with `LUX_DB_URL` set,
plus the cases that only exist with a shared store: two `luxd`
processes against one database, a lease held by one of them, a spend
counter that both see within one flush, a Key cache invalidated through
the journal, and a restart that resumes an unacknowledged event
([[010-state]], [[012-request-log-and-events]]). `internal/store`'s own
`TestPostgresStoreConformance` is in the tier too: it is `storetest.Run`
([[010-state]]) against the same URL, so the store suite that runs
against memory in the untagged run runs against Postgres here.

It is a tag rather than an environment check because a tier that
silently skips is a tier nobody notices is not running. Under
`-tags=postgres` an unset `LUX_DB_URL` fails in `TestMain` naming the
variable and one way to satisfy it, `docker run --rm -p 5432:5432 -e
POSTGRES_PASSWORD=lux postgres:17-alpine`; it never skips. CI points
the variable at a service container. Nothing in the tier starts a
container itself and no embedded Postgres is vendored: the database is
a dependency of the tier, declared by a variable, not a dependency of
the module.

### The tiers and the gate

`go tool lateregate`'s `test`, `race`, `hermetic`, and `cover` gates
each run `go test ./...` with no build tag. A directory whose files are
all excluded by a tag is passed over by a `./...` pattern, so those four
gates compile the unit, the stub, and the conformance tiers and never
see the other two. That is what lets `.lateregate.yaml` keep
`hermetic.allow: []` while the integration tier starts processes: the
tagged tiers are not in the hermetic run at all, and what they start
they built themselves with the toolchain the gate already puts first on
`PATH`.

`TestContract` is in the untagged run and skips without `LUX_TEST_URL`,
so `test/conformance` measures no statements there. That package is the
one `cover.exempt` row this spec adds to `.lateregate.yaml`, with its
reason: a package whose whole body runs against a server the gate does
not start. `cmd/lux-stubs` is not exempted; it stays a thin `main`
whose own test starts it and reads the lines it prints.

Neither tagged job passes `-race`. The gate's `race` gate already runs
the in-process suite under the detector, and a tier whose assertions are
HTTP requests against a separate process would instrument the driver
and not the server.

### CI

`verify.yml` keeps its `gate`, `tidy`, and `image` jobs and gains two,
each on GitHub's runners, each with every third-party action pinned by
commit with its version in a comment, as the file already does.

| Job | Runs | Does |
|---|---|---|
| `e2e` | every push and pull request | `go test -tags=integration -run '^TestE2E' -v ./...` |
| `postgres` | every push and pull request | the same with `-tags=postgres -run '^TestPostgres'`, with `LUX_DB_URL` pointed at a service container |

The package pattern is `./...` and not `./test/...` because the
postgres tier reaches `internal/store` as well as `test/e2e`.

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
      - run: go test -tags=postgres -run '^TestPostgres' -v ./...
        env:
          LUX_DB_URL: postgres://postgres:lux@127.0.0.1:5432/postgres?sslmode=disable
```

Both jobs pass `-v`, so a log names the tests that ran and a tier that
silently matched nothing is visible as an empty list rather than a
green check.

### What the build changed

Each row is a departure from the design above, with the reason, so the
Outcome at `complete` records nothing the tree does not.

| Where | The design said | The build does | Why |
|---|---|---|---|
| the usage table | `lux` reports `usage.input` and `usage.output`; embeddings and the counts share the generation routes' members | `usage.input_tokens` and `usage.output_tokens`; embeddings report `prompt_tokens` and `total_tokens`; each count answers its API's own top-level count | those are the wire names `ir.Usage`, `llmdialect/lux`, and the three APIs use, and a stub that reports members its API does not have is not honest about the contract |
| the stub provider | the failure table | the table as written, with `Lux-Stub-Fail` winning over the model name, `events-<n>` read on a streamed request alone, `fail-stream-mid` closing the connection through `http.ErrAbortHandler`, the catch-all checking the credential too, and a `:streamGenerateContent` without `alt=sse` answered as a JSON array | what the gateway and the dialect's own SDKs send; `TestFailureInjection` walks every row on every dialect by name and by header |
| the stub provider | the model list | one entry, `stub-<dialect>` | a discovered Model then names the dialect it came from |
| the stub sink | `-fail-first <n>` | `-fail-first` on the binary and `PUT /_fail {"first": n}` at run time; `attempts` counts every verified delivery of an id, refused ones included; a duplicate id is acknowledged and stored once; a verified body with no `id` is a `400` recorded apart | a tier that starts one binary drives the outage without restarting it; a sink deduplicates on `id` |
| `lux-stubs` | one line per stub with its URL | `lux-stubs: <stub> <url>` for `openai`, `anthropic`, `gemini`, `lux`, `issuer`, `authorizer`, `sink`, then `lux-stubs: issuer url <url>` and `lux-stubs: ready`; the flags are `-<stub>-addr`, `-credential`, `-issuer-url`, `-es256`, `-authorizer-token`, `-authorizer-deny`, `-authorizer-fail`, `-sink-secret`, `-fail-first` | a reader across a process boundary waits for `ready`; every address is bound before anything serves |
| `make run` | `LUX_URL=http://127.0.0.1:8080` and stub ports derived from the checkout's name | every port derived from the name, `RUN_PORT` the override, and `LUX_PUBLIC_URL` on `localhost` while the stubs sit at `127.0.0.1` | [[003-manifest-contract]]'s loop check compares hostnames alone, so a Provider on the gateway's own hostname is refused whatever its port |
| `make run` | applies every manifest under `deploy/examples/` | renders them into `out/run/examples/` first: the stub ports rewritten and, in server mode, the Key's `valueFrom` block dropped | a Provider's `valueFrom` is refused through `/v1` and a Key's `valueFrom` is required in the file mode, so one directory serves both modes only through a render |
| `deploy/examples/` | one Provider per dialect at its stub | the same, with `credential.value: stub-credential` and `discovery.mode: none` | the stub's credential is no secret and a literal is what both modes accept; discovery would list `stub-<dialect>` beside the declared Models of the same name |
| `make run`, `make run-file` | nothing on `PATH` but the toolchain | `curl` beside it, and `make` itself | the token is minted and the examples applied over HTTP from a recipe; the tier's two `make` rows fail, never skip, without them |
| `TestE2ECheckAgainstTheStubs` | `luxd check`'s authorizer row | the row's call, `auth.Authorizer.Check` over `authz.Client`, against the stub as a process with an allow-everything table in force | `luxd check` is [[017-release-and-installation]]'s and not in this build |
| the postgres tier | the same tree of cases against a shared store | `TestMain` refuses an unset `LUX_DB_URL` as designed; with one set, `TestPostgresTwoReplicas` skips naming [[010-state]]'s phase 6, and `internal/store`'s conformance run is that spec's | `luxd` refuses `LUX_DB_URL` until the Postgres store lands, and a red `postgres` job on every push until then would teach nobody anything |
| the tiers' rules | `TestEveryTestIsInATier`, `TestPostgresMainRefusesWithoutAURL` | both in `test/stubs`, the root package of the stubs tree, untagged | the tagged run cannot host a test of its own refusal; the name keeps the tier's prefix so `make test-postgres` runs it too |
| the tier | `TestE2E*` as the table names them | two more: `TestE2EFailureInjectionReachesTheDoors` over the failure table through a Model's target, and `TestE2EEventsReachTheSink` with a first delivery refused | the table is the stub's own; these prove the two contracts through the gateway |
| the tier | `TestE2EConformance` calls `conformance.Run` | `runConformance` in `test/e2e/conformance_seam_test.go` calls it with the stack's public URL, a token minted per subject from the stack's issuer, and `StubsURL` at the index, so the stub table runs rather than skipping | the suite was built on another branch and reads the stubs through one address |
| the binary | a listener per stub, one line each | the index beside the seven, with `-index-addr` and the line `lux-stubs: index <url>` in the same shape | a caller handed one address reads the other seven from the document rather than parsing the lines |
| the record | `GET /_received` names the headers | the member is `headers` on the wire, and the record's five wire names are held to a test | the record is decoded in another process, [[018-conformance-suite]] among them, and a member named on one side alone arrives empty rather than as a failure |
| the stub provider | five content events followed by the route's final usage event | the chat stream's finish reason rides the last content event, and a stream of no content events keeps a frame of its own for it | a frame between the content and the usage is one more than the design counts, and a reader counting frames reads the count the upstream model name asked for |
| `.lateregate.yaml` | one `cover.exempt` row | that row, and a `depcheck` row for `cmd/lux-stubs` | the binary reaches `latere.ai/x/pkg` and, through the sink's `events.Verify`, the OpenTelemetry SDK behind `internal/events`' delivery client |

Two things the tier learned about the gateway are findings for other
specs rather than departures here: a declared Model is absent from a
door's `GET /v1/models` until the health job's next tick publishes its
availability ([[005-providers]], at most `LUX_HEALTH_INTERVAL`), and a
first probe that fails leaves a Provider `Unreachable` until that tick,
so the tier's first request after a start is retried.

## Not in this spec

The conformance suite the stubs are wired into
([[018-conformance-suite]]); the release pipeline and the test image
([[017-release-and-installation]]); the s3 archive's own assertions,
which run against `s3test` ([[012-request-log-and-events]]); what each
stub's contract is, which is its owning spec's; the issuer's and the
authorizer's own behaviour, which is `latere.ai/x/pkg`'s and is tested
there, not here.

The two changes outside this tree an earlier draft named, `GET
/requests` and `DELETE /requests` on `latere.ai/x/pkg/authkit/issuertest`
and the two malformed-body outages on `latere.ai/x/pkg/authz/stub`, are
in `pkg` v0.66.0, the version `go.mod` pins, and nothing waits on them.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each stub package written here drives every route and every behaviour flag it offers from its own test | `TestProviderStub`, `TestProviderStubRoutes`, `TestSinkStub`, `TestIndexServesTheDocument`, `TestIndexRefusesEveryOtherRoute`; the issuer's and the authorizer's are `pkg`'s own, and the wrappers' additions are `TestAuthorizerStubNamesResourcesLuxsWay`, `TestAuthorizerStubFlags`, `TestAuthorizerStubDeniesTheProbe` | passing, `test/stubs/provider`, `test/stubs/sink`, `test/stubs/index`, `test/stubs/authorizer` |
| The index names every stub of the run at `GET /`, with the credential the providers require, so a caller handed that one address reaches each of them and guesses no port | `TestIndexServesTheDocument`, `TestLuxStubsIndexNamesEveryStub` | passing, `test/stubs/index` and `cmd/lux-stubs`, the second against the running binary |
| A streamed chat answer is the content events the upstream model name asked for, then the final usage event, then `[DONE]`, and no frame between | `TestChatStreamFrameBudget`, over three and five events and none | passing, `test/stubs/provider` |
| The conformance suite of [[018-conformance-suite]] runs against the stubs with no case skipped for want of them | `TestE2EConformance`, whose seam passes `StubsURL` | the stub table runs; `case004Streaming` fails on an assertion of that spec's own, that a translated stream carries no occurrence of the upstream model name, which the deterministic content of this spec carries by construction |
| The stub provider answers deterministically: one request twice yields byte-identical bodies, and the content names the dialect, the model, and the digest of the last user text | `TestProviderStubIsDeterministic` | passing |
| Every row of the failure injection table produces its behaviour on each of the four dialects, by upstream model name and by header | `TestFailureInjection`, table-driven over rows and dialects | passing, 13 rows × 4 dialects × 2 channels |
| The stub provider refuses a request whose credential is not the configured one and records it | `TestProviderStubChecksTheCredential` | passing |
| `GET /_received` returns every request in order with its headers and body, and `DELETE /_received` clears it | `TestReceivedRecording` | passing |
| The stub issuer's discovery document and key set verify a minted token through the same verifier `luxd` uses, under RS256 and under ES256 | `TestIssuerStubMintsVerifiableTokens`, table-driven over the two algorithms | passing, `test/stubs/issuer`, through `internal/auth.NewVerifier` |
| The stub provider reports the usage members of the route, not of the dialect, so a `/v1/responses` answer and a `/v1/chat/completions` answer carry different member names and both are metered | `TestProviderStubUsageShapes`, table-driven over the route table | passing, with the table as "What the build changed" has it |
| A Model whose target is `tokens-1000-500` produces a usage record of exactly 1000 and 500 tokens and the cost [[009-usage-and-metering]]'s arithmetic gives for its pricing | `TestE2ECostIsExact` | passing, `test/e2e`: 7500 micro-units of USD at 2.50 and 10 per million |
| Each of [[006-identity]]'s six unavailability forms, driven through `latere.ai/x/pkg/authz/stub`, is `authorizer_unavailable` at the gateway and none is an allow | `TestE2EAuthorizerUnavailability`, table-driven over the six rows | passing; the refused connection and the TLS failure through a `luxd` of the row's own |
| `lux-stubs` mounts `latere.ai/x/pkg/authkit/issuertest` and `latere.ai/x/pkg/authz/stub` rather than a reimplementation: no package under `test/stubs/` serves a discovery document, a key set, or an authorization decision | `TestStubsMountTheSharedPackages`, over `go list -deps ./cmd/lux-stubs` and the routes each package registers | passing, `cmd/lux-stubs` |
| The stub authorizer denies `authz.ProbeID` whatever rules are set, so `luxd check`'s authorizer row passes against it | `TestE2ECheckAgainstTheStubs` | passing, through the row's call; `luxd check` itself is [[017-release-and-installation]]'s |
| The stub sink refuses a body whose signature does not verify and records it apart | `TestSinkVerifiesSignature` | passing |
| `make run` on a clean clone prints the three exports, and a request through the `/openai` door with the printed Key returns a stub answer, produces one usage record, and delivers one event to the sink | `TestE2EMakeRun` | passing; the applies' ten events reach the sink, and the record costs the 450 micro-units the example names |
| `make run-file` serves the same door from the manifest directory and refuses a `PUT` with `read_only` | `TestE2EMakeRunFileMode` | passing |
| The integration tier starts `luxd` as a process on port 0 and covers every door, the four kinds' grammar, and a clean shutdown | `TestE2ELifecycle` | passing |
| The postgres tier runs two `luxd` processes against one database and proves the shared lease, the shared spend counter, the journal-driven cache invalidation, and the resumed event | `TestPostgresTwoReplicas` | skips naming [[010-state]]'s phase 6 until the Postgres store lands |
| Every test function in a file tagged `integration` begins with `TestE2E` and every one in a file tagged `postgres` begins with `TestPostgres`, wherever in the tree the file sits, and `test/conformance`'s one server-driven entry point is `TestContract` | `TestEveryTestIsInATier`, parsing every `_test.go` file's build tags and function names | passing, `test/stubs`; the `test/conformance` half applies once the package exists |
| The untagged `go test ./...` the gate runs compiles no test that needs a database, a network, or a binary on `PATH`, and the whole bar passes on a machine with only the Go toolchain installed | `go tool lateregate`, the `hermetic` gate | passing, `hermetic.allow: []` |
| `make test-postgres` with `LUX_DB_URL` unset fails naming the variable and one way to get a database, and never reports a skip | `TestPostgresMainRefusesWithoutAURL` | passing, `test/stubs`, over `go test -tags=postgres ./test/e2e/` |
| The `e2e` and `postgres` jobs pass on the first push to `main`, with every action pinned by commit | the `verify` workflow run | waits for the push; both jobs pass as commands on this branch |
