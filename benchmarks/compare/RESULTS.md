<!--
SPDX-FileCopyrightText: 2026 Latere AI
SPDX-License-Identifier: Apache-2.0
-->

# Results

Measured on 2026-09-15 by `benchmarks/compare/run.sh`. See `README.md` for
the methodology, the pinned parameters, and the fairness caveats. These are
real numbers from one machine; re-run on your own hardware, and trust the ratios
more than the absolutes.

## Environment

- **luxd**: this checkout, built by `make run` with `go1.27.0`.
- **LiteLLM**: `1.100.1` (`litellm[proxy]`, Python 3.13, single worker).
- **Machine**: a 12-thread Apple M4 Pro laptop (arm64), macOS / Darwin, all
  subjects on loopback, otherwise quiescent during the run.
- **Load**: closed-loop, concurrency 50; **10 independent trials** per
  condition against one steady-state bring-up. Each trial is its own
  measurement window, 4000 measured requests (non-streaming) or 2000
  (streaming) after a discarded 1000-request warmup, and each records its
  own peak RSS.
- Every measurement across all trials completed with **0 errors**.

## Method: trials, intervals, and effect size

A single run reports a point; a point cannot tell signal from noise. So the
matrix runs **N = 10** times, and every table below reports, per condition
and metric, the **median across the 10 trials** with a **95% confidence
interval** (a seeded bootstrap over the 10 per-trial values, so a re-render
is reproducible). The full per-trial data is `results.csv`; every aggregate,
including the percentiles not shown here, is `results-aggregate.csv`.

The luxd-vs-LiteLLM gap is **~45–55x** in both latency and throughput,
orders of magnitude beyond the confidence intervals, which do not overlap in
any condition. That is reported as an **effect size with non-overlapping
CIs**, not a p-value: with tens of thousands of requests behind each trial a
significance test would return a vanishingly small p that says nothing about
whether a 45x difference matters. For the small in-repo deltas (passthrough
vs translated), where a single run genuinely cannot separate the translation
cost from noise, `docs/performance.md` reports `benchstat`'s verdict instead.

Each cell is `median [95% CI]`. Peak RSS is `n/a` for the baseline, which has
no proxy process.

## Passthrough (OpenAI in, OpenAI upstream), non-streaming

| Subject | p50 (ms) | p90 (ms) | p99 (ms) | Throughput (req/s) | Peak RSS (MB) | N |
|---|---|---|---|---|---|---|
| baseline | 0.43 [0.42, 0.44] | 1.03 | 1.50 | 94706 [90287, 98610] | n/a | 10 |
| luxd | 2.59 [2.43, 2.78] | 3.75 | 4.86 | 19980 [18211, 21263] | 44.9 [44.2, 45.2] | 10 |
| litellm | 112.5 [110.8, 113.7] | 119.8 | 227.9 | 421 [415, 423] | 410.7 [409.1, 414.6] | 10 |

## Passthrough (OpenAI in, OpenAI upstream), streaming (SSE)

| Subject | p50 (ms) | p90 (ms) | p99 (ms) | Throughput (req/s) | Peak RSS (MB) | N |
|---|---|---|---|---|---|---|
| baseline | 1.08 [1.06, 1.12] | 1.83 | 2.68 | 43538 [40463, 44495] | n/a | 10 |
| luxd | 3.73 [3.61, 3.95] | 5.57 | 7.51 | 13260 [12487, 13680] | 44.9 [44.2, 45.4] | 10 |
| litellm | 193.1 [190.8, 197.1] | 288.7 | 338.5 | 240 [237, 242] | 410.5 [409.4, 414.6] | 10 |

## Translated (OpenAI in, Anthropic upstream), non-streaming

| Subject | p50 (ms) | p90 (ms) | p99 (ms) | Throughput (req/s) | Peak RSS (MB) | N |
|---|---|---|---|---|---|---|
| baseline | 0.43 [0.42, 0.44] | 0.99 | 1.38 | 98374 [92468, 100351] | n/a | 10 |
| luxd | 2.83 [2.75, 2.95] | 4.20 | 5.85 | 18080 [17445, 18816] | 44.9 [44.3, 45.4] | 10 |
| litellm | 105.7 [104.1, 107.0] | 113.6 | 219.0 | 448 [417, 456] | 410.5 [409.4, 413.0] | 10 |

## Translated (OpenAI in, Anthropic upstream), streaming (SSE)

| Subject | p50 (ms) | p90 (ms) | p99 (ms) | Throughput (req/s) | Peak RSS (MB) | N |
|---|---|---|---|---|---|---|
| baseline | 1.24 [1.22, 1.28] | 2.13 | 2.90 | 37875 [36527, 39000] | n/a | 10 |
| luxd | 4.62 [4.54, 4.77] | 7.32 | 10.04 | 10532 [10235, 10802] | 45.0 [44.5, 45.5] | 10 |
| litellm | 213.8 [210.4, 215.4] | 280.3 | 355.5 | 222 [205, 224] | 410.7 [409.6, 414.6] | 10 |

## Charts

The figures render straight from `results.csv` with `render.py`; re-run it
after a fresh `run.sh` to reproduce them.

![Per-request latency by percentile, p50 through p99, on a log y-axis, one
line per subject with a 95% CI band, faceted by shape and streaming mode](figures/latency-percentiles.png)

*Latency (median of 10 trials, 95% CI band), log y-axis. luxd sits ~2–5 ms
above the direct-to-mock baseline; LiteLLM sits ~100–350 ms above it. The
band widens at p99, LiteLLM's most of all: the tail is genuinely noisy
under a single worker at concurrency 50.*

![Single-process throughput in requests per second, bars per subject on a log
y-axis with 95% CI error bars, faceted by shape and streaming mode](figures/throughput.png)

*Throughput (median of 10 trials, 95% CI error bars), log y-axis. luxd
sustains ~10k–20k req/s in one process; LiteLLM ~220–450 req/s. The error
bars are dwarfed by the ~45–55x gap between the subjects.*

## Added overhead over baseline (p50, median)

| Shape / mode | luxd | LiteLLM |
|---|---|---|
| Passthrough, non-streaming | +2.2 ms | +112.1 ms |
| Passthrough, streaming | +2.6 ms | +192.1 ms |
| Translated, non-streaming | +2.4 ms | +105.3 ms |
| Translated, streaming | +3.4 ms | +212.6 ms |

## Reading the numbers

- On this workload luxd adds **~2–3 ms** of p50 overhead over the raw mock;
  LiteLLM adds **~105–213 ms**. luxd's translated path costs it under
  ~1 ms more p50 than its passthrough path (the `pkg/llmdialect` conversion),
  a delta the in-repo `benchstat` numbers isolate cleanly.
- Single-process throughput at concurrency 50 is **~10k–20k req/s** for luxd
  and **~220–450 req/s** for LiteLLM; luxd is ~45–55x higher here, with
  non-overlapping confidence intervals in every condition.
- Peak RSS is **~45 MB** for luxd and **~410 MB** for LiteLLM (~9x), stable
  across trials (its CI is a fraction of a percent).
- p99 tails are noisy under loopback saturation, LiteLLM's especially, since
  it runs a single worker at concurrency 50; the widening CI band at p99 in
  the latency figure is that noise made honest. Trust the medians over the
  tails, and the ratios over the absolutes.

Much of the gap is a Go binary versus a Python/uvicorn service, and this
measures proxy overhead only, not model quality, provider coverage, or
feature breadth. LiteLLM scales out with more workers, as luxd does with
more replicas.
