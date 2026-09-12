---
title: "Manifest contract: the four kinds, decoding, validation, defaulting, resolve"
status: drafted
track: core
depends_on:
  - specs/001-architecture.md
affects: [manifest/, manifest/v1/, internal/api/, internal/store/, docs/]
effort: large
created: 2026-09-13
updated: 2026-09-13
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

Nothing is built. The hosted gateway this design is extracted from
declares providers, models, and keys through hand-written JSON endpoints
with no shared schema, no defaulting a caller can read back, and no
notion of a budget beyond a per-key spend cap. Discovered model lists
are fetched from upstreams and merged into a catalog table by code paths
that differ per provider.

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
  owner: https://login.example.com|alice
  credential: {set: true, version: 2, prefix: "sk-live-", updatedAt: 2026-09-13T10:00:00Z}
  health: {state: Healthy, since: 2026-09-13T10:00:05Z, lastProbeAt: 2026-09-13T10:41:00Z, lastError: ""}
  discovered: {count: 34, at: 2026-09-13T10:00:05Z}
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
  models: ["gpt-5", "anthropic/*"]     # required, non-empty: exact names or globs
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
  owner: https://login.example.com|alice
  prefix: lux_ab12cd34ef56             # the first twelve characters of the value
  value: lux_ab12cd34ef56...           # in the response that created the key, and nowhere else
  state: Active                        # Active | Disabled | Expired | Exhausted
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
| `name` | string | no | the name rule of the kind below; unique per kind in one installation; generated by `Options.NewName` when absent, `<adjective>-<noun>-<4 hex>` from the API's generator |
| `labels` | map | yes | Kubernetes label syntax for keys and values; keys under `lux.latere.ai/` are `reserved_prefix` |
| `annotations` | map | yes | keys as labels, values any string up to 4 KiB; total at most 64 KiB; `lux.latere.ai/` refused |

Name rules. A `Provider`, `Key`, or `Budget` name is a DNS-1123 label
of at most 63 characters. A `Model` name is one or two segments joined
by `/`, each segment `[a-z0-9]([a-z0-9._-]*[a-z0-9])?`, at most 128
characters in all, because model names carry dots (`gpt-4.1`) and a
discovered model is named `<provider>/<upstream name>`, the form callers
already write. A declared Model may use either form. The upstream name
in a target is the provider's own string, any printable characters up
to 256, never validated beyond that.

