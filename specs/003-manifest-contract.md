---
title: "Manifest contract: the four kinds, decoding, validation, defaulting, resolve"
status: complete
track: core
depends_on:
  - specs/001-architecture.md
affects: [manifest/, manifest/v1/, internal/api/, internal/store/, docs/]
effort: large
created: 2026-09-13
updated: 2026-09-16
author: changkun
---

# Manifest contract

## Overview

A manifest is a Kubernetes-shaped object under `apiVersion:
lux.latere.ai/v1beta1` in one of four kinds: `Provider`, `Model`, `Key`,
`Budget`. It is the only way to declare anything in Lux. This spec is the
contract a caller codes against: every field, its type, its default, its
mutability, and the rule that refuses it; how a body is decoded; the
stages `Resolve` runs; the error codes; and the promise the schema makes
about its own evolution. The package `manifest` and its types package
`manifest/v1` are the implementation, imported by the API, the store,
the `lux` command, the file mode, and any platform that wants the
contract without the server, so that all of them mean the same thing by
a manifest ([[001-architecture]], invariant 1).

The kinds' semantics beyond the schema, what the gateway does with a
resolved object, live with their owners: [[005-providers]],
[[008-routing-and-models]], [[007-keys-and-limits]]. This spec owns the
schema, the rules any surface applies before an object exists, and the
package.

## Current state

Built: `manifest` and `manifest/v1` are in the tree with the golden
corpus under `manifest/testdata/v1/`, and every criterion below has its
passing test. The Outcome records what was built and where the text
below moved to describe it.

## Design

### The objects

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Provider
metadata:
  name: openai
  labels:
    tier: paid
spec:
  dialect: openai                      # openai | anthropic | gemini | lux
  baseURL: https://api.openai.com/v1   # required
  credential:
    value: sk-live-...                 # write-only: accepted on apply, never returned
    header: Authorization              # default per dialect
    scheme: bearer                     # bearer | raw; default per dialect
  headers:                             # static, added to every request; never a credential
    OpenAI-Organization: org_example
  discovery:
    mode: auto                         # auto | none
    include: ["gpt-*", "o*"]           # globs over upstream names; empty is everything
    exclude: ["*-preview"]
  health:
    mode: probe                        # probe | passive | none
  timeout: 10m                         # one request including its stream
  concurrency: 0                       # in-flight requests to this provider; 0 is no limit
status:                                # written by the server, ignored on apply
  id: prv_01J9ZK2P7Q8R9S0T1U2V3W4X5Y
  version: 3                           # the store's row version; the ETag of 011
  owner: https://login.example.com|alice
  credential: {set: true, version: 2, updatedAt: 2026-09-13T10:00:00Z}
  health: {state: Healthy, since: 2026-09-13T10:00:05Z, lastProbeAt: 2026-09-13T10:41:00Z, lastError: ""}
  discovered: {count: 34, at: 2026-09-13T10:00:05Z, warnings: []}  # warnings: upstream names the name rule refused (005)
  tunnel: null                         # the block of 013 when spec.tunnel is true
  createdAt: 2026-09-13T10:00:00Z
  updatedAt: 2026-09-13T10:00:00Z
  warnings: []
```

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Model
metadata:
  name: gpt-5
  labels:
    family: gpt
spec:
  targets:                             # required, at least one
    - provider: openai                 # a Provider the caller may see
      model: gpt-5                     # the upstream's name; default metadata.name
      weight: 100                      # spread among healthy targets of one priority; 0 is fallback only
      priority: 0                      # lower tried first; equal priorities share by weight
    - provider: azure
      model: gpt-5
      weight: 0
      priority: 1
  fallback: onError                    # onError | never
  pricing:                             # absent is unpriced
    currency: USD
    per: 1000000                       # tokens
    input: "1.25"                      # decimal strings, at most 6 fraction digits
    output: "10"
    cachedInput: "0.125"
    cacheWrite: "1.25"
  modalities:
    input: [text, image]
    output: [text]
  contextWindow: 400000
  maxOutputTokens: 128000
status:
  id: mdl_01J9ZK2P7Q8R9S0T1U2V3W4X5Z
  version: 1
  owner: https://login.example.com|alice
  source: declared                     # declared | discovered
  available: true                      # at least one target's provider is not Unreachable
  targets:
    - {provider: openai, model: gpt-5, health: Healthy}
    - {provider: azure, model: gpt-5, health: Unknown}
  createdAt: 2026-09-13T10:00:00Z
  updatedAt: 2026-09-13T10:00:00Z
  warnings: []
```

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Key
metadata:
  name: run-42
  labels:
    run: r_42
spec:
  models: ["gpt-5", "anthropic/*"]     # non-empty unless disabled: exact names or globs
  # value: eyJhbGciOi...               # optional, write-once, never returned: a value the caller supplies (007)
  limits:
    requestsPerMinute: 60              # 0 is none; default from configuration
    tokensPerMinute: 100000
    spend:                             # absent is no spend limit on the key itself
      amount: "5"
      currency: USD
      window: 24h                      # a duration, month, or none
  budget: team-research                # a Budget the caller may draw from
  ttl: 168h                            # or expiresAt; from createdAt; absent is never
  allowUnpriced: false                 # an unpriced Model under a spend limit or a Budget
  passthrough: false                   # routes the gateway does not translate
  disabled: false
status:
  id: key_01J9ZK2P7Q8R9S0T1U2V3W4X60
  version: 1
  owner: https://login.example.com|alice
  prefix: lux_ab12cd34ef56             # the first twelve characters of a minted value; sup_ and eight hex for a supplied one
  value: lux_ab12cd34ef56...           # in the response that created the key, and nowhere else; absent when the caller supplied the value
  state: Active                        # Active | Disabled | Expired | Exhausted
  selectors:                           # what each selector matched at resolve; the data plane matches again at request time
    - {selector: gpt-5, matched: [gpt-5]}
    - {selector: "anthropic/*", matched: [anthropic/claude-sonnet-4, anthropic/claude-opus-4]}
  budget: {name: team-research, id: bud_01J9...}
  usage:
    window: {requests: 12, tokens: 48210, spend: "0.61", resetsAt: 2026-09-14T00:00:00Z}
    total: {requests: 12, tokens: 48210, spend: "0.61"}
  lastUsedAt: 2026-09-13T10:41:00Z
  expiresAt: 2026-09-20T10:00:00Z
  createdAt: 2026-09-13T10:00:00Z
  updatedAt: 2026-09-13T10:00:00Z
  warnings: []
