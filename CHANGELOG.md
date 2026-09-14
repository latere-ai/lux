# Changelog

What changed for whoever writes a manifest, runs `luxd`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- The request path: the `gateway` package is the data plane as one
  `http.Handler` for the four doors, `/openai`, `/anthropic`, `/gemini`,
  and `/lux`, each serving its dialect's own API under its prefix. A
  request presents a Key in any of the forms the SDKs use, names a
  model, and is answered by the provider that serves it: byte for byte
  when the door's dialect and the provider's are the same, translated
  through `latere.ai/x/pkg/llmdialect` when they differ with every
  field the provider cannot take named in `Lux-Loss`, streamed as it
  arrives either way. Every refusal is one fixed code in the door's own
  error shape with `Lux-Error` beside it, raised before a byte reaches a
  provider; `GET /v1/models` on a door is the Key's own model list in
  that dialect's shape; a token count no provider answers is estimated
  and says so with `Lux-Estimated: true`. Every request ends in one
  record and counts in `lux_requests_total`,
  `lux_request_duration_seconds`, and `lux_time_to_first_byte_seconds`.
  The handler is not mounted yet: the Key lookup, the windows, the
  routing, the upstream client, and the record's cost are the specs
  that follow, and `luxd serve` gains the doors when they land.
- The store: `luxd serve` holds desired state, Key hashes, credential
  rows, spend counters, leases, the event journal, and the tunnel
  registry in memory by default and says so at start in one line, and
  `LUX_MANIFEST_DIR` selects the file mode, a directory of manifests read
  at start and on `SIGHUP`, one object per `.yaml` or `.json` file, with
  a Provider's credential and a Key's value read from the variables the
  manifests name and the API read-only. `LUX_DB_URL` and
  `LUX_DB_MAX_CONNS` are read and checked; the Postgres store itself
  lands in a later release, and setting the URL is refused at start
  until it does.
- Identity for the control plane: `luxd` accepts a bearer from any
  OpenID Connect issuer listed in `LUX_OIDC_ISSUERS`, signed `RS256` or
  `ES256`, with `aud` containing `LUX_OIDC_AUDIENCE` (default `lux`),
  and refuses to start on an issuer it cannot read or whose key set has
  no usable key. Permission is asked of the authorizer at
  `LUX_AUTHORIZER_URL` with `LUX_AUTHORIZER_TOKEN`, in the twenty-four
  actions of the identity spec with one flat `resource` per action and
  every claim of the token forwarded verbatim; a deny is `forbidden`,
  any answer that is no decision is `authorizer_unavailable`, and a
  refused reference in a manifest reads as `not_found`. Without an
  authorizer the built-in owner policy applies, with `LUX_ADMIN_SUBJECTS`
  as its administrators. `LUX_AUTHORIZER_TIMEOUT` bounds one decision;
  `LUX_OIDC_INSECURE_ISSUERS` admits an `http://` issuer off loopback
  for a test. The `/v1` routes that use all of this are not mounted
  yet; the specs say when.
- The manifest contract: `manifest` and `manifest/v1` decode a
  `Provider`, `Model`, `Key`, or `Budget` from YAML or JSON with one
  schema, refuse an unknown field with its path, validate every field
  rule, fill every default, and resolve the references through a
  `Lookup`; the golden corpus under `manifest/testdata/v1/` is the
  contract's fixture. Fields of note: a Model name is any number of
  `/`-joined segments; money is at most 12 integer digits and renders
  in its shortest form; a Key's effective expiry is `status.expiresAt`.
- The repository: the `luxd` binary serving its probes on two listeners,
  typed configuration from `LUX_*` variables, the quality gate, and the
  design specs. Nothing routes a model request yet; the specs say what
  will.
- The design: Lux is an LLM gateway that serves every provider's API at
  one address; the API group is `lux.latere.ai/v1beta1`; four kinds, a
  `Provider` for an upstream with its dialect, base URL, and write-only
  credential, a `Model` for a routable name with targets, weights,
  fallback, and pricing, a `Key` for the credential a workload holds with
  its model selectors, limits, and expiry, and a `Budget` for a spend
  window several keys draw from. A provider credential never leaves the
  gateway. Identity comes from any OpenID Connect issuer and permission
  from an authorizer endpoint the operator writes, with a built-in owner
  policy. Dialect translation between OpenAI, Anthropic, Gemini, and the
  lux-native dialect goes through `latere.ai/x/pkg/llmdialect`.
