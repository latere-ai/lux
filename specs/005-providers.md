---
title: "Providers: dialects, credential custody, discovery, health, the upstream client"
status: complete
track: core
depends_on:
  - specs/001-architecture.md
  - specs/003-manifest-contract.md
  - specs/010-state.md
affects: [gateway/, internal/store/, internal/secrets/, internal/serve/, internal/check/, internal/config/]
effort: medium
created: 2026-09-13
updated: 2026-09-14
author: changkun
---

# Providers

## Overview

A `Provider` is one upstream: a dialect, a base URL, the credential Lux
holds for it, and two background jobs that keep the gateway's picture of
it current. This spec owns four things. The routes each dialect serves,
so a contributor knows which paths a door offers and which of them
`latere.ai/x/pkg/llmdialect` models. Credential custody: how a value is
sealed, where the ciphertext lives, when it is opened, and how a key is
rotated. Discovery: how a Provider's own model list becomes `Model`
objects through the same `manifest.Resolve` every other surface uses.
Health: how a Provider is probed or observed, the states it moves
through, and what that signal means to target selection. And the HTTP
client the gateway dials an upstream with, which is the one place
[[001-architecture]]'s invariant 2 is enforced in code.

What happens to a request once a target is chosen is
[[008-routing-and-models]]'s; the door handler and its error table are
[[004-request-path]]'s. This spec ends at the edge of the socket.

## Current state

Built, over the memory store and the file mode of [[010-state]]:
`internal/secrets` holds the custody; `internal/config` the four
rows and the rewrap role's two; `gateway/upstream.go` the client
builder, the credential injection, and the errors the door maps;
`internal/serve` the discovery and health jobs and the two credential
sources; `internal/rewrap` the run of the rewrap role; and `luxd serve`
checks the keys against every stored row, starts the two jobs after the
store and the identity, and stops them with the process, while
`luxd rewrap` reads its two variables and re-wraps every row of the
store `LUX_DB_URL` names ([[010-state]]).
The door handler that takes the client and the credential source is
[[004-request-path]]'s and is built beside this. The readings this pass
fixed where the text was open are written into the Design below, each
beside the rule it settles.

## Design

### Dialects and the routes they serve

A dialect names the wire API an upstream speaks and the door a caller
uses ([[003-manifest-contract]]). [[004-request-path]] owns the door
table and the three route classes; this table is the upstream side of
the same operations: the path under a Provider's `baseURL` and the
codec, if any, that `latere.ai/x/pkg/llmdialect` carries for it.

| Dialect | Operation | Upstream path under `baseURL` | Class | Codec |
|---|---|---|---|---|
| `openai` | chat completions | `/chat/completions` | translated | `llmdialect/openaichat` |
| `openai` | responses | `/responses` | translated | `llmdialect/openairesp` |
| `openai` | embeddings | `/embeddings` | model | none |
| `openai` | model list | `/models` | jobs only | none |
| `anthropic` | messages | `/messages` | translated | `llmdialect/anthropic` |
| `anthropic` | count tokens | `/messages/count_tokens` | model | none |
| `anthropic` | model list | `/models` | jobs only | none |
| `gemini` | generate, stream, count, embed | `/models/{model}:generateContent`, `:streamGenerateContent`, `:countTokens`, `:embedContent` | model | none |
| `gemini` | model list | `/models` | jobs only | none |
| `lux` | generate | `/generate` | translated | `llmdialect/lux` |
| `lux` | model list | `/models` | jobs only | none |

A translated route can be bridged between any two dialects that both
carry a codec for it. A model route resolves a Model and reaches a
target of the door's own dialect only. Every other path is an opaque
route, which no dialect table can enumerate and which
[[004-request-path]] forwards to one Provider of the door's dialect for
a Key with `passthrough: true`.

A model list route is reached by the discovery and health jobs of this
spec and by nothing else. A caller's `GET /v1/models` on a door is
answered by the gateway from the Models the Key may use, so it never
becomes an upstream call ([[004-request-path]]).

`llmdialect` carries four wire dialects: `anthropic-messages`,
`openai-chat`, `openai-responses`, and `lux`. The `openai` manifest
dialect therefore covers two of them, one per route, and the `gemini`
manifest dialect covers none, which is why every `gemini` row has no
codec.

### What a credential is

A credential is one static string written into one request header:
`credential.header` and `credential.scheme` of
[[003-manifest-contract]], `Authorization: Bearer <value>` for
`openai` and `lux`, `x-api-key: <value>` for `anthropic`,
`x-goog-api-key: <value>` for `gemini`, or any header an operator
names with `raw`, which is how an OpenAI-compatible upstream that wants
`api-key: <value>` is declared. That is the whole of what the core can
inject. An upstream that signs requests (AWS SigV4 for Bedrock), that
wants a short-lived token exchanged from a service account (OAuth for
Vertex AI), or that needs the model in the path with a deployment name
and an `api-version` query (Azure OpenAI) is not a `Provider` of this
spec: its request shape is not one of the dialect table's, and a
credential that has to be refreshed or derived per request is not a
value the store can hold sealed. An operator reaches such an upstream
through a translating proxy of its own declared as the `baseURL`, and
a dialect for one of them is a new row in the dialect table with its
own spec, not a new `scheme`. The core's `scheme` set is therefore
`bearer` and `raw` and nothing else, on purpose.