```

```yaml
apiVersion: lux.latere.ai/v1beta1
kind: Budget
metadata:
  name: team-research
spec:
  amount: "500"                        # required
  currency: USD
  window: month                        # a duration, month, or none
  hard: true                           # refuse at the limit; false records and continues
status:
  id: bud_01J9ZK2P7Q8R9S0T1U2V3W4X61
  version: 2
  owner: https://login.example.com|alice
  state: Open                          # Open | Exhausted
  spent: "123.45"
  remaining: "376.55"
  resetsAt: 2026-10-01T00:00:00Z
  keys: 12                             # keys drawing from it now
  createdAt: 2026-09-13T10:00:00Z
  updatedAt: 2026-09-13T10:00:00Z
  warnings: []
```

The four examples are manifests `Resolve` accepts under options where
the actor may see the providers and the budget they name; the
acceptance criteria hold them to that.

### Fields

`metadata`, every kind:

| Field | Type | Mutable | Rule |
|---|---|---|---|
| `name` | string | no | the name rule of the kind below; unique per kind in an installation; generated by `Options.NewName` when absent, `<adjective>-<noun>-<4 hex>` from the API's generator |
| `labels` | map | yes | Kubernetes label syntax for keys and values; keys under `lux.latere.ai/` are `reserved_prefix`. The prefix is reserved for the gateway and the gateway writes no label under it in `v1beta1`, so an object read back re-applies unchanged; an object's id is `status.id` and nowhere else |
| `annotations` | map | yes | keys as labels, values any string up to 4 KiB; total at most 64 KiB; `lux.latere.ai/` refused |

Name rules. A `Provider`, `Key`, or `Budget` name is a DNS-1123 label
of at most 63 characters. A `Model` name is one or more segments joined
by `/`, each segment `[a-z0-9]([a-z0-9._:-]*[a-z0-9])?`, at most 128
characters in all, because model names carry dots (`gpt-4.1`), a local
runtime's carry a colon tag (`llama3.1:8b`, [[013-tunnelled-runtimes]]),
and a discovered model is named `<provider name>/<upstream name>` with
the upstream name taken verbatim, which may itself carry a `/`: an
aggregator lists `anthropic/claude-sonnet-4`, and the discovered Model
is `relay/anthropic/claude-sonnet-4`. The first segment of a name of two
or more is what the data plane reads as the provider hint and the rest
is the upstream name ([[004-request-path]], [[008-routing-and-models]]).
A declared Model may use any of the forms. No name of any kind begins
with one of the id prefixes `prv_`, `mdl_`, `key_`, `bud_`; one that
does is `reserved_prefix`, so a path segment that carries a prefix is
an id and one that does not is a name, with no third case
([[011-api]]). Only a Model name can carry `_` at all, so the rule
bites on `mdl_` alone and is stated for every kind so it never needs
restating. The upstream name in a target is the provider's own
string, any printable characters up to 256, never validated beyond
that.

`Provider.spec`. The mutability column names whether a field may change
after create: `no`, or `yes` for any caller the authorizer allows.

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `dialect` | enum | none, required | no | `openai`, `anthropic`, `gemini`, `lux`; the wire API the upstream speaks, per the dialect table below |
| `tunnel` | bool | `false` | no | `true` makes the upstream whichever agent connects through the tunnel rather than an address the gateway holds ([[013-tunnelled-runtimes]]); `exclusive_fields` with `baseURL` and with `credential`; `invalid_field` when `Options.TunnelEnabled` is false |
| `baseURL` | string | none, required unless `tunnel` | yes | `https://`, a host under the upstream host rule below, an optional path, no userinfo, query, or fragment; `http://` and a loopback, link-local, or private host only when `Options.AllowPrivateUpstreams` is set, and then with a warning; the host of `Options.PublicURL` is `invalid_field`, because a provider that is this gateway is a loop |
| `credential.value` | string | none | yes; bumps `status.credential.version` | write-only; optional: a Provider with neither `value` nor `valueFrom` injects no credential header, which is what a runtime on the operator's own network needs ([[005-providers]]); 1 to 4096 bytes when set; returned by no read, carried by no event or log |
| `credential.valueFrom.env` | string | none | no | a POSIX variable name the file mode reads at start; `exclusive_fields` with `value`; `invalid_field` in server mode |
| `credential.header` | string | per dialect | yes | a header name; `Authorization` for `openai` and `lux`, `x-api-key` for `anthropic`, `x-goog-api-key` for `gemini`; defaulted with `scheme` on every Provider that is not tunneled, a Provider with no credential included, because the gateway strips a caller's copy of that header whether or not it injects one ([[005-providers]]) |
| `credential.scheme` | enum | per dialect | yes | `bearer` prefixes `Bearer `; `raw` writes the value verbatim; `bearer` on `Authorization`, `raw` elsewhere; the default follows the dialect, not a header the caller chose |
| `headers` | map | empty | yes | header names to values, at most 16, values up to 4 KiB of visible ASCII and space with no CR or LF (`invalid_field`); the effective `credential.header`, given or the dialect's, `Host`, `Content-Length`, and the hop-by-hop headers are `reserved_prefix`, compared case-insensitively |
| `discovery.mode` | enum | `auto` | yes | `auto` lists the upstream's models on the discovery interval and declares each as a discovered Model ([[005-providers]]); `none` declares nothing |
| `discovery.include`, `.exclude` | []string | empty | yes | globs under the glob rule below over upstream names; `include` empty is everything; `exclude` wins |
| `health.mode` | enum | `probe` | yes | `probe` calls the upstream's model list on the health interval; `passive` infers health from traffic; `none` reports `Unknown` and never marks a target unavailable |
| `timeout` | duration | `Defaults.Timeout` | yes | Go syntax, at least `1s`, at most `1h` |
| `concurrency` | int | `0` | yes | in-flight requests toward this provider across one replica; `0` is none |

The dialect table. A dialect names the wire API an upstream speaks and
the door a caller uses; the two are one vocabulary.

