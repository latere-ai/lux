#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# The live check after a deploy. The release pipeline runs it once the
# rollout is done and the deploy credential is removed, again every 5s for
# up to 120s until it passes, and attaches its Markdown evidence to the
# release. An operator can run it by hand after an upgrade.
#
# BASE_URL is the public base of the Models API, the origin and the path
# prefix together: https://api.latere.ai/v1/models in production. The
# script has no default target.
#
# Checks, in order, stopping at the first failure:
#   1. GET $BASE_URL/.well-known/lux answers 200 with the lux descriptor,
#      so the ingress rule reaches this build.
#   2. Health: none. /livez and /readyz are served on the internal listener
#      and are not routed under the public prefix, so there is no public
#      health route to check and none is invented here.
#   3. GET $BASE_URL/version serves a version, and when TAG is set it
#      equals TAG.
#
# Unauthenticated, read-only, safe to run any number of times, no wait loop.
#
# Required tools: curl, grep.
#
# Environment:
#   BASE_URL   the public base to check; required
#   TAG        the release being proved; when set the served version must equal it
#   COMMIT     the release commit, for the evidence
#   DEPLOY_URL the deploy run, for the evidence
#   OUTPUT_MD  where to write the Markdown evidence; nowhere when unset

set -euo pipefail

BASE_URL="${BASE_URL:-}"
BASE_URL="${BASE_URL%/}"
TAG="${TAG:-}"
COMMIT="${COMMIT:-unknown}"
DEPLOY_URL="${DEPLOY_URL:-}"
OUTPUT_MD="${OUTPUT_MD:-}"

pass() { printf 'ok: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

[ -n "$BASE_URL" ] || fail "BASE_URL is required; this smoke has no default target"
for cmd in curl grep; do
  command -v "$cmd" >/dev/null || fail "$cmd is required"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

get() {
  local label="$1" path="$2" out="$3" code
  code=$(curl -sS --max-time 15 -o "$out" -w '%{http_code}' "${BASE_URL}${path}") || fail "$label: curl failed"
  [ "$code" = "200" ] || fail "$label: expected 200, got $code (body: $(head -c 200 "$out" 2>/dev/null || true))"
  pass "$label (200)"
}

get "GET /.well-known/lux" "/.well-known/lux" "$tmp/descriptor"
grep -q '"name":"lux"' "$tmp/descriptor" || fail "GET /.well-known/lux: not the lux descriptor ($(head -c 200 "$tmp/descriptor"))"

get "GET /version" "/version" "$tmp/version"
# `|| true`: grep exits 1 on no match, and the message below should be said.
served=$(grep -o '"version":"[^"]*"' "$tmp/version" | head -1 | cut -d'"' -f4 || true)
[ -n "$served" ] || fail "GET /version: no version in $(head -c 200 "$tmp/version")"
if [ -n "$TAG" ]; then
  [ "$served" = "$TAG" ] || fail "version mismatch: served $served, expected $TAG"
  pass "served version matches the tag ($served)"
else
  pass "served version recorded ($served)"
fi

if [ -n "$OUTPUT_MD" ]; then
  {
    echo "<!-- release-evidence -->"
    echo
    echo "## Release evidence"
    echo
    echo "- Tag: \`${TAG:-unknown}\`"
    echo "- Commit: \`${COMMIT}\`"
    if [ -n "$DEPLOY_URL" ]; then echo "- Deploy: ${DEPLOY_URL}"; fi
    echo "- Base URL: ${BASE_URL}"
    echo "- Served version: \`${served}\`"
    echo "- Public route: \`/.well-known/lux\` answered 200 with the lux descriptor"
    echo "- Health: no public health route; \`/livez\` and \`/readyz\` are internal"
    echo "- Version: \`/version\` served \`${served}\`${TAG:+, equal to the tag}"
  } > "$OUTPUT_MD"
fi

printf '\nrelease smoke passed\n'
