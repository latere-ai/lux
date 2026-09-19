---
title: "Agent client: the lux command and the skill"
status: complete
track: core
depends_on:
  - specs/003-manifest-contract.md
  - specs/011-api.md
affects: [cmd/lux/, internal/luxcli/, client/, skills/lux/, docs/cli.md, .lateregate.yaml]
effort: medium
created: 2026-09-13
updated: 2026-09-16
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

Built: `cmd/lux` is the wiring, `internal/luxcli` the command tree, the
flag forms, the output renderers, the exit codes, the error rendering,
and the `lux serve` loop, and `client` the `/v1` client with
`next_cursor` paging and the token source. `skills/lux/SKILL.md`
and `docs/cli.md` are in the tree, the `depcheck` row for `./cmd/lux`
is in `.lateregate.yaml`, and every acceptance row has its test.
`lux serve` is one row of the command table over
[[013-tunnelled-runtimes]]'s `internal/tunnel/agent`: the `PUT` of the
Provider goes through this package's own client, and the same token
source serves the session's requests and its heartbeats.

## Design

### Reaching the server

| Variable | Flag | Purpose |
|---|---|---|
| `LUX_URL` | `--url` | the gateway's public URL; the base of every route |
| `LUX_TOKEN` | `--token` | a bearer from an issuer the gateway lists ([[006-identity]]); every `/v1` command sends it |
| `LUX_TOKEN_FILE` | `--token-file` | a file holding the same, read per request rather than once, so a token a process refreshes on disk is picked up by a long `lux serve` |
| `LUX_KEY` | `--key` | a Key value; the credential of the door commands, `lux models` today |

`LUX_BASE_URL` and `LUX_API_KEY` are read as fallbacks for `LUX_URL`
and `LUX_KEY` on a door command, and for nothing else. They are
`latere.ai/x/pkg/luxsdk`'s own `EnvBaseURL` and `EnvAPIKey`, the two
variables a program that already calls a door through that client has
set; a shell that exported them for an SDK does not export two more for
this command. `LUX_URL` and `LUX_KEY` win where both are set. Nothing
else is read from the environment: no configuration file, no cached
credential, no profile.

There is no `lux login` and no device grant. A token comes from the
caller's own issuer, which is the one place [[006-identity]] says a
person's credential comes from, and this command mints none, so the
binary embeds no issuer URL, no client id, and no audience, which is
[[001-architecture]]'s invariant 8 applied to the one artifact a person
downloads. A person gets a token the way that issuer gives one out, an
`oidc` helper, a platform's own command, or a client credentials grant
in a script, and puts it in `LUX_TOKEN`; a token that a helper
refreshes on disk goes in `LUX_TOKEN_FILE`, which is read per request
so a long `lux serve` picks the new one up without a restart.
`LUX_TOKEN` and `LUX_TOKEN_FILE` together is a usage error; neither, on
a `/v1` command, is a usage error naming both.

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
| `lux list <kind> [-l k=v]... [--owner s] [--source s] [--provider p] [--limit n]` | `GET /v1/{kind}s` | the selectors are [[011-api]]'s; the command follows `next_cursor` to the end and prints one envelope, stopping after `--limit` items, which is a count of items and not the page size the route takes |
| `lux delete <kind> <name> [--if-match <version>]` | `DELETE /v1/{kind}s/{name}` | prints nothing on success |
| `lux keys rotate <name>` | `POST /v1/keys/{name}/rotate` | prints the new value once |
| `lux usage [--key k]... [--model m]... [--provider p]... [--owner s]... [--since d] [--from t] [--to t] [--by d]... [--interval i] [--label k=v]...` | `GET /v1/usage` | `--since 24h` is `--from now-24h`; the parameters are [[009-usage-and-metering]]'s |
| `lux requests [the same filters] [--status s] [--error c] [--limit n]` | `GET /v1/requests` | records, newest first |
| `lux models` | `GET /lux/v1/models` with `LUX_KEY` | the door's view: the Models this Key may call now ([[004-request-path]]) |
| `lux serve --dialect d --upstream u --as n [...]` | [[013-tunnelled-runtimes]] | attaches a local runtime as a Provider |
| `lux whoami` | `GET /v1/self` | the subject, the claims, the policy, and the limits this server holds |

