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

A single run is a point, and a point cannot tell the code's cost from the
scheduler's noise. For numbers worth comparing, repeat each benchmark and
summarize with `benchstat`, which reports the mean, its variation, and — when
comparing two inputs — a p-value:

    GOMAXPROCS=8 go test -run '^$' -bench . -benchmem -count=10 \
      ./gateway/ ./manifest/ ./metering/ ./internal/serve/ > new.txt
    go run golang.org/x/perf/cmd/benchstat@latest new.txt

Pin `GOMAXPROCS` and keep the machine quiet, so the variation the tool reports
is the code's and not the operating system's.

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
| `BenchmarkCatalogLookups/store`, `/snapshot` | what one request resolves from the catalog, the Model by name, its Provider, and the Provider's credential opened, read from the memory store and from the in-memory catalog every replica holds |
| `BenchmarkPostgresCatalogLookups/store`, `/snapshot` | the same over the Postgres store, built with `-tags=postgres` and `LUX_DB_URL` set: the store half is the three round trips per request the in-memory catalog removes |

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

## The catalog in memory

Every replica holds the catalog, every Model, Provider, and sealed
credential, in memory, kept current by the journal and re-read whole every
`LUX_CATALOG_RELOAD`, so a request resolves its Model, Provider, and
credential without a store round trip. The credential stays sealed in
memory and is opened per request. On an Apple M5 Pro, five runs each, with
Postgres 17 on the same machine over loopback:

| Benchmark | store | in memory |
|---|---|---|
| `CatalogLookups` (memory store) | 6.5µs to 7.3µs, 31 allocs | 0.65µs to 0.72µs, 8 allocs |
| `PostgresCatalogLookups` | 398µs to 443µs, 68 allocs | 0.66µs to 0.95µs, 8 allocs |

The Postgres figure is on loopback; across a network each of the three
round trips adds the network's latency, and each held one of the pool's
connections for its length. With the catalog in memory the data plane
reads the store for a Key once per `LUX_KEY_CACHE` and for nothing else.

## A measured run

From one machine: an Apple M4 Pro (arm64), `GOMAXPROCS=8`, otherwise
quiescent, each benchmark repeated `-count=10` and summarized with
`benchstat`. The `±` is benchstat's reported variation over the ten runs.
These are machine-relative and do not compare across machines; run the
commands above to get your own.

`benchstat` of `-count=10`:

| Benchmark | sec/op | B/op | allocs/op |
|---|---|---|---|
| `GatewayPassthrough` | 55.34µs ± 3% | 28.21Ki ± 1% | 223.0 ± 0% |
| `GatewayTranslated` | 67.15µs ± 2% | 35.87Ki ± 0% | 370.0 ± 0% |
| `GatewayCount` | 6.580µs ± 1% | 14.99Ki ± 3% | 87.00 ± 0% |
| `GatewayStreaming` | 83.54µs ± 3% | 101.9Ki ± 0% | 302.0 ± 0% |
| `Resolve/Provider` | 2.383µs ± 2% | 2.125Ki ± 0% | 23.00 ± 0% |
| `Resolve/Model` | 2.094µs ± 1% | 1.694Ki ± 0% | 21.00 ± 0% |
| `Resolve/Key` | 2.234µs ± 0% | 2.244Ki ± 0% | 25.00 ± 0% |
| `Resolve/Budget` | 1.863µs ± 1% | 1.429Ki ± 0% | 21.00 ± 0% |
| `Cost` | 1.633ns ± 0% | 0 | 0 |
| `Window` | 27.41ns ± 2% | 0 | 0 |
| `CounterKey` | 71.17ns ± 2% | 80.00 ± 0% | 2.000 ± 0% |
| `LimiterReserveSettle` | 1.081µs ± 2% | 1.453Ki ± 0% | 29.00 ± 0% |

The translation cost is the delta between the passthrough and the translated
hot path — the number a single run cannot separate from noise. `benchstat`
comparing the two paths (each `n=10`) can, and it is unambiguous:

| Metric | Passthrough | Translated | Delta | p (n=10) |
|---|---|---|---|---|
| sec/op | 55.34µs | 67.15µs | +21.35% | 0.000 |
| B/op | 28.21Ki | 35.87Ki | +27.14% | 0.000 |
| allocs/op | 223 | 370 | +65.92% | 0.000 |

Translation adds about 12µs, ~7.7 KB, and ~147 allocations per request over a
passthrough on this machine — the `pkg/llmdialect` decode-and-re-encode both
ways. With p ≈ 0 at n=10 that is signal, not scheduler jitter, and it is the
honest reading of the translation overhead: the serial CPU and memory delta
here, not the concurrent p50, which the constant round-trip dominates.

`LUX_LATENCY=1 ... TestGatewayAddedLatency` (one illustrative run), 20000
requests at concurrency 8:

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
which proxy routes better.

Across 10 independent trials on an Apple M4 Pro at concurrency 50 against the
shared mock, each with its own warmup and reported as the median with a 95%
confidence interval: for a same-format passthrough, `luxd` added about 2 ms of
p50 latency over calling the mock directly and sustained ~20,000 requests per
second in a single process at ~45 MB of resident memory, while LiteLLM 1.100.1
with one worker added ~112 ms of p50 and sustained ~420 requests per second at
~410 MB. The translated shape held the same ratio, ~2–3 ms added for `luxd`
against ~105 ms for LiteLLM, and streaming tracked it, with zero errors on
every trial. The gap is **~45–55x** in both latency and throughput, with
non-overlapping CIs in every condition — an effect size, not a close call, so
no p-value is reported: with tens of thousands of requests behind each trial a
significance test would return a vanishingly small p that says nothing about
whether a 45x difference matters. Trust the ratios over the absolutes: these
are single-process, machine-relative figures that compare a Go binary with a
Python service and measure proxy overhead alone.

The runnable harness, its full per-condition table with the CIs, and the
charts live under `benchmarks/compare/`: `README.md` for the method,
`RESULTS.md` for the numbers and the figures. That harness is a separate
artifact from the in-repo Go benchmarks above.

## Where the design lives

[`specs/023-performance-and-benchmarks.md`](../specs/023-performance-and-benchmarks.md)
owns what each benchmark measures and the methodology, and the
[request path spec](../specs/004-request-path.md) is the pipeline they
exercise.
