# Observability

What `luxd` exposes so you can tell whether it is serving, which provider
is costing you, and whether anything is wrong. The design, for
contributors, is
[`specs/019-observability.md`](../specs/019-observability.md).

## Health probes

Four paths from the health package. `/livez`, `/readyz`, and `/version`
answer on both listeners; `/metrics` is on the internal listener only.

| Path | Answers |
|---|---|
| `GET /livez` | `200 ok`; touches no dependency |
| `GET /readyz` | `200 ok` when every check passes, else `503 not ready: <check>: <error>`; checks the store within a 2s budget, and answers 503 while draining |
| `GET /version` | `{"version","commit","build_time"}` |
| `GET /metrics` | the Prometheus registry, text format; internal listener only |

Point the liveness probe at `/livez` and the readiness probe at
`/readyz`. A provider that fails its own health probe takes its targets
out of rotation and does not fail readiness, so one bad provider does not
pull a replica out of service.

## Metrics

Scrape `/metrics` on the internal listener. The metrics worth watching:

| Metric | Type | Labels | Reads |
|---|---|---|---|
| `lux_requests_total` | counter | `door`, `model`, `provider`, `status`, `code` | every request on both planes; `door=control` is `/v1` |
| `lux_request_duration_seconds` | histogram | `door`, `model`, `status` | request latency |
| `lux_time_to_first_byte_seconds` | histogram | `door`, `model` | how long a caller waits for the first byte |
| `lux_refusals_total` | counter | `code` | every error envelope written, by code |
| `lux_upstream_requests_total` | counter | `provider`, `status` | one per target tried; `status` is `ok`, `error`, `timeout` |
| `lux_upstream_duration_seconds` | histogram | `provider` | upstream latency |
| `lux_provider_health` | gauge | `provider`, `state` | `1` on the state a replica acts on, over `Healthy`, `Degraded`, `Unreachable`, `Unknown` |
| `lux_tokens_total` | counter | `direction` | tokens by `input`, `output`, `cached_input`, `cache_write` |
| `lux_spend_microunits_total` | counter | `currency` | spend, in millionths of the currency unit |
| `lux_metering_flush_lag_seconds` | gauge | none | how stale a replica's spend counters are |
| `lux_authorizer_requests_total` | counter | `decision` | control-plane decisions; `decision` is `allow`, `deny`, `unavailable` |
| `lux_authorizer_duration_seconds` | histogram | `decision` | authorizer latency |
| `lux_store_operations_total` | counter | `op`, `result` | store calls; `result` is `ok`, `conflict`, `error` |
| `lux_key_cache_hits_total` | counter | `result` | Key lookups; `result` is `hit`, `miss`, `negative` |
| `lux_circuit_open` | gauge | `provider`, `model` | `1` while a target's circuit is open |
| `lux_events_pending` | gauge | none | events waiting for the sink |
| `lux_requestlog_dropped_total` | counter | none | request-log records lost when the archive is unreachable |
| `lux_tunnel_sessions` | gauge | none | tunnel sessions this replica holds |

`model` and `provider` are the resolved object's `metadata.name`, empty
when none was resolved; they are never a caller-controlled string, so a
flood of invented model names adds no series. No Key, subject, owner,
request id, or client address is ever a label: those live in
`GET /v1/usage` and the usage record, where each is a row. The metrics
answer how the gateway is behaving in aggregate; per-key, per-owner spend
is `GET /v1/usage`'s.

## Alerts

`deploy/base/prometheusrule.yaml` ships ten alerts as a `PrometheusRule`
the Prometheus Operator reads. Copy and tune them; nothing here is
required for the gateway to run. What each fires on:

| Alert | Fires when |
|---|---|
| `LuxDown` | a replica has answered no scrape for 5m |
| `LuxRefusalRateHigh` | more than a tenth of data-plane requests are refused: a limit, a key, or a catalog problem |
| `LuxUpstreamErrorRateHigh` | one provider is failing more than a twentieth of its requests |
| `LuxProviderUnreachable` | a Provider has been Unreachable for 10m and its targets take no traffic |
| `LuxTimeToFirstByteSlow` | the 95th-percentile first byte on a door is above 10s |
| `LuxMeteringFlushLagging` | a replica has not flushed spend counters for 30s, so hard limits overshoot further |
| `LuxAuthorizerUnavailable` | the authorizer gave no decision in 5m; every write is refused |
| `LuxStoreFailing` | store operations have returned errors for 10m |
| `LuxEventsBacklog` | more than a thousand events have waited for the sink for 15m |
| `LuxRequestLogDropping` | a batch of request-log records was dropped |

## Logs

`slog` JSON on stderr, one line per request at `INFO`. Every line carries
`service` (`luxd`), `version`, and `replica`, plus `trace_id` and
`span_id` when it is written inside a span. The message is the span's
name, `lux.request` on a door and `lux.api` on `/v1`, so you tell the
planes apart by it.

| Plane | Fields |
|---|---|
| data | `request_id`, `door`, `route`, `model`, `provider`, `status`, `code`, `key_prefix`, `owner`, `duration_ms`, `ttfb_ms`, `input_tokens`, `output_tokens`, `stream` |
| control | `request_id`, `route`, `action`, `kind`, `name`, `status`, `code`, `subject`, `duration_ms` |

`WARN` marks an event delivery failure, a dropped request-log batch, a
provider health transition, and a circuit opening; `ERROR` a failed store
operation or a start-up problem. No body, header, prompt, completion, or
credential is ever a log argument, and any `lux_` value is truncated to
its first twelve characters.

## Traces

Off unless you set `OTEL_EXPORTER_OTLP_ENDPOINT`. When set, `luxd` exports
OTLP over HTTP through the standard `OTEL_*` variables; no `LUX_*` knob
configures tracing. A data-plane request is one `lux.request` span with a
`lux.upstream` child per target tried; a control-plane request is
`lux.api` with `lux.authorizer` and `lux.store` children. Every span
carries `lux.request_id`, which joins it to the log line and the usage
record, and carries no subject, owner, Key, or address, so a tracing
system you share with other teams does not become a list of who called
what.