`lux -version` prints `lux <version> (<commit>, <date>)` and exits 0,
the shape `luxd -version` prints ([[002-repository-scaffold]]), and
`lux -help`, with no command or after one, prints that command's usage
and exits 0. Flags are the standard library's, one dash or two, because
the binary carries no command line framework. The first argument
without a leading dash selects the command; an unknown one is a usage
error. Flags and arguments interleave, so `lux get key run-42 -o yaml`
and `lux -o yaml get key run-42` are one command. The global flags are
`--url`, `--token`, `--token-file`, `--key`, `-o`, and `-v`; on `lux
usage` and `lux requests`, which carry a token and never a Key, `--key`
after the command is the filter of the table above and the credential
flag is absent, so the two spellings the table gives never collide.

There is no `lux create`, no `lux edit`, and no `lux patch`. Apply is
create-or-update on the server ([[011-api]]), so one verb covers both,
and the loop for a change is `lux get`, edit the file, `lux apply`,
which closes because `status` in an applied manifest is ignored.

There is no `lux check` either: verifying an installation is the
server's own `luxd check`, which reaches the store, the KEK, and the
operator's endpoints that no client can see
([[017-release-and-installation]]). There is no `lux env` printing an
SDK's variables and no `lux invoke` sending a prompt: a door **is** an
SDK base URL and a Key **is** its api key, so the two variables a
caller exports are its SDK's own, and the program that sends the prompt
is the caller's SDK or `latere.ai/x/pkg/luxsdk`. A command that wrapped
either would be a second, smaller client of the doors that the
conformance suite would then have to prove.

#### Flag forms

The common cases need no file. Each `create` builds the manifest from
its flags and applies it through the same `PUT` as `lux apply`, so the
server sees one shape and the flags are a client convenience the
conformance suite never has to know about.

| Command | Builds |
|---|---|
| `lux keys create <name> --models m[,m...] [--rpm n] [--tpm n] [--spend amount[/window]] [--budget b] [--ttl d] [--label k=v]... [--passthrough]` | a `Key`; prints the value once |
| `lux providers create <name> --dialect d --base-url u [--credential-from-env NAME] [--header k=v]...` | a `Provider` |
| `lux models create <name> --target provider/model[@weight[:priority]]... [--price-input p --price-output p] [--fallback onError\|never]` | a `Model` |
| `lux budgets create <name> --amount a [--currency c] [--window w] [--soft]` | a `Budget` |

`--dry-run` on any of them prints the manifest and applies nothing, so
a flag form is also how a first file gets written: JSON by default,
YAML under `-o yaml`, and YAML under `-o table` and `-o wide` too, since
a manifest is a document and not a table. The printed manifest carries
`apiVersion`, `kind`, `metadata`, and `spec` and never `status`, which
the server owns, and never a credential value, which
`--credential-from-env` puts into the request body alone. A flag whose
zero value has a meaning, `--rpm 0` for no limit, is sent only when
given, so an absent flag leaves the server's default in place.

#### `lux apply` and the credential

`--credential-from-env NAME` reads the named environment variable and
sends its value as a `Provider`'s `spec.credential.value`, so a
credential reaches the server from the environment and is never written
into a file a repository might hold. The rules:

- The flag is repeatable and takes `NAME` or `<provider name>=NAME`.
  The bare form applies to the one `Provider` among the documents; with
  two Providers and no `=`, it is a usage error naming both.
- A document that already carries a credential source,
  `spec.credential.value` or `spec.credential.valueFrom`, and is also
  named by the flag is a usage error, refused before any request, rather
  than the server's `exclusive_fields` after one. A `credential` block
  carrying `header` or `scheme` alone merges with the flag, since it
  names no source.
- An unset or empty variable is a usage error naming the variable.
- The value is put into the decoded object and the object is sent as
  JSON, with `status` dropped, because the object's own encoders skip
  the write-only value and the body is the one place it may travel. It
  is never printed, never logged, and never written to a temporary
  file.
- A document the flag does not name is sent as written, its own bytes
  as YAML or JSON, so the server decodes what the person wrote.

