---
title: "Observability: metrics, traces, logs, alerts"
status: drafted
track: core
depends_on:
  - specs/002-repository-scaffold.md
  - specs/004-request-path.md
  - specs/009-usage-and-metering.md
affects: [internal/serve/, internal/api/, internal/store/, internal/events/, internal/reqlog/, internal/tunnel/, gateway/, deploy/base/prometheusrule.yaml, .lateregate.yaml, docs/]
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

Nothing is built. The scaffold of [[002-repository-scaffold]] serves
`/livez`, `/readyz`, `/version`, and an empty `/metrics` on the
internal listener.

The hosted gateway this design is extracted from does four things this
spec does differently, and each difference is deliberate. It pushes
OpenTelemetry instruments over OTLP and serves no `/metrics` at all, so
an operator with Prometheus and no collector sees nothing; this spec
serves the Prometheus text format on the internal listener and treats
OTLP as the exporter for traces. Its per-request metrics carry the
request's raw URL path as a label, so every path-embedded model name is
a series of its own; this spec labels with a resolved object's name and
never a caller's string. Its spans carry the caller's organisation,
principal, and key id, behind a variable that hashes them rather than
removes them; this spec puts no identity on a span at all and joins
through the request id instead. And it writes no per-request log line,
so an incident starts from the archive rather than from the logs; this
spec writes one line per request on both planes.

## Design

### The four probes and the registry

[[002-repository-scaffold]] owns the listeners and mounts
`latere.ai/x/pkg/health`, whose four paths are `GET /livez`, `GET
/readyz`, `GET /version`, and `GET /metrics`. This spec adds no path
and changes none of the first three: liveness touches no dependency,
readiness runs that spec's checks inside its budget, and `/version` is
the build identity. It fills the fourth. `health.Options.Metrics` takes
an `http.Handler`, and the one this spec supplies writes
`Content-Type: text/plain; version=0.0.4; charset=utf-8` and then the
registry's `WritePrometheus`. `/metrics` exists only where that option
is set, which is the internal listener alone
([[011-api]], [[002-repository-scaffold]]).

### Metrics

One `latere.ai/x/pkg/metrics.Registry`, constructed once in
`internal/serve` and passed to everything that records. That package has
no package-level default registry on purpose, so there is one object to
pass and one object a test can read; nothing here reaches for a global.
It is served in the Prometheus text format at `GET /metrics` on the
internal listener and nowhere else ([[002-repository-scaffold]]). The
table is the reference: a metric not in it does not exist, and
`TestMetricsTable` reads this file.

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
| `lux_provider_health` | gauge | `provider`, `state` | [[005-providers]] |
| `lux_key_cache_hits_total` | counter | `result` | [[007-keys-and-limits]] |
| `lux_metering_flush_lag_seconds` | gauge | none | [[009-usage-and-metering]] |
| `lux_events_pending` | gauge | none | [[012-request-log-and-events]] |
| `lux_requestlog_dropped_total` | counter | none | [[012-request-log-and-events]] |
| `lux_authorizer_requests_total` | counter | `decision` | [[006-identity]] |
| `lux_authorizer_duration_seconds` | histogram | `decision` | [[006-identity]] |
| `lux_store_operations_total` | counter | `op`, `result` | [[010-state]] |
| `lux_circuit_open` | gauge | `provider`, `model` | [[008-routing-and-models]] |
| `lux_tunnel_sessions` | gauge | none | [[013-tunnelled-runtimes]] |

`lux_output_tokens_per_second` is a stream's output tokens over the time
from its first byte to its last, and a non-stream's over its upstream
duration; it is the figure an operator of a model server watches, and
the one hosted metric this spec carries over by name.

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
Model's name of [[008-routing-and-models]]'s key and is bounded by the
catalog like every other pair. `lux_tunnel_sessions` is the number of
tunnel sessions this replica holds ([[013-tunnelled-runtimes]]); the
provider name is not a label on it, because a session's Provider is
already a series of `lux_provider_health`. Both are gauges, which
`latere.ai/x/pkg/metrics` serves as a callback read at scrape time, so
neither is a counter this code has to keep in step.

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
  ([[009-usage-and-metering]]).

The upper bound on the registry is therefore `doors × models ×
providers × statuses × codes` for the request counter, which an
installation sizes from its own catalog, and a constant for every other
metric.

### Traces

`latere.ai/x/pkg/otel` reads the standard `OTEL_*` variables;
without `OTEL_EXPORTER_OTLP_ENDPOINT` no span is produced and the
instrumentation is a no-op, and `OTEL_SDK_DISABLED=true` turns it off
with an endpoint set. The exporter is OTLP over HTTP, the propagator is
W3C `traceparent` and `baggage`, and the sampler is parent-based with
the ratio of `OTEL_TRACES_SAMPLER_ARG`, all of that package's. No
`LUX_*` variable configures tracing, and none configures redaction,
because there is nothing to redact: the attributes below are the whole
set. The hosted gateway's `LUX_OTEL_REDACT_IDENTITY`, a switch between
identity on a span and a hash of identity on a span, has no counterpart
here, because neither state of it is a state this spec allows.

