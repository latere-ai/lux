---
title: "The base path in the place of /v1: one version segment in every address behind a shared origin"
status: complete
track: core
depends_on:
  - specs/011-api.md
  - specs/014-agent-client.md
  - specs/018-conformance-suite.md
  - specs/.archive/034-serving-behind-a-shared-origin.md
affects: [internal/config/, internal/api/, internal/check/, cmd/luxd/, client/, internal/luxcli/, test/conformance/, test/e2e/, .github/workflows/verify.yml, docs/, specs/002-repository-scaffold.md]
effort: medium
created: 2026-09-26
updated: 2026-09-26
author: changkun
---

# The base path in the place of /v1

## Overview

[034-serving-behind-a-shared-origin](034-serving-behind-a-shared-origin.md)
moves the whole public listener under `LUX_BASE_PATH` as a plain prefix.
Behind an origin that already versions its namespace, every control
plane address then carries two version segments:
`https://api.example.com/v1/models/v1/models/{name}`. The outer `/v1` is
the origin's and the inner one is this core's own.

This spec adds a second way to mount under a base path, the one a sibling
control plane on the same kind of origin already uses: the base path
stands in the place of the control plane's `/v1`, so every public address
carries one version segment. It is an opt-in, `LUX_BASE_PATH_MODE=replace`.
The default, `prefix`, is spec 034's mount unchanged, so an installation
that upgrades serves the addresses it served until its operator sets the
new value.

## Current state

On 2026-09-26, `v0.8.0`:

| Fact | Where |
|---|---|
| `LUX_BASE_PATH` is read, held to one clean spelling, and to the path of `LUX_PUBLIC_URL` | `internal/config/api.go` `basePath`, `internal/config/config.go` after both loaders |
| The public listener is wrapped by `mountAt`, which trims the base and serves the rooted mux | `cmd/luxd/basepath.go` |
| The served OpenAPI document puts the base in front of every path and names `LUX_PUBLIC_URL` as its server | `internal/api/openapi.go` `openAPIJSONAt` |
| `/.well-known/lux` names the control plane at `LUX_PUBLIC_URL + "/v1"` and each door at `LUX_PUBLIC_URL + "/" + dialect` | `internal/api/self.go` `wellKnown` |
| The `client` package, the `lux` command and the tunnel agent send every control plane request to `BaseURL + "/v1/..."` | `client/client.go` `Do`, `client/tunnel/agent.go` |
| The conformance suite requires the discovery document's `api` to end in `/v1` and derives the base by trimming it | `test/conformance/api.go` `case011WellKnown`, `test/conformance/client.go` `basePath` |
| A Model's name carries slashes, and its item route takes the rest of the path | `internal/api/handler.go` `routes`, `{name...}` |

## Design

### The setting

`LUX_BASE_PATH_MODE`, one of two values, read at load:

| Value | Mount |
|---|---|
| `prefix`, the default | spec 034: every route of the public listener answers under the base with its own path appended; the control plane is at `<base>/v1` |
| `replace` | the base stands in the place of the control plane's `/v1`: `/v1/<rest>` answers at `<base>/<rest>`; every other public route answers at `<base>` plus its own path |

| Rule at load | Reason |
|---|---|
| Unset or blank is `prefix` | An installation that upgrades is unchanged |
| A value other than the two is a problem naming the variable | A typo would otherwise pick a mount silently |
| `replace` with `LUX_BASE_PATH` unset is a problem naming both | With no base there is nothing to stand in the place of `/v1` |

### The addresses under `replace`

Base `/v1/models` at `https://api.example.com`:

| Route at the root | Under `prefix` (spec 034) | Under `replace` |
|---|---|---|
| `/v1/providers`, `/v1/providers/{name}` | `/v1/models/v1/providers/...` | `/v1/models/providers/...` |
| `/v1/providers/{name}/tunnel`, `.../tunnel/carry` | `/v1/models/v1/providers/{name}/tunnel...` | `/v1/models/providers/{name}/tunnel...` |
| `/v1/models`, `/v1/models/{name...}` | `/v1/models/v1/models/...` | `/v1/models/models/...` |
| `/v1/keys`, `/v1/keys/{name}`, `.../fence`, `.../rotate` | `/v1/models/v1/keys/...` | `/v1/models/keys/...` |
| `/v1/budgets`, `/v1/budgets/{name}` | `/v1/models/v1/budgets/...` | `/v1/models/budgets/...` |
| `/v1/usage`, `/v1/usage/redact`, `/v1/requests`, `/v1/self` | `/v1/models/v1/usage`, ... | `/v1/models/usage`, ... |
| `/v1/openapi.json` | `/v1/models/v1/openapi.json` | `/v1/models/openapi.json` |
| `/openai/v1/...`, `/anthropic/v1/...`, `/gemini/v1beta/...`, `/lux/v1/...` | `/v1/models/openai/v1/...`, ... | unchanged from `prefix` |
| `/.well-known/lux` | `/v1/models/.well-known/lux` | unchanged from `prefix` |
| `/version` | `/v1/models/version` | unchanged from `prefix` |
| `/`, the build identity | `/v1/models` and `/v1/models/` | unchanged from `prefix` |
| `/livez`, `/readyz` | `/v1/models/livez`, `/v1/models/readyz` | not on the public listener |