A file is cut into documents at every line that is `---` alone or
followed by a space or a tab, the rule `kubectl` applies; a document of
blank lines and comments alone is skipped. Each document is decoded
with `manifest.Decode` and an empty hint before anything is sent, so a
document the server would refuse is exit 2 here with the code's fixed
sentence after `<file>: document <n>:` and the paths in parentheses,
and a document without `metadata.name` is exit 2 too, since apply is by
name. `--if-match` with more than one document is a usage error, one
version naming one object.

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
reason a retry cannot fix: `superseded`, because another agent holds
the Provider; `provider_deleted`; and `token_expired` after one retry
with a fresh token from `--token-file`, terminal only when none is
available. `draining` reconnects at once, and a `forbidden` answer is a
refusal to connect, not a close reason ([[013-tunnelled-runtimes]]).
When the file behind `--token-file` changes, `lux serve` sends the
fresh token in a heartbeat frame rather than reconnecting. `SIGINT` and `SIGTERM` close the session cleanly, so the
Provider is `Unreachable` at once rather than at the registry TTL.

What `lux serve` prints is what every other command prints: the
applied Provider, the server's own bytes, on stdout, and one refusal
on stderr at the end. A close reason a retry cannot fix is rendered as
a refusal is: the reason is the code, so the word the close frame
carried is the word `-v` prints, and each of the three has one fixed
sentence. The session's own lines, a reconnect and a broken stream, go
to stderr through a log handler, at warning normally and at info under
`-v`, because they are the developer's register and not the answer.

### Output

`-o json` is the default and is the response body as received, never
decoded and re-encoded, so field order and bytes are the server's. For
a list that spanned pages, the items of every page are concatenated
into one envelope with `next_cursor` empty and each item's bytes unchanged.

`-o yaml` renders the same value locally, because the server serves
JSON only ([[011-api]]): the body is decoded with its member order kept
and encoded by the same YAML library `manifest` decodes with, so no
second YAML library joins the build and a string that would read as
another type is quoted. `-o table` renders the columns below and
`-o wide` adds the wide columns; a field the client does not know is
not a column, so a newer server is readable from an older client. A
response that is none of the four kinds, `whoami`, `usage`,
`requests`, and the door's model list, renders generically under
`table`: one `key: value` line per member for an object, and the first
item's scalar members as columns for a list.

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
printed. Under `-o table` and `-o wide` it is one line after the table,
`key: lux_...`, followed by a line with the sentence that it is shown
once. It goes to stdout and never to stderr, so a shell that captures
stdout captures it and a shell that logs stderr does not.

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
| `rate_limited`, `spend_exceeded`, `budget_exhausted` | `retry after <Retry-After> seconds`, or `retry later` when the header is absent |
| `conflict` | `read it again with lux get and re-apply` |
| `unsupported_version` | `server: <version> serves <apiVersion>` from `GET /.well-known/lux`, and nothing when that document cannot be read |
| `authorizer_unavailable`, `store_unavailable` | nothing; exit 1 and the sentence already says retry |

An unknown code is printed with its sentence and exits 1 like any
other, so a server newer than the command is still usable. A failure
before a response, a dial, a TLS handshake, a refusing proxy, or the
deadline, is exit 1 with the client's own fixed sentence and the code
`unreachable` under `-v`; a response outside 2xx whose body is not the
envelope, or a 2xx list whose body is not a list, is exit 1 with the
code `unreadable_response`, the sentence saying to check that `LUX_URL`
names a Lux gateway, and the status and an excerpt under `-v`. Those
two codes are the command's own, and the only other sentences it
builds are the close reasons of `lux serve` above, one per reason.
A token file that cannot be read or is empty is exit 2 naming
`LUX_TOKEN_FILE`, since no request was made.

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
output renderers, the exit codes, and, once [[013-tunnelled-runtimes]]
lands its agent package, the `lux serve` loop. `client` is the client
of the `/v1` API, one method per route of [[011-api]], returning the
response bytes as they arrived beside the status and the request id so
`-o json` can print what arrived; it decodes nothing but the error
envelope and a list's members, the latter with each item's bytes kept
so pages fold into one envelope unchanged.

`client` is a root package, `latere.ai/x/lux/client`, because the
control plane client has consumers outside this command: a plane that
applies Providers, Models, Keys, and Budgets to a core it runs, and a
tool that migrates objects into one. A package with one consumer
belongs in `internal/` and moves to the module root when a second
appears ([`CONTRIBUTING.md`]), and the promise it carries there is
[[001-architecture]]'s for a root tree: the Go API is additive within a
module major, and the package owns no policy, decodes no kind, and
dials one address, the base URL its caller hands it.