`Provider.spec`. The mutability column names whether a field may change
after create: `no`, or `yes` for any caller the authorizer allows.

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `dialect` | enum | none, required | no | `openai`, `anthropic`, `gemini`, `lux`; the wire API the upstream speaks, per the dialect table below |
| `tunnel` | bool | `false` | no | `true` makes the upstream whichever agent connects through the tunnel rather than an address the gateway holds ([[013-tunnelled-runtimes]]); `exclusive_fields` with `baseURL` and with `credential`; `invalid_field` when `Options.TunnelEnabled` is false |
| `baseURL` | string | none, required unless `tunnel` | yes | `https://`, a host under the upstream host rule below, an optional path, no userinfo, query, or fragment; `http://` and a loopback, link-local, or private host only when `Options.AllowPrivateUpstreams` is set, and then with a warning; the host of `Options.PublicURL` is `invalid_field`, because a provider that is this gateway is a loop |
| `credential.value` | string | none | yes; bumps `status.credential.version` | write-only; required in server mode unless `valueFrom` is set in file mode or `tunnel` is true; 1 to 4096 bytes; returned by no read, carried by no event or log |
| `credential.valueFrom.env` | string | none | no | a POSIX variable name the file mode reads at start; `exclusive_fields` with `value`; `invalid_field` in server mode |
| `credential.header` | string | per dialect | yes | a header name; `Authorization` for `openai` and `lux`, `x-api-key` for `anthropic`, `x-goog-api-key` for `gemini` |
| `credential.scheme` | enum | per dialect | yes | `bearer` prefixes `Bearer `; `raw` writes the value verbatim; `bearer` on `Authorization`, `raw` elsewhere |
| `headers` | map | empty | yes | header names to values, at most 16, values up to 4 KiB; `credential.header`, `Host`, `Content-Length`, and the hop-by-hop headers are `reserved_prefix` |
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
| `pricing.input`, `.output`, `.cachedInput`, `.cacheWrite` | money | `cachedInput` defaults to `input`, `cacheWrite` to `input`; the first two required | yes | the money rule below, non-negative |
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
| `models[]` | []string | none, required, non-empty | yes | at most 64 selectors, each a Model name or a glob under the glob rule; `*` alone is every model; duplicates `invalid_field`; each selector is a `model.use` decision at resolve, below |
| `limits.requestsPerMinute`, `.tokensPerMinute` | int | `Defaults.RequestsPerMinute`, `Defaults.TokensPerMinute` | yes | `0` is none; above the authorizer's `Limits` is `ceiling_exceeded` |
| `limits.spend.amount` | money | absent | yes | positive; above `Limits.MaxSpend` is `ceiling_exceeded`; requires `window` |
| `limits.spend.currency` | string | `USD` | yes | ISO 4217; a request for a Model priced in another currency is `currency_mismatch` ([[007-keys-and-limits]]) |
| `limits.spend.window` | window | none | yes | the window rule below; `none` is the key's lifetime |
| `budget` | string | absent | yes | a `Budget` the caller may draw from, by name or `bud_` id; `Lookup.Budget` answers `not_found` otherwise; a spend limit and a budget may both be set and both hold |
| `ttl` | duration | absent | no | Go syntax, at least `1m`; `expiresAt` is `createdAt` plus `ttl`; `exclusive_fields` with `expiresAt`; above `Limits.MaxTTL` is `ceiling_exceeded` |
| `expiresAt` | timestamp | absent | yes | RFC 3339, in the future at resolve; absent and no `ttl` is never |
| `allowUnpriced` | bool | `false` | yes | with a spend limit or a budget, a request for a Model with no pricing is `model_unpriced` unless this is true ([[007-keys-and-limits]]) |
| `passthrough` | bool | `false` | yes | admits routes the gateway does not translate, embeddings, files, batches, toward a provider one of the selectors reaches ([[004-request-path]]) |
| `disabled` | bool | `false` | yes | refuses every request with `key_disabled` while true; the value is kept |
| `valueFrom.env` | string | none | no | file mode only ([[010-state]]): a POSIX variable name whose value is the Key's, matching `^lux_[A-Za-z0-9_-]{40}$` ([[007-keys-and-limits]]); `invalid_field` in server mode, where the server mints the value |

`Budget.spec`:

| Field | Type | Default | Mutable | Rule |
|---|---|---|---|---|
| `amount` | money | none, required | yes | positive |
| `currency` | string | `USD` | no | ISO 4217; a Key whose Models price in another currency is refused at request time with `currency_mismatch` ([[007-keys-and-limits]]) |
| `window` | window | `month` | no | the window rule below |
| `hard` | bool | `true` | yes | `true` refuses with `budget_exhausted` at the limit; `false` emits `budget.exhausted` once per window and continues |

Shared rules:

- Money is a decimal string `^[0-9]+(\.[0-9]{1,6})?$`, at most 18
  integer digits, parsed to an integer count of micro-units
  (`v1.Money`), so no float touches a price. Rendered back with the
  fraction digits it was given, and the arithmetic of
  [[009-usage-and-metering]] is integer arithmetic on micro-units.
- A window is a Go duration of at least `1m` and at most `8760h`, the
  word `month`, or the word `none`. A duration window is fixed, not
  rolling: window `n` covers `[n·d, (n+1)·d)` from the Unix epoch in
  UTC, so every replica agrees on the boundary without coordination.
  `month` is the calendar month in UTC. `none` never resets.
- A glob is a string of `[a-z0-9._/*-]`, 1 to 128 characters, where
  `*` matches any run of characters including `/` and every other
  character matches itself. There is no `?`, no character class, and
  no escape. A selector without `*` is an exact name.
- A duration is Go syntax. A timestamp is RFC 3339. `status` in an
  applied manifest is ignored, never an error, so a caller may `GET`,
  edit, and `PUT` what it read.

### Decoding

`Decode(body []byte, contentType string) (v1.Object, error)`, shared by
every kind.

- Content types: `application/json`; `application/yaml`,
  `application/x-yaml`, `text/yaml`. Anything else is
  `unsupported_media_type`. A body that begins with `{` under a YAML
  type is decoded as JSON.
- YAML is one document. A second document is `multi_document`.
- Unknown fields anywhere are `unknown_field` with the path.
- `apiVersion` other than `lux.latere.ai/v1beta1` is
  `unsupported_version`; an unknown `kind` is `unsupported_kind`. Both
  are checked before anything else, so a caller learns the version
  problem first.
