# Changelog

What changed for whoever writes a manifest, runs `luxd`, or builds on the
packages, one section per release. A `v*` tag without a section here is
refused before it is pushed.

## Unreleased

- A personal access token can now be narrower than the person holding
  it, and `luxd` answers inside it. Such a token carries the grants its
  holder chose, RFC 9396's `authorization_details`, each naming actions
  and either one object or every object of a kind. A key narrowed to
  some Models or some Keys is refused everywhere else with reason
  `grant`, 403, and a key granted nothing at all reaches nothing: an
  absent grant list is not full access.

  A grant never widens. The owner policy answers first and the grants
  narrow that answer, so a grant on an object the person may not touch
  still reaches nothing, and a token that is not a personal access token
  is decided exactly as before.

  With `LUX_AUTHORIZER_URL` set, the decision is the operator's and the
  claim reaches it in `claims` with every other claim of the token, as
  it always has. An endpoint written on `latere.ai/x/pkg/authz/server`
  applies the same narrowing by taking the shared library's new version;
  an endpoint written by hand applies `authz.Restrict` to its own
  answer, and until it does, a narrowed key is as wide at that endpoint
  as the person who holds it. The shared library is `latere.ai/x/pkg`
  v0.75.0.

  The example front of `docs/plane.md` reads the claim and forwards it
  the same way, so a platform built by copying it accepts a narrowed key
  instead of refusing it at the door.

- The first control plane request after a restart no longer waits for an
  issuer's key set. `luxd` already read every issuer's keys at start to
  refuse one it cannot verify against; the verifier now keeps that read,
  so the fetch is paid for at start-up and not by whoever arrives first.

- `latere.ai/x/lux/authorizer`'s vocabulary carries the heading a person
  reads for each resource kind: Providers, Models, Keys, Budgets, Usage.
  A console that offers a person a narrowed key groups the actions by
  function and hard codes no heading of its own. The action table is
  unchanged.

- A release no longer ends with a failed job. The last step of the
  pipeline used to open a pull request for the next conformance
  fixture, which the GitHub organisation refuses to let an Action
  create, so every release finished red with the work already done. The
  step now pushes the branch `conformance/fixture-<tag>` and stops: the
  fixture directory is on that branch, ready to merge, and the release
  run is green. The same bytes are still attached to the release as
  `fixture-<tag>.tar.gz`, and no job of a release writes to `main`.

- A release now refuses to cut while the repository's CI is red. When a
  run is red, `lateregate release` says who acts: `latere.ai/x/ci-gate`
  v0.40.0.

- A discovery document fetched for one issuer is refused when its own
  `issuer` field names a different one. The shared library is
  `latere.ai/x/pkg` v0.74.0, whose OIDC discovery, `authkit/jwt`, now
  checks the document against the URL it answered rather than trusting
  whatever it claims, `ErrBadDiscovery` with reason `issuer`. A fetch
  that fails outright, with no cached key set behind it, now reads
  `ReasonIssuerUnavailable` (`issuer_unavailable`) instead of no reason
  at all. Nothing changes for an issuer whose document names itself.

- A bearer naming a key `luxd` does not hold is refused as an unknown
  key rather than as a bad signature. The shared library is
  `latere.ai/x/pkg` v0.73.0, whose verifier picks the one key the
  token's `kid` names instead of trying every key of the issuer's set,
  so a token whose `kid` is in no listed key set is refused before its
  signature is read, after one key set refresh in case the issuer has
  published a new key. A token carrying a `kid` the set does hold and a
  signature some other key made is still a bad signature. Both stay
  `unauthenticated`, 401, with the finding in the developer detail
  alone, so nothing changes for a caller; the detail sentence changes
  for whoever reads it.

- The release pipeline verifies the images it published. The clean-runner
  job iterated the binary names and asked `ghcr.io` for `<owner>/luxd`,
  a repository no release of Lux pushes to, so `v0.1.0` and `v0.2.0` both
  ended in `DENIED` there with every artifact already published, signed,
  and attested. The job now maps the binary to its repository the way the
  build job does, `lux` for `luxd` and `lux-stubs` for the stubs. Nothing
  changes in what a release carries. `TestEveryImageReferenceNamesAPublishedRepository`
  resolves the values a shell variable holds before reading a reference,
  which the guard over literal names could not see.