A Provider with no credential, neither `value` nor `valueFrom`, is
valid ([[003-manifest-contract]]): the gateway injects no header toward
it and strips a caller's copy of the dialect's credential header all
the same, which is what a runtime on the operator's own network, an
Ollama or a vLLM behind `LUX_UPSTREAM_ALLOW_PRIVATE`, needs.
`status.credential.set` is then `false`.

### Credential custody

A `Provider.spec.credential.value` is sealed the moment `Resolve`
returns and is never written anywhere in the clear. The scheme is
envelope encryption with one fixed suite; there is no in-band format
version, because a second suite would be a new column by migration and
not a byte a reader has to switch on.

| Element | Size | Algorithm |
|---|---|---|
| data key | 32 bytes from `crypto/rand`, fresh per credential and per write of a value | none: it is key material, zeroed after use |
| value ciphertext | the value plus 16 bytes | AES-256-GCM under the data key; `nonce` is 12 bytes from `crypto/rand`, fresh per seal, stored beside it |
| wrapped data key | 48 bytes | AES-256-GCM under one KEK of `LUX_SECRETS_KEK`; `wrapped_nonce` is 12 bytes from `crypto/rand`, fresh per wrap, stored beside it |
| additional data, on both AEADs | | the UTF-8 bytes of `<provider id>:<credential version>`, the id with its `prv_` prefix and the version in decimal, `prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y:2`; a row copied onto another Provider or onto another version fails to open |

The four byte strings and the version are the `Sealed` row of
[[010-state]]. `internal/secrets` holds the key encryption keys as a
`*Keyring`, which `Parse(raw string) (*Keyring, error)` reads from the
variable, and owns them as its methods: `Seal(providerID string,
version int, value []byte) (store.Sealed, error)`, `Open(providerID
string, s store.Sealed) ([]byte, error)`, `Rewrap(providerID string, s
store.Sealed) (store.Sealed, bool, error)`, which returns the row with
a new wrapped key under the first KEK and `false` when it was already
under it, and `Unwrap(providerID string, s store.Sealed) (position
int, err error)`, which opens the wrapped data key alone and says which
listed key did, so the start-up check and `luxd rewrap` never touch a
value. `Check(ctx, store.Credentials, *Keyring) (int, error)` is the
start-up check over every stored row. The data key is zeroed before
each call returns. `internal/store` holds the row and returns it as
ciphertext, and imports nothing of `internal/secrets`
([[010-state]]). `gateway` receives a plaintext only through the
`CredentialSource` interface of [[004-request-path]], which
`internal/serve` satisfies with `StoreCredentials` from
`store.Credentials()` and `Keyring.Open`, only for the Provider a
chosen target names, and only for the life of one outbound request
([[001-architecture]]); the discovery and health jobs open the same way
for the models route. A Provider that stores no credential answers a
nil value, and the door and the jobs then inject no header. In file
mode the seam is `filemode.Store.CredentialValue` ([[010-state]]),
which `serve.FileCredentials` hands back as the value read from the
environment, and nothing is opened. An excerpt of an upstream body a job
keeps in a status has the value redacted from it first, because an
upstream that echoes the header it was sent must not put the credential
into `status.health.lastError`. The value is redacted by type, not by discipline: it is decoded into a
field the JSON and YAML encoders skip ([[003-manifest-contract]]), so
no object that carries it can be serialized into a response, an event,
a log line, or a request log record.

`status.credential` is `{set, version, updatedAt}` and nothing else:
`set` is whether a value is stored, `version` counts the values ever
applied, `updatedAt` is when the current one was. No prefix, suffix,
length, or hash of the value is ever in `status`, because any of them
is part of the value ([[001-architecture]], invariant 2).

The memory store seals a value the same way and holds the `Sealed` row
in memory, so the only difference between the memory and the Postgres
custody is where the row lives. In file mode a Provider names
`credential.valueFrom.env` instead, and the value is read from the
process environment once at start and at each re-read, and held in
memory for the life of the process ([[010-state]]). Nothing is sealed
and nothing is stored, because there is nowhere to store it. The
variable is whichever name the manifest gives; the documentation's
examples use `LUX_PROVIDER_<NAME>_CREDENTIAL` so a deployment's
secrets read as what they are, and [[002-repository-scaffold]]'s table
lists the `valueFrom.env` variables as one row, named by the manifests,
owned here. A named variable that is unset or empty is a start-up
failure naming the Provider and the variable, never a value.

### Configuration

| Variable | Default | Rule |
|---|---|---|
| `LUX_SECRETS_KEK` | none | one or more 32-byte keys, each standard base64 with padding (RFC 4648 section 4, 44 characters), comma separated, at most 8, a blank entry ignored; the first wraps every new data key, every key is tried to open one; required by `serve` and `rewrap` in every mode but file mode, where it is read, checked when set, and unused |
| `LUX_UPSTREAM_ALLOW_PRIVATE` | unset | `1` sets `Options.AllowPrivateUpstreams` ([[003-manifest-contract]]) and admits a private destination at dial, below; blank is unset and any other value is a configuration error |
| `LUX_DISCOVERY_INTERVAL` | `1h` | Go duration, at least `1m`, at most `24h` |
| `LUX_HEALTH_INTERVAL` | `30s` | Go duration, at least `5s`, at most `10m` |

The four are in [[002-repository-scaffold]]'s table with this spec as
their owner. The list shape of `LUX_SECRETS_KEK` is what rotation
needs: a re-wrap reads under the old key and writes under the new one,
so both are live in one process for the length of the rotation. A
single-key variable would make rotation impossible without a second
variable naming the same thing.

