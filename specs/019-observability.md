---
title: "Observability: metrics, traces, logs, alerts"
status: testing
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/004-request-path.md
  - specs/009-usage-and-metering.md
affects: [cmd/luxd/, internal/serve/, internal/api/, internal/auth/, internal/store/, internal/events/, internal/reqlog/, internal/tunnel/, gateway/, deploy/base/prometheusrule.yaml, .lateregate.yaml, docs/]
effort: small
created: 2026-09-13
updated: 2026-09-14
author: changkun
---

# Observability

## Overview

An operator answers three questions from outside the process: is the
gateway serving, which provider is costing it, and is anything leaking.
Metrics on the internal listener answer the first two in aggregate,
traces answer them for one request, logs carry the developer detail,
and a rules file turns the metrics into the alerts an installation
starts with. Everything goes through `latere.ai/x/pkg/metrics` and
`latere.ai/x/pkg/otel`, so the exporter is the standard `OTEL_*`
variables and nothing is on without an endpoint.

Telemetry is the one surface where a gateway can undo its own custody
rules by accident: a label, a span attribute, or a log line is a copy
of whatever it names, exported to a system with its own retention and
its own readers. So this spec states what may be a label, what may be a
span attribute, and what a log line carries, and the acceptance
criteria are canary tests rather than promises.

## Current state

Built on 2026-09-14. `cmd/luxd` starts telemetry through
`serve.Telemetry`, the `gateway` opens `lux.request` and `lux.upstream`
and writes the data plane's line, `internal/api` opens `lux.api` and
writes the control plane's line, `internal/auth` opens
`lux.authorizer`, `internal/store` opens `lux.store` under a parent,
the Recorder emits `lux_output_tokens_per_second`, the gateway's
attempt emits the two upstream metrics of [[005-providers]]'s row, and
`deploy/base/prometheusrule.yaml` carries the ten alerts. Every row of
the acceptance table that is this spec's own passes; four metrics of
the table wait on their owners, `lux_provider_health` on
[[005-providers]], `lux_events_pending` and
`lux_requestlog_dropped_total` on [[012-request-log-and-events]], and
`lux_tunnel_sessions` on [[013-tunnelled-runtimes]], and the rules
file's `promtool` step on [[017-release-and-installation]], which is
what keeps this spec at `testing`.

## Design

### The four probes and the registry

[[002-repository-scaffold]] owns the listeners and mounts
`latere.ai/x/pkg/health`, whose four paths are `GET /livez`, `GET
/readyz`, `GET /version`, and `GET /metrics`. This spec adds no path
and changes none of the first three: liveness touches no dependency,
readiness runs that spec's checks inside its budget, and `/version` is
the build identity. It fills the fourth. `health.Options.Metrics` takes
an `http.Handler`, and the one this spec supplies, `serve.Metrics`,
writes `Content-Type: text/plain; version=0.0.4; charset=utf-8` and
then the registry's `WritePrometheus`. `/metrics` exists only where
that option is set, which is the internal listener alone
([[011-api]], [[002-repository-scaffold]]).

### Metrics

One `latere.ai/x/pkg/metrics.Registry`, constructed once by the serve
role's wiring in `cmd/luxd` and passed to everything that records. That
package has no package-level default registry on purpose, so there is
one object to pass and one object a test can read; nothing here reaches
for a global. It is served in the Prometheus text format at `GET
/metrics` on the internal listener and nowhere else
([[002-repository-scaffold]]). The table is the reference: a metric not
in it does not exist, and `TestMetricsTable` reads this file against
the registry the process wires. The gateway emits its three when
`gateway.Options.Metrics` is set, under the names
`gateway.MetricRequests`, `MetricRequestDuration`, and
`MetricTimeToFirstByte` with the buckets `gateway.DurationBuckets` and
`TimeToFirstByteBuckets`, which the test reads beside this file
([[004-request-path]]).