## v0.2.0 - 2026-09-16

- `latere.ai/x/lux/authorizer` exports the action table itself.
  `authorizer.Vocabulary()` is the twenty-four actions `luxd` asks as
  one `authz.Vocabulary`, each paired with the resource kind it acts on,
  so a control plane that decides for Lux imports the table instead of
  rebuilding it from `Actions()` and `Kind()` row by row or keeping a
  copy of the strings. `Actions`, `Kind`, and `Known` keep their
  signatures and are now three readings of that one value. An endpoint
  written in Go with `latere.ai/x/pkg/authz/server` passes it as
  `Options.Vocabulary` and declares no page action, because every Lux
  list answers a decision whose `filter` narrows the gateway's own list.
  The shared library is `latere.ai/x/pkg` v0.70.1.

- `luxd` refuses an action it does not declare before the request leaves
  the gateway. The authorizer client carries the vocabulary, so a string
  outside the table costs no round trip and no authorizer is asked to
  name it. Nothing changes for an installation: every action the gateway
  sends is one of the table's.

- The minimal authorizer of `docs/plane.md` is that scaffold and a
  policy. The bearer, the body bound, the decode, the check that an
  action and its `resource.kind` are the gateway's, the reserved probe
  id, and the answer are `latere.ai/x/pkg/authz/server`'s; what the page
  prints is the permission model and a listener. Two answers changed for
  anyone running a copy of it: an action outside the table is now a 400
  rather than a 200 deny, and the probe is denied by the scaffold before
  the policy is reached. `luxd` reads either refusal as
  `authorizer_unavailable` and fails closed, as it always has.

## v0.1.0 - 2026-09-16

- A Key can be created from the SHA-256 of its value.
  `Key.spec.valueSHA256` takes the hash as 64 lower-case hex characters
  and stands in for `spec.value` on a create, for an importer whose
  source kept hashes alone: the credentials its holders already present
  keep opening the doors after the move, with no re-issue. The gateway
  stores the hash as the one a supplied value would have produced, so
  the value behind it authenticates on every door and `status.prefix`
  is `sup_` and the hash's first eight characters. The field is
  write-only, as `spec.value` is: no response, event, record, or log
  line returns it and a resolved manifest carries it absent. It is
  refused beside `spec.value` or `spec.valueFrom`, on an update, and in
  file mode; a hash another Key already holds is `invalid_field` at
  `spec.valueSHA256`. A rotate mints a `lux_` value and the hash stops
  matching. The gateway cannot check the bounds of a value it never
  sees, so a value shorter than the 32 bytes `spec.value` requires is
  the importer's own decision.

- The `/v1` client is an importable package, `latere.ai/x/lux/client`.
  A program that applies Providers, Models, Keys, and Budgets to a core,
  a plane that drives the core it runs or a tool that migrates objects
  into one, constructs a `client.Client` with a base URL and a
  `TokenSource` and calls `Apply`, `Get`, `List`, `Delete`, `Rotate`,
  `Self`, `Usage`, `Requests`, `Models`, and `WellKnown` rather than
  assembling requests of its own. Every method answers with the response
  bytes as they arrived beside the status and the request id, a list
  follows `next_cursor` to the end or to a count of items, and a refusal
  decodes into a `*client.Error` carrying the code, the fixed sentence,
  the paths, the developer detail, and the request id. The `lux`
  command drives the same package.

- The gateway image is published as `ghcr.io/<owner>/lux:<tag>`; `luxd`
  is the binary inside it and the Deployment's name, not the repository
  a release pushes to. The stub image stays `ghcr.io/<owner>/lux-stubs`.
  `compose.yaml`, the quick start, `docs/install.md`, and the deploy
  archive's pinned reference name the new repository, and
  `TestGatewayImageIsLux` refuses any reference to the old one.

