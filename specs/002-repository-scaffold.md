---
title: "Repository scaffold: module, binary, configuration, quality gate, images, workflows"
status: complete
track: core
depends_on:
  - specs/001-architecture.md
affects: [cmd/luxd/, internal/config/, internal/version/, Makefile, .lateregate.yaml, Dockerfile, .github/workflows/, .githooks/, docs/]
effort: small
created: 2026-09-13
updated: 2026-09-16
author: changkun
---

# Repository scaffold

## Overview

A compiling, testable repository that passes its whole quality gate
before any gateway code exists: the Go module, the `luxd` binary with
its two listeners and probes, typed configuration, the gate, the
developer image, the verify workflow, and the community files an open
source repository is judged by. The shape is chosen so the repository
reads as an ordinary open source Go service to a newcomer and so every
later spec lands into a tree that already enforces the bar.

This spec is also the configuration reference. It owns every `LUX_*`
variable the server reads, including the ones later specs give a
meaning to, so an operator has one table. It fixes the layout
[[001-architecture]] names without waiting for that design to settle,
because a repository that passes its gate is needed to write and test
anything else.

## Current state

In the tree and building. `cmd/luxd` serves the probes,
`internal/config` reads two variables, `internal/version` carries the
build identity, `.lateregate.yaml` configures the shared gate pinned as
a Go tool, and `.github/workflows/verify.yml` calls the shared
pipeline. Every gate but the spec tree passes locally; the spec tree
completes when the rest of the deck lands beside this file.

## Design

### Layout

Every entry either is in the tree or names the spec that builds it.

```
api/                    openapi.yaml, generated from the kinds, the routes, and the error table; embedded (011)
cmd/luxd/               main: the subcommand dispatcher, configuration, listeners, run group
cmd/lux/                main of the agent client, and nothing else (014)
cmd/lux-stubs/          main of the stub providers, issuer, authorizer, and sink; a test image, never an installation (015)
manifest/               decoding, validation, defaulting, resolve, for every kind (003)
manifest/v1/            the Provider, Model, Key, and Budget types (003, 005, 007, 008)
gateway/                the dialect doors, routing to a target, translation, streaming, credential injection (004, 008)
metering/               the usage record, costing from a Model's prices, the per-key and per-target aggregates (009)
client/                 the typed client of the /v1 API a plane, a migration tool, or the lux command drives (014)
internal/config/        typed configuration from the environment; every problem in one message
internal/version/       build identity set by -ldflags
internal/auth/          the verifier over the issuers, the authorizer client, the owner policy (006)
internal/secrets/       envelope encryption of a Provider's credential under LUX_SECRETS_KEK (005)
internal/api/           the /v1 control plane handlers and the OpenAPI document (011)
internal/events/        the signed sink client and the journal (012)
internal/reqlog/        the request log, what it redacts, and the archive exporter (012)
internal/store/         manifests, key material, budgets, usage, and the journal; memory, Postgres, and the read-only file mode (010)
internal/serve/         the serve role of luxd: wiring of the API, the gateway, the store, and the webhook clients (011)
internal/check/         the check role of luxd (017)
internal/rewrap/        the rewrap role of luxd (005)
internal/tunnel/        the reverse tunnel a local runtime connects out over, and the registry of serving nodes (013)
internal/luxcli/        the lux command: flags, defaults, exit codes (014)
test/e2e/               luxd as a process against the stubs (integration build tag) (015)
test/conformance/       the contract as an importable test package (018)
test/stubs/             the stub providers, issuer, authorizer, and sink (015)
tools/                  generators and release scripts (002, 017)
deploy/                 kustomize base, overlays, components, bootstrap (017); examples (015)
skills/lux/             the skill that teaches an agent the lux command (014)
docs/                   for people who run luxd or build against it
specs/                  this deck
```

### Local run

`make run` builds the binary and runs it on loopback with every state
in memory, because neither `LUX_DB_URL` nor `LUX_MANIFEST_DIR` is set.
Until [[004-request-path]] lands the process serves the probes and
nothing else; once [[015-test-stubs-and-tiers]] lands, `make run` also
starts the stub providers, issuer, authorizer, and sink beside it and
prints a key, so a clean clone sends its first model request in one
command.

### Binary and listeners

| Listener | Default address | Serves |
|---|---|---|
| public | `:8080` (`LUX_PUBLIC_ADDR`) | the dialect doors and the control plane under `/v1/*` (004, 011), `GET /` with the build identity, and `GET /livez`, `GET /readyz`, `GET /version` so a release smoke reaches them through the ingress |
| internal | `:8081` (`LUX_INTERNAL_ADDR`) | the four probes below, for the cluster |

