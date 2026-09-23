#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Prepares the runner for part 1 of docs/install.md in CI (spec 035), in
# which luxd runs as a process on the runner rather than in the kind
# cluster: the stub OpenAI provider of spec 015 started from the given
# lux-stubs binary on 127.0.0.1:9101, and the in-cluster name of the stub
# Pod, lux-stubs.lux.svc.cluster.local, added to /etc/hosts as 127.0.0.1.
# tools/docs/stubs-in-cluster.sh writes LUX_INSTALL_UPSTREAM as that
# name, so one input reaches this process from the runner in part 1 and
# the Pod from inside the cluster in part 2. This script writes no input
# of its own:
#
#	tools/docs/stubs-local.sh LUX_STUBS_BINARY
#
# The binary starts every stub; the ones part 1 does not use listen on
# ephemeral loopback ports. The process is detached and outlives the
# script, so the next step of the job reaches it, and the runner stops
# it with the job. Its output goes to lux-stubs.log in the current
# directory.
set -euo pipefail

stubs=${1:?usage: stubs-local.sh LUX_STUBS_BINARY}
[ -x "$stubs" ] || { echo "stubs-local: $stubs is not an executable" >&2; exit 1; }
name=lux-stubs.lux.svc.cluster.local
addr=127.0.0.1:9101
credential=stub-credential
log=$PWD/lux-stubs.log

nohup "$stubs" -openai-addr "$addr" -credential "$credential" > "$log" 2>&1 < /dev/null &
pid=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $credential" "http://$addr/v1/models" 2>/dev/null && break
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.2
done
curl -fsS -o /dev/null -H "Authorization: Bearer $credential" "http://$addr/v1/models" \
  || { echo "stubs-local: the stub provider does not answer at $addr" >&2; cat "$log" >&2; exit 1; }

sudo=""
[ "$(id -u)" -eq 0 ] || sudo=sudo
grep -q "[[:space:]]$name\$" /etc/hosts || echo "127.0.0.1 $name" | $sudo tee -a /etc/hosts >/dev/null
curl -fsS -o /dev/null -H "Authorization: Bearer $credential" "http://$name:9101/v1/models" \
  || { echo "stubs-local: $name does not reach the stub provider from this machine" >&2; exit 1; }
echo "stubs-local: the stub provider answers at http://$name:9101/v1 (pid $pid, log $log)"