- `docs/api.md`, `docs/security.md`, and `docs/observability.md` are the
  operator pages for the API, security, and observability, and the docs hub
  routes to them instead of to the specs. `api.md` orients a caller in the
  `/v1` control plane, the dialect doors, authentication, and the error
  shape, then points at the OpenAPI document (`api/openapi.yaml`, served at
  `GET /v1/openapi.json`) as the authoritative reference. `security.md` is
  what the gateway protects and what you must do to run it safely: the KEK,
  TLS, `LUX_TRUSTED_PROXIES`, the authorizer, key rotation, and verifying
  releases. `observability.md` is the `/metrics` names and labels, the
  health probes, the log fields, traces, and the shipped alerts. The specs
  stay named as the contributor design record.
- A hard spend or Budget limit refuses the request that follows a settled
  answer even while that answer's spend is being flushed to the store: the
  metering counters keep a flushed delta in a replica's own total for the
  whole store round-trip, so a Key's `spend_exceeded` or a Budget's
  `budget_exhausted` no longer races the flush and admits one request over
  the limit.
- `docs/configuration.md` is the operator reference for every `LUX_*`
  variable `luxd` reads: its meaning, default, and when it is required,
  grouped by area, with the meanings the repository scaffold spec owns. A
  root `.env.example` carries the same set as a file to copy, its required
  variables uncommented with a placeholder and its optional ones commented
  with their default; `LUX_SECRETS_KEK` says how to generate a key rather
  than shipping one. Both are generated from `internal/config` and held
  current by a test that fails if either omits a variable the binary reads
  or names one it does not, so they cannot drift from the code.
- `go test -bench` benchmarks measure the gateway's own overhead per
  request, in process against a stub upstream and isolated from the
  provider and the network: one per route class over the handler
  (`gateway`), `Resolve` per kind (`manifest`), cost and window
  arithmetic (`metering`), and the limiter's reserve-and-settle cycle
  (`internal/serve`). A percentile latency-and-throughput harness in
  `gateway` reports p50/p75/p90/p95/p99, requests per second, and bytes
  per request per route class, opt-in behind `LUX_LATENCY` and never a
  pass/fail gate. `docs/performance.md` says what they measure and how to
  run them; the design is `specs/023-performance-and-benchmarks.md`.
- `benchmarks/compare/` measures `luxd`'s proxy overhead against LiteLLM's on
  loopback against a shared mock upstream, over 10 independent trials per
  condition (passthrough and translated shapes, non-streaming and streaming):
  `run.sh` drives the closed-loop load matrix, the load driver writes tidy
  per-trial CSV, and `render.py` aggregates each condition to a median and a
  95% confidence interval and draws the latency-percentile and throughput
  figures (log scale, with CI bands and error bars). `RESULTS.md` reports the
  numbers with their CIs and embeds the charts; `docs/performance.md` reports
  the in-repo benchmarks over `-count=10` with `benchstat`. The whole harness
  is opt-in and never enters `go build`, `go test`, the coverage gate, or CI.
- `compose.yaml` and [`docs/quickstart.md`](docs/quickstart.md): try
  `luxd` on your machine with no checkout and no build, `docker compose
  up` against the published `luxd` and `lux-stubs` images and a few
  `curl`s to mint a token, declare a Provider, a Model, and a Key, and
  send one request through a door. It is `make run` without the
  toolchain, on the memory store, and points at the stub issuer over the
  compose network with the built-in owner policy. The images are cut by
  the release pipeline; until the first tag, the doc gives the commands
  to build them from a checkout.
- `/metrics` carries `lux_tunnel_sessions`, the number of tunnel
  sessions the scraped replica holds, on every installation: the gauge
  moves with a session opening and closing where the tunnel is on, and
  reads zero where it is off, so an expression over it tells a replica
  holding no session from one that is not being scraped. It has no
  labels; a session's Provider is already a series of
  `lux_provider_health`.
- `/metrics` carries `lux_provider_health`, a gauge with one series per
  Provider and state, `1` on the state the replica acts on and `0` on
  the other three, so `lux_provider_health{state="Unreachable"}` names
  the Providers whose targets are out of selection and the shipped
  `LuxProviderUnreachable` alert fires on them. A Provider deleted from
  the catalogue leaves the metric at the health job's next tick.
