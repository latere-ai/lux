#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0

# run.sh brings up the three subjects on loopback, drives the load matrix,
# prints the table, and tears everything down. See README.md for the
# methodology. Nothing here is committed output: results land under
# out/bench, which is gitignored. This harness is opt-in and is not part of
# `go test` or CI.
#
# Knobs (environment overrides):
#   CONCURRENCY       in-flight requests held constant        (default 50)
#   REQUESTS          measured non-streaming requests/subject (default 20000)
#   STREAM_REQUESTS   measured streaming requests/subject     (default 10000)
#   WARMUP            discarded warmup requests/subject       (default 2000)
#   LITELLM_PORT      port for the LiteLLM proxy              (default 8123)
#   BENCH_VENV_DIR    venv location, kept OUTSIDE the repo    (default $TMPDIR/lux-bench-venv)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$REPO"

CONCURRENCY="${CONCURRENCY:-50}"
REQUESTS="${REQUESTS:-20000}"
STREAM_REQUESTS="${STREAM_REQUESTS:-10000}"
WARMUP="${WARMUP:-2000}"
LITELLM_PORT="${LITELLM_PORT:-8123}"
LITELLM_MASTER_KEY="sk-lux-bench"   # must match litellm.config.yaml
VENV_DIR="${BENCH_VENV_DIR:-${TMPDIR:-/tmp}/lux-bench-venv}"
VENV_DIR="${VENV_DIR%/}"

OUTDIR="$REPO/out/bench"
RESULTS="$OUTDIR/results.jsonl"
REPORT="$OUTDIR/table.md"
DRIVER="$OUTDIR/driver"
mkdir -p "$OUTDIR"
: > "$RESULTS"

LITELLM_PID=""
cleanup() {
  set +e
  if [ -n "$LITELLM_PID" ]; then kill "$LITELLM_PID" 2>/dev/null; fi
  pkill -f 'litellm --config' 2>/dev/null
  make -C "$REPO" run-down >/dev/null 2>&1
}
trap cleanup EXIT

# --- LiteLLM venv (outside the repo; installed once, reused thereafter) ----
if [ ! -x "$VENV_DIR/bin/litellm" ]; then
  echo ">> creating venv at $VENV_DIR and installing litellm[proxy] (once)"
  uv venv --python 3.13 "$VENV_DIR"
  VIRTUAL_ENV="$VENV_DIR" uv pip install --python "$VENV_DIR/bin/python" 'litellm[proxy]'
fi
LITELLM_VERSION="$("$VENV_DIR/bin/python" -c "from importlib.metadata import version; print(version('litellm'))")"
GO_VERSION="$(go env GOVERSION)"
echo ">> litellm $LITELLM_VERSION, $GO_VERSION"

# --- build the load driver once (it is //go:build ignore, named explicitly) -
go build -o "$DRIVER" "$SCRIPT_DIR/driver.go"

# --- luxd + lux-stubs via make run (retry once past the known mint race) ----
echo ">> make run (luxd + lux-stubs on loopback)"
RUNOUT="$OUTDIR/makerun.out"
if ! make run > "$RUNOUT" 2> "$OUTDIR/makerun.err"; then
  echo ">> make run failed once; retrying after run-down"
  make run-down >/dev/null 2>&1 || true
  make run > "$RUNOUT" 2> "$OUTDIR/makerun.err"
fi
LUX_URL="$(sed -n 's/^export LUX_URL=//p' "$RUNOUT")"
LUX_TOKEN="$(sed -n 's/^export LUX_TOKEN=//p' "$RUNOUT")"
LUX_KEY="$(sed -n 's/^export LUX_KEY=//p' "$RUNOUT")"
LUXD_PID="$(cat "$REPO/out/run/luxd.pid")"
OPENAI_ADDR="$(sed -n 's|^lux-stubs: openai http://||p' "$REPO/out/run/stubs.log")"
ANTHROPIC_ADDR="$(sed -n 's|^lux-stubs: anthropic http://||p' "$REPO/out/run/stubs.log")"
if [ -z "$LUX_URL" ] || [ -z "$LUX_KEY" ] || [ -z "$OPENAI_ADDR" ] || [ -z "$ANTHROPIC_ADDR" ]; then
  echo "!! could not parse luxd/stub coordinates; see $RUNOUT and out/run/*.log" >&2
  exit 1
fi
echo ">> LUX_URL=$LUX_URL openai_stub=$OPENAI_ADDR anthropic_stub=$ANTHROPIC_ADDR luxd_pid=$LUXD_PID"

# --- add the translated model (OpenAI door -> Anthropic upstream) to luxd ---
curl -fsS -o /dev/null -X PUT "$LUX_URL/v1/models/xlate-openai-anthropic" \
  -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' \
  --data-binary @"$SCRIPT_DIR/model-xlate-openai-anthropic.yaml"

# --- a high-headroom Budget and Key so the run never trips the shared $10
#     dev Budget; the limit arithmetic still runs per request. -------------
curl -fsS -o /dev/null -X PUT "$LUX_URL/v1/budgets/bench" \
  -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' \
  --data-binary @"$SCRIPT_DIR/budget-bench.yaml"