An Owner cell that ends `not built` names a metric its owner has not
emitted yet. `TestMetricsTable` tolerates the absence of a metric from
the registry only under that mark, and fails once the metric appears
while the mark is still there, so the mark cannot outlive the build.

| Metric | Type | Labels | Owner |
|---|---|---|---|
| `lux_requests_total` | counter | `door`, `model`, `provider`, `status`, `code` | [[004-request-path]] |
| `lux_request_duration_seconds` | histogram | `door`, `model`, `status` | [[004-request-path]] |
| `lux_time_to_first_byte_seconds` | histogram | `door`, `model` | [[004-request-path]] |
| `lux_output_tokens_per_second` | histogram | `provider`, `model` | [[009-usage-and-metering]] |
| `lux_tokens_total` | counter | `direction` | [[009-usage-and-metering]] |
| `lux_spend_microunits_total` | counter | `currency` | [[009-usage-and-metering]] |
| `lux_refusals_total` | counter | `code` | [[011-api]] |
| `lux_upstream_requests_total` | counter | `provider`, `status` | [[005-providers]] |
| `lux_upstream_duration_seconds` | histogram | `provider` | [[005-providers]] |
| `lux_provider_health` | gauge | `provider`, `state` | [[005-providers]], not built |
| `lux_key_cache_hits_total` | counter | `result` | [[007-keys-and-limits]] |
| `lux_metering_flush_lag_seconds` | gauge | none | [[009-usage-and-metering]] |
| `lux_events_pending` | gauge | none | [[012-request-log-and-events]], not built |
| `lux_requestlog_dropped_total` | counter | none | [[012-request-log-and-events]], not built |
| `lux_authorizer_requests_total` | counter | `decision` | [[006-identity]] |
| `lux_authorizer_duration_seconds` | histogram | `decision` | [[006-identity]] |
| `lux_store_operations_total` | counter | `op`, `result` | [[010-state]] |
| `lux_circuit_open` | gauge | `provider`, `model` | [[008-routing-and-models]] |
| `lux_tunnel_sessions` | gauge | none | [[013-tunnelled-runtimes]], not built |

`lux_output_tokens_per_second` is a stream's output tokens over the time
from its first byte to its last, and a non-stream's over its upstream
duration; it is the figure an operator of a model server watches.
[[009-usage-and-metering]] built its three and left this one to this
spec to place, and it is placed in the Recorder of `internal/serve`,
`serve.MetricOutputTokensPerSecond` on `serve.OutputTokensPerSecondBuckets`,
because the record is the one place that has the tokens and both times:
a stream's time is the record's latency less its time to first byte, a
non-stream's is the answering attempt's duration, and a refusal, a
failure, a record with no output, or a time too short to measure in
milliseconds observes nothing.

The two upstream metrics are emitted by the gateway's attempt,
`gateway.MetricUpstreamRequests` and `MetricUpstreamDuration`, one
observation per target tried, the opaque route's one attempt included,
on `gateway.DurationBuckets`. [[005-providers]] did not build them and
names neither; its Design owes the paragraph that claims them. An
attempt's `status` is `timeout` for `upstream_timeout`, `error` for
`upstream_error` and `provider_unavailable`, a transport failure, a
5xx, a refused credential, or a redirect, and `ok` otherwise, which
includes `upstream_rejected`, the request's own refusal by the provider,
and a caller that left before the answer, so the alert on the ratio
names a provider that is failing and not a caller that is.

`lux_provider_health` is not built. Its writer is the health job of
[[005-providers]], which has the state per Provider and no registry;
`serve.HealthOptions.Metrics` and the gauge in `NewHealth`, one series
per Provider and state read from `Health.View`, are that spec's to add,
with the one wiring line in `cmd/luxd`. `LuxProviderUnreachable` in the
rules file names it now and fires once it exists.

Label values, each from a closed set:

| Label | Values |
|---|---|
| `door` | `openai`, `anthropic`, `gemini`, `lux`, and `control` for a `/v1` request |
| `model`, `provider` | the resolved Model's and the answering Provider's `metadata.name`, empty when none was resolved or reached |
| `status` | `ok`, `refused`, `failed` on a request; `ok`, `error`, `timeout` on an upstream request |
| `code` | a code of [[011-api]]'s error table, empty when `status` is `ok` |
| `direction` | `input`, `output`, `cached_input`, `cache_write` |
| `state` | `Healthy`, `Degraded`, `Unreachable`, `Unknown`; the gauge is `1` on the current state and `0` on the other three |
| `result` | `hit`, `miss`, `negative` on the Key cache; `ok`, `conflict`, `error` on a store operation |
| `decision` | `allow`, `deny`, `unavailable`. `latere.ai/x/pkg/authz`'s `Options.Observe` reports `allow`, `deny`, and `error`; the serve wiring passes an `Observe` that renames `error` to `unavailable`, because that is the code the caller sees ([[011-api]]) and an operator reading the alert should not have to translate |
| `op` | the store's method name ([[010-state]]) |
| `currency` | the ISO 4217 code of the Model's pricing |

`lux_circuit_open` is `1` while a target's circuit is open and `0`
otherwise, one series per target, which is the `provider` and the
target's upstream model name of [[008-routing-and-models]]'s key and is
bounded by the catalog like every other pair. `lux_tunnel_sessions` is
the number of tunnel sessions this replica holds
([[013-tunnelled-runtimes]]); the provider name is not a label on it,
because a session's Provider is already a series of
`lux_provider_health`. Both are gauges, which `latere.ai/x/pkg/metrics`
serves as a callback read at scrape time, so neither is a counter this
code has to keep in step.

A `control` request carries an empty `model` and `provider`, so one
counter answers both planes and an expression scopes to a plane with
`door`. Per-model token and spend accounting is `GET /v1/usage`'s
([[009-usage-and-metering]]), which is why `lux_tokens_total` carries a
direction and nothing else: the metrics answer how the gateway is
behaving, the usage API answers who spent what.

### Buckets

Model latency is not web latency: a first token can take seconds and a
stream can run for minutes, up to `LUX_UPSTREAM_TIMEOUT`
([[004-request-path]]). `latere.ai/x/pkg/metrics.DefaultDurationBuckets`
ends at 10 seconds, which would put every stream in `+Inf`, so this
spec declares its own and passes them explicitly; `Registry.Histogram`
takes the boundaries per metric and defaults to none.

| Histogram | Buckets, seconds |
|---|---|
| `lux_request_duration_seconds`, `lux_upstream_duration_seconds` | `0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600` |
| `lux_time_to_first_byte_seconds` | `0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 60` |
| `lux_output_tokens_per_second`, in tokens per second | `1, 2, 5, 10, 20, 50, 100, 200, 500, 1000` |
| `lux_authorizer_duration_seconds` | `latere.ai/x/pkg/metrics.DefaultDurationBuckets` |

Thirteen, ten, and ten boundaries, so the four request histograms cost
`doors × models × statuses` series times fifteen and eleven lines
rather than a quantile per series. `LuxTimeToFirstByteSlow`'s 10 second
threshold is a boundary in the second row, so the alert's
`histogram_quantile` reads an edge and not an interpolation.
`TestHistogramBuckets` reads this table and holds the Go values beside
the code to it: `gateway.DurationBuckets`,
`gateway.TimeToFirstByteBuckets`, `serve.OutputTokensPerSecondBuckets`,
and the shared default.

### Cardinality

The rule, in one sentence: a label value is either from a closed set in
the table above or is the name of an object an operator declared.

- `model` is the **resolved** Model's name and is empty when the name
  the caller sent resolved to nothing. A caller controls the string in
  its request body; it does not control the catalog, so a flood of
  `model_not_found` requests with ten thousand invented names adds no
  series. This is the one rule that would otherwise let a caller grow
  the registry without limit.