| Dialect | Wire APIs | Translated by `llmdialect` | Credential default |
|---|---|---|---|
| `openai` | chat completions, responses, embeddings, models | chat completions, responses | `Authorization: Bearer` |
| `anthropic` | messages, count tokens, models | messages | `x-api-key`, raw |
| `gemini` | generateContent, streamGenerateContent, embedContent, models | none: a `gemini` door serves `gemini` targets only | `x-goog-api-key`, raw |
| `lux` | generate, models: the lux-native dialect, another Lux | generate | `Authorization: Bearer` |

The upstream host rule: a fully qualified name or an IP literal. A
single-label name, a loopback, link-local, or private address by syntax,
and the `.local` and `.internal` suffixes are `invalid_field` unless
`Options.AllowPrivateUpstreams` is set. This is a different rule from
Cella's manifest host rule, which admits patterns for a scope; a
provider is one host, never a pattern, and the gateway dials that host
and no other ([[001-architecture]], invariant 2).

`Model.spec`:

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `targets[]` | list | none, required, at least one | yes | each `{provider, model, weight, priority}`; the pair `(provider, model)` unique (`duplicate_target`); at least one entry with `weight` above `0` (`invalid_field`) |
| `targets[].provider` | string | none, required | yes | a `Provider` the caller may see, by name or `prv_` id; `Lookup.Provider` answers `not_found` otherwise |
| `targets[].model` | string | `metadata.name` | yes | the upstream's name for the model, 1 to 256 printable characters |
| `targets[].weight` | int | `100` | yes | `0` to `1000`; `0` is used only when every target of a lower priority is unavailable |
| `targets[].priority` | int | `0` | yes | `0` to `9`; the fallback order |
| `fallback` | enum | `onError` | yes | `onError` tries the next target on a retryable failure before any response bytes reached the caller ([[008-routing-and-models]]); `never` fails on the first |
| `pricing` | object | absent | yes | absent is unpriced, which `status.warnings` says; present requires `input` and `output` |
| `pricing.currency` | string | `USD` | yes | an ISO 4217 code, upper case |
| `pricing.per` | int | `1000000` | yes | tokens the prices are quoted per; `1`, `1000`, or `1000000` |
| `pricing.input`, `.output`, `.cachedInput`, `.cacheWrite` | money | `cachedInput` defaults to `input`, `cacheWrite` to `input`; the first two required | yes | the money rule below, non-negative; `"0"` is a price, so the Go fields are `*v1.Money` and an absent price is told from a free one |
| `modalities.input`, `.output` | []enum | `[text]`, `[text]` | yes | `text`, `image`, `audio`, `video`, `file`, `embedding`; non-empty, unique |
| `contextWindow`, `maxOutputTokens` | int | absent | yes | positive; `maxOutputTokens` at most `contextWindow` when both are set |

A discovered Model ([[005-providers]]) is `Resolve`d by the discovery
job under the Provider's owner with one target, the provider and the
upstream name, `weight` `100`, `priority` `0`, `fallback` `never`, no
pricing, default modalities, and `status.source` `discovered`. A
declared Model with the same name replaces it and the replacement is
`declared`; deleting the declared one lets the next discovery restore
the discovered one.

`Key.spec`:

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `models[]` | []string | non-empty unless disabled ([027-disabled-key-provisioning](.archive/027-disabled-key-provisioning.md)) | yes | at most 64 selectors, each a Model name or a glob under the glob rule; `*` alone is every model; duplicates `invalid_field`; each selector is a `model.use` decision at resolve, below |
| `limits.requestsPerMinute`, `.tokensPerMinute` | int | `Defaults.RequestsPerMinute`, `Defaults.TokensPerMinute` | yes | `0` is none; above the authorizer's `Limits` is `ceiling_exceeded`, and `0` is above any ceiling, because no limit exceeds every limit |
| `limits.spend.amount` | money | absent | yes | positive; above `Limits.MaxSpend` is `ceiling_exceeded`, and so is an absent spend limit under a `MaxSpend`; requires `window` |
| `limits.spend.currency` | string | `USD` | yes | ISO 4217; a request for a Model priced in another currency is `currency_mismatch` ([[007-keys-and-limits]]) |
| `limits.spend.window` | window | none | yes | the window rule below; `none` is the key's lifetime |
| `budget` | string | absent | yes | a `Budget` the caller may draw from, by name or `bud_` id; `Lookup.Budget` answers `not_found` otherwise; a spend limit and a budget may both be set and both hold |
| `ttl` | duration | absent | no | Go syntax, at least `1m`; `status.expiresAt` is `createdAt` plus `ttl`, `createdAt` being the existing object's on an update and `Now` on a create; `exclusive_fields` with `expiresAt`; above `Limits.MaxTTL` is `ceiling_exceeded`, and so is a Key that never expires under a `MaxTTL` |
| `expiresAt` | timestamp | absent | yes | RFC 3339, in the future at resolve; copied to `status.expiresAt`, the one field the data plane reads for expiry; absent and no `ttl` is never |
| `allowUnpriced` | bool | `false` | yes | with a spend limit or a budget, a request for a Model with no pricing is `model_unpriced` unless this is true ([[007-keys-and-limits]]) |
| `passthrough` | bool | `false` | yes | admits routes the gateway does not translate, embeddings, files, batches, toward a provider one of the selectors reaches ([[004-request-path]]) |
| `disabled` | bool | `false` | yes | refuses every request with `key_disabled` while true; the value is kept |
| `value` | string | none | no | server mode only: a value the caller supplies instead of one the gateway mints, the composition [[007-keys-and-limits]] owns; 32 to 4096 bytes; write-only, decoded into the encoder-skipped field the credential rule below describes, so it is returned by no response, event, record, or log line and the resolved manifest carries the field absent; present on an update, whatever its value, is `immutable_field`; `exclusive_fields` with `valueFrom.env`; `invalid_field` in file mode, because a credential in a file is what `valueFrom` exists to avoid; `status.prefix` is then `sup_` and the first eight lower-case hex characters of `SHA-256(value)`, computed by the surface that stores the hash, since a supplied value has no `lux_` prefix to show; a rotate mints a `lux_` value and the supplied one stops working |
| `valueFrom.env` | string | none | no | file mode only ([[010-state]]): a POSIX variable name whose value is the Key's, matching `^lux_[A-Za-z0-9_-]{40}$` ([[007-keys-and-limits]]); `invalid_field` in server mode, where the server mints the value or the caller supplies one |
| `valueSHA256` | string | none | no | server mode only: the SHA-256 of a value the caller holds only as a hash, 64 lower-case hex characters (`invalid_field` otherwise), the composition [[007-keys-and-limits]] owns for an importer whose source kept hashes alone; write-only exactly as `value` is, decoded into an encoder-skipped member, returned by no response, event, record, or log line, absent from the resolved manifest; stored as the hash a supplied value would have produced, so the Key authenticates by any value whose SHA-256 it is; present on an update is `immutable_field`; `exclusive_fields` with `value` and with `valueFrom.env`; `invalid_field` in file mode; `status.prefix` is `sup_` and the hash's first eight characters; a rotate mints a `lux_` value and the hash stops matching |

