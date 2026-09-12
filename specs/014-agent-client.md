---
title: "Agent client: the lux command and the skill"
status: drafted
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/011-api.md
affects: [cmd/lux/, internal/luxcli/, internal/luxclient/, skills/lux/, docs/cli.md, .lateregate.yaml]
effort: medium
created: 2026-09-13
updated: 2026-09-13
author: changkun
---

# Agent client

## Overview

`lux` is one binary that speaks the `/v1` API from a shell or from an
agent: apply a manifest of any kind, read one object, list a kind,
delete, rotate a Key, read usage, ask a door what models a Key may
call, attach a local runtime, and say who the token belongs to. It is
small and dependency-light because it runs where agents run, next to
the model rather than next to the cluster.

Three properties make it usable by a program rather than only by a
person. Output is JSON by default and is the server's own bytes, so a
pipe into `jq` needs no knowledge of this command. Exit codes are three
and mean one thing each, so a caller knows whether to fix its arguments
or to give up. Secrets have one direction: a Key's value and a
Provider's credential go out of the process or into it, never both, and
neither reaches stderr.

This spec owns the command set, the flags, the output, the exit codes,
the client package, and the skill file that teaches an agent the
command. The routes it calls are [[011-api]]'s and the manifest it
sends is [[003-manifest-contract]]'s; `lux serve`'s protocol is
[[013-tunnelled-runtimes]]'s and only its flags are here.

## Current state

Nothing is built. The hosted gateway this design is extracted from has
no command: its surfaces are a dashboard and a hand-written HTTP API,
so every script against it is `curl` with a bearer and a hand-built
body, and there is nothing an agent can be given that teaches it the
API in one file.

## Design

### Reaching the server

| Variable | Flag | Purpose |
|---|---|---|
| `LUX_URL` | `--url` | the gateway's public URL; the base of every route |
| `LUX_TOKEN` | `--token` | a bearer from an issuer the gateway lists ([[006-identity]]); every `/v1` command sends it |
| `LUX_TOKEN_FILE` | `--token-file` | a file holding the same, read per request rather than once, so a token a process refreshes on disk is picked up by a long `lux serve` |
| `LUX_KEY` | `--key` | a Key value; the credential of the door commands, `lux models` today |

Nothing else is read from the environment: no configuration file, no
login, no cached credential. A token comes from the caller's issuer,
which is the one place [[006-identity]] says a person's credential
comes from, and this command mints none. `LUX_TOKEN` and
`LUX_TOKEN_FILE` together is a usage error; neither, on a `/v1`
command, is a usage error naming both.

The two credentials never cross planes. A `/v1` command with `LUX_KEY`
and no token is a usage error rather than a request, because the
gateway would answer `unauthenticated` and the message would be about
the server rather than about the mistake; a door command with a token
and no Key is the same. That mirrors the boundary [[006-identity]]
draws, one step earlier.

### Commands

`<kind>` is `provider`, `model`, `key`, or `budget`, singular or
plural. `<name>` is a name or a prefixed id, addressed as [[011-api]]
addresses it, so a Model's two-segment name works unescaped.

| Command | Route | Notes |
|---|---|---|
| `lux apply -f <file>... [--credential-from-env NAME] [--if-match <version>]` | `PUT /v1/{kind}s/{name}` per document | the kind and the name come from the document; several files and several YAML documents apply in file order, and the first refusal stops the run |
| `lux get <kind> <name>` | `GET /v1/{kind}s/{name}` | one object with its `status` |
| `lux list <kind> [-l k=v]... [--owner s] [--source s] [--provider p] [--limit n]` | `GET /v1/{kind}s` | follows `next` to the end unless `--limit` stops it |
| `lux delete <kind> <name> [--if-match <version>]` | `DELETE /v1/{kind}s/{name}` | prints nothing on success |
| `lux keys rotate <name>` | `POST /v1/keys/{name}/rotate` | prints the new value once |
| `lux usage [--key k]... [--model m]... [--provider p]... [--owner s]... [--since d] [--from t] [--to t] [--by d]... [--interval i] [--label k=v]...` | `GET /v1/usage` | `--since 24h` is `--from now-24h`; the parameters are [[009-usage-and-metering]]'s |
| `lux requests [the same filters] [--status s] [--error c] [--limit n]` | `GET /v1/requests` | records, newest first |
| `lux models` | `GET /lux/v1/models` with `LUX_KEY` | the door's view: the Models this Key may call now ([[004-request-path]]) |
| `lux serve --dialect d --upstream u --as n [...]` | [[013-tunnelled-runtimes]] | attaches a local runtime as a Provider |
| `lux whoami` | `GET /v1/self` | the subject, the claims, the policy, and the limits this server holds |

