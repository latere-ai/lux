#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Walks docs/install.md against this checkout, both parts (specs 035 and
# 017), the way the `install` suite of .lateregate.yaml runs it:
#
#	tools/docs/install-walk.sh
#
# It builds what the document installs from the tree: the developer image,
# the stub image, luxd for part 1, and the lux command. It brings up the
# kind cluster and the stubs (stubs-in-cluster.sh), starts the stub
# provider on the machine for part 1 (stubs-local.sh), and runs the fenced
# blocks of the document in order (run-blocks.sh). kind and kubectl come
# from the PATH the suite is given. The cluster, the stub process and the
# scratch directory are removed when the script ends, passed or failed, so
# a laptop run leaves nothing behind.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

# Docker builds the images where it is installed, as on a runner; podman
# does where it is not, and kind then needs to be told to use it.
if command -v docker >/dev/null 2>&1; then
  engine=docker
elif command -v podman >/dev/null 2>&1; then
  engine=podman
  export KIND_EXPERIMENTAL_PROVIDER=podman
else
  echo "install-walk: neither docker nor podman is on PATH" >&2
  exit 1
fi

scratch=$(mktemp -d)
work=$root/work
stubs_bin=$scratch/lux-stubs

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "install-walk: failed with status $status; the cluster's state and the local logs follow" >&2
    kubectl -n lux get all || true
    kubectl -n lux describe pod -l app.kubernetes.io/name=luxd || true
    kubectl -n lux logs deploy/luxd --tail=200 || true
    tail -n 200 "$work/luxd.log" "$work/lux-stubs.log" || true
  fi
  pkill -f "$stubs_bin" 2>/dev/null || true
  kind delete cluster --name lux >/dev/null 2>&1 || true
  rm -rf "$scratch" "$work"
  exit "$status"
}
trap cleanup EXIT

# The images and binaries from the checkout. The stub image carries a Linux
# binary of the architecture the engine builds for, which is this machine's
# (amd64 on a runner); the stub provider part 1 reaches runs on this
# machine, so it is built for the machine itself.
"$engine" build -t luxd:candidate .
CGO_ENABLED=0 GOOS=linux go build -trimpath -o "dist/lux-stubs_linux_$(go env GOARCH)" ./cmd/lux-stubs
"$engine" build -f Dockerfile.stubs -t lux-stubs:candidate .
make build
mkdir -p "$scratch/bin"
go build -o "$scratch/bin/lux" ./cmd/lux
CGO_ENABLED=0 go build -trimpath -o "$stubs_bin" ./cmd/lux-stubs

# The cluster and the stubs. The script hands the inputs the document
# reads to a file when GITHUB_ENV names one; here that file is ours, and
# each line is exported for the walk below.
rm -rf "$work"
mkdir -p "$work"
cd "$work"
inputs=$scratch/inputs
GITHUB_ENV=$inputs ../tools/docs/stubs-in-cluster.sh lux-stubs:candidate luxd:candidate lux-stubs:candidate
while IFS= read -r kv; do
  export "${kv?}"
done < "$inputs"

# The stub provider on this machine, for part 1.
../tools/docs/stubs-local.sh "$stubs_bin"

# The walk.
export LUX_INSTALL_BIN=$root/out/luxd
export LUX_INSTALL_IMAGE=luxd:candidate
export LUX_INSTALL_MANIFESTS=../deploy
PATH="$scratch/bin:$PATH" ../tools/docs/run-blocks.sh ../docs/install.md