The doors do not move, so an SDK's base URL is the same under both
values. The probes leave the public listener under `replace` because the
orchestrator reads them on the internal listener, which neither value
touches; `/version` stays public because a client reads the build
identity without a bearer.

#### Why the Models collection is not the base itself

`/v1/models/{name}` would read shorter than `/v1/models/models/{name}`,
and it cannot be served. A Model's name carries slashes and takes the rest
of the path, and names such as `openai/gpt-5`, `keys`, `self` or `version`
are valid. At the base, the item route would share one path space with
the doors, the sibling collections, the discovery document, the build
identity and the base itself, and which of them a path reached would
depend on the names an installation happened to declare. The collection
keeps its segment and every control plane route keeps its own.

#### Collisions the mount has to keep out

Under `replace` the first segment after the base selects a door, the
discovery document, the build identity, or a control plane collection.
The two sets are disjoint today: the control plane's first segments are
`providers`, `models`, `keys`, `budgets`, `usage`, `requests`, `self` and
`openapi.json`, and the others are `openai`, `anthropic`, `gemini`, `lux`,
`.well-known` and `version`. A test reads the control plane's first
segments from the route table and fails the day a route would shadow one
of the others.

`<base>/v1/...` answers nothing under `replace`: it reaches the control
plane as `/v1/v1/...`, which is not in the route table.

### The mount

Under `replace` the wrapper is an outer mux, as under `prefix`, with
patterns for the routes that keep their own path and one for the rest:

| Pattern | Serves |
|---|---|
| `GET <base>`, `GET <base>/{$}` | the build identity |
| `GET <base>/version` | the probes handler's `/version` |
| `<base>/openai`, `<base>/openai/`, and the same for `anthropic`, `gemini`, `lux`; `<base>/.well-known/lux` | the rooted mux, the base trimmed |
| `<base>/` | the rooted mux, the base replaced by `/v1` |

The base is trimmed from `URL.Path` and from `URL.RawPath`; a `RawPath`
that does not carry the base literally is dropped, so the escaped form
never disagrees with the decoded one. The rooted mux is the one spec 034
wraps, unchanged: the handlers, the request log, the metric route labels
and the authorizer's `request` see the rooted path under every mount.

### The documents

| Document | Under `replace` |
|---|---|
| `GET <base>/openapi.json` | every `/v1/<rest>` path as `<base>/<rest>`, every other path as `<base>` plus the path, and one `servers` entry equal to `LUX_PUBLIC_URL` |
| `GET <base>/.well-known/lux` | `api` is `LUX_PUBLIC_URL`, `openapi` is `api + "/openapi.json"`, each door `LUX_PUBLIC_URL + "/" + dialect` as today |

The committed `api/openapi.yaml` stays the rooted shape. With no base the
served document is byte for byte the committed one, as today.

### The clients in this tree

The `client` package, the `lux` command and the tunnel agent are given
an installation's public URL and today send a control plane request to
`BaseURL + "/v1/..."`. They cannot infer the mount from the URL's path,
because an installation under `prefix` has a path too. The discovery
document already tells them: under both values each door is `P + "/" +
dialect` for one address `P`, and `api` is `P + "/v1"` under `prefix` or
at the root, and `P` itself under `replace`.

- `client.Client` gains `BaseReplacesV1`: set, a request path under `/v1`
  is sent to `BaseURL` with that segment removed. `Discover` reads
  `/.well-known/lux` and sets it. Only the relation between `api` and the
  doors is read, never a host, so a bearer is never sent to an address
  the document named, and a client that reaches the installation at
  another address, such as a cluster-internal one, keeps using its own.
- The `lux` command discovers once before a control plane command. A
  discovery that fails leaves the rooted shape, and the command's own
  request reports the failure in its own terms.
- The tunnel agent discovers at the start of each session, so an agent
  that reconnects after its installation changed mount follows it.

### The conformance suite

The suite takes the address `P` from the doors, which do not move,
accepts `api` as `P + "/v1"` or `P`, and reads every route template
relative to that `api`. The Model item template is recognized by its
last two segments rather than by a `/v1/models/` prefix, which under any
base path matched every route. The same case files run against a rooted
installation, one under `prefix`, and one under `replace`.

## Not in this spec

- Any change to `prefix`, or to its default. Retiring it is a later,
  breaking change with its own CHANGELOG entry.
- An alias under `replace` for the `prefix` addresses. `<base>/v1/...`
  answers `not_found`.
