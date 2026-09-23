---
title: "Performance and benchmarks: the gateway's own overhead, in process against a stub upstream"
status: complete
track: core
depends_on:
  - specs/004-request-path.md
affects: [gateway/, manifest/, metering/, internal/serve/, docs/]
effort: small
created: 2026-09-14
updated: 2026-09-14
author: changkun
---

# Performance and benchmarks

## Overview

The tree had no benchmarks: `grep -rn '^func Benchmark' --include='*_test.go'`
returned nothing, so nothing communicated the runtime cost the gateway
adds. A reader could not tell whether Lux adds a microsecond or a
millisecond on top of a provider call, nor what it costs in memory, nor
how translation compares to a byte-for-byte forward.

This spec adds `go test -bench` benchmarks that measure the gateway's own
overhead per request, isolated from the provider and the network, a
percentile latency-and-throughput harness for the hot path, and
`docs/performance.md` that frames what they measure and how to run them.
Every benchmark drives the in-process handler against the stub providers
the `gateway` tests already use ([[004-request-path]]), so nothing dials a
real provider; the arch tests forbid it ([[001-architecture]], invariant
9). The point is to bound what Lux adds, not to time a provider: real
end-to-end latency is dominated by the provider and the network, which
these deliberately exclude.

There is no CI pass/fail gate on performance. Benchmarks are measurements,
not assertions.

## Current state

The `gateway` package is a handler driven in its tests through fakes for
every interface of `Options` and `httptest` stub providers, one per
dialect, wired by `newWorld`, `post`, and the fakes of
`gateway/harness_test.go` ([[004-request-path]]). A request runs entirely
in process against a stub upstream on loopback, which is exactly what
isolates the gateway's overhead from a real provider. `manifest.Resolve`
([[003-manifest-contract]]), `metering.Cost` and the window arithmetic
([[009-usage-and-metering]]), and `internal/serve`'s `Limiter`
([[007-keys-and-limits]]) are each unit-tested but not benchmarked.

## Design

### Methodology

Each benchmark reuses the `gateway` harness so the setup is the one the
tests trust: `newWorld` builds the handler, the fakes, and four stub
`httptest` providers on loopback, with a `countingTransport` that refuses
any host that is not a stub. The harness's `newWorld` and `newStub` take
`testing.TB`, so a benchmark drives the same world a test does.

- Nothing dials out. The stub upstream is in process on loopback, so a
  request pays the real HTTP client machinery but not a provider's compute.
  That constant cost is the same for every route class, so the difference
  between classes is gateway work.
- The body, the catalog, the Key, and the answers are fixed, so runs
  compare.
- Timers are reset after setup with `b.ResetTimer`, and `b.ReportAllocs`
  reports `allocs/op` and `B/op` beside `ns/op`.
- A benchmark that reaches a provider carries the loopback round-trip in
  its `ns/op`; the count route reaches none, so its figure is the clean
  gateway-only cost, and the translated-minus-passthrough delta and the
  memory per request isolate translation independently of the round-trip.

### The benchmarks

`gateway`, one per route class ([[004-request-path]]):

| Benchmark | Isolates |
|---|---|
| `BenchmarkGatewayPassthrough` | the same-dialect hot path: an `/openai` door to an `openai` target with equal names, forwarded byte for byte, no codec |
| `BenchmarkGatewayTranslated` | the translated hot path: an `/openai` door to an `anthropic` target through `llmdialect/bridge` both ways; the delta from the passthrough is the translation cost |
| `BenchmarkGatewayCount` | a token count the gateway answers itself, an `/anthropic` count toward an `openai` target served from the estimator, reaching no provider |
| `BenchmarkGatewayStreaming` | the streaming relay of a fixed event count on a passthrough stream: the event loop and the usage scan |

`manifest` ([[003-manifest-contract]]):

| Benchmark | Isolates |
|---|---|
| `BenchmarkResolve/Provider`, `/Model`, `/Key`, `/Budget` | the six-stage resolve every control plane surface runs, per kind, against an in-memory lookup |

`metering` ([[009-usage-and-metering]]):

| Benchmark | Isolates |
|---|---|
| `BenchmarkCost` | the per-request costing |
| `BenchmarkWindow` | the window bounds arithmetic every spend check runs |
| `BenchmarkCounterKey` | rendering a spend counter's store key |

`internal/serve` ([[007-keys-and-limits]]):

| Benchmark | Isolates |
|---|---|
| `BenchmarkLimiterReserveSettle` | stage 7 on one replica in its in-memory path over a memory store: the two rate buckets, the pricing and spend projection, admit, and settle, with no store I/O on the timed path |

### The latency and throughput harness

A benchmark reports a mean; a gateway is judged on its tail. The
distribution harness in `gateway/latency_test.go` drives the in-process
handler at a fixed concurrency for a fixed request count and reports, per
route class (passthrough, translated, count): p50, p75, p90, p95, and p99
of the added latency, throughput in requests per second, and bytes
allocated per request. Percentiles are a standard-library nearest-rank
pick over sorted durations; no new dependency. The translation overhead is
the translated-minus-passthrough delta, called out alongside the memory per
request, which is the steadiest isolation because the concurrent wall-clock
p50 is dominated by the constant in-process round-trip.