`lux -version` prints `lux <version> (<commit>, <date>)` and exits 0,
the shape `luxd -version` prints ([[002-repository-scaffold]]). The
first argument without a leading dash selects the command; an unknown
one is a usage error.

There is no `lux create`, no `lux edit`, and no `lux patch`. Apply is
create-or-update on the server ([[011-api]]), so one verb covers both,
and the loop for a change is `lux get`, edit the file, `lux apply`,
which closes because `status` in an applied manifest is ignored.

#### `lux apply` and the credential

`--credential-from-env NAME` reads the named environment variable and
sends its value as a `Provider`'s `spec.credential.value`, so a
credential reaches the server from the environment and is never written
into a file a repository might hold. The rules:

- The flag is repeatable and takes `NAME` or `<provider name>=NAME`.
  The bare form applies to the one `Provider` among the documents; with
  two Providers and no `=`, it is a usage error naming both.
- A document that already carries `spec.credential` and is also named
  by the flag is a usage error, refused before any request, rather than
  the server's `exclusive_fields` after one.
- An unset or empty variable is a usage error naming the variable.
- The value is put into the decoded object and the object is sent as
  JSON. It is never printed, never logged, and never written to a
  temporary file.

#### `lux serve`

| Flag | Default | Purpose |
|---|---|---|
| `--dialect` | none, required | the dialect the local runtime speaks: `openai`, `anthropic`, `gemini`, or `lux` |
| `--upstream` | none, required | the runtime's base URL on this machine; it stays on this machine ([[013-tunnelled-runtimes]]) |
| `--as` | none, required | the `Provider` name to apply and attach |
| `--carriers` | `4` | streams parked at the gateway |
| `--include`, `--exclude` | empty | globs written into the Provider's `discovery` |
| `--label k=v` | empty | labels written into the Provider's `metadata` |
| `--no-apply` | false | attach to an existing Provider instead of applying one |

`lux serve` runs until it is stopped, reconnecting with backoff from 1
second to 30 seconds with full jitter while the gateway is reachable
and the token verifies, and exiting 1 when the session is closed for a
reason a retry cannot fix (`token_expired`, `provider_deleted`,
`forbidden`). `SIGINT` and `SIGTERM` close the session cleanly, so the
Provider is `Unreachable` at once rather than at the registry TTL.

### Output

`-o json` is the default and is the response body as received, never
decoded and re-encoded, so field order and bytes are the server's. For
a list that spanned pages, the items of every page are concatenated
into one envelope with `next` empty and each item's bytes unchanged.

`-o yaml` renders the same value locally, because the server serves
JSON only ([[011-api]]). `-o table` renders the columns below; a field
the client does not know is not a column, so a newer server is readable
from an older client.

| Kind | Columns | Wide adds |
|---|---|---|
| Provider | `NAME`, `DIALECT`, `BASEURL`, `HEALTH`, `MODELS`, `AGE`, `OWNER` | `ID`, `CREDENTIAL`, `TUNNEL` |
| Model | `NAME`, `SOURCE`, `TARGETS`, `AVAILABLE`, `PRICED`, `AGE`, `OWNER` | `ID`, `CONTEXT`, `MODALITIES` |
| Key | `NAME`, `PREFIX`, `STATE`, `MODELS`, `BUDGET`, `EXPIRES`, `OWNER` | `ID`, `RPM`, `TPM`, `SPEND` |
| Budget | `NAME`, `STATE`, `AMOUNT`, `SPENT`, `REMAINING`, `RESETS`, `OWNER` | `ID`, `WINDOW`, `HARD`, `KEYS` |

`BASEURL` of a tunnelled Provider is empty and `TUNNEL` carries
`status.tunnel.state`. `CREDENTIAL` is `set` or `unset` and a version,
never a value; there is no column, in any mode, that could carry one.

A Key's value appears in exactly two places: the `status.value` of the
body `lux apply` printed for a create, and the body `lux keys rotate`
printed. Under `-o table` it is one line after the table,
`key: lux_...`, followed by the sentence that it is shown once. It goes
to stdout and never to stderr, so a shell that captures stdout captures
it and a shell that logs stderr does not.