The probes are `latere.ai/x/pkg/health`, mounted whole on the internal
listener and path by path on the public one.

| Method | Path | Body |
|---|---|---|
| GET | `/livez` | 200 `ok`, touches no dependency |
| GET | `/readyz` | 200 `ok` when every check passes; 503 `not ready: <check>: <error>` otherwise, and `not ready: draining: shutting down` during shutdown; text, the developer register |
| GET | `/version` | `{"version","commit","build_time"}` from `internal/version`, set by `-ldflags` |
| GET | `/metrics` | the `latere.ai/x/pkg/metrics` registry in the Prometheus text format (019); internal listener only |

Readiness runs its checks with a 2 second budget, `draining` and
`store` ([[010-state]]): luxd keeps nothing on local disk, so there is
no disk check. A
provider that fails its health probe takes its targets out of rotation
([[005-providers]]) and never fails readiness, because a replica that
can still route to another target is still serving. Shutdown on
`SIGTERM` or `SIGINT`: readiness answers 503 at once, the process waits
a 3 second drain delay, then closes the HTTP servers with a 60 second
grace period so an in-flight stream finishes, then writes the usage
deltas it still holds. A Deployment sets
`terminationGracePeriodSeconds` 90.

`luxd -version` prints `luxd <version> (<commit>, <date>)` and exits 0;
a bad flag exits 2; a configuration or start-up failure exits 1 with
one line on stderr prefixed `luxd:`. The first argument that does not
start with `-` selects a subcommand; without one the binary serves.

| Subcommand | Reads | Does | Spec |
|---|---|---|---|
| `serve` (default) | the whole table | the gateway: the listeners, the dialect doors, the control plane API, the metering flush | this spec |
| `check` | the whole table | one line per requirement of the installation, exit 1 on any failure | 017 |
| `rewrap` | `LUX_SECRETS_KEK`, `LUX_DB_URL`, `LUX_DB_POOL_URL` | re-wraps every stored credential's data key under the first key of the list, then exits; a no-op when nothing is wrapped under another key | 005 |

One binary, one image, one role per process: a Deployment selects the
role by its args. Each role is a package under `internal/` with its own
dependency allow list in the gate, so the binary carrying every role
does not loosen what any one role may reach. An unknown subcommand is a
usage error, exit 2. `check` and `rewrap` landed with their specs,
[[017-release-and-installation]] and [[005-providers]].

### Configuration