- `provider` is a declared Provider's name, likewise bounded.
- Never a label: a Key id, a Key prefix, a Key value, a subject, an
  owner, a request id, a target's upstream model name, a client
  address, a label from `Lux-Labels`, or a Model's `metadata.labels`.
  Each of those is unbounded or identifying, and each is already in the
  usage record, where it is a row rather than a series
  ([[009-usage-and-metering]]). The one exception is `lux_circuit_open`,
  whose `model` is the upstream name because that is the circuit's key
  ([[008-routing-and-models]]); it is bounded by the catalog's targets.

The upper bound on the registry is therefore `doors × models ×
providers × statuses × codes` for the request counter, which an
installation sizes from its own catalog, and a constant for every other
metric.

### Traces

`latere.ai/x/pkg/otel` reads the standard `OTEL_*` variables;
without `OTEL_EXPORTER_OTLP_ENDPOINT` no span is exported and the
instrumentation is a no-op, and `OTEL_SDK_DISABLED=true` turns it off
with an endpoint set. The exporter is OTLP over HTTP, the propagator is
W3C `traceparent` and `baggage`, and the sampler is parent-based with
the ratio of `OTEL_TRACES_SAMPLER_ARG`, all of that package's. No
`LUX_*` variable configures tracing, and none configures redaction,
because there is nothing to redact: the attributes below are the whole
set. A switch between identity on a span and a hash of identity on a
span has no counterpart here, because neither state of it is a state
this spec allows.

Every package that opens a span takes its tracer from the global
provider, `otel.Tracer(scope)` resolved per call with the package's
import path as the scope, so the wiring passes no tracer and no option
carries one: `Bootstrap` installs the SDK's provider when an endpoint is
set, and without one the global provider is the SDK's no-op, whose span
is a value with no span context that no exporter sees and that nothing
downstream can tell from no span. `TestTracingOffByDefault` reads that
absence where the store would: the context the Key lookup receives
carries no valid span context.

| Span | Parent | Attributes |
|---|---|---|
| `lux.request` | the caller's context, when it propagated one | `lux.door`, `lux.route`, `lux.model`, `lux.provider`, `lux.status`, `lux.code`, `lux.request_id`, `lux.stream`, `lux.translated` |
| `lux.upstream` | `lux.request` | `lux.provider`, `lux.attempt`, `http.request.method`, `url.template`, `http.response.status_code`, `lux.ttfb_ms` |
| `lux.api` | the caller's context | `lux.route`, `lux.action`, `lux.kind`, `lux.status`, `lux.code`, `lux.request_id` |
| `lux.authorizer` | `lux.api` | `lux.action`, `lux.decision` |
| `lux.store` | `lux.api` or `lux.request` | `lux.op`, `lux.kind`, `lux.result` |

The attribute names are constants beside the spans that write them:
`gateway.Span*` and `gateway.Attr*`, `api.SpanAPI` and `api.Attr*`,
`auth.SpanAuthorizer`, `auth.AttrAction`, and `auth.AttrDecision`,
`store.SpanStore` and `store.Attr*`. What each carries:

- `lux.request` and `lux.api` are server spans, each the child of the
  `traceparent` the caller sent when it sent one, extracted from the
  request's headers by the propagator `Bootstrap` installs; without an
  endpoint there is no propagator and the span is a root. The
  attributes are set when the request ends, from the record, so a
  refused request's `lux.model` is empty as its label is. `lux.route`
  is the door table's template on the data plane and the mux's pattern
  of [[011-api]]'s route table on the control plane, empty for a
  request no route answered.
- `lux.upstream`'s `lux.attempt` is the attempt's ordinal from one,
  `url.template` is the upstream path with the model's place held by
  `{model}` on the gemini model routes, the target dialect's path on a
  translation, the caller's path relative to the door on a passthrough
  of a route the table names, and the route's own template on the
  opaque route, so it is bounded by the route table and never the
  caller's own path; `http.response.status_code` and `lux.ttfb_ms`, the
  time from the attempt's start to the response line, are set when a
  response line arrived and absent when none did.