`Budget.spec`:

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `amount` | money | none, required | yes | positive |
| `currency` | string | `USD` | no | ISO 4217; a Key whose Models price in another currency is refused at request time with `currency_mismatch` ([[007-keys-and-limits]]) |
| `window` | window | `month` | no | the window rule below |
| `hard` | bool | `true` | yes | `true` refuses with `budget_exhausted` at the limit; `false` emits `budget.exhausted` once per window and continues |

Shared rules:

- Money is a decimal string `^[0-9]+(\.[0-9]{1,6})?$`, at most 12
  integer digits, parsed to an integer count of micro-units
  (`v1.Money`, an `int64`), so no float touches a price and every
  amount fits the integer with headroom for the arithmetic of
  [[009-usage-and-metering]], which multiplies a price by a token
  count; a thirteenth integer digit is refused. Rendered back as the
  shortest string naming the amount, the trailing zeros of the fraction
  dropped, so `"100.50"` reads back `"100.5"`; a JSON number is refused,
  because a float has already lost what the string keeps.
- A window is a Go duration of at least `1m` and at most `8760h`, the
  word `month`, or the word `none`. A duration window is fixed, not
  rolling: window `n` covers `[n·d, (n+1)·d)` from the Unix epoch in
  UTC, so every replica agrees on the boundary without coordination.
  `month` is the calendar month in UTC. `none` never resets.
- A glob is a string of `[a-z0-9._:/*-]`, 1 to 128 characters, where
  `*` matches any run of characters including `/` and every other
  character matches itself; the colon is in the alphabet because a
  local runtime's names carry a tag. There is no `?`, no character
  class, and no escape. A selector without `*` is an exact name and is
  held to the Model name rule.
- A duration is Go syntax, kept as the text the caller wrote
  (`v1.Duration`), so `10m` reads back `10m`; one the configuration
  supplies renders without the zero units Go's own formatting adds. A
  timestamp is RFC 3339. `status` in an applied manifest is ignored,
  never an error, so a caller may `GET`, edit, and `PUT` what it read. Every kind's `status` carries `id`,
  `version`, `owner`, `createdAt`, `updatedAt`, and `warnings`;
  `version` is the store's row version ([[010-state]]), which
  [[011-api]] also sends as the `ETag`, and the rest of `status` is the
  kind's owner's.

### Decoding

`Decode(body []byte, contentType string, hint Hint) (v1.Object, error)`,
shared by every kind. `Hint{APIVersion, Kind, Name string}` is what the
surface already knows: the API fills it from the route (`PUT
/v1/keys/run-42` means `lux.latere.ai/v1beta1`, `Key`, `run-42`), the
file mode and the `lux` command leave it empty.

- Content types: `application/json`; `application/yaml`,
  `application/x-yaml`, `text/yaml`. Anything else is
  `unsupported_media_type`. A body that begins with `{` under a YAML
  type is decoded as JSON.
- One schema decides every field question. A YAML body is parsed by
  `github.com/goccy/go-yaml` into its syntax tree, and the tree is
  turned into a generic one, anchors, aliases, and merge keys resolved
  by this package so the expansion is counted against the limits below
  as it happens; a JSON body is read token by token into the same
  generic tree. The tree is then held to the kind's Go type by the
  `json` tags of that type, the same tags `encoding/json` reads, which
  is what gives an unknown field its full path, and `encoding/json`
  with `DisallowUnknownFields` decodes it. So an unknown field is found
  by one schema with one path spelling whichever format the body came
  in, and YAML and JSON forms of one manifest decode to equal objects
  by construction. A path is dotted for a field, `[n]` for a list
  entry, and `["k"]` for a map key: `spec.targets[1].model`,
  `metadata.labels["tier"]`. A body that does not parse as JSON, or as
  YAML under a YAML type, a duplicate key in either format, and a
  document that is not a mapping are `malformed_body`, with the
  parser's line and column in the developer detail.
- YAML is one document. A second document is `multi_document`.
- Unknown fields anywhere are `unknown_field` with the path. A value of
  the wrong shape, a string for an integer, a list for an object, a
  number for a money string, a string that is not RFC 3339 for a
  timestamp, is `invalid_field` at the path from `Decode`, because
  `v1.Money` and `time.Time` hold a value and not text.
- `apiVersion`, `kind`, and `metadata.name` absent from the body take
  the hint's values, so a body of `{"spec": {...}}` on a kind's route is
  a complete manifest and the ceremony of the envelope is the route's,
  not the caller's. A field present in the body and different from the
  hint is refused: `unsupported_version`, `unsupported_kind`, or
  `invalid_field` at `metadata.name`. Without a hint `apiVersion` and
  `kind` are required, by `missing_field`; `metadata.name` may still be
  absent and is then `Options.NewName`'s at resolve, or `missing_field`
  when there is no generator, which is the file mode's case.
- `apiVersion` other than `lux.latere.ai/v1beta1` is
  `unsupported_version`; an unknown `kind` is `unsupported_kind`. Both
  are checked before anything else, so a caller learns the version
  problem first.
- The body limit is the API's (`LUX_MAX_MANIFEST_BYTES`); the package
  itself sets none. The generic tree, from either format, is held to
  two bounds as it is built: an expansion past 1 MiB of scalar bytes,
  which is what an alias chain buys an attacker, and nesting past 64
  levels are each `invalid_field` at the path where the limit was
  crossed. The library's own depth guard sits far above at ten
  thousand and is not the bound this contract makes.
- `Provider.spec.credential.value` and `Key.spec.value` are each
  decoded into a field the JSON and YAML encoders skip, so the type
  cannot be serialized with the value in it by accident; the store
  reads them through an accessor and the API never encodes the object
  before the store has taken the value out.

