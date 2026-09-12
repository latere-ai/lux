---
title: "Release and installation: images, binaries, attestations, deploy manifests, luxd check, upgrades"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/015-test-stubs-and-tiers.md
  - specs/018-conformance-suite.md
affects: [.github/workflows/release.yml, Dockerfile.release, Dockerfile.stubs, deploy/base/, deploy/overlays/, deploy/bootstrap/, tools/release/, tools/docs/, internal/check/, cmd/luxd/, docs/install.md, docs/upgrades/, CHANGELOG.md]
effort: medium
created: 2026-09-13
updated: 2026-09-13
author: changkun
---

# Release and installation

## Overview

An operator outside Latere installs Lux from a release, not from a
checkout, and upgrades on its own schedule. A `v*` tag produces two
images, two binaries for four platforms, checksums, signatures, bills
of materials, provenance, and a deploy archive, and publishes a GitHub
release whose notes are the CHANGELOG section for that version.
Installation is one document CI walks against a real cluster on every
push, so it is never stale. `luxd check` tells an operator whether an
installation meets its requirements before the first manifest reaches
it, and before the first request does.

This spec owns the artifacts, the pipeline, the deploy manifests, the
`check` role, the install document, and what a version number promises.
Nothing in it names a Latere host or namespace: the image namespace is
the repository owner running the workflow, so a fork's tag publishes
under the fork's ([[001-architecture]], invariant 8).

## Current state

`verify.yml` runs the gate, the tidy check, and the developer image
build on every push and pull request, and skips itself on a tag
([[002-repository-scaffold]]). There is no `release.yml`, no `deploy/`,
no `docs/install.md`, and no `check` role; `luxd check` is an unknown
subcommand today. The CHANGELOG rule is already in force through the
gate's pre-push hook, and `CHANGELOG.md` carries an `Unreleased`
section. No tag has been cut; `SECURITY.md` says the first is `v0.1.0`.

## Design

### Artifacts

| Artifact | Name | Notes |
|---|---|---|
| gateway image | `ghcr.io/<owner>/luxd:<tag>` | `linux/amd64` and `linux/arm64` as one multi-arch image, built by `Dockerfile.release` from the binaries the pipeline already built |
| stub image | `ghcr.io/<owner>/lux-stubs:<tag>` | the stub providers, issuer, authorizer, and sink of [[015-test-stubs-and-tiers]] in one image, published beside `luxd` under the same tag, so the conformance job and an operator's first installation run a released, signed stub rather than a checkout |
| gateway binaries | `luxd_<tag>_<os>_<arch>.tar.gz` | `linux` and `darwin`, `amd64` and `arm64` |
| client binaries | `lux_<tag>_<os>_<arch>.tar.gz` | the same four pairs; the client runs where agents run rather than in a cluster, so it takes an archive and no image ([[014-agent-client]]) |
| checksums | `checksums.txt` | SHA-256 over every archive, signed |
| bills of materials | `sbom-module.spdx.json`, `sbom-luxd.spdx.json`, `sbom-lux-stubs.spdx.json` | SPDX for the module graph and one per image |
| signatures | cosign keyless over both images and over `checksums.txt` | the workflow's OIDC identity, verified by `cosign verify` with the identity and issuer |
| attestations | an SBOM attestation and a build provenance attestation per image | this repository is public, so `gh attestation verify` answers for the image about to run |
| deploy archive | `deploy-<tag>.tar.gz` | `deploy/base`, `deploy/overlays`, `deploy/bootstrap`, and the example manifests of [[015-test-stubs-and-tiers]], with the `luxd` image reference pinned to the tag by digest |
| contract fixture | `fixture-<tag>.tar.gz` | the resolved manifests and archived records the conformance run produced, for the next release's fixture group, below |

`<owner>` is `github.repository_owner` lowered, read at run time and
written nowhere in the tree. `TestReleasePublishesUnderTheOwnersNamespace`
refuses a literal namespace anywhere in the workflow, the deploy tree,
or the archive, which is [[001-architecture]]'s invariant 8 as a test.

### The release image