- `lux.authorizer` is one per question asked, the request's own action
  and each of Resolve's lookups alike, with `lux.decision` in the
  metric's vocabulary, `allow`, `deny`, or `unavailable`.
- `lux.store` is opened only when the context carries a span, so a
  request's reads and writes are its children and a job's tick, a flush,
  or the Recorder's pricing read traces nothing; `lux.kind` is on the
  object methods and absent on the other collections' ops. `Transact`
  is one span, and because its callback takes no context the operations
  inside it are the parent's children beside it, within its time.

Two spans on each trace are not this table's. `latere.ai/x/pkg/otel`'s
`Transport`, which the `otel-client` gate requires of every outbound
HTTP client in this module, opens a client span inside each
`lux.upstream`, inside each `lux.authorizer`, and around the verifier's
fetches; it carries `http.*` attributes, `url.full` among them, which
names the provider's, the authorizer's, or the issuer's address and
never a caller's. That package's `Handler`, which would open a server
span around each listener, is not mounted in this build: the two
listeners are [[002-repository-scaffold]]'s mount, `lux.request` and
`lux.api` are the roots a trace starts at when the caller sent no
parent, and mounting the server span is that spec's edit if it wants
one. The table is what this module names, and the acceptance criteria
read every span an e2e run produces, the transports' included.

One `lux.request` span per data plane request with one `lux.upstream`
child per target tried, so a fallback is visible as two children of one
parent ([[008-routing-and-models]]). A streamed response's span ends
when the stream ends, so its duration is the stream's life and
`lux.ttfb_ms` is how long the caller waited for the first event.

Identity is never a span attribute. There is no subject, owner, Key id,
Key prefix, or caller address on any span, by default or by
configuration, so a tracing system that an operator shares with other
teams cannot become a list of who called what. `lux.request_id` joins a
span to the log line and to the usage record, both of which the
operator holds itself, and that join is the supported way to get from a
slow trace to an attributed request.

### Logs

`slog` in JSON on stderr through the OTel bridge, from
`latere.ai/x/pkg/otel`'s `Bootstrap`, one line per request at `INFO`.
That package puts `service`, `version`, and `replica` on every record,
and `trace_id` and `span_id` on one written inside a span, which is the
join from a log line to a trace; the fields below are what this module
adds and the two sets together are the whole line. `service` is
`luxd`, `serve.ServiceName`; `version` is the build's; `replica` is the
pod's name, then the host's, as that package derives it.

| Plane | Fields |
|---|---|
| data | `request_id`, `door`, `route`, `model`, `provider`, `status`, `code`, `key_prefix`, `owner`, `duration_ms`, `ttfb_ms`, `input_tokens`, `output_tokens`, `stream` |
| control | `request_id`, `route`, `action`, `kind`, `name`, `status`, `code`, `subject`, `duration_ms` |

The line's message is the span's name, `lux.request` or `lux.api`, so
a reader tells the planes apart by it and no `plane` field is added.
The data plane's line is written by the gateway at the end of the
pipeline, inside the span and before it ends, through
`gateway.Options.Logger`, which is `slog.Default` when nil, so the
process's default logger is what a door writes to and a platform that
mounts the handler passes its own ([[004-request-path]] lists the
option). The control plane's line is written by `internal/api` when the
route has answered or the refusal is written, the file mode's public
`/v1` included; its `status` is `ok` without a refusal, `refused` for a
code answered under 500, and `failed` at or above; `name` is the
`{name}` of the path, empty on a route without one; `action` and `kind`
are what the authorizer was asked, empty when nothing was.