- `latere.ai/x/lux/authorizer`: the vocabulary an authorization endpoint
  is written against, importable. It carries the actions `luxd` asks,
  one constant each, the resource shape it sends per action with the
  builders that render them, and the `limits` an allow may carry, as
  `WireLimits` for the endpoint that answers and `DecodeLimits` for the
  reading `luxd` does. A platform that kept a copy of the twenty-four
  strings and of the six wire names deletes it and imports this
  instead; the promise is the root packages', additive within a module
  major, so an action never changes its string and a `limits` member
  never changes its name. Nothing changes on the wire, and the minimal
  authorizer of `docs/plane.md` now decides by the package rather than
  by string literals.
- `LUX_DB_URL` selects the Postgres store: desired state, the Key
  hashes, the sealed credentials, the spend windows, the leases, the
  journal, the tunnel registry, and the usage aggregates become rows
  every replica shares and a restart keeps. `luxd serve` applies the
  embedded migrations at start after holding the stored schema to three
  rules: a dirty schema or one of another major refuses to start naming
  the version, one ahead within the major starts with one `WARN` line
  and serves, and one behind is migrated. The pool is sized by
  `LUX_DB_MAX_CONNS` (default 8), and the start-up line names the
  endpoint and the schema version, never the URL. `luxd check`'s
  `store`, `migrations`, and `db conns` rows read the database: `SELECT
  1` inside the readiness budget, the stored and embedded schema
  versions, and the cluster's `max_connections` less its reserved slots
  beside what the replicas open, a `warn` when the replicas together
  exceed it and a `fail` when one alone does; over a database whose
  schema is not applied yet the rows over the objects say so rather
  than fail. `luxd rewrap` runs over the store's rows and refuses a
  schema not at its own version. A database that does not answer at
  start is exit 1 naming the endpoint; one that goes away while serving
  fails readiness and serves again when it is back. The postgres tier
  runs the whole store suite, two `luxd` processes over one database,
  and `luxd check` against the cluster.
- A whole answer's spend is settled before its body reaches the caller,
  so the request a caller sends the moment it has the answer meets the
  Budget and spend the answer moved; before, a fast caller could slip
  one more request past a Budget the previous answer had exhausted.
- `lux-stubs`: an eighth listener, the index, whose `GET /` answers one
  document naming the URL of every other stub of the run and the
  credential the stub providers require, so a suite or a script handed
  that one address reaches the rest without parsing the startup lines
  or guessing a port; `-index-addr` binds it and the line
  `lux-stubs: index <url>` names it. `GET /_received` on a stub
  provider now names the recorded headers `headers`, the wire name a
  reader in another process decodes, and a streamed chat answer carries
  its finish reason on the last content event rather than in a frame of
  its own. With `LUX_TEST_STUBS_URL` pointed at the index, the
  conformance suite runs its stub-backed cases instead of skipping
  them.
- The doors' translation between dialects, their error envelopes and
  stream error frames, their model lists, the count emulation, the
  reading of every dialect's usage members, and the two member edits a
  passthrough body admits are `latere.ai/x/pkg/llmdialect/bridge`'s,
  imported by `gateway`, so a program that holds bytes in one
  provider's dialect and wants them in another imports the bridge with
  no gateway running. Nothing a caller of a door sees changes: the same
  bytes, headers, envelopes, codes, and records, held by the door tests
  and by the bridge's goldens, which are the doors' own bytes.
- Building a platform on the gateway: `docs/plane.md` is the page for a
  team that sells or governs model access. It says how a platform
  composes the gateway, either by running `luxd` and writing the
  webhooks or by importing `manifest`, `gateway`, and `metering` into
  its own binary; where each of its own concerns goes, row by row; what
  a minimal authorization endpoint looks like, as a program that
  compiles and runs (`examples/authorizer`); how a sandbox running
  untrusted code calls a model without holding a credential, and which
  credential each hop of that composition carries; the one command that
  proves a front still serves the contract; and what the gateway
  promises a platform and what it does not. `examples/plane` is a
  server of the second kind, built from the three packages with a
  store, an identity, and a control plane of its own, and the
  conformance suite runs against it.
