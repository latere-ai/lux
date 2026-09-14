# Changelog

What changed for whoever writes a manifest, runs `luxd`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- The usage routes: `GET /v1/usage` answers usage aggregated over a
  range, grouped by up to three of key, model, provider, owner, door,
  status, and a key label, in hour, day, or month buckets or one total,
  and filtered by key, model, provider, owner, and label, each taking a
  name or an id. `GET /v1/requests` answers the records themselves,
  newest first, with the same filters plus status, error, and stream,
  paged by `limit` (default `50`, at most `1000`) and `cursor`, and
  says in `source` whether they came from this replica's own recent
  records (`memory`) or from the archive. Both need a bearer, ask
  permission once with the action `usage.read`, and narrow the answer
  to what the permission service allows, so a query outside it comes
  back empty rather than refused; both are in the OpenAPI document.
- Usage and metering: every request through a door ends in one usage
  record, the gateway's record with the cost the Model's `pricing` gives
  its tokens, an integer count of micro-units and never a float: one
  rounding, half up, over the whole sum, per 1, 1000, or 1000000 tokens,
  in any currency. Cached input is billed once at its own price,
  reasoning tokens are recorded and not billed, a count route, an
  opaque route, and a Model without a price are `priced: false`, and a
  request refused before it reached a provider has a record too. The
  records fold into hourly rows per key, model, provider, owner, door,
  status, and currency, which the store keeps and the usage API reads
  grouped by up to three dimensions, including a Key label, over
  hours, days, months, or a total, never summing two currencies; each
  replica also keeps the last 1000 records per key for the request
  list. `LUX_METERING_FLUSH` (default `1s`, between `100ms` and `1m`) is
  how often a replica writes its spend counters and its aggregates,
  `lux_tokens_total{direction}` and `lux_spend_microunits_total{currency}`
  follow the records, and `lux_metering_flush_lag_seconds` grows while
  the store refuses a flush. The two usage routes above read all of
  this.
- Keys and limits: a Key's value is `lux_` and forty characters from a
  secure source, shown once and stored as a hash, and a platform may
  supply its own value of 32 to 4096 bytes instead, which opens the
  doors by its exact bytes and is shown as `sup_` and eight hex
  characters of its hash. Each replica caches a Key lookup for
  `LUX_KEY_CACHE` (default `10s`), positive or negative, at most 100 000
  entries, drops an entry the moment the journal reports the Key updated,
  rotated, or deleted, and empties the cache after a file-mode `SIGHUP`;
  every lookup counts in `lux_key_cache_hits_total{result}`. A Key's
  `requestsPerMinute` and `tokensPerMinute` are token buckets per
  replica, so an installation with `n` replicas admits at most `n` times
  the configured rate; the token bucket is charged the request's
  estimate before it runs and settled to the measured count after, and a
  request refused later gives its whole reservation back. A spend limit
  and a Budget count in fixed windows in the store, and a replica refuses
  on its own view plus the store's last flushed total, so with `R`
  replicas, a flush of `F` seconds, `T` requests a second per replica,
  and a largest request cost `C`, a hard limit is exceeded by at most
  `(R − 1) × F × T × C + C`. An unpriced model or an opaque route is
  refused `model_unpriced` under any spend limit or Budget unless the Key
  sets `allowUnpriced`; a soft Budget never refuses; an exhausted window
  raises `key.exhausted` or `budget.exhausted` exactly once, across
  every replica. `status.state`, `status.usage`, and a Budget's `status`
  render from the counters at read time; `status.lastUsedAt` is written at
  most once a minute per Key. `LUX_DEFAULT_REQUESTS_PER_MINUTE` and
  `LUX_DEFAULT_TOKENS_PER_MINUTE` (default `0`, no limit) are the rates a
  Key gets when it names none. The `metering` package holds the window
  arithmetic, the counter key scheme, and a replica's deltas over the
  store; the doors that read all of this mount with the API.
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
- Providers: a Provider's credential is sealed the moment it is applied,
  envelope encryption under the keys in `LUX_SECRETS_KEK`, one to eight
  keys of which the first wraps and every one opens, and `luxd serve`
  refuses to start without a key that opens every stored credential,
  naming a bad key by its position and never by its value; the file mode
  reads its credentials from the environment and needs no key. Rotation
  is `LUX_SECRETS_KEK=new,old`, then `luxd rewrap`, which re-wraps every
  stored data key under the new key and prints one line, then
  `LUX_SECRETS_KEK=new`; until the Postgres store lands the role says so
  and exits. Discovery reads every Provider's model list on
  `LUX_DISCOVERY_INTERVAL` (default `1h`), and at once when a Provider
  is created or re-addressed, and declares each name as a Model
  `<provider>/<name>` with `status.source: discovered`; a declared Model
  of the same name wins, a failed list changes nothing, and a name the
  schema refuses is a warning in `status.discovered.warnings`. Health
  probes every Provider's model list on `LUX_HEALTH_INTERVAL` (default
  `30s`), publishes `status.health` and every Model's
  `status.available`, and raises `provider.unreachable` and
  `provider.healthy` once per transition. Every request toward a
  provider goes through one client per Provider: pinned to its base URL,
  no proxy, no redirects, TLS 1.2 or later, HTTP/2, no compression added,
  a private address refused at dial unless `LUX_UPSTREAM_ALLOW_PRIVATE=1`,
  and `spec.concurrency` in-flight requests at most.
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
- Routing: a request to a Model goes to one of its targets by
  `priority`, then by `weight`, with a weight of `0` kept for when every
  weighted target at that priority has failed; a target whose provider
  is `Unreachable` or whose circuit is open is not tried. `fallback:
  onError` moves a request to the next target on a connection failure, a
  timeout, or a `408`, `429`, or `5xx` before any byte reached the
  caller, once per target and with no pause between; `fallback: never`
  answers the first failure. Five such failures in a row open a target's
  circuit for 30 seconds, after which one request probes it, and the
  `lux_circuit_open` gauge shows an open circuit by provider and upstream
  model. `gateway.NewTargetRouter` is the router for a platform that
  mounts the handler; the doors mount in `luxd serve` with the API.