`luxd serve` refuses to start without `LUX_SECRETS_KEK` in every mode
but file mode, and reports which of the listed keys failed to decode to
32 bytes, by position and never by value, in the one configuration
line. It then opens the wrapped data key, not the value, of every
stored credential through `secrets.Check` and refuses to start naming
the Providers whose data key no key in the list opens, so a deployment
carrying the wrong key fails at start rather than on the first request
through a door; the start-up log then names how many keys were listed
and how many rows they open. With the memory store there is nothing
stored at start and the variable is still required, because the first
`Provider` applied is sealed under it.

### Rotation

`luxd rewrap` is the third role of the server binary, its own package
under `internal/rewrap` with its own dependency allow list. It reads
`LUX_SECRETS_KEK` and `LUX_DB_URL`, through `config.LoadRewrap`, and
nothing else of the table; without `LUX_DB_URL` it exits 1 with a
configuration error naming the variable, because the memory store
survives no process and the file mode seals nothing, so there is
nothing durable to re-wrap. The run itself, `rewrap.Run(ctx,
store.Credentials, *secrets.Keyring, report io.Writer) (Summary,
error)`, works over the interface and is proven against the memory
store and, under the postgres tag, over a real database in
`cmd/luxd`'s `TestPostgresRewrapRole` ([[010-state]]). It applies
no migration and refuses a schema that is not at the binary's version
([[010-state]]). For every row of `Credentials.List` it tries the
first key against the wrapped data key: a row that opens under it is
already current and is skipped; a row that opens under a later key is
re-wrapped under the first with a fresh `wrapped_nonce` and written
through `Credentials.Rewrap` with the version it read, so a value
applied through the API during the run is never overwritten by a stale
wrap, and the row whose version moved counts as already current, since
the apply sealed it under the first key; a row no listed key opens is
reported by provider id, one line on stderr, and the run continues. The value ciphertext is never touched, never decrypted, and
never re-encrypted, which is why the two are separate columns. The run
is idempotent and resumable: an interrupted run is repeated rather
than repaired. It prints one line, `rewrap: <n> re-wrapped, <m> already
current, <k> unopenable`, and exits 0 when `k` is zero and 1 otherwise.
The operator's sequence is deploy with `LUX_SECRETS_KEK=new,old`, run
`luxd rewrap`, deploy with `LUX_SECRETS_KEK=new`.

A role rather than a route because a re-wrap touches every credential
row, needs no caller's identity, and has no decision an authorizer
could make about it.

### Discovery

```mermaid
flowchart LR
  T[tick: LUX_DISCOVERY_INTERVAL] --> L{discovery lease held?}
  L -- no --> T
  L -- yes --> P[for each Provider with discovery.mode auto]
  P --> C[GET the dialect's models route]
  C -- error --> K[keep the catalogue, record lastError]
  C -- ok --> F[include then exclude globs]
  F --> R[manifest.Resolve one Model per surviving name]
  R --> U[upsert discovered Models]
  U --> D[delete discovered Models the list no longer carries]
```

The job is `internal/serve`'s, `serve.Discovery`, over the store
interfaces of [[010-state]] and the client below, and runs on one
replica at a time, under the store lease named `discovery`, renewed at
a third of its TTL, because a list that two replicas resolve
concurrently would race on the same object versions to write the same
result. The tick is `LUX_DISCOVERY_INTERVAL` with up to ten percent
jitter, and the first tick is at start. The lease holder also tails
the journal ([[010-state]], `Since`) once a second and lists a Provider
at once when it reads a `provider.created`, or a `provider.updated`
whose changed paths include `spec.baseURL` or a path under
`spec.credential`, so a new Provider's Models appear within seconds
rather than at the next tick; the changed paths are read from the
event's `data` as a list of paths or as an object whose `paths` member
is one, and a replica that takes the lease skips the journal to its
end first so the past is not replayed as lists. A tunneled Provider
is skipped until [[013-tunnelled-runtimes]] gives it a client. The list
request is sent through the Provider's upstream client with its
credential injected, as any request is, within the Provider's
`timeout`.

| Dialect | Models route | Names read from | Pagination |
|---|---|---|---|
| `openai` | `GET {baseURL}/models` | `data[].id` | none |
| `anthropic` | `GET {baseURL}/models?limit=1000` | `data[].id` | while `has_more` is true, repeat with `after_id` set to `last_id` |
| `gemini` | `GET {baseURL}/models?pageSize=1000` | `models[].name`, with the leading `models/` removed, for an entry whose `supportedGenerationMethods` names `generateContent` or `embedContent`; an `embedContent`-only entry is discovered with `modalities.output: [embedding]`, and every other entry is skipped | while `nextPageToken` is present, repeat with `pageToken` |
| `lux` | `GET {baseURL}/models` | `data[].id` | none |

A list follows at most 20 pages; a longer one is a failed list. Every
name the list carries is a candidate, whatever the upstream says the
model can do: a `gemini` list carries embedding models beside
generating ones, and the operator's `include` and `exclude` are the
filter, not a capability heuristic.

