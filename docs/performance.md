# Performance

These benchmarks measure the gateway's own overhead: the cost Lux adds on
top of a provider call, in process, against a stub upstream. They bound the
latency and the allocations the gateway itself contributes and say nothing
about a provider's compute or the network between you and it. End-to-end
latency of a real request is dominated by the provider and the network,
which these benchmarks deliberately exclude, so a fast benchmark here does
not promise a fast request in production; it promises the gateway is not
what makes a request slow.

Every benchmark drives the same in-process handler, fakes, and stub
providers the `gateway` tests use ([`gateway/harness_test.go`](../gateway/harness_test.go)).
Nothing dials a real provider; the arch tests forbid it. The stub upstream
is an in-process `httptest` server on loopback, so a request pays the real
HTTP client machinery (serialization, a pooled loopback connection) but not
a provider's compute; that constant cost is the same for every route class,
so the difference between classes is the gateway work, not the upstream.

There is no CI pass/fail gate on performance. Benchmarks are measurements,
not assertions; a number that regresses is read by a person, not a gate.

## Running them

    go test -bench . -benchmem ./gateway/... ./manifest/... ./metering/...

Add `./internal/serve/...` for the limiter benchmark. Per package:

    go test -bench . -benchmem ./gateway/
    go test -bench BenchmarkResolve -benchmem ./manifest/
    go test -bench . -benchmem ./metering/
    go test -bench BenchmarkLimiterReserveSettle -benchmem ./internal/serve/

The latency distribution is a separate, opt-in test (below).

## What each benchmark isolates

`gateway`, one benchmark per route class ([request path](../specs/004-request-path.md)):

| Benchmark | Isolates |
|---|---|
| `BenchmarkGatewayPassthrough` | the same-dialect hot path: an `/openai` door to an `openai` target with equal names, so the body is forwarded byte for byte and no codec runs. Routing, key check, limit, forward, and record, and nothing else |
| `BenchmarkGatewayTranslated` | the translated hot path: an `/openai` door to an `anthropic` target, decoded and re-encoded through `llmdialect/bridge` both ways. The translation cost is the delta from the passthrough |
| `BenchmarkGatewayCount` | a token count the gateway answers itself, an `/anthropic` count toward an `openai` target served from the estimator, so no request reaches a provider: the clean gateway-only figure, with no loopback |
| `BenchmarkGatewayStreaming` | the streaming relay of a fixed event count on a passthrough stream: the event loop and the usage scan |

`manifest`:

| Benchmark | Isolates |
|---|---|
| `BenchmarkResolve/Provider`, `/Model`, `/Key`, `/Budget` | the six-stage resolve every control plane surface runs, per kind, against an in-memory lookup |

`metering`:

| Benchmark | Isolates |
|---|---|
| `BenchmarkCost` | the per-request costing: the integer sum of the priced token members with one rounding |
| `BenchmarkWindow` | the window bounds arithmetic every spend check runs |
| `BenchmarkCounterKey` | rendering a spend counter's store key, the window arithmetic plus the string |

`internal/serve`:

| Benchmark | Isolates |
|---|---|
| `BenchmarkLimiterReserveSettle` | stage 7 on one replica in its in-memory path: the two rate buckets, the pricing and spend projection, admit, and settle, with no store I/O on the timed path |

## Added latency distribution and throughput

A benchmark reports a mean; a gateway is judged on its tail. The
distribution harness drives the in-process handler at a fixed concurrency
and reports p50, p75, p90, p95, and p99 of the added latency, throughput in
requests per second, and bytes allocated per request, for the passthrough,
translated, and count route classes. It isolates the same overhead the
`-bench` benchmarks do; the numbers are the gateway's added cost, and real
end-to-end p95 is dominated by the provider, which this excludes.

It is opt-in and never part of the gate:

    LUX_LATENCY=1 go test -run TestGatewayAddedLatency -v ./gateway/

Without `LUX_LATENCY` the test runs a small sweep with no latency threshold,
so the harness is exercised but nothing is asserted about speed.

Read this distribution together with the memory column, not alone. Under
concurrency the wall-clock p50 of a passthrough and a translated request are
close, because the constant in-process round-trip dominates and a
request's translation CPU overlaps another request's I/O across cores. The
serial CPU cost of translation shows in `BenchmarkGatewayTranslated` minus
`BenchmarkGatewayPassthrough` above. The steadiest cross-cutting isolation
is memory: translation allocates about 8 KB more per request than a
passthrough, on every machine.

## A sample run

Illustrative output from one developer machine. The CPU is unspecified;
these are not a guarantee and do not compare across machines. Run the
commands above to get your own.

`go test -bench . -benchmem`:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `BenchmarkGatewayPassthrough` | 60217 | 29162 | 223 |
| `BenchmarkGatewayTranslated` | 116608 | 37692 | 370 |
| `BenchmarkGatewayCount` | 6845 | 14891 | 87 |
| `BenchmarkGatewayStreaming` | 138051 | 106894 | 303 |
| `BenchmarkResolve/Provider` | 2606 | 2182 | 23 |
| `BenchmarkResolve/Model` | 2175 | 1742 | 21 |
| `BenchmarkResolve/Key` | 2355 | 2309 | 25 |
| `BenchmarkResolve/Budget` | 1928 | 1468 | 21 |
| `BenchmarkCost` | 1.7 | 0 | 0 |
| `BenchmarkWindow` | 28 | 0 | 0 |
| `BenchmarkCounterKey` | 73 | 80 | 2 |
| `BenchmarkLimiterReserveSettle` | 1106 | 1488 | 29 |

`LUX_LATENCY=1 ... TestGatewayAddedLatency`, 20000 requests at concurrency 8:

| Class | p50 | p75 | p90 | p95 | p99 | req/s | B/req |
|---|---|---|---|---|---|---|---|
| passthrough | 162µs | 218µs | 284µs | 347µs | 553µs | 42408 | 34023 |
| translated | 169µs | 225µs | 290µs | 350µs | 601µs | 40040 | 42072 |
| count | 8µs | 12µs | 24µs | 46µs | 182µs | 360593 | 14894 |

The count route reaches no provider, so its microsecond figures are the
gateway's own work with no loopback. The passthrough and translated figures
carry the constant in-process round-trip; the honest reading of translation
overhead is the memory delta (about 8 KB per request) and the serial CPU
delta from the `-bench` table, not the concurrent p50, which the round-trip
dominates.

## Comparison with other proxies

A cross-proxy comparison holds the provider and the network constant with a
shared mock upstream, runs each proxy against it, and reports the added
overhead as the proxy's latency minus a direct-to-mock baseline, with the
same percentile set (p50/p75/p90/p95/p99), throughput, and peak RSS, for
the passthrough and translated shapes. It measures proxy overhead, not
model quality, and a Go proxy against a Python one measures the runtimes as
much as the code; the numbers are machine-relative and prove nothing about
which proxy routes better. The runnable harness and its measured table live
under `benchmarks/compare/`: `README.md` for the method, `RESULTS.md`
for the numbers. That harness is a separate artifact
from the in-repo Go benchmarks above.

## Where the design lives

[`specs/023-performance-and-benchmarks.md`](../specs/023-performance-and-benchmarks.md)
owns what each benchmark measures and the methodology, and the
[request path spec](../specs/004-request-path.md) is the pipeline they
exercise.