- Release and installation: a `v*` tag runs the release pipeline,
  `.github/workflows/release.yml`, which requires the tag's commit to
  have passed `verify`, builds `luxd` and `lux` for `linux` and
  `darwin` on `amd64` and `arm64`, pushes the `luxd` and `lux-stubs`
  images by digest, signs them with cosign, attaches an SPDX bill of
  materials and a build provenance attestation to each, runs the
  conformance suite against the two digests, and only then tags them
  and creates the GitHub release with the CHANGELOG section as its
  body, `checksums.txt` signed as a blob, a deploy archive with the
  image pinned by digest, and the fixture the next release's suite
  reads. Images publish under the account that pushed the tag, so a
  fork's tag publishes under the fork's.
- `deploy/`: a kustomize base hardened as the threat model says, a
  `kind` overlay for a laptop cluster and a `generic` one for two
  replicas over Postgres, an HPA component, and the bootstrap Secret
  template for the four values no repository should carry. The
  Deployment rolls one replica at a time and never surges, so a rollout
  never asks the store for more connections than the running replicas
  hold.
- `luxd check`: the second role of the server binary prints one line
  per requirement of the installation, `ok`, `warn`, or `fail` with one
  sentence, in a fixed order from `version` to `tunnels`, and exits 1
  when any line failed. It dials the issuers, the authorizer with the
  probe every authorizer denies, the event sink with one signed
  `check.ping`, the request log archive with one empty object it
  deletes again, and every Provider, and changes nothing, so it is safe
  against a serving installation.
- `docs/install.md` walks an installation from nothing to a request
  through a door on a kind cluster, and CI runs its fenced blocks on
  every push against the checkout and after every release against the
  published artifacts, so the document cannot go stale.
- `Dockerfile.release` and `Dockerfile.stubs` copy the binaries the
  pipeline built onto the same runtime stage the developer image uses,
  now pinned by digest.

- `lux serve`: a session's carriers and heartbeat have ended when the
  session ends, so nothing of a session that closed writes to stderr
  after the command printed its close reason or connected again.