| Span | Parent | Attributes |
|---|---|---|
| `lux.request` | the caller's context, when it propagated one | `lux.door`, `lux.route`, `lux.model`, `lux.provider`, `lux.status`, `lux.code`, `lux.request_id`, `lux.stream`, `lux.translated` |
| `lux.upstream` | `lux.request` | `lux.provider`, `lux.attempt`, `http.request.method`, `url.template`, `http.response.status_code`, `lux.ttfb_ms` |
| `lux.api` | the caller's context | `lux.route`, `lux.action`, `lux.kind`, `lux.status`, `lux.code`, `lux.request_id` |
| `lux.authorizer` | `lux.api` | `lux.action`, `lux.decision` |
| `lux.store` | `lux.api` or `lux.request` | `lux.op`, `lux.kind`, `lux.result` |

Two spans on each trace are not this table's. `latere.ai/x/pkg/otel`'s
`Handler` opens a server span around the listener, and its `Transport`,
which the `otel-client` gate requires of every outbound HTTP client in
this module, opens a client span inside each `lux.upstream`. Both carry
`http.*` attributes and neither carries identity, so they add no rule;
the table is what this module names, and the acceptance criteria read
every span an e2e run produces, those two included.

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
adds and the two sets together are the whole line.

| Plane | Fields |
|---|---|
| data | `request_id`, `door`, `route`, `model`, `provider`, `status`, `code`, `key_prefix`, `owner`, `duration_ms`, `ttfb_ms`, `input_tokens`, `output_tokens`, `stream` |
| control | `request_id`, `route`, `action`, `kind`, `name`, `status`, `code`, `subject`, `duration_ms` |

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
leaves for a system with another audience. A redacting `slog.Handler`
runs before every exporter and truncates any string matching
`lux_[A-Za-z0-9_-]{40}` to its first twelve characters, wherever it
appears and whatever the field name, so a Key value logged by accident
becomes its own prefix. No request body, response body, event body,
prompt, completion, or header value is ever a log argument, and no
Provider credential can be one: the credential reaches the upstream
client and nothing else ([[005-providers]]).

The credential a door was presented is hashed and discarded before any
handler runs and is never a log argument in any shape
([[007-keys-and-limits]]); the `lux_` redaction above is a second line
for a minted value a deliberate caller writes, and a supplied value has
no pattern to redact, which is why it never reaches a logger at all.

### Alerts

`deploy/base/prometheusrule.yaml`, shipped by
[[017-release-and-installation]] and checked with `promtool check
rules` in CI. Every metric an expression names is in the table above.

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
telemetry ([[014-agent-client]]).

## Not in this spec

Dashboards: a platform builds those from the same metrics, and the
project ships none. The record and the usage API that carry the
per-key, per-owner, and per-label dimensions
([[009-usage-and-metering]]); the request log archive and the event
sink ([[012-request-log-and-events]]); the listeners, the readiness
checks, and what `/livez`, `/readyz`, and `/version` answer, which this
spec reads and does not define ([[002-repository-scaffold]]); the rules
file's place in the deploy tree ([[017-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The registry holds exactly the metrics in the table, each with its type, and every label in the table and no other | `TestMetricsTable`, reading this file and the registry | not built |
| Every label value a handler writes is in the closed set of its row | `TestMetricLabelValues`, table-driven over every label | not built |
| Ten thousand requests naming ten thousand model strings that resolve to nothing add no series to the registry, and one hundred requests to one Model add one | `TestUnresolvedModelAddsNoSeries` | not built |
| No metric, span attribute, or log line in an e2e run contains a canary Key value, a canary provider credential, a canary prompt, or a canary completion | `TestTelemetryCarriesNoSecrets` | not built |
| A Key value written to a log argument by a deliberate caller appears truncated to twelve characters | `TestLogRedactsKeyValues` | not built |
| No span carries a subject, owner, Key id, Key prefix, or caller address, over every span the e2e tier produces | `TestSpansCarryNoIdentity` | not built |
| A data plane request produces one `lux.request` span with one `lux.upstream` child per target tried, each carrying the request id's parent; a control plane request produces `lux.api` with its authorizer and store children | `TestRequestSpans`, with an in-memory exporter | not built |
| With no `OTEL_EXPORTER_OTLP_ENDPOINT` no span is exported and the request path allocates no span | `TestTracingOffByDefault` | not built |
| Each histogram in the buckets table is registered with exactly the boundaries in its row, and a ten minute stream lands in a bucket below `+Inf` | `TestHistogramBuckets` | not built |
| Every log line of an e2e run carries the base fields and exactly the fields of its plane's row and no other, and a name, label, and model string of newlines and terminal escapes produce one line each | `TestLogFieldsAreTheTable` | not built |
| An authorizer answer of each kind moves `lux_authorizer_requests_total` on the matching `decision`, with a `pkg/authz` `error` counted as `unavailable` | `TestAuthorizerMetricMapping` | not built |
| A target whose circuit opens sets `lux_circuit_open` to 1 for that provider and model and back to 0 when it closes; an open and a closed tunnel session move `lux_tunnel_sessions` by one | `TestCircuitAndTunnelGauges` | not built |
| `./cmd/luxd`'s build list carries the OpenTelemetry SDK under the row this spec adds, and `./cmd/lux`'s carries none of it | the `depcheck` gate | not built |
| The rules file passes `promtool check rules` and names only metrics and labels in the table | the CI step, `TestAlertsNameKnownMetrics` | not built |
| `/metrics` is served on the internal listener in the Prometheus text format with the `version=0.0.4` content type, and answered 404 on the public one, while the other three probes answer on both | [[002-repository-scaffold]]'s `TestServeAnswersTheProbesOnBothListenersAndStopsCleanly`, `TestMetricsContentType` | not built |