- The API: `luxd serve` mounts the four doors and the control plane
  under `/v1` on the public listener. Apply a `Provider`, `Model`, `Key`,
  or `Budget` with `PUT /v1/{kind}s/{name}`, a manifest in JSON or YAML
  with or without its envelope, `201` on create and `200` on update,
  with `ETag` and `If-Match`, `If-None-Match: *` for create-once; read by
  id or name; list with `label`, `owner`, `limit`, `cursor`, and for
  Models `source` and `provider`, paged by `next_cursor`; delete, with
  `budget_in_use` and `provider_in_use` while something still names the
  object; `POST /v1/keys/{id-or-name}/rotate` for a new value. A Key's
  value is in the create and rotate responses and nowhere else; a
  supplied one shows as `sup_`. Every request carries a bearer from an
  issuer in `LUX_OIDC_ISSUERS` and asks the authorizer its route's
  action; every refusal is one JSON envelope with a code from the one
  error table of both planes, the fixed sentence in `message`, the
  developer detail in `details.detail`, and `Lux-Request-Id` on every
  response with a caller's `X-Request-Id` echoed. `GET /v1/self` is the
  caller as the server sees it, `GET /.well-known/lux` the server before
  any token, and `GET /v1/openapi.json` the document generated from the
  Go types, committed as `api/openapi.yaml`. `LUX_PUBLIC_URL` is now
  required in every mode; `LUX_REQUESTS_PER_MINUTE` (default `600`) is
  the rate per subject and `LUX_UNAUTHENTICATED_REQUESTS_PER_MINUTE`
  (default `60`) the rate of refused credentials per client address on
  both planes, behind the proxies `LUX_TRUSTED_PROXIES` names;
  `LUX_MAX_MANIFEST_BYTES` (default `65536`) bounds a manifest;
  `LUX_UPSTREAM_TIMEOUT` and `LUX_MAX_BODY_BYTES` now reach the resolver
  and the doors. In file mode `/v1` is read-only on the internal
  listener and needs no bearer. `/metrics` on the internal listener
  serves one registry with every family.
- Request log and events: every mutation through `/v1` and every state
  change worth knowing about, a Provider becoming unreachable or healthy
  again, a Key or a Budget reaching its spend, a Model discovered or
  removed by discovery, is journalled and, with `LUX_EVENTS_URL` and
  `LUX_EVENTS_SECRET` set, delivered to that URL as one JSON body per
  event with `Lux-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of
  "<t>.<body>">`, at least once and in order per object, retried from a
  second to five minutes for 24 hours and then dropped with an error
  line; a sink named later is not sent what happened before it was set,
  and a sink deduplicates on `id`. With `LUX_REQUESTLOG_EXPORTER=s3`
  every usage record is also archived as NDJSON objects in
  `LUX_S3_BUCKET` at `LUX_S3_ENDPOINT` under `LUX_S3_PREFIX` (default
  `lux/`), keyed by UTC hour, replica, and ULID, in batches of 5000
  records or every 30 seconds, from a buffer of 50 000 records that
  drops the oldest when the bucket does not keep up; `LUX_S3_REGION`
  (default `us-east-1`), `LUX_S3_ACCESS_KEY`, and `LUX_S3_SECRET_KEY`
  are the client's, with no credential chain. `lux_events_pending` and
  `lux_requestlog_dropped_total` say how much waits and how much was
  lost. `GET /v1/requests` reads the archive as `source: archive` once
  the API takes the reader.
- Test stubs and tiers: `lux-stubs` serves a stub provider per dialect,
  the stub issuer and authorizer of `latere.ai/x/pkg`, and a stub sink
  that verifies `Lux-Signature`, each on its own loopback address. A stub
  provider answers as a function of the request and fails on demand by
  upstream model name (`fail-500`, `fail-429`, `hang`, `redirect`,
  `tokens-<in>-<out>`, and the rest of the table) or by the
  `Lux-Stub-Fail` header. `make run` starts the stubs and `luxd` on
  ports derived from the checkout, applies `deploy/examples/`, and
  prints `LUX_URL`, `LUX_TOKEN`, and `LUX_KEY`; `make run-file` is the
  same in the file mode; `make run-down` stops both. `make test-e2e`
  runs the integration tier, `luxd` and `lux-stubs` as processes, and
  `make test-postgres` the postgres tier, which refuses to run without
  `LUX_DB_URL`; `verify.yml` runs both on every push.