### Resolve

```go
// Actor is who is applying: the rendered subject of 006.
type Actor struct {
	Subject string
}

// Lookup answers the references a manifest names, scoped to the actor:
// it returns manifest.ErrNotFound, or an *Error with code not_found, for
// an object that does not exist and for one the authorizer refuses
// (provider.read for a target, budget.draw for a budget, model.use for
// a selector), so existence does not leak, and ErrAuthorizerUnavailable,
// or any other error, when it cannot decide; a nil object with a nil
// error reads as not_found, and a nil Lookup as authorizer_unavailable.
// A Provider comes back without its credential value. The API
// constructs it per request; an importer constructs its own.
type Lookup interface {
	Provider(ctx context.Context, nameOrID string) (*v1.Provider, error)
	Budget(ctx context.Context, nameOrID string) (*v1.Budget, error)
	// Models is the model.use decision for one selector: the models it
	// matches now that the actor may use, possibly none, or a refusal
	// of the selector as a whole.
	Models(ctx context.Context, selector string) ([]v1.ModelRef, error)
}

// ModelRef is one matched Model as the authorizer sees it in the
// model.use resource (006): {"id", "name", "owner"}. It lives in
// manifest/v1 beside the kinds.
type ModelRef struct {
	ID, Name, Owner string
}

// Defaults are the values an absent field takes, from the operator's
// configuration.
type Defaults struct {
	RequestsPerMinute, TokensPerMinute int
	Timeout                            time.Duration
}

// Limits are what the authorizer granted this actor (006); zero is no
// limit. The API decodes them from the decision's limits object, one
// wire name to one field: max_key_requests_per_minute to
// MaxRequestsPerMinute, max_key_tokens_per_minute to
// MaxTokensPerMinute, max_key_spend (a money string) to MaxSpend,
// max_key_ttl (a Go duration string) to MaxTTL. The two remaining
// limits fields, requests_per_minute and max_keys, are the API's own
// and never reach Resolve (011).
type Limits struct {
	MaxRequestsPerMinute, MaxTokensPerMinute int
	MaxSpend                                 v1.Money
	MaxTTL                                   time.Duration
}

type Options struct {
	Actor                 Actor
	Lookup                Lookup
	Defaults              Defaults
	Limits                Limits
	Existing              v1.Object    // the current object on update; nil on create
	FileMode              bool         // valueFrom allowed, value from a file allowed
	AllowPrivateUpstreams bool
	TunnelEnabled         bool         // LUX_TUNNEL_ENABLED; a tunnel: true Provider is refused without it (013)
	PublicURL             *url.URL     // the gateway's own address; a baseURL there is a loop
	Now                   func() time.Time
	NewName               func() string
}

func Resolve(ctx context.Context, in v1.Object, o Options) (*Resolved, error)

type Resolved struct {
	Object   v1.Object // spec and metadata fully resolved; status carries only warnings and, for a Key, the models each selector matched and the effective expiresAt
	Warnings []string
}
```

The stages, in order, each one total before the next begins:

1. Structural validation: the field rules above that need no defaults
   or lookups (syntax, enums, ranges, the name rules, reserved names,
   exclusive pairs, `duplicate_target`, the money, window, glob, and
   upstream host rules, `valueFrom` outside file mode).
2. Defaulting: every absent field with a default is set from the table
   and from `Defaults`; `metadata.name` from `NewName` when absent;
   `targets[].model` from the name; the credential header and scheme
   from the dialect; `status.expiresAt` from `spec.expiresAt`, or from
   `ttl` and the existing object's `createdAt`, `Now` on a create.
   Stage 1 runs over the object as given and the later stages over a
   copy, so the caller's object is never changed.
3. References, through `Lookup`: every target's provider, the budget,
   and every selector. A `not_found` or `authorizer_unavailable` is
   returned with the field's path; any other error `Lookup` returns is
   its own failure, a catalog or a store that could not answer, and is
   returned wrapped with the path and not as a refusal, so the API
   answers `store_unavailable` for it ([[011-api]]). For a Key, the models each selector
   matched now are recorded in `status.selectors[]` as
   `{selector, matched: [names]}`, and a selector that matched nothing
   is a warning, not an error, because a discovered model may appear
   later and the data plane matches selectors at request time
   ([[007-keys-and-limits]]).
4. Limits: `requestsPerMinute`, `tokensPerMinute`, `spend.amount`, and
   the effective `ttl`, from `ttl` or from `expiresAt` less `createdAt`,
   are held to `Limits`; a value above is `ceiling_exceeded` naming the
   field and the limit in the developer detail. A zero `Limits` field is
   no ceiling. On the manifest's side, no limit is above every ceiling:
   a rate of `0`, an absent spend limit, and a Key that never expires
   are each `ceiling_exceeded` under the matching ceiling, so a subject
   with ceilings sets its Keys' limits explicitly, and an operator whose
   `Defaults` are `0` while an authorizer grants ceilings has every
   defaulted Key refused, which the authorizer's operator sees at once.
5. Update rules, when `Existing` is set: every field the table marks
   `no` that differs is collected into one `immutable_field` error
   naming every path; a duration or a window is compared by value, so
   `1h` and `60m` are one. A `Provider` whose `credential.value` is
   absent on update keeps its stored value; one whose value is present
   replaces it and bumps the version, the bump being the API's from
   whether the resolved object carries a value. A `Key` update never
   changes the value: a `spec.value` present on an update is
   `immutable_field` at `spec.value` whether or not it equals the stored
   one, and whatever its length, because the stored one is a hash and
   cannot be compared, and a caller that wants a new value rotates
   ([[007-keys-and-limits]]). An `Existing` of another kind is a
   caller's mistake and a plain error, not a refusal.
6. Consistency across fields: `maxOutputTokens` within `contextWindow`;
   `spend.amount` with `spend.window`; a `Budget` window and the Keys
   drawing from it need no cross-check, because the Key's currency is
   the Models' and is checked at request time.

`Resolve` is deterministic: the same input, options, `Now`, `NewName`,
and `Lookup` answers produce byte-identical output, which is what
[[001-architecture]]'s `TestAPIAndImporterResolveAgree` compares across
surfaces. Discovery ([[005-providers]]) and the file mode
([[010-state]]) call the same function.

