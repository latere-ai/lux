<!--
SPDX-FileCopyrightText: 2026 Latere AI
SPDX-License-Identifier: Apache-2.0
-->

# luxd vs LiteLLM: a proxy-overhead comparison

A fair, reproducible latency-and-memory comparison between the Lux open-core
gateway (`luxd`) and [LiteLLM](https://github.com/BerriAI/litellm)'s proxy.
It runs entirely on loopback against a shared mock upstream, so what it
measures is the proxy's own overhead, not any model or any network.

This harness is opt-in. The load driver carries `//go:build ignore`, so it
is excluded from `go build ./...`, `go test ./...`, the coverage gate, and
CI; run it by hand with `benchmarks/compare/run.sh`.

## What it measures

Both proxies forward to the **same mock upstream**: the stub provider from
`cmd/lux-stubs` (spec 015), which returns a canned completion with no model
latency. Because provider and network cost are identical and near-zero, the
difference between a proxy and the baseline is the proxy's added overhead.

- **Baseline** — the load driver calls the mock directly. This is the floor:
  raw request/response cost with no proxy in the path.
- **luxd** — the Lux gateway, brought up exactly as `make run` does, with the
  stub provider as its upstream. Its data-plane hot path dials no authorizer
  and no issuer (spec 016, invariant 3), so the comparison is not distorted
  by an out-of-band webhook.
- **LiteLLM** — the LiteLLM proxy, pointed at the same stub provider.

**Added overhead** for a subject is its latency minus the baseline latency
for the same request shape and mode.

### Request shapes

- **Passthrough** — OpenAI in, OpenAI upstream. Isolates pure proxy
  overhead: no format conversion, the body is relayed.
- **Translated** — OpenAI in, Anthropic upstream. Both proxies accept an
  OpenAI Chat Completions request and convert it to the Anthropic Messages
  format for the upstream, then convert the answer back. This shows
  translation overhead. luxd does it through `pkg/llmdialect`; LiteLLM does
  it in its Python request/response transformers.

Each shape is run **non-streaming** and **streaming (SSE)**.

The translated baseline drives the Anthropic-format mock directly in the
Anthropic Messages shape, so translation overhead is measured against the
raw cost of the format the upstream actually speaks.

### Metrics

The Go load driver (`driver.go`, standard library only) is closed-loop: it
holds a fixed number of requests in flight (`-concurrency`) and issues a
fixed number of them (`-requests`), after a discarded warmup (`-warmup`). It
records every request's wall latency (dial to full body read, including the
whole SSE stream) and reports **p50, p75, p90, p95, p99, throughput
(req/s), and error count** per subject.

**Peak RSS** is sampled while the load runs: every 50 ms the driver reads
`ps -Ao pid=,ppid=,rss=` and sums the resident set of the proxy's process
and its descendants, keeping the maximum. This is a Darwin-friendly method
(`/proc` is Linux-only); it samples rather than instruments, so it can miss
a spike between samples, but it is applied identically to every proxied
subject. The baseline has no proxy process, so its RSS is `n/a`.

## Pinned parameters

| Parameter | Value |
|---|---|
| Concurrency | 50 in-flight requests |
| Measured requests (non-streaming) | 20000 per subject |
| Measured requests (streaming) | 10000 per subject |
| Warmup (discarded) | 2000 per subject |
| RSS sample interval | 50 ms |

Override any of them with the environment variables named at the top of
`run.sh` (`CONCURRENCY`, `REQUESTS`, `STREAM_REQUESTS`, `WARMUP`).

## Versions and environment

- **LiteLLM**: `1.100.1`, installed with `uv` into a Python 3.13 venv.
- **Go**: `go1.27.0` (the toolchain that builds `luxd` and the driver).
- **CPU class**: a 12-core Apple-silicon (arm64) laptop, 48 GB RAM,
  macOS / Darwin. Numbers are machine-relative; see the caveats below.

## How to run

```sh
benchmarks/compare/run.sh
```

`run.sh`:

1. Creates (once) a Python 3.13 venv **outside the repository** and installs
   `litellm[proxy]` with `uv`. The venv default is `$TMPDIR/lux-bench-venv`;
   point `BENCH_VENV_DIR` elsewhere if you like, but keep it out of the tree
   — the venv is large and its files are third-party. It is never committed.
2. Builds the load driver and brings up `luxd` + `lux-stubs` with `make run`.
   It applies a translated Model (OpenAI door → Anthropic upstream) and a
   high-headroom Budget and Key over the control plane, so the run exercises
   luxd's per-request limit arithmetic without the shared $10 `dev` Budget
   exhausting mid-run.
3. Starts LiteLLM against the same stubs and confirms it proxies a request.
4. Drives the three subjects across both shapes and both modes, prints the
   table, and tears everything down (`make run-down` and a LiteLLM kill run
   from an EXIT trap).

Raw per-run JSON lines land in `out/bench/results.jsonl` and the rendered
table in `out/bench/table.md` (both under the gitignored `out/`).

### LiteLLM configuration

`litellm.config.yaml` declares two models, both pointed at the stub provider
through environment variables `run.sh` exports (so the file carries no
per-checkout port):

- `stub-openai` → `openai/stub-openai` (passthrough).
- `xlate-openai-anthropic` → `anthropic/stub-anthropic` (translation).

It sets a fixed `master_key`; LiteLLM otherwise sends any other bearer to a
database it has none of. `run.sh` also sets `LITELLM_LOCAL_MODEL_COST_MAP`
so LiteLLM does not fetch its cost map over the network, and disables
telemetry. LiteLLM runs with `--num_workers 1`.

## Fairness caveats

- This is a **Go proxy versus a Python/uvicorn proxy**. Much of the gap is
  the runtime, not the design; read it as "what each stack costs today on
  this workload", not as a verdict on either project's engineering.
- It measures **proxy overhead only** — routing, auth, limit checks, header
  handling, and (for the translated shape) format conversion — against a
  zero-latency upstream. It says nothing about model quality, provider
  coverage, or feature breadth, on which the two projects differ greatly.
- LiteLLM runs with a single worker (`--num_workers 1`). It scales out with
  more workers/processes; so does luxd with more replicas. The numbers are a
  per-process picture at a fixed concurrency, not a maximum-throughput
  shoot-out.
- **Numbers are machine-relative.** Absolute latencies and req/s depend on
  the CPU, and loopback benchmarking under high concurrency has real tail
  variance (the p99 column especially). Re-run on your own hardware; trust
  the ratios more than the absolutes.
- Peak RSS is a sampled maximum, not an instrumented high-water mark.

## Files

| File | What it is |
|---|---|
| `driver.go` | the closed-loop load driver and the report renderer (`//go:build ignore`) |
| `litellm.config.yaml` | the LiteLLM proxy config (third-party schema) |
| `model-xlate-openai-anthropic.yaml` | luxd Model for the translated shape |
| `budget-bench.yaml`, `key-bench.yaml` | high-headroom Budget and Key for the run |
| `run.sh` | brings everything up, runs the matrix, prints the table, tears down |
| `RESULTS.md` | the real numbers from a run on the reference machine |