### Exit codes

Three, and the rule that separates them is whether a request was made.

| Exit | Means | When |
|---|---|---|
| 0 | the command did what it was asked | every 2xx |
| 1 | the server refused, failed, or could not be reached | every response outside 2xx, and every dial, TLS, or timeout failure before one |
| 2 | the command was wrong and nothing was sent | an unknown command, an unknown or missing flag, a file that does not exist or does not decode, a missing or conflicting credential variable, a `--credential-from-env` problem |

2 means fix the arguments; 1 means the arguments were understood and
the answer was no. A caller that needs to know which no it was reads
the code the refusal printed, which is [[011-api]]'s and is stable.

A refusal prints the API's `message` on stderr as one line, verbatim.
It is the fixed user sentence of the code and the command never builds
one of its own, so the sentence a person reads is the sentence the
error table owns. With `-v` the code, the paths, the developer detail,
and the request id follow on their own lines.

```
$ lux apply -f provider.yaml
A field has a value it cannot take.
$ lux apply -f provider.yaml -v
A field has a value it cannot take.
code: invalid_field
paths: spec.baseURL
detail: a loopback host needs LUX_UPSTREAM_ALLOW_PRIVATE
request: req_01J9ZK2P7Q8R9S0T1U2V3W4X63
```

Four codes get one line more than the sentence, because the next step
is not in the sentence:

| Code | The command adds |
|---|---|
| `rate_limited`, `spend_exceeded`, `budget_exhausted` | `retry after <Retry-After> seconds` |
| `conflict` | `read it again with lux get and re-apply` |
| `unsupported_version` | the server's `apiVersion` and version from `GET /.well-known/lux` |
| `authorizer_unavailable`, `store_unavailable` | nothing; exit 1 and the sentence already says retry |

An unknown code is printed with its sentence and exits 1 like any
other, so a server newer than the command is still usable.

### Client policy

No retry, ever. A caller that wants one has the exit code and the
`Retry-After` line; a command that retried by itself would turn a
`conflict` into a lost edit and a `rate_limited` into a longer flood.
The one exception is `lux serve`'s reconnect, which is a session and
not a request.

A 10 second deadline to the first response byte and none on a stream.
`User-Agent: lux/<version>`. Every request carries the token as
`Authorization: Bearer`, and `Lux-Request-Id` is read from the response
and printed under `-v`; the command never sends one, because
[[011-api]] replaces a caller's.

The transport reads `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`,
because `lux` runs on a person's machine and reaching the gateway
through that machine's proxy is what a person expects. The gateway's
own upstream client refuses a proxy, for the custody reason
[[005-providers]] gives; the two are different processes with different
reasons, and this spec says so rather than leaving a reader to wonder
which rule applies where.

### The packages

`internal/luxcli` is the command: flag parsing, the file walk, the
output renderers, the exit codes, and the `lux serve` loop.
`internal/luxclient` is the typed client of the `/v1` API, one method
per route of [[011-api]], returning the response bytes beside the
decoded value so `-o json` can print what arrived.

`internal/luxclient` is not exported, for one reason that has a
timeline attached. `latere.ai/x/pkg/luxsdk` is the data plane client a
program already has: it speaks the lux dialect against a door with a
Key, which is what a workload needs. The control plane client has one
consumer, this command, and [[011-api]] serves an OpenAPI document a
platform generates its own client from. A package with one consumer
inside the repository belongs in `internal/` ([`CONTRIBUTING.md`]); it
moves to the module root when a second consumer appears, which is the
same rule every other package here follows.

The build list of `./cmd/lux` is the standard library,
`latere.ai/x/pkg/httpjson` for the error envelope, and this module's
own `manifest` and `manifest/v1`, which reach only the standard library
([[003-manifest-contract]]). No HTTP client library, no command line
framework, no YAML library of the command's own, no store driver, no
identity library, no provider SDK. The `depcheck` gate holds the list
as a row for `./cmd/lux` beside the one for `./cmd/luxd`
([[002-repository-scaffold]]).

`lux apply` decodes with `manifest.Decode`, the same function the
server decodes with. A second decoder in the client would be a second
meaning for a manifest, which [[001-architecture]]'s invariant 1
forbids, and would let the command accept a document the server
refuses. What the command does not do is resolve: defaulting and the
reference checks need the store and the authorizer, so the object goes
to the server as written and the resolved object comes back.