It does not overlap `latere.ai/x/pkg/luxsdk`, the data plane client a
program already has: that one `POST`s the lux dialect to
`/lux/v1/generate` with a Key in `Authorization: Bearer`, which is what
a workload needs and is a door of [[004-request-path]], not a `/v1`
route. That one holds a Key and speaks one door, this one holds an
issuer token and speaks the whole control plane, and neither can do the
other's work, which is the plane boundary [[006-identity]] draws
expressed as two packages. [[011-api]] serves an OpenAPI document as
the third path, for a consumer that would rather generate a client than
import one. `luxsdk` is not in this command's build list: the one door
command, `lux models`, is a `GET` this client already makes.

The build list of `./cmd/lux` is the standard library,
`latere.ai/x/pkg/httpjson` for the error envelope, and this module's
own `client`, `manifest`, and `manifest/v1`, which reach the standard
library and the one YAML decoder [[003-manifest-contract]] names.
[[001-architecture]]'s dependency paragraph says the same; the schema
packages are this module's own and pull in that decoder alone, so the
binary carries one first-party module, `latere.ai/x/pkg`, and one
third-party one, and this spec is where those two rows are written
down. No HTTP client library, no command line framework, no YAML
library of the command's own, no multiplexer and no WebSocket codec for
`lux serve`, whose transport is `net/http`'s HTTP/2
([[013-tunnelled-runtimes]]), no store driver, no identity library, no
provider SDK, and no OpenTelemetry SDK: the command is a person's
process and exports nothing. The `depcheck` gate holds the list as a
row for `./cmd/lux` in `.lateregate.yaml` beside the one for
`./cmd/luxd` ([[002-repository-scaffold]]).

`lux apply` decodes with `manifest.Decode`, the same function the
server decodes with. A second decoder in the client would be a second
meaning for a manifest, which [[001-architecture]]'s invariant 1
forbids, and would let the command accept a document the server
refuses. What the command does not do is resolve: defaulting and the
reference checks need the store and the authorizer, so the object goes
to the server as written and the resolved object comes back.

### The skill and the document

`skills/lux/SKILL.md` is one file at `skills/<name>/SKILL.md` whose
frontmatter is exactly two keys, `name: lux` and one `description`
sentence saying what the command is for. Nothing else is in the
frontmatter, because an agent harness reads those two keys to decide
whether to load the file at all, and what it costs to keep resident is
then the file's own length, which stays under 200 lines. The body teaches an agent: the two
variables, a minimal manifest per kind, `apply`, `get`, `list`,
`models`, `usage`, the three exit codes, and how to read a refusal with
and without `-v`.
It says in one sentence that `LUX_TOKEN` opens `/v1` and `LUX_KEY`
opens a door, and that neither works on the other side, because that is
the mistake an agent makes first.

The claim to test is reachability rather than prose: an agent given
only the skill and the two variables creates a Budget, a Key under it,
and sends a request through a door, which is the agent case of
[[018-conformance-suite]]. Until [[015-test-stubs-and-tiers]] lands
its stub binaries, the test runs the skill's own manifests against the
gateway assembled in process the way `cmd/luxd` assembles it, with an
`httptest` server on loopback as the provider: the four documents of
the skill's YAML block apply in order, the Key's value opens the door
for `lux models` and for one SDK-shaped `POST` that reaches the stub
carrying the Provider's credential, and the token, the Key, and the
credential each stay on their own side.