`Dockerfile` compiles inside the image so a `docker build .` from a
checkout works ([[002-repository-scaffold]]). `Dockerfile.release`
copies the binary the pipeline built, signed, and attested, so the
image carries the same bytes the archive does. The two share a runtime
stage byte for byte: everything between the `# >>> shared runtime base <<<`
and `# <<< shared runtime base >>>` markers is identical in both files,
and `TestRuntimeStagesMatch` compares them. The stage is
`gcr.io/distroless/static-debian12:nonroot` pinned by digest, carrying
the CA roots and nothing else, running as `nonroot:nonroot` with both
ports exposed and `luxd` as the entry point.

A developer's image and a released image therefore differ in where the
binary came from and in nothing else, which is what makes a bug
reproduced locally a bug reproduced against the release.

### The pipeline

`release.yml` on a `v*` tag. `verify.yml` skips tags, so the gate does
not run twice and the first job below is where the tag's evidence
starts.

```mermaid
flowchart LR
  G[gate-green] --> B[build]
  B --> C[conformance]
  C --> P[publish]
  P --> I[install-release]
  P --> V[release-verify]
```

1. `gate-green`: the tag's commit must have a successful `verify`
   run. A tag on a commit whose gate never ran, or ran red, fails here
   and publishes nothing, so a release is cut from a green tree by
   construction rather than by habit.
2. `build`: `go build` for the four `os/arch` pairs with the `-ldflags`
   of [[002-repository-scaffold]] setting `internal/version`; the
   archives and `checksums.txt`; `docker buildx` of `Dockerfile.release`
   and `Dockerfile.stubs` for both architectures, each pushed as one
   multi-arch image; `cosign sign` keyless on both images and on
   `checksums.txt`; the three SPDX documents, attached with
   `actions/attest-sbom`, and provenance with
   `actions/attest-build-provenance`; and the deploy archive with both
   images pinned by digest.
3. `conformance`: the e2e stack of [[015-test-stubs-and-tiers]] brought
   up from the published images rather than from a checkout, then
   `TestContract` of [[018-conformance-suite]] against it, whose
   `fixture` group reads the directories the previous releases left in
   the tree. The run then writes this release's fixture directory,
   below. A failure here stops the release.
4. `publish`: the GitHub release with every artifact and, as the body,
   the CHANGELOG section for the tag, read with
   `go tool lateregate release-notes <tag>`. A tag with no section fails
   the job, which is the same rule the pre-push hook enforces on a
   developer's machine.
5. `install-release`: the fenced blocks of `docs/install.md` run against
   a fresh kind cluster with `LUX_INSTALL_IMAGE` and
   `LUX_INSTALL_MANIFESTS` pointing at the published image and the
   unpacked deploy archive, ending in `luxd check` and one request
   through a door. Both are variables of this job and of the block
   runner, read by no server: they substitute an image reference and a
   directory into the document's commands, so `internal/config` gains
   nothing and [[002-repository-scaffold]]'s table does not grow. A
   failure fails the workflow after the release exists, which the
   release notes link.
6. `release-verify`: from a clean runner with no checkout on the path,
   `cosign verify` on both images and on `checksums.txt` against the
   workflow identity, `sha256sum -c checksums.txt` over the downloaded
   archives with `--pattern 'luxd_*.tar.gz'` and `--pattern 'lux_*.tar.gz'`
   downloaded separately so neither glob can satisfy the other's sums,
   `gh attestation verify` on both images, and the release body compared
   with the CHANGELOG section.

Every third-party action is pinned by commit with its version in a
comment, as `verify.yml` pins them.

A maintainer cuts a release with `go tool lateregate release vX.Y.Z`,
which refuses a dirty tree, an existing tag, and an empty `Unreleased`
section; otherwise it writes `## vX.Y.Z - <today>` under a fresh
`Unreleased`, commits `changelog: vX.Y.Z`, creates the annotated tag,
and pushes the commit and the tag in one push, so this pipeline runs
once.

### Deploy manifests

`deploy/base`, kustomize, no namespace of its own so an operator names
one:

| Object | What it carries |
|---|---|
| `Deployment` | 2 replicas; the hardening fields of [[016-security-and-threat-model]]; `terminationGracePeriodSeconds: 90` for the drain of [[002-repository-scaffold]]; `strategy.rollingUpdate` with `maxSurge: 0` and `maxUnavailable: 1`; `livenessProbe` on `/livez` and `readinessProbe` on `/readyz`, both on the internal port; the configuration from a ConfigMap and the four secrets from a Secret |
| `Service` | the public port; the internal port is named and reachable inside the namespace for `/metrics` and for the tunnel forward route ([[013-tunnelled-runtimes]]) |
| `ServiceAccount` | no Role and no RoleBinding: `luxd` calls no Kubernetes API, and its token is not mounted |
| `NetworkPolicy` | egress to DNS, to the providers, the issuers, the authorizer, the sink, the archive, and the store, and to the other replicas on the internal port; ingress from the ingress controller on the public port and from the other replicas on the internal port |
| `PodDisruptionBudget` | `minAvailable: 1` |
| `PrometheusRule` | the rules of [[019-observability]], checked with `promtool check rules` in CI |
| `HorizontalPodAutoscaler` | in `deploy/components/hpa`, a kustomize component an operator adds; it is not in the base, because the connection ceiling below makes replica count a decision rather than a dial |

`maxSurge: 0` is not a preference. Each replica opens
`LUX_DB_MAX_CONNS` connections, default 8 ([[010-state]]), so two
replicas hold 16 and a rollout that surged would ask for 24 against a
managed cluster that often caps connections in the low tens. A rollout
that cannot open its pool fails after the release, which is the worst
moment to discover the ceiling. `luxd check`'s `db conns` row reports
the same arithmetic before a first install.

`deploy/overlays/kind` is an overlay for a laptop cluster: one replica,
the memory store, a `NodePort`, and a `Kustomization` an operator
applies in one command, against an issuer the operator names.
`deploy/overlays/generic` is the same shape with Postgres and two
replicas. Neither names a stub: `lux-stubs` is a test image and never
part of an installation ([[001-architecture]],
[[015-test-stubs-and-tiers]]). The `install-release` job needs an
issuer, and applies the published `lux-stubs` image as a Pod of its
own from the job rather than from the deploy tree, so nothing an
operator applies carries one.

`deploy/examples/` is the directory of example manifests
[[015-test-stubs-and-tiers]] owns and `make run` applies, one Provider
per dialect and the Model, Budget, and Key that name them. The deploy
archive carries it beside the overlays, because it is what an operator
edits first after the gateway is serving; the two directories are not
the same thing and the archive keeps them apart.

`deploy/bootstrap` holds the Secret templates an operator fills once
and applies by hand, because they are the four values no manifest in a
repository should carry:

| Key | Spec |
|---|---|
| `LUX_SECRETS_KEK` | [[005-providers]] |
| `LUX_AUTHORIZER_TOKEN` | [[006-identity]] |
| `LUX_EVENTS_SECRET` | [[012-request-log-and-events]] |
| `LUX_DB_URL` | [[010-state]] |

Every overlay renders in CI, and the rendered base is read by
`TestBaseIsConfined` for the hardening fields.

### `luxd check`

The second role of the server binary, `internal/check` with its own
dependency allow list ([[002-repository-scaffold]]). It reads the whole
configuration, prints one line per requirement, and exits 1 when any
line failed. A line is `ok`, `warn`, or `fail`, the requirement's name,
and one developer sentence; a `warn` never changes the exit code.

The rows this spec owns:

| Line | Passes when |
|---|---|
| `version` | the binary's identity prints; never fails, so a check run always says what it is |
| `configuration` | `internal/config.Load` returns without a problem |
| `public url` | `LUX_PUBLIC_URL` is absolute, and no Provider's `baseURL` names its host, which would be a loop ([[003-manifest-contract]]) |
| `issuers` | every `LUX_OIDC_ISSUERS` entry answers `/.well-known/openid-configuration` and its `jwks_uri` with at least one `RS256` or `ES256` key |
| `authorizer` | the endpoint answers a probe inside `LUX_AUTHORIZER_TIMEOUT` with a body that parses as a decision. The probe is `provider.read` on the reserved id `prv_00000000000000000000000000`, which names no object in any installation, and its answer is not entered in the decision cache ([[006-identity]]), so a check run never changes what a later request is told. The line warns on `allow`: an authorizer that allows an action on an object that cannot exist is answering without looking |
| `events` | with `LUX_EVENTS_URL` set, the sink answers 2xx to one signed ping whose body is an event of type `check.ping` and whose signature is computed exactly as [[012-request-log-and-events]] says. `check.ping` is this spec's addition to that spec's type table: it names no object, carries an empty `data`, has `reason: check`, and is never written to the journal, so a sink that keys on `type` has a row to ignore rather than an unknown body to refuse |
| `requestlog` | with the exporter `s3`, the bucket accepts and then deletes one empty object under `LUX_S3_PREFIX` |
| `image` | the running image's digest, when the process can read it, so a support conversation starts from one |

The rows other specs own, printed in this order and cited rather than
restated: `store`, `migrations`, `manifest dir`, `db conns`
([[010-state]]); `secrets kek`, `credentials`, `providers`, `dialects`
([[005-providers]]); `tunnels` ([[013-tunnelled-runtimes]]).

`luxd check` dials the operator's endpoints and the providers and
writes nothing but the one archive object it deletes. It is safe to run
against a serving installation, which is the point: an operator runs it
when something is wrong, not only before the first install.

### `docs/install.md`

One document, in the user register, from nothing to a request through a
door: the prerequisites, the bootstrap secrets, `kubectl apply -k`, the
first `luxd check`, a first `Provider` and `Model` with `lux apply`, a
first `Key`, and one `curl` through a door. Every command is in a
fenced block with a language tag, and `tools/docs/run-blocks.sh` runs
the blocks in order against a kind cluster, so a command that stopped
working fails a build rather than an operator.

CI runs it twice with the same script and different inputs: on every
push against the developer image and `deploy/` from the checkout, and
in `install-release` against the published image and the unpacked
archive. The first catches a prose defect the day it lands; the second
proves the artifacts an operator actually downloads.

### What a version promises

Semantic versioning on the tag. Before `v1.0.0` a minor may break any
row below with a CHANGELOG entry naming the break; from `v1.0.0` the
table binds.

| Surface | A patch may | A minor may | A major may |
|---|---|---|---|
| the manifest schema ([[003-manifest-contract]]) | fix a rule that was refusing a valid manifest | add an optional field with a behaviour-preserving default, or an enum value | change the API group to `lux.latere.ai/v2`, with a conversion in both directions and a period where both are served |
| `/v1` routes ([[011-api]]) | fix a status or a sentence that was wrong | add a route, a parameter, or a response field | remove or repurpose one |
| error codes ([[011-api]]) | nothing | add a code | remove a code or change what one means |
| `LUX_*` variables ([[002-repository-scaffold]]) | fix a default that was wrong | add a variable, or widen what one accepts | remove a variable or change its meaning; a removed variable is read and ignored with a `WARN` for one minor series first |
| event types and payloads ([[012-request-log-and-events]]) | nothing | add a type or a `data` member | remove a type or a member |
| the usage record ([[009-usage-and-metering]]) | nothing | add a field | remove or repurpose a field |
| the exported packages ([[001-architecture]]) | nothing | add a function, a type, or a field | break a call site |
| the store schema ([[010-state]]) | nothing | add a migration that an older binary in the same minor can still read | add one that it cannot |

A cost computed from one pricing is computed the same by every later
build, at every level, because a bill that changes under a patch is not
a bill ([[009-usage-and-metering]]).

### Upgrade and rollback

Any release upgrades from any earlier release in the same major with no
step: migrations are embedded and applied at start ([[010-state]]), and
the Deployment rolls one replica at a time with none surging. During a
roll the two versions serve together, which is why a minor's migrations
must be readable by the previous minor's binary: a rollback inside a
minor series is `kubectl rollout undo` and nothing else.

Across a major, `docs/upgrades/<major>.md` says what to verify, in what
order, and what cannot be undone. A binary that finds a schema version
above its own refuses to start naming both ([[010-state]]), rather than
running against a schema it does not know.