### Errors

One type, `*manifest.Error{Code, Paths, Message, Detail}`, where
`Paths` are the JSON paths of the fields the refusal names, one for
most codes, two for `exclusive_fields`, every changed field for
`immutable_field`, none for a body that did not parse; `Message` is
the one fixed sentence of the code in the user register, which
`Code.Message()` gives and which is [[011-api]]'s table verbatim, so
the API writes the same sentence for the same code; and `Detail` is
the developer's, the value, the rule, the line and column, apart from
the sentence as the registers rule asks. [[011-api]] owns the HTTP
status, and its `TestErrorTable` asserts every code here has one row
there.

| Code | When |
|---|---|
| `unsupported_media_type` | the content type is not JSON or YAML |
| `malformed_body` | the body does not parse as JSON, or as YAML under a YAML type; a duplicate key in either; a document that is not a mapping |
| `multi_document` | more than one YAML document |
| `unsupported_version` | `apiVersion` is not `lux.latere.ai/v1beta1` |
| `unsupported_kind` | `kind` is not one of the four |
| `unknown_field` | a field the schema does not have |
| `missing_field` | a required field is absent |
| `invalid_field` | a value fails its syntax, enum, range, name, money, window, glob, or host rule; the YAML limits; a `valueFrom` in server mode; a `Key.spec.value` or `Key.spec.valueSHA256` in file mode |
| `reserved_prefix` | a label or annotation under `lux.latere.ai/`; a reserved header in `headers`; a name beginning with an id prefix |
| `exclusive_fields` | `ttl` with `expiresAt`; `credential.value` with `credential.valueFrom`; `Key.spec.value`, `Key.spec.valueSHA256`, and `Key.spec.valueFrom` with one another; `tunnel: true` with `baseURL` or any `credential` |
| `duplicate_target` | two targets with one `(provider, model)` pair |
| `not_found` | a named `Provider`, `Budget`, or selector the actor cannot see or use |
| `immutable_field` | an update changes a field the table marks `no`, or carries a `Key.spec.value` or `Key.spec.valueSHA256` |
| `ceiling_exceeded` | a resolved value exceeds an authorizer limit |
| `authorizer_unavailable` | `Lookup` could not decide a reference |

The data plane's refusals, `key_disabled`, `key_expired`,
`model_not_found`, `model_not_allowed`, `model_unpriced`,
`rate_limited`, `spend_exceeded`, `budget_exhausted`,
`currency_mismatch`, `dialect_unsupported`, are the gateway's
([[004-request-path]], [[007-keys-and-limits]]); `Resolve` never emits
them.

### Schema evolution

- Within `lux.latere.ai/v1beta1`, a change adds an optional field with
  a default that preserves the previous behavior, or adds an enum
  value. A field never changes type or meaning, and is never removed.
- A manifest accepted by stages 1 and 2 of one `v1` build is accepted
  by every later `v1` build and resolves to the same object, defaults
  aside. Stages 3 to 6 depend on the actor and the referenced objects,
  and are outside the promise.
- A change that cannot meet those rules is `lux.latere.ai/v2`, with a
  conversion in both directions and a period where both are served.
- Until the first tagged release, the schema may still change; the
  CHANGELOG names every field change.
- `manifest/testdata/v1/` holds the golden corpus; a change that
  alters a golden file is a schema change and needs its CHANGELOG line.
- The OpenAPI description of every kind is generated from the Go types
  ([[011-api]]) and a drift between the two fails the gate.

### The golden corpus

One directory, read by this package's `TestGoldenCorpus` and by the
`manifest` group of [[018-conformance-suite]], so the contract the unit
test holds and the contract the suite proves against a server are one
set of files:

| Path | Holds |
|---|---|
| `manifest/testdata/v1/accepted/<kind>/<case>.yaml` | one manifest `Resolve` accepts under the fixed options; the four examples above are `provider/openai.yaml`, `model/gpt-5.yaml`, `key/run-42.yaml`, `budget/team-research.yaml` |
| `manifest/testdata/v1/accepted/<kind>/<case>.golden.json` | the `Resolved.Object` of that case, JSON, indented two spaces, keys in struct order; `status` carries only `warnings` and, for a Key, `selectors` |
| `manifest/testdata/v1/refused/<code>/<case>.yaml` | one manifest refused with `<code>`, one case per row of the code's `When` column that stage 1 or 2 can reach |
| `manifest/testdata/v1/refused/<code>/<case>.golden.json` | `{"code": "...", "paths": ["..."]}`: the code and the JSON paths the error names |
| `manifest/testdata/v1/options.json` | the fixed options, below, so a reader can reproduce a golden file by hand |

The directory is embedded by the `manifest` package and exported as
`manifest.Corpus`, an `fs.FS` rooted at `testdata/v1` through `fs.Sub`
over the embedded tree, since an `embed.FS` cannot be re-rooted, so the
`manifest` group of [[018-conformance-suite]] reads the same files
through the import from any module and never by a relative path;
`//go:embed` cannot reach another package's directory, and a copy would
drift. The accepted cases are one installation's worth of objects and
apply in kind order, Provider, Budget, Model, Key, because a Model
names a Provider and a Key names Models and a Budget.

The fixed options: `Now` is `2026-09-13T10:00:00Z`; `NewName` returns
`fixed-name-0000`; `Defaults` is `{0, 0, 10m}`; `Limits` is zero;
`Existing` is nil; `FileMode` false; `AllowPrivateUpstreams` false;
`TunnelEnabled` true; `PublicURL` is `https://lux.example.com`; `Lookup`
answers from the accepted corpus itself, so `openai` and `azure` are
Providers, `team-research` is a Budget, `gpt-5` matches `gpt-5`, and
`anthropic/*` matches `anthropic/claude-sonnet-4` and
`anthropic/claude-opus-4`; `Actor` is `https://login.example.com|alice`
and is read by nothing. The suite applies the accepted cases through
the API and compares the read-back's `spec` and `metadata` to the
golden file; the refused cases it sends and compares the code and
`details.paths`. The golden of an accepted case is a fixed point:
decoded and resolved again under the same options it is byte-identical,
which is what lets a caller `GET`, edit, and `PUT` what it read. A case the corpus cannot express, one that needs a
second object's state or a caller's identity, is a criterion of the
kind's owner and not a corpus file.