`route` is the route template of [[004-request-path]]'s door table or
[[011-api]]'s route table, never the request's own path, for the reason
the cardinality rule gives above. Every field name is from these two
tables and no caller string is ever a field name; a value is a JSON
string the encoder escapes, so a name or a label carrying newlines or
terminal escapes is one line and not two
([[016-security-and-threat-model]]).

`WARN` for an event delivery failure, a dropped request-log batch, a
Provider health transition, and a circuit opening; `ERROR` for a store
operation that failed and for a start-up problem. The register is the
developer's: a log line says what happened and what to look at, and is
never the user sentence of the error table.

A log line carries the subject and the Key's owner because the log is
the operator's own record of its own installation and is the
correlation an incident starts from; a span does not, because a span
leaves for a system with another audience. A redacting `slog.Handler`,
`serve.Redact`, runs outermost, before the local stream and the OTLP
bridge alike, and truncates any string matching `lux_[A-Za-z0-9_-]{40}`
to its first twelve characters, `serve.RedactedLength`, wherever it
appears and whatever the field name: the message, a string attribute at
any depth, an error's text, and the attributes a logger was built with.
So a Key value logged by accident becomes its own prefix. No request
body, response body, event body, prompt, completion, or header value is
ever a log argument, and no Provider credential can be one: the
credential reaches the upstream client and nothing else
([[005-providers]]).

The credential a door was presented is hashed and discarded before any
handler runs and is never a log argument in any shape
([[007-keys-and-limits]]); the `lux_` redaction above is a second line
for a minted value a deliberate caller writes, and a supplied value has
no pattern to redact, which is why it never reaches a logger at all.

### Start-up

`serve.Telemetry(ctx, version, stderr)` is the one call the serve role
makes, and the one block `cmd/luxd` carries for this spec: `Bootstrap`
with the service name, the build's version, and a JSON handler on the
process's stderr, then the redacting handler over the logger it
returns. The wiring installs that logger as `slog`'s default and passes
it to every job and both planes, and defers the returned stop, which
flushes every exporter after the listeners and the jobs have stopped.
A log exporter that cannot start is one `WARN` line and local logging,
because telemetry never stops the gateway.

### Alerts

`deploy/base/prometheusrule.yaml`, shipped by
[[017-release-and-installation]] and checked with `promtool check
rules` in CI. Every metric an expression names is in the table above.
`TestAlertsNameKnownMetrics` in `internal/arch` reads the file and this
spec's two tables: the file parses as a `PrometheusRule` with one group,
carries exactly the alerts of the table with the table's expressions
and durations, names only metrics of the metric table with a
histogram's suffixes on a histogram alone and only labels of each
metric's row, and gives every rule a severity and a summary. The
`promtool` step is [[017-release-and-installation]]'s CI job.

| Name | Expression | For | Means |
|---|---|---|---|
| `LuxDown` | `up{job="luxd"} == 0` | 5m | a replica is not being scraped |
| `LuxRefusalRateHigh` | `sum(rate(lux_refusals_total[5m])) / sum(rate(lux_requests_total{door!="control"}[5m])) > 0.1` | 10m | more than a tenth of data plane requests are refused; a limit, a key, or a catalog problem |
| `LuxUpstreamErrorRateHigh` | `sum by (provider) (rate(lux_upstream_requests_total{status!="ok"}[5m])) / sum by (provider) (rate(lux_upstream_requests_total[5m])) > 0.05` | 10m | one provider is failing a twentieth of its requests |
| `LuxProviderUnreachable` | `max by (provider) (lux_provider_health{state="Unreachable"}) == 1` | 10m | a Provider's targets are out of selection |
| `LuxTimeToFirstByteSlow` | `histogram_quantile(0.95, sum by (le, door) (rate(lux_time_to_first_byte_seconds_bucket[5m]))) > 10` | 10m | callers are waiting; the gateway or a provider is slow |
| `LuxMeteringFlushLagging` | `max(lux_metering_flush_lag_seconds) > 30` | 5m | spend counters are stale, so hard limits overshoot further than the stated bound ([[009-usage-and-metering]]) |
| `LuxAuthorizerUnavailable` | `increase(lux_authorizer_requests_total{decision="unavailable"}[5m]) > 0` | 5m | control plane requests are being refused `authorizer_unavailable` |
| `LuxStoreFailing` | `increase(lux_store_operations_total{result="error"}[5m]) > 0` | 10m | the store is failing operations |
| `LuxEventsBacklog` | `max(lux_events_pending) > 1000` | 15m | the operator's sink is not acknowledging |
| `LuxRequestLogDropping` | `increase(lux_requestlog_dropped_total[10m]) > 0` | 0m | the archive is unreachable and records are being lost ([[012-request-log-and-events]]) |