A KEK rotation is not an upgrade and has its own sequence: deploy with
`LUX_SECRETS_KEK=new,old`, run `luxd rewrap`, deploy with
`LUX_SECRETS_KEK=new` ([[005-providers]]).

### The previous-release fixture

[[018-conformance-suite]]'s `fixture` group reads
`test/conformance/testdata/previous/<version>/`: one resolved manifest
per kind and a bucket of archived records as NDJSON, one directory per
tagged release. This pipeline is what writes the next one. After
`TestContract` passes, the `conformance` job reads the resolved
manifests back through `GET /v1/{kind}s/{name}` and drains the archive
the run wrote, writes them to
`test/conformance/testdata/previous/<tag>/`, commits that directory to
`main` as `conformance: fixture <tag>`, and attaches the same bytes to
the release as `fixture-<tag>.tar.gz` for anyone reading a release
rather than the tree.

The set therefore grows by one directory per release and old ones are
kept, which is what turns the schema evolution promise of
[[003-manifest-contract]] and the record additivity promise of
[[009-usage-and-metering]] into a test that runs on every push rather
than a rule in a document. The job commits and does not tag, so the
fixture commit lands after the release exists and triggers the ordinary
`verify` run and no second release.

## Not in this spec

The scaffold's workflow, image, and variable table
([[002-repository-scaffold]]); the stubs and the tiers the conformance
job runs ([[015-test-stubs-and-tiers]]); the suite itself and the
upgrade case ([[018-conformance-suite]]); the alert rules the
`PrometheusRule` carries ([[019-observability]]); the hardening fields'
reasons ([[016-security-and-threat-model]]); the `check` rows other
specs own.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every artifact in the table is attached to the release of a tag, and every signature, checksum, and attestation verifies from a clean runner | the `release-verify` job | not built |
| No workflow, deploy manifest, or archived file names a fixed image namespace; the published references are the repository owner's | `TestReleasePublishesUnderTheOwnersNamespace` | not built |
| `Dockerfile` and `Dockerfile.release` are byte-identical between the shared runtime markers | `TestRuntimeStagesMatch` | not built |
| A tag whose commit has no successful `verify` run publishes nothing | the `gate-green` job, driven against a red fixture commit | not built |
| A tag with no CHANGELOG section fails the release and the pre-push hook, with the same message from `go tool lateregate release-notes` | the gate's `release-notes` command, the `publish` job | passing for the rule, pipeline not built |
| Every overlay in `deploy/` renders, the rendered base carries every hardening field, the `PrometheusRule` passes `promtool check rules`, and no file under `deploy/` names the stub image | `TestOverlaysRender`, `TestBaseIsConfined`, `TestDeployNamesNoStub`, the rules step | not built |
| The Deployment's strategy surges no replica, so a rollout never opens more than `replicas × LUX_DB_MAX_CONNS` connections | `TestRolloutDoesNotSurgeThePool`, reading the rendered Deployment | not built |
| `luxd check` prints one line per requirement in the order above and exits 1 when any one of them fails, with each requirement removed one at a time | `TestCheckNamesEachFailure`, table-driven over every row | not built |
| The authorizer probe asks `provider.read` on the reserved id, is not entered in the decision cache, and warns rather than fails on an allow | `TestCheckAuthorizerProbe` | not built |
| `luxd check` against a serving installation changes no object and leaves no archive object behind | `TestCheckIsReadOnly` | not built |
| The blocks of `docs/install.md` run green against a kind cluster from the checkout on every push, and against the published artifacts with no checkout on the path after a tag | the `install` and `install-release` jobs | not built |
| The conformance suite passes against the candidate images before anything is published | the `conformance` job, [[018-conformance-suite]]'s `TestContract` | not built |
| A tag writes `test/conformance/testdata/previous/<tag>/` with one resolved manifest per kind and the run's archived records, commits it, and attaches the same bytes to the release | the `conformance` job, with [[018-conformance-suite]]'s `fixture` group reading it on the next release | not built |
| Every row of the version promise table is checked against the tag being cut: a schema change, a removed variable, a removed error code, and a removed record field each require the bump the table names | `TestVersionPromise`, reading the CHANGELOG section and the committed OpenAPI and variable tables | not built |