### Package layout

`manifest/v1` holds the types of every kind, `Money`, `Duration`,
`Window` with its parser and its `Bounds`, `ModelRef`, and the `Object`
interface every kind implements (`Kind() string`, `ID() string`,
`Owner() string`, `Name() string`), which `Decode` returns and the
store keys by; it imports nothing but the standard library, so
`metering` imports it for `Pricing` and `Money` without pulling the
resolver. The money and window parsers live here beside their types,
`ParseMoney` and `Window.Validate`, because `Money`'s JSON decoding
needs the parser. A kind's Go value stores no `apiVersion` and no
`kind`: its kind is its type and its version is the package's, so its
`MarshalJSON` writes both first and a caller building a literal fills
neither, which is also why the interface method can be `Kind()`. A
field whose explicit zero differs from its default is a pointer,
`Target.Weight`, `KeyLimits.RequestsPerMinute` and `TokensPerMinute`,
`BudgetSpec.Hard`, and the four prices, and is set on every resolved
object. It also holds `NewID(prefix string, now
time.Time, random io.Reader) string`, the one generator of every
prefixed ULID in the tree, `prv_`, `mdl_`, `key_`, `bud_`, `req_`,
`evt_`, and [[013-tunnelled-runtimes]]'s `tun_`: 48 bits of
milliseconds and 80 random bits in Crockford base32, written with the
standard library rather than a ULID module, so the binaries' build
lists gain nothing for an identifier. `manifest` holds `Decode`,
`Resolve`, the glob matcher, the field rules, the upstream host rule,
and the error type, and imports `manifest/v1`,
`github.com/goccy/go-yaml` for the YAML parse, and the standard
library. The YAML module is the one dependency this package adds to
`./cmd/luxd` and `./cmd/lux`, and each binary's `depcheck` row names it
([[002-repository-scaffold]], [[014-agent-client]]); it is already in
the module graph through `latere.ai/x/pkg`. Neither package imports
`internal/`, `gateway`, or `metering`. The glob matcher is exported as
`manifest.Match(selector, name string) bool` so the gateway matches a
request's model against a Key's selectors with the same function that
validated them.

## Not in this spec

