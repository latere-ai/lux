<!--
SPDX-FileCopyrightText: 2026 Latere AI
SPDX-License-Identifier: Apache-2.0
-->

# Results

Measured on 2026-09-15 by `benchmarks/compare/run.sh`. See `README.md` for
the methodology, the pinned parameters, and the fairness caveats. These are
the actual numbers from one run on the reference machine; re-run on your own
hardware — trust the ratios more than the absolutes.

## Environment

- **luxd**: this checkout, built by `make run` with `go1.27.0`.
- **LiteLLM**: `1.100.1` (`litellm[proxy]`, Python 3.13, single worker).
- **Machine**: a 12-core Apple-silicon (arm64) laptop, 48 GB RAM,
  macOS / Darwin, all subjects on loopback.
- **Load**: closed-loop, concurrency 50; 20000 measured requests
  (non-streaming) / 10000 (streaming) per subject, after 2000 warmup.
- Every measurement completed with **0 errors**.

## Passthrough (OpenAI in, OpenAI upstream), non-streaming

| Subject | p50 (ms) | p75 (ms) | p90 (ms) | p95 (ms) | p99 (ms) | Throughput (req/s) | Errors | Peak RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| baseline | 0.420 | 0.889 | 1.317 | 1.636 | 2.375 | 81252 | 0 | n/a |
| luxd | 2.420 | 3.315 | 4.204 | 4.981 | 7.094 | 19458 | 0 | 41.6 |
| litellm | 115.227 | 121.112 | 134.884 | 181.876 | 233.204 | 408 | 0 | 334.3 |

## Passthrough (OpenAI in, OpenAI upstream), streaming (SSE)

| Subject | p50 (ms) | p75 (ms) | p90 (ms) | p95 (ms) | p99 (ms) | Throughput (req/s) | Errors | Peak RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| baseline | 1.046 | 1.384 | 1.915 | 2.257 | 3.286 | 43760 | 0 | n/a |
| luxd | 3.953 | 4.994 | 6.077 | 7.028 | 11.431 | 12314 | 0 | 40.9 |
| litellm | 195.606 | 213.716 | 313.030 | 335.670 | 705.811 | 223 | 0 | 328.4 |

## Translated (OpenAI in, Anthropic upstream), non-streaming

| Subject | p50 (ms) | p75 (ms) | p90 (ms) | p95 (ms) | p99 (ms) | Throughput (req/s) | Errors | Peak RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| baseline | 0.414 | 0.643 | 1.131 | 1.328 | 1.677 | 93682 | 0 | n/a |
| luxd | 3.186 | 4.004 | 4.808 | 5.409 | 6.914 | 15921 | 0 | 41.3 |
| litellm | 109.602 | 114.641 | 125.141 | 174.905 | 235.920 | 420 | 0 | 328.7 |

## Translated (OpenAI in, Anthropic upstream), streaming (SSE)

| Subject | p50 (ms) | p75 (ms) | p90 (ms) | p95 (ms) | p99 (ms) | Throughput (req/s) | Errors | Peak RSS (MB) |
|---|---|---|---|---|---|---|---|---|
| baseline | 1.214 | 1.608 | 2.111 | 2.532 | 4.161 | 38092 | 0 | n/a |
| luxd | 4.765 | 6.145 | 7.458 | 8.409 | 10.930 | 10321 | 0 | 42.4 |
| litellm | 227.060 | 239.551 | 340.924 | 371.875 | 779.331 | 195 | 0 | 341.1 |

## Added overhead over baseline (p50)

| Shape / mode | luxd | LiteLLM |
|---|---|---|
| Passthrough, non-streaming | +2.0 ms | +114.8 ms |
| Passthrough, streaming | +2.9 ms | +194.6 ms |
| Translated, non-streaming | +2.8 ms | +109.2 ms |
| Translated, streaming | +3.6 ms | +225.8 ms |

## Reading the numbers

- On this workload luxd adds **~2–4 ms** of p50 overhead over the raw mock;
  LiteLLM adds **~110–230 ms**. luxd's translated path costs it roughly
  1 ms more p50 than its passthrough path (the `pkg/llmdialect` conversion).
- Single-process throughput at concurrency 50 is **~10k–19k req/s** for luxd
  and **~200–420 req/s** for LiteLLM — luxd is ~40–55x higher here.
- Peak RSS is **~41–42 MB** for luxd and **~328–341 MB** for LiteLLM (~8x).
- p99 tails are noisy under loopback saturation, LiteLLM's especially, since
  it runs a single worker at concurrency 50. Treat the tails as indicative.

Much of the gap is a Go binary versus a Python/uvicorn service, and this
measures proxy overhead only — not model quality, provider coverage, or
feature breadth. LiteLLM scales out with more workers, as luxd does with
more replicas.
