#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Prepares a kind cluster for a CI run of docs/install.md (spec 017): the
# cluster the document would create, the images it would pull loaded
# from the runner, the stubs of spec 015 as a Pod the document's first
# Provider and issuer point at, and a token minted from the stub issuer.
# It writes the LUX_INSTALL_* inputs the document reads to $GITHUB_ENV,
# or prints them as export lines when there is none:
#
#	tools/docs/stubs-in-cluster.sh STUBS_IMAGE [IMAGE...]
#
# The document creates the cluster when there is none, so a run after
# this script finds one and moves on; that is what lets one document
# serve an operator's laptop, a build from a checkout, and a release.
set -euo pipefail

stubs=${1:?usage: stubs-in-cluster.sh STUBS_IMAGE [IMAGE...]}
here=$(cd "$(dirname "$0")" && pwd)
cluster=lux
namespace=lux
issuer=http://lux-stubs.$namespace.svc.cluster.local:9106

# The document's own cluster configuration, written by the runner.
"$here/run-blocks.sh" --write "$here/../../docs/install.md" >/dev/null
if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" --config kind-lux.yaml --wait 120s
fi
kind load docker-image --name "$cluster" "$@"

kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -
sed "s|image: lux-stubs$|image: $stubs|" "$here/stubs.yaml" | kubectl apply -f -
kubectl -n "$namespace" wait --for=condition=Ready pod/lux-stubs --timeout=120s

# A token for the subject the document installs as: minted through a
# port-forward, since the issuer serves inside the cluster.
kubectl -n "$namespace" port-forward pod/lux-stubs 19106:9106 >/dev/null 2>&1 &
forward=$!
trap 'kill $forward 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null http://127.0.0.1:19106/.well-known/openid-configuration 2>/dev/null && break
  sleep 0.2
done
token=$(curl -fsS -X POST http://127.0.0.1:19106/mint -H 'Content-Type: application/json' -d '{"sub":"admin"}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$token" ] || { echo "stubs-in-cluster: the stub issuer minted no token" >&2; exit 1; }

inputs=(
  "LUX_INSTALL_ISSUER=$issuer"
  "LUX_INSTALL_TOKEN=$token"
  "LUX_INSTALL_ADMIN=$issuer|admin"
  "LUX_INSTALL_UPSTREAM=http://lux-stubs.$namespace.svc.cluster.local:9101/v1"
  "LUX_INSTALL_UPSTREAM_KEY=stub-credential"
  "LUX_INSTALL_MODEL=stub-openai"
)
for kv in "${inputs[@]}"; do
  if [ -n "${GITHUB_ENV:-}" ]; then
    echo "$kv" >> "$GITHUB_ENV"
  else
    echo "export $kv"
  fi
done