KEY_JSON="$(curl -fsS -X PUT "$LUX_URL/v1/keys/bench" \
  -H "Authorization: Bearer $LUX_TOKEN" -H 'Content-Type: application/yaml' \
  --data-binary @"$SCRIPT_DIR/key-bench.yaml")"
BENCH_KEY="$(printf '%s' "$KEY_JSON" | sed -n 's/.*"value":"\([^"]*\)".*/\1/p')"
if [ -z "$BENCH_KEY" ]; then
  echo "!! the bench Key value was not returned: $KEY_JSON" >&2
  exit 1
fi
LUX_KEY="$BENCH_KEY"

# --- LiteLLM proxy, pointed at the same stubs -----------------------------
echo ">> starting litellm on :$LITELLM_PORT"
export LITELLM_OPENAI_API_BASE="http://$OPENAI_ADDR/v1"
export LITELLM_ANTHROPIC_API_BASE="http://$ANTHROPIC_ADDR"
export LITELLM_STUB_CREDENTIAL="stub-credential"
export LITELLM_LOCAL_MODEL_COST_MAP="True"    # no cost-map fetch from the network
export LITELLM_DONT_SHOW_FEEDBACK_BOX="1"
"$VENV_DIR/bin/litellm" --config "$SCRIPT_DIR/litellm.config.yaml" \
  --port "$LITELLM_PORT" --num_workers 1 > "$OUTDIR/litellm.log" 2>&1 &
LITELLM_PID=$!
i=0
until curl -fsS -o /dev/null "http://127.0.0.1:$LITELLM_PORT/health/liveliness" 2>/dev/null; do
  i=$((i+1))
  if [ "$i" -ge 120 ]; then echo "!! litellm did not come up; see $OUTDIR/litellm.log" >&2; exit 1; fi
  sleep 0.5
done

# endpoints
BASE_OPENAI_URL="http://$OPENAI_ADDR/v1/chat/completions"
BASE_ANTHROPIC_URL="http://$ANTHROPIC_ADDR/v1/messages"
LUXD_URL="$LUX_URL/openai/v1/chat/completions"
LITELLM_URL="http://127.0.0.1:$LITELLM_PORT/chat/completions"

# drun wraps the driver with the pinned concurrency, warmup, and results file.
# The driver sends one confirm request first and fails loudly if a subject
# does not answer, so a broken subject never silently produces a number.
drun() {
  if ! "$DRIVER" run "$@" -concurrency "$CONCURRENCY" -warmup "$WARMUP" -out "$RESULTS"; then
    echo "!! a subject returned nonzero (errors, or an unreachable endpoint); continuing: $*" >&2
  fi
}

echo ">> passthrough (OpenAI in, OpenAI upstream)"
for stream in "" "-stream"; do
  if [ -z "$stream" ]; then n="$REQUESTS"; else n="$STREAM_REQUESTS"; fi
  # shellcheck disable=SC2086
  drun -subject baseline -group passthrough -shape openai $stream -requests "$n" -url "$BASE_OPENAI_URL" -key stub-credential      -model stub-openai -rss-pid 0
  # shellcheck disable=SC2086
  drun -subject luxd     -group passthrough -shape openai $stream -requests "$n" -url "$LUXD_URL"        -key "$LUX_KEY"           -model stub-openai -rss-pid "$LUXD_PID"
  # shellcheck disable=SC2086
  drun -subject litellm  -group passthrough -shape openai $stream -requests "$n" -url "$LITELLM_URL"     -key "$LITELLM_MASTER_KEY" -model stub-openai -rss-pid "$LITELLM_PID"
done

echo ">> translated (OpenAI in, Anthropic upstream)"
for stream in "" "-stream"; do
  if [ -z "$stream" ]; then n="$REQUESTS"; else n="$STREAM_REQUESTS"; fi
  # shellcheck disable=SC2086
  drun -subject baseline -group translated -shape anthropic $stream -requests "$n" -url "$BASE_ANTHROPIC_URL" -key stub-credential      -model stub-anthropic         -rss-pid 0
  # shellcheck disable=SC2086
  drun -subject luxd     -group translated -shape openai    $stream -requests "$n" -url "$LUXD_URL"           -key "$LUX_KEY"           -model xlate-openai-anthropic -rss-pid "$LUXD_PID"
  # shellcheck disable=SC2086
  drun -subject litellm  -group translated -shape openai    $stream -requests "$n" -url "$LITELLM_URL"        -key "$LITELLM_MASTER_KEY" -model xlate-openai-anthropic -rss-pid "$LITELLM_PID"
done

echo
echo "=== results (litellm $LITELLM_VERSION, $GO_VERSION, concurrency $CONCURRENCY) ==="
"$DRIVER" report -in "$RESULTS" -md "$REPORT"
cat "$REPORT"
echo ">> raw samples: $RESULTS"
echo ">> markdown table: $REPORT"