Every variable is read once at start-up by `internal/config.Load`,
which collects every problem and fails with one message
`configuration: <problem>; <problem>; ...` sorted by variable name. A
blank value is unset. An unknown variable is never an error, so a
deployment that sets one before its spec lands is not refused. Variables
the test suites read, the `LUX_TEST_*` family, the ones a CI job
reads, the `LUX_INSTALL_*` family, and the `lux` command's own, are
not the server's and live in the tables of [[018-conformance-suite]],
[[017-release-and-installation]], and [[014-agent-client]].

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `LUX_PUBLIC_ADDR`, `LUX_INTERNAL_ADDR` | no | `:8080`, `:8081` | listen addresses; a test binds `127.0.0.1:0`; the two must differ unless both ask for port 0 |
| `LUX_PUBLIC_URL` | yes, from 011 | none | the absolute URL callers reach the public listener at; the base of every URL in a response |
| `LUX_MANIFEST_DIR` | 010 | unset | a directory of manifests read at start: file mode, where desired state comes from disk and the kinds it declares are read-only through the API |
| the variables a file-mode manifest names in `credential.valueFrom.env` or `Key.spec.valueFrom.env` | 003, 010 | none | the operator's own names, outside the `LUX_` namespace, read once at start in file mode and refused in server mode |
| `LUX_DB_URL`, `LUX_DB_POOL_URL`, `LUX_DB_MAX_CONNS` | 010, 024 | unset, unset, `8` | a Postgres URL and the pool size; the URL unset keeps every state in memory, and a set URL selects the Postgres store, whose migrations `luxd serve` applies directly at start; the optional pool URL carries serving queries ([[010-state]]) |
| `LUX_SECRETS_KEK` | yes for `serve` and `rewrap`, except in file mode, from 005 | none | one to eight 32-byte keys, standard base64, comma separated; the first wraps every new data key, every key is tried to open one, so rotation is prepending a key and running `luxd rewrap` |
| `LUX_OIDC_ISSUERS` | yes, from 006 | none | comma separated issuer URLs whose tokens are accepted on the control plane |
| `LUX_OIDC_AUDIENCE` | 006 | `lux` | the audience a caller token must contain |
| `LUX_OIDC_INSECURE_ISSUERS` | 006 | unset | issuers from the list that may use `http://` on a host other than loopback; set by the test stubs, never in production |
| `LUX_AUTHORIZER_URL`, `LUX_AUTHORIZER_TOKEN` | 006 | unset | the operator's authorization endpoint and the bearer luxd sends it; unset selects the built-in owner policy; the URL without the token is a start-up failure |
| `LUX_AUTHORIZER_TIMEOUT` | 006 | `5s` | one decision's deadline, the retry included; the cache times are the shared contract's (`latere.ai/x/pkg/authz`): an allow for the answer's `ttl`, default 60 s and at most 600 s, a deny for 5 s, and no variable changes them |
| `LUX_ADMIN_SUBJECTS` | 006 | unset | comma separated subjects the built-in owner policy lets act on every object; read and unused when an authorizer is set |
| `LUX_KEY_CACHE` | 007 | `10s` | how long a Key lookup is cached per replica on the data plane |
| `LUX_DEFAULT_REQUESTS_PER_MINUTE`, `LUX_DEFAULT_TOKENS_PER_MINUTE` | 007 | `0`, `0` | the limits a Key gets when it names none; `0` is no limit, and under an authorizer ceiling a Key with no limit is refused (003), so an installation with ceilings sets these or its callers name limits |
| `LUX_UPSTREAM_TIMEOUT` | 004 | `10m` | the deadline of one upstream request including its stream |
| `LUX_UPSTREAM_ALLOW_PRIVATE` | 005 | unset | `1` lets a Provider base URL name a loopback or private address, for a model served on the operator's own machine |
| `LUX_MAX_BODY_BYTES` | 004 | `64Mi` | the largest data-plane request body accepted, and the cap on an upstream body the gateway reads whole (005) |
| `LUX_DISCOVERY_INTERVAL`, `LUX_HEALTH_INTERVAL` | 005 | `1h`, `30s` | how often a Provider's model list is refreshed and its health probed |
| `LUX_METERING_FLUSH` | 009 | `1s` | how often a replica's usage deltas are written to the store |
| `LUX_EVENTS_URL`, `LUX_EVENTS_SECRET` | 012 | unset | the event sink and the HMAC key; events are off when the URL is unset; the URL without the secret is a start-up failure |
| `LUX_REQUESTLOG_EXPORTER` | 012 | `none` | where the request log is archived: `none` or `s3` |
| `LUX_S3_ENDPOINT`, `LUX_S3_REGION`, `LUX_S3_BUCKET`, `LUX_S3_ACCESS_KEY`, `LUX_S3_SECRET_KEY`, `LUX_S3_PREFIX` | 012 | unset; region `us-east-1`, prefix `lux/` | the request-log archive; the endpoint, the bucket, the access key, and the secret key are required when the exporter is `s3`, and there is no default endpoint and no credential chain |
| `LUX_REQUESTS_PER_MINUTE`, `LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE` | 011 | `600`, `60` | control-plane requests one subject, and one client address before authentication, may send in a minute |
| `LUX_TRUSTED_PROXIES` | 011 | unset | CIDR ranges of the proxies in front of the gateway whose `X-Forwarded-For` names the client; unset trusts no header |
| `LUX_MAX_MANIFEST_BYTES` | 011 | `65536` | the largest manifest or JSON body accepted on the control plane |
| `LUX_TUNNEL_ENABLED`, `LUX_TUNNEL_REGISTRY_TTL` | 013 | unset, `30s` | the reverse tunnel for local runtimes, and the liveness window of a serving node in the registry |
| `LUX_TUNNEL_FORWARD_ADDR`, `LUX_TUNNEL_FORWARD_SECRET` | 013 | unset, unset | the address other replicas reach this one's internal listener at, and the bearers on the forward route, a comma separated list of which the first is sent and every one is accepted, so a rotation is prepending; unset serves a tunnelled Provider on the holding replica only; the address without the secret is a start-up failure |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_*` | 019 | unset | the standard OpenTelemetry exporter variables, read by `latere.ai/x/pkg/otel`; telemetry is off without the endpoint |

### The gate

`make` is `go tool lateregate`. The gates live in `latere.ai/x/ci-gate`,
pinned in `go.mod` with a `tool` directive, and `.lateregate.yaml`
holds only what this repository chose: the spec vocabulary and required
frontmatter, the empty hermetic allowance (luxd forks no binary of its
own), the `depcheck` allow list of `./cmd/luxd`, and the licence. The
git hooks are two-line shims that call the gate. A coverage exemption
or a waiver is a line in that file with a reason, never a tag in the
code.

### Images

`Dockerfile` compiles inside the image and runs `luxd` on a distroless
static base as a non-root user with both ports exposed. The base
carries the CA roots because luxd dials the providers, the issuers, and
the webhooks, and nothing else: the gateway writes no local state, so
the image declares no volume. The release image of
[[017-release-and-installation]] copies a binary the pipeline built and
attested and shares this runtime stage byte for byte between the two
markers, checked by a test in that spec.