What the gateway does with a resolved Provider, Model, Key, or Budget
([[005-providers]], [[008-routing-and-models]], [[007-keys-and-limits]]);
where `Defaults` come from ([[002-repository-scaffold]]) and `Limits`
([[006-identity]]); how the file mode reads a directory
([[010-state]]); the HTTP mapping and user sentences of the errors
([[011-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The four examples above decode from YAML and from their JSON forms to equal objects, and resolve without error under the corpus's fixed options | `TestDecodeYAMLAndJSONAgree`, `TestTheExamplesResolve` | passing |
| Every unknown field, at any depth in every kind, is refused with its path, and the path is spelled the same from a YAML and a JSON body | `TestUnknownFieldNamesThePath`, table-driven over twenty paths in both formats | passing |
| A second YAML document, a wrong version, a wrong kind, an unsupported content type, and a body that does not parse are refused with their codes, version before kind; `malformed_body` carries the line and column in the detail | `TestDecodeRefusals` | passing |
| A name beginning with `prv_`, `mdl_`, `key_`, or `bud_` is `reserved_prefix` for every kind, and `openai/mdl_x` and `relay/anthropic/claude-sonnet-4` are valid Model names | `TestNamesNeverLookLikeIds`, `TestFieldSyntax` | passing |
| A body of `spec` alone decodes under a hint to the hinted version, kind, and name, byte-identical to the full envelope's result; a body whose envelope disagrees with the hint is refused with the field's code; without a hint the envelope is required | `TestHintFillsTheEnvelope`, `TestHintDisagreementIsRefused` | passing |
| An alias chain past 1 MiB and nesting past 64 levels are each refused in under 100 ms | `TestYAMLLimits` | passing |
| Every syntax rule in the field tables has a refusing case: the three name rules, money, window, glob, duration, RFC 3339, ISO 4217, header names, ranges of weight, priority, per, concurrency, timeout | `TestFieldSyntax`, table-driven | passing |
| Every default in the tables is applied and returned; a field the caller set is never overwritten; the credential header and scheme follow the dialect; `expiresAt` is `Now` plus `ttl`; an absent name comes from `NewName` | `TestDefaultsFillOnlyAbsentFields`, `TestDialectDefaults`, `TestNameGeneration` | passing |
| Each `exclusive_fields`, `duplicate_target`, and `missing_field` case in the tables is refused with the code | `TestExclusiveMissingAndDuplicates` | passing |
| The upstream host rule: an IP literal is accepted; a single label, a loopback, link-local, and private address, `.local`, `.internal`, `http://`, userinfo, query, and fragment are `invalid_field`; with `AllowPrivateUpstreams` the private forms resolve with a warning; the `PublicURL` host is refused in both modes | `TestUpstreamHostRule` | passing |
| `credential.value` is absent from the JSON and YAML encodings of a decoded Provider and present through the accessor; `valueFrom` is `invalid_field` in server mode and accepted in file mode | `TestCredentialValueNeverEncodes`, `TestValueFromIsFileModeOnly` | passing |
| `Key.spec.value` of 32 bytes resolves and of 31 or 4097 is `invalid_field`; it is absent from the JSON and YAML encodings of the decoded Key and present through the accessor; with `valueFrom.env` it is `exclusive_fields`; in file mode it is `invalid_field`; on an update, equal to the stored value or not, it is `immutable_field` at `spec.value` | `TestSuppliedValueSchema`, table-driven | passing |
| `Key.spec.valueSHA256` of 64 lower-case hex characters resolves; 63 or 65 characters, an upper-case digit, or a non-hex character is `invalid_field`; it is absent from the JSON and YAML encodings of the decoded Key and present through the accessor; with `value` or with `valueFrom.env` it is `exclusive_fields`; in file mode it is `invalid_field`; on an update it is `immutable_field` at `spec.valueSHA256` | `TestHashSuppliedValueSchema`, table-driven | passing |
| `Lookup` returning not-found and refused both surface as `not_found` with the field's path; unavailable surfaces as `authorizer_unavailable`; any other `Lookup` error passes through as a plain error and never as a refusal; a selector matching nothing is a warning and the resolved Key records every selector's matches | `TestLookupErrors`, `TestLookupFailurePassesThrough`, `TestSelectorsRecordTheirMatches` | passing |
| Every immutable field changed on update is named in one `immutable_field` error; a Provider update without a value keeps the stored one and with a value bumps the version | `TestImmutableFields`, `TestCredentialUpdate` | passing |
| Limits refuse with the field and the limit; a zero limit is no limit | `TestLimits` | passing |
| The glob matcher: `*` matches across `/`, a selector without `*` is exact, and the same function accepts a selector and matches a name | `TestGlob`, table-driven | passing |
| The window arithmetic: a duration window's boundary is the same for every `Now` inside it and differs across it; `month` resets on the first of the month UTC; `none` never resets | `TestWindows` in `manifest/v1` | passing |
| Money parses and renders without loss for every corpus value, the shortest form of each, and refuses seven fraction digits and a thirteenth integer digit | `TestMoney` | passing |
| Every accepted corpus case resolves byte-identically to its golden file under `options.json`, every refused case yields its code and paths, every kind has at least one accepted case, and every code of the table that stage 1 or 2 can raise has at least one refused case | `TestGoldenCorpus`, `TestCorpusCoversEveryDecodeCode` | passing |
| `manifest/v1` imports the standard library alone, `manifest` adds only `manifest/v1` and `github.com/goccy/go-yaml`, and neither imports `internal/`, `gateway`, or `metering` | `TestManifestImports`, [[001-architecture]]'s `TestRootPackagesDialNothing` | passing |
| `NewID` yields 26 Crockford base32 characters after the prefix, sorts by the time given, and ten thousand ids at one instant are distinct | `TestNewID` in `manifest/v1` | passing |

## Outcome

Built on 2026-09-14 in `manifest` and `manifest/v1`, with the golden
corpus under `manifest/testdata/v1/`: seventeen accepted cases and
eighty-four refused ones. Every criterion's named test passes;
coverage is 95.4% of `manifest` and 98.8% of `manifest/v1`, and the
gate passes whole. The Design text above was changed wherever the code
had to depart from the dispatched text, so the two agree; the
departures and their reasons:

- Model names are one or more `/`-joined segments, not one or two. An
  aggregator's upstream names carry a `/` themselves, so a discovered
  Model named `<provider>/<upstream name>` can have three segments, and
  the two-segment rule would have refused every such model at
  discovery. The corpus carries `relay/anthropic/claude-sonnet-4`.
  [[011-api]]'s model routes, which refuse three segments or more,
  must admit them.
- Money is at most 12 integer digits, not 18, and renders in its
  shortest form rather than with the digits it was given: `v1.Money`
  is an `int64` of micro-units, as [[009-usage-and-metering]]'s
  arithmetic needs, and eighteen integer digits of micro-units do not
  fit one; an integer cannot remember how many zeros it was written
  with.
- The glob alphabet gained `:`, so a selector can name a local
  runtime's tagged model by prefix.
- The error type carries `Paths`, plural, and a `Detail` apart from the
  fixed `Message`: one refusal names two fields for `exclusive_fields`
  and every changed field for `immutable_field`, and the registers rule
  keeps the developer's detail out of the user's sentence.
- Decoding holds the generic tree to the kind's type by its `json`
  tags before `encoding/json` decodes it, because `encoding/json`'s
  unknown-field error carries the key and no path; the money and
  timestamp syntax is therefore refused at `Decode`, the two bounds
  apply to JSON as to YAML, a duplicate key is `malformed_body`, and a
  map key is spelled `["k"]` in a path.
- A Key's effective expiry is `status.expiresAt`, from `spec.expiresAt`
  or from `ttl` and `createdAt`, so `ttl` and `expiresAt` stay
  exclusive in `spec` and a read-back re-applies; the Key's golden
  therefore carries `expiresAt` beside `selectors` and `warnings`.
- Under a ceiling, no limit is above it: a rate of `0`, an absent
  spend limit, and a Key that never expires are `ceiling_exceeded`.
  The text said only that a value above the ceiling is refused; a Key
  asking for no limit asks for more than any ceiling grants.
- `Corpus` is an `fs.FS` rooted at `testdata/v1` through `fs.Sub`, not
  an `embed.FS`, which cannot be re-rooted.
- The money and window parsers live in `manifest/v1`, because
  `Money.UnmarshalJSON` needs the parser and `v1` imports nothing of
  `manifest`. `TestWindows` and `TestNewID` live there with them.
- A kind's Go value stores no `apiVersion` or `kind`; `MarshalJSON`
  writes them. A struct field named `Kind` cannot coexist with the
  `Kind()` method the `Object` interface names.
- Fields whose explicit zero differs from their default are pointers:
  `weight`, the two rates, `hard`, and the four prices. A free model
  prices at `"0"`, and a fallback-only target weighs `0`.
- Without a hint, `metadata.name` may be absent from a decoded body and
  is `NewName`'s at resolve, or `missing_field` with no generator; the
  text had listed it among the three required fields while also
  letting `NewName` supply it.
- Stage 1 runs over the caller's object and the later stages over a
  copy, so `Resolve` never changes its input; the copy is through JSON,
  which is why stage 1 has to run first: `omitempty` would turn an
  empty modalities list into an absent one.

What other specs carry from this: [[011-api]]'s model routes admit
names of three or more segments, and its handlers take the fixed
sentence from `manifest.Code.Message()` and the envelope's `paths` and
`detail` from `Error.Paths` and `Error.Detail`; [[009-usage-and-metering]]'s
`Cost` dereferences the `*v1.Money` prices and its `Window` function
wraps `v1.Window.Bounds`; [[004-request-path]] and
[[008-routing-and-models]] split a discovered name at its first `/`;
[[006-identity]] and [[002-repository-scaffold]] note that `Defaults`
of `0` under an authorizer's ceilings refuse every defaulted Key;
[[018-conformance-suite]] applies the accepted corpus in kind order,
`PUT`s the nameless Key case to `fixed-name-0000`, and expects the
`platform-dev` case to carry a supplied value.

Amended 2026-09-14, after the identity build: a `Lookup` error that is
neither a refusal nor one of the two sentinels passes through `Resolve`
unchanged in kind, where before it read as `authorizer_unavailable`, so
a store that cannot answer is reported as the store and not as the
authorizer (`TestLookupFailurePassesThrough`).