`docs/cli.md` is the `-help` output of the root and of every command,
in the table's order, each under its own heading, and a test holds it
equal to the binary's output command by command; `LUX_CLI_DOC_WRITE=1`
on that test rewrites the file.

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
| Every command in the table calls the route in its row with the method, the addressing, and the flags named, and `apply` dispatches on the document's `kind` and `metadata.name` | `TestCommandTable` against an `httptest` server, table-driven | passing, `internal/luxcli` |
| A file with three YAML documents of three kinds applies in file order, and a refusal on the second stops before the third | `TestApplyWalksDocumentsInOrder` | passing, `internal/luxcli`; the file also carries a leading comment, a marker with a trailing comment, and an empty document |
| `--credential-from-env` sends the variable's value as `spec.credential.value`; an unset variable, a document that already carries a credential, two Providers with the bare form, and a non-Provider document are each exit 2 with nothing sent | `TestCredentialFromEnv`, table-driven | passing, `internal/luxcli`, the named form and a `header`-only block among the rows |
| Every code in [[011-api]]'s error table exits 1 with the table's sentence on stderr and nothing on stdout; every usage error exits 2 with no request made; an unknown code exits 1 | `TestErrorsBecomeExits`, table-driven over every code | passing, `internal/luxcli`, over `api.Codes()` |
| The built binary carries the three exit codes through the process | `TestBinaryExitCodes`, running the binary as a subprocess for one case of each code | passing, `cmd/lux`; the binary is built into a temporary directory by the test rather than read from `out/lux`, so the gate's `tempdir` run leaves nothing behind |
| The four codes in the extra-line table print their extra line and no other code does | `TestRefusalExtraLines` | passing, `internal/luxcli` |
| `LUX_TOKEN`, a token file's contents, a `--credential-from-env` value, and a Key value reach no stderr byte, `-v` included; a Key value reaches stdout exactly once per create and per rotate | `TestSecretsGoOneWay` with canaries | passing, `internal/luxcli`, under every output mode and both token sources |
| `-o json` for one object is byte-identical to the response; a two-page list is one envelope with every item's bytes unchanged and `next_cursor` empty; `-o yaml` round-trips to the same value | `TestOutputFidelity` | passing, `internal/luxcli` |
| Every column in the table renders for each kind, a tunnelled Provider shows an empty `BASEURL` and its tunnel state, and no column in any mode carries a credential value | `TestColumns`, `TestNoColumnCarriesASecret` | passing, `internal/luxcli` |
| A `/v1` command with only `LUX_KEY`, a door command with only `LUX_TOKEN`, and both token variables together are each exit 2 naming the variables | `TestCredentialsDoNotCrossPlanes` | passing, `internal/luxcli` |
| With `LUX_TOKEN` unset and a token file that changes between two requests, each request sends the file's current bytes | `TestTokenFileIsReadPerRequest` | passing, `internal/luxcli` |
| A door command with only `LUX_BASE_URL` and `LUX_API_KEY` set reaches the door with that Key; with `LUX_URL` and `LUX_KEY` also set those win; neither fallback is read on a `/v1` command | `TestSDKVariableFallbacks` | passing, `internal/luxcli` |
| The binary contains no issuer URL, no OAuth client id, and no audience string, and no command performs a token exchange of any kind | `TestClientEmbedsNoIssuer`, over `go list -deps` and the string table of the built binary | passing, `cmd/lux`; the claim and parameter names are matched quoted, since a bare `id_token` is a substring of a libc symbol, and `login.example.com` is not among the canaries because `manifest` embeds its golden corpus, whose example owners carry it, into every importer |
| With `HTTPS_PROXY` set to a refusing address the command fails to reach the server and exits 1, and with it unset it reaches it | `TestClientHonoursProxyVariables` | passing, `cmd/lux`, as a subprocess with `HTTP_PROXY`, because the standard transport reads the proxy variables once per process and never proxies loopback, so the target is a name off loopback the proxy answers for |
| Each `create` flag form builds the manifest the table says, applies it through `PUT`, and prints it under `--dry-run` without a request; the manifest `lux keys create` builds resolves identically to the equivalent file | `TestFlagFormsBuildTheManifest`, `TestDryRunAppliesNothing` | passing, `internal/luxcli`; the built body and the equivalent file decode to equal objects, and the Budget pair resolves equally without a Lookup |
| `lux serve` applies the Provider its flags describe, reconnects with backoff across a gateway restart, exits 1 on a close reason a retry cannot fix, and closes cleanly on `SIGTERM` | `TestServeFlags`, `TestServeReconnects`, `TestServeExitsOnACloseReason`, `TestServeTokenExpiryIsTerminalAfterOneRetry`, `TestServeSendsAFreshTokenInAHeartbeat`, [[013-tunnelled-runtimes]]'s `TestCleanDisconnectIsImmediate` | passing, `internal/luxcli`, against a gateway with the tunnel on assembled in process and a fake runtime on loopback, the listener closed and opened again at one address for the restart |
| `skills/lux/SKILL.md` parses with frontmatter of exactly `name` and `description`, and every command and flag it names is in the table above | `TestSkillFrontmatter`, `TestSkillNamesOnlyRealCommands` | passing, `internal/luxcli`; every `lux` line in the skill's shell blocks and prose resolves to a command and its flags, and every code it names is in 011's table |
| An agent given only `skills/lux/SKILL.md` and the two variables creates a Budget, a Key under it, and sends one request through a door against the stubs of [[015-test-stubs-and-tiers]] | `TestAgentWithOnlyTheSkill` | passing, `internal/luxcli`, against the gateway assembled in process with an `httptest` stub provider; the same run against the stub binaries is owned by [[015-test-stubs-and-tiers]] |
| `docs/cli.md` equals the binary's `-help` output for every command in the table, and `lux -help` and `lux -version` each exit 0 | `TestCLIDocIsCurrent`, `TestHelpAndVersionExitZero` | passing, `internal/luxcli` |
| `./cmd/lux`'s build list is the standard library, `latere.ai/x/pkg/httpjson`, this module's `client`, `manifest`, `manifest/v1`, `internal/tunnel/wire`, and `internal/tunnel/agent`, and the YAML decoder they use ([[003-manifest-contract]]), `lux serve` included | the `depcheck` gate over the `./cmd/lux` row of `.lateregate.yaml` | passing: the row admits `latere.ai/x/pkg/httpjson`, `github.com/google/uuid` through it, and `github.com/goccy/go-yaml` through `manifest`, and the two tunnel packages add no third-party module of their own |