### Workflows

`verify.yml` runs on every push to `main`, every pull request, and on
demand; on a tag it triggers and every job skips, because the release
pipeline of [[017-release-and-installation]] owns tags: the `gate` job calls
`latere-ai/ci/.github/workflows/lateregate.yml@v1` on GitHub's runners,
`tidy` checks `go mod tidy -diff`, and `image` builds the developer
image and asks it for its version. The tiers that need a provider
beside them join in [[015-test-stubs-and-tiers]]; the release pipeline
is [[017-release-and-installation]]'s. Every third-party action is
pinned by commit with its version in a comment.

### Community files

`README.md` positions the project and says what works today. `LICENSE`
is Apache-2.0. `CONTRIBUTING.md` says how to build, the bar, specs
first, and where a package belongs. `CODE_OF_CONDUCT.md` is the
Contributor Covenant 2.1. `SECURITY.md` names the address, the
response times, and the properties the design commits to.
`CHANGELOG.md` has one section per release and a tag without one is
refused. `.github/` carries the bug and feature templates, the pull
request template, and the actionlint configuration, which declares no
runner of its own. `AGENTS.md` holds the conventions an agent working in
the tree follows, the first of which is that this repository is public:
no hostname, token, or internal reference appears in it except as a
default or an example, and no particular deployment of Lux is named.

## Not in this spec

The release pipeline, the deploy manifests, and the install document
([[017-release-and-installation]]); the stubs and the tiers that need a
provider beside them ([[015-test-stubs-and-tiers]]); the generated
configuration page, which lands with the generator once the table has
two owners.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `luxd -version` prints the identity and exits 0; an unknown subcommand and a bad flag exit 2 | `TestVersionFlagPrintsTheIdentityAndExitsZero`, `TestUnknownSubcommandIsAUsageError`, `TestBadFlagIsAUsageError` | passing |
| The subcommand is the first argument without a leading dash, and the flags around it are the subcommand's | `TestSubcommandSplitsAroundTheFirstBareWord` | passing |
| A configuration with two problems fails with one sorted line, exit 1 | `TestLoadReportsEveryProblemInOneSortedMessage`, `TestBadConfigurationExitsOneWithOneLine` | passing |
| Every unset variable takes its default, a blank value is unset, and both listeners may ask for port 0 | `TestLoadAppliesEveryDefault`, `TestLoadReadsEveryVariable`, `TestLoadTreatsBlankAsUnset`, `TestLoadAllowsPortZeroOnBothListeners`, `TestLoadRefusesOneSocketForBothListeners` | passing |
| Both listeners answer `/livez`, `/readyz`, `/version`; the public one answers `/` and not `/metrics`; a stop returns 0 | `TestServeAnswersTheProbesOnBothListenersAndStopsCleanly` | passing |
| Readiness fails once draining begins, and the drain wait ends with the process context | `TestReadinessFailsOnceDrainingBegins`, `TestSleepCtxReturnsEarlyWhenTheContextEnds` | passing |
| An occupied address is a start-up failure with the variable named | `TestOccupiedAddressExitsOne` | passing |
| The build identity carries every field and marks a development build | `TestStringCarriesEveryField`, `TestDefaultsMarkADevelopmentBuild` | passing |
| Every package clears 90% coverage, `internal/config` 100%, and every gate passes locally | `go tool lateregate` | passing, 14 gates |
| The gate, the tidy check, and the image build pass on the first push to `main` | the `verify` workflow run | passing, run 34725523595 |
| The developer image runs `luxd -version` as a non-root user | the `image` job | passing, the same run |

## Outcome

Built on 2026-09-13 in six commits and proven by the first `verify` run
on `main` (run id 34725523595, eighteen jobs green). The scaffold
mirrors Cella's of the day before with three deliberate departures:
there is no data directory and no disk readiness check, because `luxd`
keeps no local state (the store's check joins with [[010-state]]);
there is no runtime selector, because the gateway has no driver to
select; and `AGENTS.md` stands alone with no `CLAUDE.md` in the tree.
The first push went out with the pre-push hook bypassed, because the
gate's push check diffs against `origin/main` and the remote had no
branch yet; the whole bar had passed locally on the same commits, and
every later push runs the hook. The developer image was built locally
through a podman backend and by the `image` job on Docker in the same
run. Deferred as the spec says: the release pipeline and the deploy
manifests to [[017-release-and-installation]], the stubs and the tiers
to [[015-test-stubs-and-tiers]], the generated configuration page until
the table has two owners.