- The body limit is the API's (`LUX_MAX_MANIFEST_BYTES`); the package
  itself sets none. The YAML decoder refuses alias expansion beyond
  1 MiB and nesting beyond 64 levels, each with `invalid_field`.
- `credential.value` is decoded into a field the JSON and YAML encoders
  skip, so the type cannot be serialized with the value in it by
  accident; the store reads it through an accessor and the API never
  encodes the object before the store has taken the value out.

### Resolve

```go
// Actor is who is applying: the rendered subject of 006.
type Actor struct {
	Subject string
}

// Lookup answers the references a manifest names, scoped to the actor:
// it returns not_found for an object that does not exist and for one
// the authorizer refuses (provider.read for a target, budget.draw for
// a budget, model.use for a selector), so existence does not leak, and
// authorizer_unavailable when it cannot decide. A Provider comes back
// without its credential value. The API constructs it per request; an
// importer constructs its own.
type Lookup interface {
	Provider(ctx context.Context, nameOrID string) (*v1.Provider, error)
	Budget(ctx context.Context, nameOrID string) (*v1.Budget, error)
	// Models is the model.use decision for one selector: the models it
	// matches now that the actor may use, possibly none, or a refusal
	// of the selector as a whole.
	Models(ctx context.Context, selector string) ([]v1.ModelRef, error)
}

// Defaults are the values an absent field takes, from the operator's
// configuration.
type Defaults struct {
	RequestsPerMinute, TokensPerMinute int
	Timeout                            time.Duration
}

// Limits are what the authorizer granted this actor (006); zero is no
// limit.
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
	Object   v1.Object // spec and metadata fully resolved; status carries only warnings and, for a Key, the models each selector matched
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
   from the dialect; `expiresAt` from `ttl` and `Now`.
3. References, through `Lookup`: every target's provider, the budget,
   and every selector. A `not_found` or `authorizer_unavailable` is
   returned with the field's path. For a Key, the models each selector
   matched now are recorded in `status.selectors[]` as
   `{selector, matched: [names]}`, and a selector that matched nothing
   is a warning, not an error, because a discovered model may appear
   later and the data plane matches selectors at request time
   ([[007-keys-and-limits]]).
4. Limits: `requestsPerMinute`, `tokensPerMinute`, `spend.amount`, and
   the effective `ttl` are held to `Limits`; a value above is
   `ceiling_exceeded` naming the field and the limit. A zero limit is
   no limit.
5. Update rules, when `Existing` is set: every field the table marks
   `no` that differs is collected into one `immutable_field` error
   naming every path. A `Provider` whose `credential.value` is absent on
   update keeps its stored value; one whose value is present replaces
   it and bumps the version. A `Key` update never changes the value.
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

One type, `*manifest.Error{Code, Path, Message}`, where `Path` is the
JSON path of the field and `Message` is one sentence in the user
register. [[011-api]] owns the fixed sentence per code and the HTTP
status, and its `TestErrorTable` asserts every code here has one.

| Code | When |
|---|---|
| `unsupported_media_type` | the content type is not JSON or YAML |
| `multi_document` | more than one YAML document |
| `unsupported_version` | `apiVersion` is not `lux.latere.ai/v1beta1` |
| `unsupported_kind` | `kind` is not one of the four |
| `unknown_field` | a field the schema does not have |
| `missing_field` | a required field is absent |
| `invalid_field` | a value fails its syntax, enum, range, name, money, window, glob, or host rule; the YAML limits; a `valueFrom` in server mode |
| `reserved_prefix` | a label or annotation under `lux.latere.ai/`, or a reserved header in `headers` |
| `exclusive_fields` | `ttl` with `expiresAt`; `credential.value` with `credential.valueFrom`; `tunnel: true` with `baseURL` or any `credential` |
| `duplicate_target` | two targets with one `(provider, model)` pair |
| `not_found` | a named `Provider`, `Budget`, or selector the actor cannot see or use |
| `immutable_field` | an update changes a field the table marks `no` |
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
  a default that preserves the previous behaviour, or adds an enum
  value. A field never changes type or meaning, and is never removed.
- A manifest accepted by stages 1 and 2 of one `v1` build is accepted
  by every later `v1` build and resolves to the same object, defaults
  aside. Stages 3 to 6 depend on the actor and the referenced objects,
  and are outside the promise.
- A change that cannot meet those rules is `lux.latere.ai/v2`, with a
  conversion in both directions and a period where both are served.
- Until the first tagged release, the schema may still change; the
  CHANGELOG names every field change.
- `manifest/testdata/v1/` holds a corpus of accepted manifests with
  golden resolved outputs under fixed options; a change that alters a
  golden file is a schema change and needs its CHANGELOG line.
- The OpenAPI description of every kind is generated from the Go types
  ([[011-api]]) and a drift between the two fails the gate.

### Package layout

`manifest/v1` holds the types of every kind, `Money`, `Window`, and
the `Object` interface every kind implements (`Kind() string`, `ID()
string`, `Owner() string`, `Name() string`), which `Decode` returns and
the store keys by; it imports nothing but the standard library, so
`metering` imports it for `Pricing` and `Money` without pulling the
resolver. `manifest` holds `Decode`, `Resolve`, the glob matcher, the
money and window parsers, the upstream host rule, and the error type,
and imports `manifest/v1` and the standard library. Neither imports
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
| The four examples above decode from YAML and from their JSON forms to equal objects, and resolve without error under options where the actor sees the providers and the budget they name | `TestDecodeYAMLAndJSONAgree`, `TestTheExamplesResolve` | not built |
| Every unknown field, at any depth in every kind, is refused with its path | `TestUnknownFieldNamesThePath`, table-driven over twenty paths | not built |
| A second YAML document, a wrong version, a wrong kind, and an unsupported content type are refused with their codes, version before kind | `TestDecodeRefusals` | not built |
| An alias chain past 1 MiB and nesting past 64 levels are each refused in under 100 ms | `TestYAMLLimits` | not built |
| Every syntax rule in the field tables has a refusing case: the three name rules, money, window, glob, duration, RFC 3339, ISO 4217, header names, ranges of weight, priority, per, concurrency, timeout | `TestFieldSyntax`, table-driven | not built |
| Every default in the tables is applied and returned; a field the caller set is never overwritten; the credential header and scheme follow the dialect; `expiresAt` is `Now` plus `ttl`; an absent name comes from `NewName` | `TestDefaultsFillOnlyAbsentFields`, `TestDialectDefaults`, `TestNameGeneration` | not built |
| Each `exclusive_fields`, `duplicate_target`, and `missing_field` case in the tables is refused with the code | `TestExclusiveMissingAndDuplicates` | not built |
| The upstream host rule: an IP literal is accepted; a single label, a loopback, link-local, and private address, `.local`, `.internal`, `http://`, userinfo, query, and fragment are `invalid_field`; with `AllowPrivateUpstreams` the private forms resolve with a warning; the `PublicURL` host is refused in both modes | `TestUpstreamHostRule` | not built |
| `credential.value` is absent from the JSON and YAML encodings of a decoded Provider and present through the accessor; `valueFrom` is `invalid_field` in server mode and accepted in file mode | `TestCredentialValueNeverEncodes`, `TestValueFromIsFileModeOnly` | not built |
| `Lookup` returning not-found and refused both surface as `not_found` with the field's path; unavailable surfaces as `authorizer_unavailable`; a selector matching nothing is a warning and the resolved Key records every selector's matches | `TestLookupErrors`, `TestSelectorsRecordTheirMatches` | not built |
| Every immutable field changed on update is named in one `immutable_field` error; a Provider update without a value keeps the stored one and with a value bumps the version | `TestImmutableFields`, `TestCredentialUpdate` | not built |
| Limits refuse with the field and the limit; a zero limit is no limit | `TestLimits` | not built |
| The glob matcher: `*` matches across `/`, a selector without `*` is exact, and the same function accepts a selector and matches a name | `TestGlob`, table-driven | not built |
| The window arithmetic: a duration window's boundary is the same for every `Now` inside it and differs across it; `month` resets on the first of the month UTC; `none` never resets | `TestWindows` | not built |
| Money parses and renders without loss for every corpus value and refuses seven fraction digits and a nineteenth integer digit | `TestMoney` | not built |
| The golden corpus resolves byte-identically under fixed options; `manifest`, `manifest/v1`, and `metering` import nothing under `internal/` | `TestGoldenCorpus`, [[001-architecture]]'s `TestRootPackagesDialNothing` | not built |