Each surviving upstream name becomes a Model named
`<provider name>/<upstream name>`, resolved by `manifest.Resolve` under
`Options{Actor: {Subject: provider.status.owner}}` with the one target,
`weight` 100, `priority` 0, `fallback` `never`, no `pricing`, the
default `modalities`, no `contextWindow` and no `maxOutputTokens`, and
`status.source` `discovered`, exactly as [[003-manifest-contract]]
states. Discovery reads names and nothing else from the list: the
context window, output limit, and capability members some dialects
carry are not imported, because the three dialects disagree on their
presence and shape and a field an operator can declare is better
absent than wrong. A discovered Model is unpriced and carries no
limits until an operator declares it. Discovery calls the same
function the API calls, so a name the schema refuses is refused here
too and is recorded in `Provider.status.discovered.warnings` rather
than stored, one warning per refused name naming the upstream name and
the rule. The warnings sit in the observed half of status, a member
this spec adds to [[003-manifest-contract]]'s `DiscoveredStatus`,
rather than in `status.warnings`, because that member is the control
plane's, written by `Put`, which the file mode refuses for a declared
Provider and which would move the Provider's version and ETag on every
list. The writes of one list are one `Transact`: the created Models
with a `model.discovered` event each, the deleted ones with a
`model.removed` each ([[012-request-log-and-events]]), an unchanged
Model left at its version, and `status.discovered`. A new Model's
`status.available` and `status.targets[].health` are written from the
Providers' published states in the same transaction, so a door's model
list carries it before the health holder's next tick.

The rules that make the catalog safe to recompute:

- A successful list is authoritative for that Provider's discovered
  Models and for nothing else. Discovered Models of the Provider whose
  upstream name the list no longer carries are deleted.
- A failed or empty list changes no object. The previous catalog
  stands, `status.discovered.at` is unchanged, and the failure is
  `status.health.lastError`. An upstream that answers 500 must not
  empty a caller's model list.
- A Model whose `status.source` is `declared` is never written and
  never deleted by discovery, whatever its name. A declared Model
  shadows the discovered one of the same name; deleting the declared
  one lets the next run restore it.
- `discovery.mode: none` lists nothing and deletes nothing, so turning
  discovery off leaves the Models it already created until an operator
  deletes them.

### Health

`health.mode` selects the source of the state published to
`Provider.status.health`. Every replica additionally keeps its own view
and takes the worse of the two, over the order `Healthy`, `Degraded`,
`Unreachable`, with `Unknown` meaning no information yet. A replica
that is failing against a Provider acts on that immediately; a replica
that is not never overrides the published state upward.

| Mode | Published by | Failure is | Success is |
|---|---|---|---|
| `probe` | the replica holding the `health` lease, every `LUX_HEALTH_INTERVAL` and once at start, calling the dialect's models route, first page only, through the Provider's client with a 5 second budget in place of the Provider's `timeout` (`serve.Health.Probe`) | a transport error, a timeout, or a 5xx | any other complete response, including a 4xx: the upstream answered. A 401 or 403 is a success for health, which measures reachability, and is written to `status.health.lastError` as `credential refused: <status>`, so a revoked credential is visible without a request through a door |
| `passive` | the replica holding the `health` lease, from its own data plane outcomes through `serve.Health.Observe(providerID, failed)`, which is [[004-request-path]]'s `HealthObserver`; an outcome moves the state and `since` and leaves `lastError` and `lastProbeAt` to the probe | a transport error, a timeout, or a 5xx from the upstream | any other complete response |
| any mode, `tunnel: true` | the tunnel registry ([[013-tunnelled-runtimes]]): a Provider with no live session is `Unreachable` whatever the mode says, and the mode's own signal applies while one is open | a missing or expired registry row | a live row |
| `none` | nobody; the state is `Unknown` forever | nothing | nothing |

```mermaid
stateDiagram-v2
  [*] --> Unknown
  Unknown --> Healthy: 1 success
  Unknown --> Degraded: 1 failure
  Healthy --> Degraded: 1 failure
  Degraded --> Healthy: 1 success
  Degraded --> Unreachable: 3 consecutive failures
  Unreachable --> Healthy: 1 success
  Unknown --> Unknown: mode none
```

The thresholds are exact: one failure moves `Healthy` to `Degraded`,
the third consecutive failure moves `Degraded` to `Unreachable`, and
one success returns the state to `Healthy` and resets the counter.
`status.health.since` is stamped on every change of state and
`lastProbeAt` on every probe. The counter is the lease holder's own: a
replica that takes the lease starts every count at zero and takes the
stored state as authoritative, so a handover never moves a state by
itself. A Provider turned to `mode: none` is written `Unknown` once, on
the holder's next tick, and left alone after.

Under `probe` the counter is fed by probes and by traffic both, so a
Provider that fails ten requests inside one probe interval is
`Unreachable` before the next probe. Every replica's `Observe` feeds
its own view; the holder's feeds the published state too. The state a
replica acts on is `serve.Health.View(providerID)`, the worse of the
two, which [[008-routing-and-models]]'s selection reads. Under `passive` there is no probe
to recover with; a Provider marked `Unreachable` is left out of
selection and is re-admitted by the per-target circuit's half-open
attempt succeeding ([[008-routing-and-models]]), which is the only
traffic it can get.

What the signal means:

- A Provider that is `Unreachable` takes every target on it out of
  selection. `Degraded` and `Unknown` take nothing out.