### Configuration and the dependency row

This spec adds no variable. The exporter is `OTEL_*` as
[[002-repository-scaffold]]'s table says, and the listener addresses
are that spec's.

It adds one dependency. `latere.ai/x/pkg/otel` pulls the OpenTelemetry
SDK and the OTLP HTTP exporters into `./cmd/luxd`'s build list, which
[[001-architecture]] admits and `.lateregate.yaml`'s `depcheck` block
records as a row under `latere.ai/x/lux/cmd/luxd` with this spec as its
reason. `latere.ai/x/pkg/metrics` adds nothing: it is a Prometheus text
writer over the standard library and is not the `client_golang`
registry. `./cmd/lux` gets no row, because the command exports no
telemetry ([[014-agent-client]]). The build moved three modules already
in the list from indirect to direct, `go.opentelemetry.io/otel/trace`
for the span API, and `go.opentelemetry.io/proto/otlp` with
`google.golang.org/protobuf` for the e2e test that decodes what the
collector received; no module joined.

### What the build settled

Each of these is written into the Design above in the same commit.

- The tracer is the global provider's, resolved per call, and no
  option carries one; the process's wiring is one block in `cmd/luxd`
  and the packages need nothing passed. The acceptance row on tracing
  off reads the absence of a span context rather than an allocation
  count, because a no-op provider's span is a value with no context
  and that is the observable fact.
- `gateway.Options.Logger` is added, nil being `slog.Default`, because
  the data plane's line must be written inside the request's span, and
  the gateway is the one place that is; [[004-request-path]]'s Options
  listing owes the field.
- The two upstream metrics of [[005-providers]]'s row are emitted by
  the gateway's attempt, with the `status` rule above; that spec's
  Design owes the claim. `lux_provider_health` stays that spec's to
  build, with `serve.HealthOptions.Metrics`.
- `lux_output_tokens_per_second` is the Recorder's, with the time rule
  above, as [[009-usage-and-metering]] left it to this spec to place.
- `lux.store` opens only under a parent span, so the jobs trace
  nothing; without the rule every tick of discovery, health, and the
  two flushes would be a root trace of its own.
- The listener carries no server span in this build; the roots are
  `lux.request` and `lux.api`, and mounting `otel.Handler` is
  [[002-repository-scaffold]]'s edit if wanted.
- The line's message is the span's name, so the two field rows need no
  field to tell the planes apart.
- The threat table of [[016-security-and-threat-model]] marked
  `TestTelemetryCarriesNoSecrets`, `TestLogFieldsAreTheTable`, and
  `TestSpansCarryNoIdentity` as owed by this spec; `internal/arch`'s
  `TestThreatTableIsGrounded` requires a marker dropped once the test
  is in the tree, so those three markers were dropped in the same
  build. The verification rows of that spec that still say `019` are
  its own to update.

## Not in this spec