It is runnable as a normal `go test` and never part of the gate:
`TestGatewayAddedLatency` skips unless `LUX_LATENCY` is set and prints the
full distribution when it is; `TestLatencySummary` runs a small sweep with
no latency threshold, so the harness is exercised in the gate without
asserting anything about speed; and `TestPercentileNearestRank` holds the
percentile math to known values. The framing is honest: this is the
gateway's own added latency against a stub upstream, and real end-to-end
p95 is dominated by the provider.

### Cross-proxy comparison

The whole performance story includes how Lux compares to another proxy.
The comparison holds the provider and the network constant with a shared
mock upstream, runs each proxy against it, and reports the added overhead as
the proxy's latency minus a direct-to-mock baseline, with the same
percentile set (p50/p75/p90/p95/p99), throughput, and peak RSS, for the
passthrough and translated shapes.

The fairness caveats are part of the method: it measures proxy overhead,
not model quality; a Go proxy against a Python or uvicorn one measures the
runtimes as much as the code; and the numbers are machine-relative and do
not compare across machines. The runnable harness and its measured table
are a separate artifact under `benchmarks/compare/`: `README.md` for the
method, `RESULTS.md` for the numbers. It is built by a separate effort and
is not one of the in-repo Go benchmarks this spec's acceptance covers.

## Not in this spec

- No CI pass/fail gate on performance: the benchmarks are measurements, and
  a regression is read by a person, not a gate.
- No provider or network timing: the stub upstream is in process, and the
  provider's compute and the network between a caller and Lux are out of
  scope by construction.
- No cross-machine guarantee: the sample numbers in `docs/performance.md`
  are from one machine with an unspecified CPU and are not a promise.
- The cross-proxy comparison harness's implementation under
  `benchmarks/compare/` is a separate artifact; this spec documents its
  approach and its caveats, not its code.

## Acceptance criteria

Each row is a benchmark that exists and runs, proven by `go test -bench`.

| Criterion | Test that proves it | State |
|---|---|---|
| The four gateway route-class benchmarks run against the in-process stubs and report `ns/op` and `allocs/op` | `BenchmarkGatewayPassthrough`, `BenchmarkGatewayTranslated`, `BenchmarkGatewayCount`, `BenchmarkGatewayStreaming` under `go test -bench . -benchmem ./gateway/` | passing |
| The resolve benchmark runs for each kind | `BenchmarkResolve/Provider`, `/Model`, `/Key`, `/Budget` under `go test -bench BenchmarkResolve -benchmem ./manifest/` | passing |
| The cost and window benchmarks run | `BenchmarkCost`, `BenchmarkWindow`, `BenchmarkCounterKey` under `go test -bench . -benchmem ./metering/` | passing |
| The limiter reserve-and-settle benchmark runs in its in-memory path | `BenchmarkLimiterReserveSettle` under `go test -bench . -benchmem ./internal/serve/` | passing |
| The latency harness reports p50/p75/p90/p95/p99, throughput, and bytes per request per route class, opt-in and off the gate, with correct percentile math | `TestGatewayAddedLatency` under `LUX_LATENCY=1 go test -run TestGatewayAddedLatency -v ./gateway/`; `TestLatencySummary` and `TestPercentileNearestRank` in the untagged run | passing |
| `docs/performance.md` frames what the benchmarks measure, gives the exact commands, names each benchmark, shows one machine's sample labeled not a guarantee, and points at `benchmarks/compare/` for the comparison; `docs/README.md` lists it | the files, read against this spec | passing |

## Outcome

Built on 2026-09-14. The `gateway` harness's `newWorld` and `newStub` were
widened to `testing.TB` so a benchmark drives the same world, fakes, and
stub providers a test does. Twelve benchmarks landed:
`gateway/bench_test.go` (four route classes), `manifest/bench_test.go`
(`BenchmarkResolve` per kind), `metering/bench_test.go` (`BenchmarkCost`,
`BenchmarkWindow`, `BenchmarkCounterKey`), and
`internal/serve/bench_test.go` (`BenchmarkLimiterReserveSettle`). The
percentile latency-and-throughput harness is `gateway/latency_test.go`, with
`TestGatewayAddedLatency` opt-in behind `LUX_LATENCY`, `TestLatencySummary`
covering the driver in the gate with no perf assertion, and
`TestPercentileNearestRank` holding the percentile math.

Representative figures from one machine, CPU unspecified, not a guarantee:
the count route, which reaches no provider, is about 7 µs and 87 allocs;
`BenchmarkCost` is about 2 ns and zero allocs; a passthrough is about 60 µs
and a translated request about 117 µs serially, translation allocating some
8 KB more per request. At concurrency 8 the passthrough p50 is about 162 µs
and the translated p50 about 169 µs, the round-trip dominating the
wall-clock delta, which is why the memory delta is the steadier read of
translation cost. The full tables are in `docs/performance.md`.

`docs/performance.md` and its `docs/README.md` row explain that these bound
the gateway's own overhead in process against a stub upstream, that
end-to-end latency is dominated by the provider and the network, and that
the cross-proxy comparison lives under `benchmarks/compare/` as a separate
artifact. No CI pass/fail gate on performance was added.