- `Model.status.available` is true when at least one of the Model's
  targets names a Provider that is not `Unreachable`, and
  `Model.status.targets[].health` is that Provider's published state.
  Both are written by the replica holding the `health` lease, which
  also raises `provider.unreachable` and `provider.healthy` at the two
  transitions, once per transition across replicas
  ([[012-request-log-and-events]]); `provider.healthy` fires on every
  entry into `Healthy` from another state, the first probe of a new
  Provider included, as that spec's table reads, with
  `wasUnreachableFor` set when the state left was `Unreachable`. The
  holder also fills the status of a Model that has none yet on every
  tick, so a Model declared through the API is available before any
  Provider changes state.
- `health.mode: none` never makes a target unavailable, which is what
  an upstream with no model list and no error convention needs.

The state a replica acts on is also what an operator scrapes.
`HealthOptions.Metrics` takes the process's registry and `NewHealth`
registers `lux_provider_health` of [[019-observability]]'s table in it:
four series per Provider, read at scrape time from `View`, `1` on the
state this replica acts on and `0` on the other three, labeled
`provider` with the Provider's `metadata.name` and `state` with one of
`Healthy`, `Degraded`, `Unreachable`, and `Unknown`. The Providers are
the last tick's, so the series of one an operator deleted leave the
family on the tick that stops acting on it, and a replica that knows no
Provider writes no series at all, the gauge being per Provider and
having nothing to hold at zero. `LuxProviderUnreachable` reads it as
`max by (provider) (lux_provider_health{state="Unreachable"}) == 1`,
so the alert fires on the first replica that cannot reach a Provider,
and a Provider the catalog lost raises none.

### The upstream client