Dashboards: a platform builds those from the same metrics, and the
project ships none. The record and the usage API that carry the
per-key, per-owner, and per-label dimensions
([[009-usage-and-metering]]); the request log archive and the event
sink ([[012-request-log-and-events]]); the listeners, the readiness
checks, and what `/livez`, `/readyz`, and `/version` answer, which this
spec reads and does not define ([[002-repository-scaffold]]); the rules
file's place in the deploy tree and its `promtool` step
([[017-release-and-installation]]); the health gauge's writer
([[005-providers]]); the two gauges and the counter of the request log,
the events, and the tunnel ([[012-request-log-and-events]],
[[013-tunnelled-runtimes]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The registry holds exactly the metrics in the table, each with its type, and every label in the table and no other | `TestMetricsTable`, reading this file and the registry | passing, `cmd/luxd`; `lux_provider_health`, `lux_events_pending`, `lux_requestlog_dropped_total`, and `lux_tunnel_sessions` tolerated as not built, 005, 012, 013 |
| Every label value a handler writes is in the closed set of its row | `TestMetricLabelValues`, table-driven over every label | passing, `gateway` |
| Ten thousand requests naming ten thousand model strings that resolve to nothing add no series to the registry, and one hundred requests to one Model add one | `TestUnresolvedModelAddsNoSeries` | passing, `gateway` |
| No metric, span attribute, or log line in an e2e run contains a canary Key value, a canary provider credential, a canary prompt, or a canary completion | `TestTelemetryCarriesNoSecrets` | passing, `cmd/luxd` |
| A Key value written to a log argument by a deliberate caller appears truncated to twelve characters | `TestLogRedactsKeyValues` | passing, `internal/serve` |
| No span carries a subject, owner, Key id, Key prefix, or caller address, over every span the e2e tier produces | `TestSpansCarryNoIdentity` | passing, `cmd/luxd` over the exported spans and `gateway` over the package's |
| A data plane request produces one `lux.request` span with one `lux.upstream` child per target tried, each carrying the request id's parent; a control plane request produces `lux.api` with its authorizer and store children | `TestRequestSpans`, with an in-memory exporter | passing, `gateway` for the data plane and `internal/api` for the control plane |
| With no `OTEL_EXPORTER_OTLP_ENDPOINT` no span is exported and the request path starts no recording span: the context the Key lookup receives carries no span context | `TestTracingOffByDefault` | passing, `gateway` |
| Each histogram in the buckets table is registered with exactly the boundaries in its row, and a ten minute stream lands in a bucket below `+Inf` | `TestHistogramBuckets` | passing, `internal/serve` |
| Every log line of an e2e run carries the base fields and exactly the fields of its plane's row and no other, and a name, label, and model string of newlines and terminal escapes produce one line each | `TestLogFieldsAreTheTable` | passing, `cmd/luxd`, with `TestRequestLineFields` in `gateway` and `TestAPILineFields` in `internal/api` |
| An authorizer answer of each kind moves `lux_authorizer_requests_total` on the matching `decision`, with a `pkg/authz` `error` counted as `unavailable` | `TestAuthorizerMetricMapping` | passing, `internal/serve` |
| A target whose circuit opens sets `lux_circuit_open` to 1 for that provider and model and back to 0 when it closes; an open and a closed tunnel session move `lux_tunnel_sessions` by one | `TestCircuitAndTunnelGauges` | the circuit half passing, `gateway`; the tunnel half not built, 013 |
| `./cmd/luxd`'s build list carries the OpenTelemetry SDK under the row this spec adds, and `./cmd/lux`'s carries none of it | the `depcheck` gate | passing; `./cmd/lux` is not built, 014 |
| The rules file passes `promtool check rules` and names only metrics and labels in the table | the CI step, `TestAlertsNameKnownMetrics` | `TestAlertsNameKnownMetrics` passing, `internal/arch`; the CI step not built, 017 |
| `/metrics` is served on the internal listener in the Prometheus text format with the `version=0.0.4` content type, and answered 404 on the public one, while the other three probes answer on both | [[002-repository-scaffold]]'s `TestServeAnswersTheProbesOnBothListenersAndStopsCleanly`, `TestMetricsContentType` | passing, `cmd/luxd` |