- The routing object, overlay or client configuration of any particular
  installation.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `LUX_BASE_PATH_MODE` unset is `prefix`; `prefix` and `replace` are read; another value is a problem naming the variable; `replace` without `LUX_BASE_PATH` is a problem naming both | `TestBasePathModeRules` in `internal/config` |
| 2 | Under `replace` every control plane route answers at `<base>/<rest>`, the doors, the discovery document, `/version` and the build identity at `<base>` plus their own path, the probes not at all, and nothing at the root or at `<base>/v1/...` | `TestBasePathReplaceMovesTheControlPlane` in `cmd/luxd`, table driven over every route the public mux registers |
| 3 | Under `prefix` the listener is spec 034's | `TestBasePathMovesThePublicListener` and `TestBasePathEmptyIsTheRoot`, unchanged |
| 4 | No control plane route's first segment under `replace` shadows a door, the discovery document or the build identity | `TestReplacedControlPlaneShadowsNoOtherRoute` in `internal/api` |
| 5 | The served document under `replace` names every route at the address it answers at and `servers` is `LUX_PUBLIC_URL`; the discovery document's `api` is `LUX_PUBLIC_URL` | `TestServedDocumentUnderReplace` and `TestWellKnownUnderReplace` in `internal/api` |
| 6 | `client.Client` with `BaseReplacesV1` sends `/v1/keys` to `BaseURL + "/keys"`; `Discover` sets it under `replace`, leaves it unset under `prefix` and at the root, and refuses a document that names neither shape | `TestDiscoverReadsTheMount` in `client` |
| 7 | The `lux` command and the tunnel agent reach a `replace` installation through its public URL | `TestCLIUnderReplacedBasePath` and `TestTunnelUnderReplacedBasePath` in `cmd/luxd` |
| 8 | The conformance suite is green against an installation under `replace` | `TestE2EConformanceUnderReplacedBasePath` in `test/e2e`, a step of the `conformance-twice` job |
| 9 | `luxd check` and the start-up line report the mode | `TestCheckCommand` in `cmd/luxd`, widened |
| 10 | `docs/configuration.md` and `.env.example` carry `LUX_BASE_PATH_MODE`, `docs/api.md` states the addresses under each mode, and spec 002's table carries the row | `TestConfigurationReferenceIsCurrent` in `internal/config`, and the files read against this spec |

## Outcome

Implemented on 2026-09-26 for `v0.9.0`. Every criterion holds; three
details differ from the text above.

- Criterion 1: `TestBasePathModeRules` in `internal/config`.
- Criteria 2 and 3: `TestBasePathReplaceMovesTheControlPlane` in
  `cmd/luxd` asserts every control plane collection at the base, a Model
  named like a door answered by the control plane, a Budget read through
  an escaped path, `<base>/v1/self` and the probes answered `not_found` by
  the control plane, the doors, the documents and the build identity at
  their own paths, a bare 404 at the root, the probes on the internal
  listener, and the two start-up lines. The spec 034 tests are unchanged
  apart from `mountAt`'s new argument.
- Criteria 4 and 5: `TestReplacedControlPlaneShadowsNoOtherRoute`, which
  reads the patterns the route table registered, `TestRoute`,
  `TestServedDocumentUnderReplace` and `TestWellKnownUnderReplace` in
  `internal/api`.
- Criterion 6: `TestDiscoverReadsTheMount` in `client`.
- Criterion 7: `TestCLIUnderReplacedBasePath` and
  `TestTunnelUnderReplacedBasePath` in `cmd/luxd`, each run under both
  values, and each failing with the discovery removed.
- Criterion 8: `TestE2EConformanceUnderReplacedBasePath` in `test/e2e`, a
  fourth step of the `conformance-twice` job.
- Criteria 9 and 10: `TestCheckCommand` widened; `docs/configuration.md`
  and `.env.example` regenerated; `docs/api.md` gained "Under a base
  path"; spec 002's table carries the row.

Divergences:

- `Discover` sends nothing when `BaseURL` has no path and clears
  `BaseReplacesV1`: at an installation's root the control plane is at
  `/v1` under both values, because `replace` needs a base path. The `lux`
  command and the tunnel agent therefore discover only for a URL with a
  path, and their tests against a rooted server see no extra request.
- The `lux` command returns a failed discovery as the command's error
  rather than falling back to the rooted shape. A fallback turns a
  transient failure against a `replace` installation into
  `GET /v1/v1/... is not in the route table`, which names nothing the
  caller did. The tunnel agent logs the failure and keeps the `/v1`
  routes, so `Run` keeps returning only the errors it documents.
- The conformance suite's per-request hold selected every URL under the
  API base. Under `replace` the doors, the discovery document and
  `/version` sit under that base, so the hold now selects control plane
  routes alone, by the same set of first segments the collision guard
  reads.