## Outcome

2026-09-14. Built as `internal/luxcli` behind `cmd/lux`, with
`client` for the `/v1` routes, `skills/lux/SKILL.md`,
`docs/cli.md`, and the `./cmd/lux` row of `.lateregate.yaml`. The last
command the table lacked, `lux serve`, is `internal/luxcli/serve.go`
over [[013-tunnelled-runtimes]]'s `internal/tunnel/agent`: it applies
the tunnelled Provider its flags describe, holds the session, and
reconnects with full jitter from one second to thirty while the
gateway can be dialled. Every acceptance row of this spec's own
passes; the one row with a second half, the skill's manifests run
against stub binaries rather than an in-process gateway, names
[[015-test-stubs-and-tiers]] as its owner. The gate passes whole, with
`internal/luxcli` at 93.2%.

What was built differs from the first writing in these points, each
carried in the Design above:

- The `/v1` client is the root package `client`, importable as
  `latere.ai/x/lux/client`, where the first writing kept it in
  `internal/`. The trigger is the one that writing named: a second
  consumer. A plane applies Providers, Models, Keys, and Budgets to
  the core it runs through `/v1`, and a migration tool applies the
  objects it exports to one, and neither is this command.
- A close reason a retry cannot fix earns a sentence of its own, so
  the command builds four more sentences than the two the exit-code
  section first allowed. The reason itself is the code, which keeps
  one code to one fixed sentence: `superseded`, `provider_deleted`,
  `token_expired`, and one sentence for a reason a newer gateway sent
  and this command does not know.
- A refused connect is terminal whatever the status, not only
  `forbidden`. The spec's reconnect holds "while the gateway is
  reachable and the token verifies", and a refusal says one of those
  is false; a gateway that cannot be dialled at all is the reconnect's
  case. So a 503 met at the connect during a restart is exit 1 rather
  than a wait, and an installation that wants it waited out restarts
  the command.
- `lux serve` prints the applied Provider on stdout, which the spec
  did not say, and its session's lines on stderr through a log
  handler, at warning normally and at info under `-v`. Without that a
  command that runs for days would say nothing when it reconnects.
- `--carriers` is held to one or more, which the spec gave only a
  default for; zero would park nothing and the session would serve no
  request.
- The Provider the flags build carries no `discovery.mode`, so the
  server's own default stands; the spec named only the globs.
- `token_expired`'s one retry reads the token file again, and when the
  file's bytes did not change the retry meets `unauthenticated` at the
  connect rather than a second `token_expired`. Both endings are exit
  1, and the second close being terminal is proved against a gateway
  that closes every session with the reason.
- The backoff bounds are package variables rather than constants, so a
  test drives several reconnects in a moment; the defaults are the
  spec's second and thirty seconds.
- The bearer of a `/v1` command moved into one `bearerSource`, which
  `controlClient` and `lux serve` share, because the serve loop has to
  know whether the source can be read again before it decides that
  `token_expired` is terminal.

## Public tunnel transport

[[028-public-tunnel-agent]] moves the transport from internal/tunnel/agent to
client/tunnel without changing protocol or reconnect behavior. The command uses
the public Run, Options and close-reason constants. Token sources remain in
client; client/tunnel shares only the private wire codec with the server.