- The conformance suite: `test/conformance` runs the acceptance
  criteria of the manifest contract, the `/v1` API, the doors, the
  Keys, the identity boundary, and the usage records against any
  server, as `go test latere.ai/x/lux/test/conformance -run
  TestContract` with `LUX_TEST_URL` and `LUX_TEST_TOKEN` in the
  environment, or through `conformance.Run` with a `Config` whose
  `Token` mints through the caller's own issuer. It holds every `/v1`
  answer to the server's own `GET /v1/openapi.json`, names every object
  it creates `conf-<run>-*` under the label `conformance=<run>` and
  deletes them at teardown, mints one Key and no issuer token, skips
  the cases that need `LUX_TEST_STUBS_URL` by name when it is unset,
  and reads a `file` mode server's `/v1` at `LUX_TEST_INTERNAL_URL`.
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
- Tunnelled runtimes: a model server on a developer's or an operator's
  machine becomes a `Provider` with `spec.tunnel: true`, no `baseURL`
  and no credential, and is reached through one outbound HTTP/2
  connection its agent opens, so no inbound port, public name, or
  certificate is needed. With `LUX_TUNNEL_ENABLED=1` the gateway serves
  `POST /v1/providers/{id-or-name}/tunnel`, the session, which asks
  `provider.tunnel` at connect and lives as long as the bearer that
  opened it, refreshed by a heartbeat carrying a fresh token, and
  `POST /v1/providers/{id-or-name}/tunnel/carry`, the carriers the
  agent parks and the gateway hands requests to; a session ends with
  `superseded`, `token_expired`, `provider_deleted`, or `draining`, each
  telling the agent what to do next. Under the owner policy any
  subject may create, update, delete, and tunnel a `Provider` it owns
  with `tunnel: true`. Discovery lists the runtime's models at connect
  and on the interval, health reads the registry, so a tunnel that is
  gone is `Unreachable` and `status.tunnel.state` `Disconnected` within
  `LUX_TUNNEL_REGISTRY_TTL` (default `30s`, between `5s` and `5m`), and
  the doors, routing, limits, and metering treat a tunnelled Provider
  as any other. With several replicas, `LUX_TUNNEL_FORWARD_ADDR` is the
  address other replicas reach this one's internal listener at and
  `LUX_TUNNEL_FORWARD_SECRET` the comma separated bearers of
  `POST /internal/tunnel/{id}`, the first sent, every one accepted, so
  a rotation is a rolling deploy; the address without the secret, or
  either without the tunnel on, refuses to start. `lux_tunnel_sessions`
  is the sessions a replica holds. The agent side is a package the
  `lux serve` command of spec 014 wraps.
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
- The `lux` command: one binary that speaks the `/v1` API from a shell
  or from an agent. `lux apply -f` sends the documents of one or more
  files in order and stops at the first refusal, and
  `--credential-from-env NAME` sends a Provider's credential from the
  environment and never from a file; `lux get`, `lux list`, and
  `lux delete` address the four kinds by name or id, `lux keys rotate`
  mints a new value, `lux usage` and `lux requests` read the two usage
  routes, `lux whoami` says who the token belongs to, and `lux models`
  asks the lux door which models a Key may call. `lux keys create`,
  `lux providers create`, `lux models create`, and `lux budgets create`
  build a manifest from flags, and `--dry-run` prints it instead of
  sending it. Output is the server's own JSON, or `-o yaml`, `-o table`,
  and `-o wide` rendered locally. Exit 0 is done; 1 is a refusal or an
  unreachable server, with the server's sentence on stderr and the code,
  the paths, the detail, and the request id under `-v`; 2 is a wrong
  command with nothing sent. `LUX_URL` with `LUX_TOKEN` or
  `LUX_TOKEN_FILE`, read on every request, reaches `/v1`; `LUX_KEY`
  reaches a door, with `LUX_BASE_URL` and `LUX_API_KEY` as an SDK's
  fallbacks. The skill at `skills/lux/SKILL.md` teaches an agent the
  command, and `docs/cli.md` is its `-help`, command by command.
  `lux serve` arrives with the tunnel.
- Observability: `luxd` reads the standard `OTEL_*` variables and, with
  `OTEL_EXPORTER_OTLP_ENDPOINT` set, exports traces, metrics, and logs
  over OTLP/HTTP with W3C propagation and a parent-based sampler; with
  it unset nothing leaves the process. Every request is one
  `lux.request` span with one `lux.upstream` child per provider tried,
  or one `lux.api` span with its `lux.authorizer` and `lux.store`
  children, and no span carries a subject, an owner, a Key id, a Key
  prefix, or a caller address. Logs are JSON on stderr with `service`,
  `version`, and `replica` on every line, `trace_id` and `span_id` on a
  line written inside a span, one `INFO` line per request with the
  fixed fields of the observability spec's two rows, and any minted Key
  value truncated to its twelve-character prefix wherever it is
  written. `/metrics` adds `lux_output_tokens_per_second{provider,model}`,
  `lux_upstream_requests_total{provider,status}`, and
  `lux_upstream_duration_seconds{provider}`; the metric table, the
  bucket boundaries, and the span and log field sets are the spec's, and
  `deploy/base/prometheusrule.yaml` carries the ten alerts an
  installation starts with.
- `lux serve` attaches a model runtime on your own machine to a
  gateway: it applies a `Provider` with `spec.tunnel: true` from
  `--dialect`, `--as`, `--include`, `--exclude`, and `--label`, then
  holds one outbound session open and serves what the gateway sends
  against `--upstream`, whose address never leaves the machine. It
  reconnects with backoff from one second to thirty while the gateway
  can be reached, sends a token the `--token-file` gains in a
  heartbeat rather than reconnecting, exits 0 on `SIGINT` and
  `SIGTERM` with the session closed cleanly, and exits 1 when the
  gateway ends the session for good.
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