One `*http.Client` per Provider, built when the Provider is first
asked for and rebuilt when `baseURL`, `timeout`, or `concurrency`
changes, the old pool's idle connections closed. The builder is
`gateway.NewClientSource(gateway.ClientOptions) *gateway.Clients`, whose
`Client(ctx, *v1.Provider) (*http.Client, error)` is the one method of
[[004-request-path]]'s `ClientSource`, so that interface has one
implementation in the tree and `internal/serve` and a platform that
imports the handler construct the same client under the same rules;
`Revoke(providerID)` forgets a deleted Provider's client.
`ClientOptions` carries `AllowPrivate` and `Version`, the User-Agent's,
and the seams a test needs, a resolver, a dial function, a root pool,
and a clock, none of which turns a rule off. The two failures the client
raises before any dial are its exported errors, `gateway.ErrHostPinned`
and `gateway.ErrProviderBusy`, for the door to map to `upstream_error`
and `provider_unavailable`; `gateway.ErrPrivateAddress` surfaces as the
dial's error. `gateway.InjectCredential(http.Header, *v1.Provider,
value []byte)` is the custody rule below in code: it strips the
credential header and writes the value under the scheme last, and a
tunneled Provider is `gateway.ErrTunnelled` until
[[013-tunnelled-runtimes]] gives it a client over its carrier
transport.

| Property | Value | Reason |
|---|---|---|
| transport | `otelhttp.NewTransport` over the `*http.Transport` the rows below configure, which is what `latere.ai/x/pkg/otel.Transport` wraps, reached directly because that package also carries the SDK and its exporters, which a root package's importer sets up and which reach `os/exec` | every outbound hop is a client span carrying the trace context; the shared bar's `otel-client` gate refuses an `&http.Client{}` literal without a `Transport` and any use of `http.DefaultClient`, and this tree waives nothing under `otel_client.skip` |
| `Proxy` | nil | a proxy variable in the environment would move a credential-bearing request to a host no manifest names, and terminate its TLS; an operator that needs an egress proxy declares it as the `baseURL` |
| `CheckRedirect` | `http.ErrUseLastResponse` | a 3xx is an upstream asking for the credential at another location; the response is returned to the caller as `upstream_error` instead |
| `TLSClientConfig` | minimum TLS 1.2, verification on, the system roots, no field turns it off; `TLSHandshakeTimeout` 10s | a Provider is a public host by the upstream host rule; a local runtime with its own certificate is [[013-tunnelled-runtimes]]'s case |
| `DialContext` | resolves the name, then drops every loopback, link-local, unique-local, private, unspecified, or multicast address among the answers unless `LUX_UPSTREAM_ALLOW_PRIVATE`, and refuses the dial with `ErrPrivateAddress` when none is left; connects only to the admitted addresses, in order; 10s connect timeout | the parse-time rule of [[003-manifest-contract]] is on the name; this is on the address, which is what closes a public name that resolves inward |
| host pin | the `RoundTripper` refuses, before dialing, a request whose URL scheme, host, or port differs from the Provider's `baseURL`, with an error the door reports as `upstream_error` | invariant 2 of [[001-architecture]] in code: a bug that builds a URL wrongly cannot carry the credential to another host |
| `ForceAttemptHTTP2` | true; `MaxIdleConnsPerHost` 32, `IdleConnTimeout` 90s | one pool per Provider, so a slow upstream cannot starve another's connections |
| `DisableCompression` | true | the transport adds no `Accept-Encoding` of its own and decodes nothing, so [[004-request-path]]'s rule that the header is removed on translated and model routes and kept on opaque ones holds byte for byte |
| deadline | `Provider.spec.timeout`, defaulted from `LUX_UPSTREAM_TIMEOUT`, over the whole request including its stream, as the request context's deadline the caller sets; no `http.Client.Timeout` and no `ResponseHeaderTimeout`; the jobs use `10m` for a Provider whose `timeout` is empty, which the file mode leaves so until [[004-request-path]] wires the default | a stream that stalls is cut rather than held; there is no idle timeout between events, because the one deadline is the one knob an operator sets |
| response cap | a response body the gateway reads whole, a non-streaming response on a translated or model route, is read by the door through `io.LimitReader` at `LUX_MAX_BODY_BYTES` ([[004-request-path]]) and one byte more is `upstream_error` with the detail naming the cap; a stream is relayed and never buffered, so it has no cap; the jobs cap one page of a model list at 16 MiB | a provider cannot exhaust a replica's memory with one answer |
| concurrency | `latere.ai/x/pkg/semaphore` of `spec.concurrency` slots per Provider per replica, `0` is no limit; `Acquire` waits at most the request's remaining deadline, and a slot is held until the response body is closed, so a stream in flight counts | a wait that outlives the request's own deadline is `provider_unavailable`, `ErrProviderBusy`, whichever of the timer and the context fired first; only a cancelled caller is its own error |

The outbound header set is [[004-request-path]]'s, which owns what is
forwarded and what is removed. Two of its rules are this spec's,
because they are custody rather than transport: the credential header
is written last from the opened value under `credential.scheme`, so it
wins over both the caller's headers and the Provider's static
`headers`; and no `X-Forwarded-*` or `Forwarded` header is ever added,
because the caller's network location is not the provider's business
and a gateway that forwards it makes itself a tracking relay.

The client's own two metrics are this spec's rows in
[[019-observability]]'s table, written by the gateway's attempt rather
than by the builder, because the attempt is the one place that knows
the Provider, the duration, and the outcome together:
`lux_upstream_requests_total`, a counter labeled `provider` and
`status`, and `lux_upstream_duration_seconds`, a histogram labeled
`provider` over that spec's duration buckets, one observation of each
per target tried. `status` is a closed set of three: `timeout` for the
Provider's own deadline, `error` for a transport failure, a 5xx, a
refused credential, or a redirect, and `ok` for everything else, the
upstream's refusal of the caller's own request and a caller that left
included, so `LuxUpstreamErrorRateHigh` names a provider that is
failing and not a caller that is. A nil registry records neither.

Bodies are buffered where [[004-request-path]] says and nowhere else.
A request body on a translated or model route is read whole by the
door, within `LUX_MAX_BODY_BYTES`, because the door decodes it or reads
`model` from it, so the outbound request carries a `Content-Length`; an
opaque route's body streams through with the caller's `Content-Length`
when it had one and chunked otherwise. A response streams to the caller
as it arrives, except the non-streaming response the door reads whole
under the cap above.

### Provider status

| Field | Written by | When |
|---|---|---|
| `status.id`, `status.owner`, `status.createdAt` | the API, or the file mode at start | at create |
| `status.version` | the store ([[010-state]]) | at every write of the row |
| `status.updatedAt`, `status.warnings` | the API | at every apply |
| `status.credential` | the API | at create and at every apply that carries a value; `version` counts values, not wraps, so a re-wrap leaves it alone; the shape is `{set, version, updatedAt}` as the custody section says |
| `status.health` | the `health` lease holder | at every change of state, and `lastProbeAt` at every probe; the `discovery` lease holder writes `lastError` alone after a failed list, keeping the other members |
| `status.discovered` | the `discovery` lease holder | at every successful list: `count`, `at`, and the refused-name `warnings` |

Deleting a Provider that a declared Model still targets is refused with
`provider_in_use`, 409 ([[011-api]]), so a Model never points at
nothing; the operator edits the Model's targets first. A delete removes
the Provider's discovered Models in the same transaction, revokes its
client, and drops its credential ciphertext; a request in flight toward
it finishes or fails on its own timeout.

### `luxd check`

| Line | Passes when |
|---|---|
| `secrets kek` | `LUX_SECRETS_KEK` is set and every listed key decodes to 32 bytes: `secrets.Parse` |
| `credentials` | every stored credential's wrapped data key opens under one of the listed keys; the line names the count and the version, never a value: `secrets.Check` |
| `providers` | every Provider's `baseURL` resolves, its address is admitted by the private-address rule, and its models route answers inside the health budget; one row per Provider with its state: `serve.Health.Probe` over the client |
| `dialects` | every Provider's dialect has the codec its Models' routes need, or every Model on it is reachable only through an unmodelled route: [[004-request-path]]'s route table |

The functions are this spec's; the role that prints the lines is
[release and installation](.archive/017-release-and-installation.md)'s.

## Not in this spec

The door handlers, the request path, and the data plane error table
([[004-request-path]]); which target a request goes to and what happens
when one fails ([[008-routing-and-models]]); the schema of `Provider`
([[003-manifest-contract]]); the rows, the leases, and the file mode
([[010-state]]); pricing and the usage record
([[009-usage-and-metering]]); a runtime that attaches itself as a
Provider ([[013-tunnelled-runtimes]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A canary credential appears in no response body, event, log line, or request log record, and reaches the stub provider's headers only on requests routed to that Provider | `TestProviderCredentialNeverLeavesTheGateway` | passing, the package half in `internal/serve`: the canary reaches the upstream on the jobs' requests and no stored object, encoding, event payload, status, or log line; the door and stub-provider half is [[015-test-stubs-and-tiers]]'s |
| A stored credential row holds no plaintext under any key; the value ciphertext and the wrapped data key are separate columns; both AEADs fail to open when the provider id or the version in the additional data is changed and open when neither is; `Seal` twice over one value yields two ciphertexts and two nonces | `TestCredentialRowsAreSealed`, `TestSealedAdditionalData`, `TestSealIsNeverDeterministic` | passing, `internal/secrets` |
| `status.credential` carries `set`, `version`, and `updatedAt` and no substring of the value, for a canary value on create, read, list, and every event | `TestCredentialStatusCarriesNoValue` | passing for the custody half, `internal/secrets`: the three members and a read and a list through the store; the API's create and the events are [[011-api]]'s and [[012-request-log-and-events]]'s |
| `luxd rewrap` under `new,old` re-wraps every data key, leaves every value ciphertext byte unchanged, opens under `new` alone afterwards, and is a no-op on a second run; a value applied through the API during the run keeps its newer wrap; a row no key opens is named and the exit code is 1 while the others are still re-wrapped; without `LUX_DB_URL` it exits 1 naming the variable | `TestRewrapUnderANewKEK`, `TestRewrapDoesNotOverwriteANewerValue`, `TestRewrapReportsUnopenableRows`, `TestRewrapNeedsTheStore` | passing, `internal/rewrap` against the memory store and `cmd/luxd` |
| `luxd serve` refuses to start with `LUX_SECRETS_KEK` absent, with a key that is not 32 bytes, and with a key that opens no stored credential, naming the failing Providers and the failing key by position and never by value; with the memory store the variable is still required; in file mode it is not | `TestStartupRequiresAWorkingKEK` | passing, in `cmd/luxd` for the absent and the short key and the file mode, and in `internal/secrets` for the key that opens no stored credential |
| A Provider whose `valueFrom.env` variable is unset or empty is a start-up failure in file mode naming the Provider and the variable | [[010-state]]'s `TestFileModeValuesFromEnvironment` | passing |
| A successful list adds new discovered Models, removes those the upstream dropped, and applies `include` before `exclude`; a discovered Model has one target, `weight` 100, `priority` 0, `fallback` `never`, no pricing, default modalities, and no `contextWindow` or `maxOutputTokens` | `TestDiscoveryAddsAndRemoves`, `TestDiscoveredModelShape` | passing, `internal/serve` |
| An `anthropic` list of three pages by `has_more` and a `gemini` list of three pages by `nextPageToken` are read whole; a list past 20 pages is a failed list | `TestDiscoveryPaginates` | passing |
| The lease holder lists a Provider within one second of reading its `provider.created`, or a `provider.updated` naming `spec.baseURL` or `spec.credential`, from the journal | `TestDiscoveryFollowsProviderChanges` | passing |
| A failed, empty, or unparseable list changes no object and records `lastError` | `TestDiscoveryFailureKeepsTheCatalogue` | passing |
| Discovery never writes or deletes a Model whose source is `declared`; a declared Model shadows the discovered one and deleting it lets the next run restore it | `TestDeclaredModelSurvivesDiscovery` | passing |
| Two replicas with the lease contended run one list per interval between them | `TestDiscoveryRunsOnOneReplica` | passing |
| Every transition in the state diagram fires at its threshold in both modes, and a 4xx is a success while a 5xx is a failure; a 401 on the probe leaves the state `Healthy` and writes `credential refused: 401` to `lastError` | `TestHealthTransitions`, table-driven, `TestProbeReportsARefusedCredential` | passing |
| A replica's own failures downgrade its view below the published state and never raise it above | `TestLocalHealthOnlyDowngrades` | passing |
| An `Unreachable` Provider's targets leave selection, `Degraded` and `Unknown` do not, and `health.mode: none` never makes a target unavailable | `TestUnreachableLeavesSelection` | passing for the status half: `status.available` and `status.targets[].health` follow the states; the selection that reads them is [[008-routing-and-models]]'s |
| Every state a replica acts on is one series of `lux_provider_health` per Provider and state, `1` on that state and `0` on the other three, following the transitions and a replica's own downgrade, and the series of a deleted Provider leave the family on the next tick | `TestProviderHealthGauge`, `TestProviderHealthGaugeForgetsADeletedProvider` | passing, `internal/serve`; the metric's row and its alert are [[019-observability]]'s |
| A caller-sent copy of the credential header and a Provider static header of the same name are both beaten by the injected credential; no `X-Forwarded-*` or `Forwarded` header reaches the upstream | `TestCredentialHeaderWins`, `TestNoForwardedHeaders` | passing for the client half, `gateway`: `InjectCredential` beats both and the transport adds no forwarded header; the door's stripping of the caller's headers is [[004-request-path]]'s |
| With `HTTPS_PROXY` set in the environment the request still reaches the Provider's host directly | `TestNoProxyEnvironmentHonoured` | passing |
| A 302 from the stub provider is not followed and reaches the caller as `upstream_error` | `TestRedirectNotFollowed` | passing at the client: the 302 is returned unfollowed; the `upstream_error` mapping is [[004-request-path]]'s |
| A public name resolving to a private address is refused at dial without `LUX_UPSTREAM_ALLOW_PRIVATE` and admitted with it | `TestPrivateAddressRefusedAtDial` | passing |
| A request handed to a Provider's client toward another scheme, host, or port is refused before any dial and reaches the caller as `upstream_error` | `TestHostPin` | passing at the client: `ErrHostPinned` before any dial; the `upstream_error` mapping is [[004-request-path]]'s |
| Every client `gateway.NewClientSource` builds carries `otel.Transport`, the outbound request to the stub provider carries `traceparent`, and no `Accept-Encoding` the caller did not send reaches it | `TestUpstreamClientIsInstrumented`, `TestNoCompressionAdded`, the `otel-client` gate | passing |
| A non-streaming upstream response one byte over `LUX_MAX_BODY_BYTES` is `upstream_error` naming the cap; a stream of twice that size is relayed whole | [[004-request-path]]'s `TestUpstreamBodyCap` | passing: the cap is applied where the door reads a body whole, in [[004-request-path]]'s handler, and `TestUpstreamBodyCap` proves both the over-cap refusal and the stream relayed whole |
| `concurrency: 2` holds a third request until one finishes, and a wait past the request deadline is `provider_unavailable` | `TestConcurrencyLimitsInFlight` | passing at the client: the third request waits and a wait past the deadline is `ErrProviderBusy`; the `provider_unavailable` mapping is [[004-request-path]]'s |
| Every operation in the dialect table reaches the upstream path the table names, and a model list route is reached by the jobs alone | `TestUpstreamPaths`, table-driven | passing for the model-list rows, `internal/serve`; the door operations' paths are [[004-request-path]]'s |
| A `Provider` with `credential.scheme` `raw` and `credential.header` `api-key` reaches the stub with that one header carrying the bare value; `bearer` on any header prefixes `Bearer ` | `TestCredentialSchemes`, table-driven over the dialect defaults and one custom header | passing, `gateway` |

## Outcome

Built on 2026-09-14 in fourteen commits on `main`, over the memory
store and the file mode of [[010-state]], and proven by the whole gate,
fifteen gates, and per-package coverage of 94.6% for `internal/secrets`,
97.7% for `gateway`, 95.8% for `internal/serve`, 96.2% for
`internal/rewrap`, 99.4% for `internal/config`, and 94.3% for
`cmd/luxd`. What diverged from the text as dispatched, each fixed in
the Design above beside the rule it settles:

- The three custody functions are methods on `*secrets.Keyring`, which
  `secrets.Parse` builds from the variable, with `Unwrap` and `Check`
  beside them; the key material has one home and no encoding.
- A refused upstream name is a warning in `status.discovered.warnings`,
  a member added to `manifest/v1.DiscoveredStatus`, and not in
  `status.warnings`: that member is the control plane's, written by
  `Put`, which the file mode refuses for a declared Provider and which
  would move the Provider's version on every list.
- The upstream client's transport is `otelhttp.NewTransport`, reached
  directly rather than through `latere.ai/x/pkg/otel.Transport`, which
  wraps the same constructor but carries the SDK and its exporters into
  a root package and, through the SDK's resource detection, `os/exec`,
  which spec 001's rule forbids `gateway`. The `gateway` row of
  `internal/arch/deps_test.go` admits the instrumentation's three
  modules for it.
- The builder returns the concrete `*gateway.Clients`, which satisfies
  [[004-request-path]]'s `ClientSource`; that interface is declared
  there. The client exports `ErrHostPinned`, `ErrProviderBusy`,
  `ErrPrivateAddress`, and `ErrTunnelled` for the door to map, and
  `InjectCredential` for the custody rule.
- The private-address rule at dial drops the refused answers and
  connects to the admitted ones, refusing the dial only when none is
  left, which is the reading of "connects only to the admitted
  addresses".
- A concurrency wait that ends with the request's deadline is
  `ErrProviderBusy` whichever of the timer and the context fired first;
  only a cancelled caller is the context's own error.
- `luxd rewrap` runs over the store `LUX_DB_URL` names: the run is
  proven over `store.Credentials` against the memory store and, since
  [[010-state]]'s Postgres store landed, over a real database in
  `cmd/luxd`'s `TestPostgresRewrapRole`.
- The jobs redact the credential value from an upstream body's excerpt
  before keeping it in `status.health.lastError`, because an upstream
  that echoes the header it was sent would otherwise put the value into
  a status.
- Passive outcomes move the state and `since` and leave `lastError`
  and `lastProbeAt` to the probe; a lease handover starts the counters
  at zero with the stored state authoritative; `provider.healthy` fires
  on the first entry into `Healthy` too, as [[012-request-log-and-events]]'s
  table reads; a Provider turned to `mode: none` is written `Unknown`
  once.
- Discovery writes a new Model's observed status in the list's
  transaction and the health holder fills any Model that has none on
  its tick; the two `model.*` events of [[012-request-log-and-events]]
  are raised in the same transaction; the event record's shape is that
  spec's, written here in `internal/serve/events.go` until its package
  lands.
- A tunneled Provider is skipped by both jobs and refused by the client
  until [[013-tunnelled-runtimes]] gives it a carrier transport.

Owned elsewhere, in [[004-request-path]]: the upstream body cap and the
stream relayed whole, `TestUpstreamBodyCap`; the door halves of
`TestCredentialHeaderWins`,
`TestNoForwardedHeaders`, `TestRedirectNotFollowed`, `TestHostPin`,
`TestConcurrencyLimitsInFlight`, and `TestUpstreamPaths`; the
selection half of `TestUnreachableLeavesSelection`
([[008-routing-and-models]]); the API and event halves of
`TestCredentialStatusCarriesNoValue` ([[011-api]],
[[012-request-log-and-events]]); the e2e half of
`TestProviderCredentialNeverLeavesTheGateway`
([[015-test-stubs-and-tiers]]); and the `luxd check` lines, whose
functions are here and whose role is [release and installation](.archive/017-release-and-installation.md)'s.