### The skill and the document

`skills/lux/SKILL.md` carries `name` and `description` frontmatter, the
resident cost, and a body that teaches an agent: the two variables, a
minimal manifest per kind, `apply`, `get`, `list`, `models`, `usage`,
the three exit codes, and how to read a refusal with and without `-v`.
It says in one sentence that `LUX_TOKEN` opens `/v1` and `LUX_KEY`
opens a door, and that neither works on the other side, because that is
the mistake an agent makes first.

The claim to test is reachability rather than prose: an agent given
only the skill and the two variables creates a Budget, a Key under it,
and sends a request through a door, which is the agent case of
[[018-conformance-suite]].

`docs/cli.md` is the command table above in the user register, and a
test holds it equal to the binary's `--help` output.

## Not in this spec

The routes, the error table, the preconditions, and the OpenAPI
document ([[011-api]]); the schema the command sends
([[003-manifest-contract]]); the usage parameters and the row shape
([[009-usage-and-metering]]); the tunnel protocol and the registry
([[013-tunnelled-runtimes]]); how the binary is built and shipped
([[017-release-and-installation]]); the suite that proves the server
this command speaks to ([[018-conformance-suite]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every command in the table calls the route in its row with the method, the addressing, and the flags named, and `apply` dispatches on the document's `kind` and `metadata.name` | `TestCommandTable` against an `httptest` server, table-driven | not built |
| A file with three YAML documents of three kinds applies in file order, and a refusal on the second stops before the third | `TestApplyWalksDocumentsInOrder` | not built |
| `--credential-from-env` sends the variable's value as `spec.credential.value`; an unset variable, a document that already carries a credential, two Providers with the bare form, and a non-Provider document are each exit 2 with nothing sent | `TestCredentialFromEnv`, table-driven | not built |
| Every code in [[011-api]]'s error table exits 1 with the table's sentence on stderr and nothing on stdout; every usage error exits 2 with no request made; an unknown code exits 1 | `TestErrorsBecomeExits`, table-driven over every code | not built |
| The built binary carries the three exit codes through the process | `TestBinaryExitCodes`, running `out/lux` as a subprocess for one case of each code | not built |
| The four codes in the extra-line table print their extra line and no other code does | `TestRefusalExtraLines` | not built |
| `LUX_TOKEN`, a token file's contents, a `--credential-from-env` value, and a Key value reach no stderr byte, `-v` included; a Key value reaches stdout exactly once per create and per rotate | `TestSecretsGoOneWay` with canaries | not built |
| `-o json` for one object is byte-identical to the response; a two-page list is one envelope with every item's bytes unchanged and `next` empty; `-o yaml` round-trips to the same value | `TestOutputFidelity` | not built |
| Every column in the table renders for each kind, a tunnelled Provider shows an empty `BASEURL` and its tunnel state, and no column in any mode carries a credential value | `TestColumns`, `TestNoColumnCarriesASecret` | not built |
| A `/v1` command with only `LUX_KEY`, a door command with only `LUX_TOKEN`, and both token variables together are each exit 2 naming the variables | `TestCredentialsDoNotCrossPlanes` | not built |
| With `LUX_TOKEN` unset and a token file that changes between two requests, each request sends the file's current bytes | `TestTokenFileIsReadPerRequest` | not built |
| With `HTTPS_PROXY` set to a refusing address the command fails to reach the server and exits 1, and with it unset it reaches it | `TestClientHonoursProxyVariables` | not built |
| `lux serve` applies the Provider its flags describe, reconnects with backoff across a gateway restart, exits 1 on a close reason a retry cannot fix, and closes cleanly on `SIGTERM` | `TestServeFlags`, `TestServeReconnects`, [[013-tunnelled-runtimes]]'s `TestCleanDisconnectIsImmediate` | not built |
| An agent given only `skills/lux/SKILL.md` and the two variables creates a Budget, a Key under it, and sends one request through a door against the stubs of [[015-test-stubs-and-tiers]] | `TestAgentWithOnlyTheSkill` | not built |
| `docs/cli.md` equals the binary's `--help` for every command | `TestCLIDocIsCurrent` | not built |
| `./cmd/lux`'s build list is the standard library, `latere.ai/x/pkg/httpjson`, and this module's `manifest` and `manifest/v1` | the `depcheck` gate | not built |
