#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Holds the two release images to the binaries build.sh produced (spec
# 017): luxd is copied out of every platform of the gateway image and
# lux-stubs out of every platform of the stub image, and
# `go run ./tools/release images` compares each with the built binary and
# the archives that carry it, so an image carries the bytes the archive
# does. The release workflow runs it once both images are pushed by digest
# and before either is tagged:
#
#	tools/release/images.sh DIST GATEWAY_IMAGE STUBS_IMAGE
#
# Each image is the <ref>@<digest> of a multi-arch index. Creating a
# container from the index with --platform stores each platform under the
# index digest and refuses the second ("cannot overwrite digest"), so each
# platform's own manifest digest is resolved from the index and the
# container is created from that.
set -euo pipefail

usage="usage: images.sh DIST GATEWAY_IMAGE STUBS_IMAGE"
dist=${1:?$usage}
gateway=${2:?$usage}
stubs=${3:?$usage}

extracted=$(mktemp -d)
trap 'rm -rf "$extracted"' EXIT

# platform prints the manifest digest of linux/<arch> in the index $1.
platform() {
  docker buildx imagetools inspect --raw "$1" | jq -r --arg a "$2" \
    '.manifests[] | select(.platform.os == "linux" and .platform.architecture == $a) | .digest'
}

# extract copies /usr/local/bin/<binary> out of linux/<arch> of the image
# <ref>@<digest> into the extracted directory as <binary>_linux_<arch>.
extract() {
  local image=$1 binary=$2 arch=$3 digest id
  digest=$(platform "$image" "$arch")
  if [ -z "$digest" ]; then
    echo "images: $image has no linux/$arch manifest" >&2
    exit 1
  fi
  id=$(docker create "${image%@*}@${digest}")
  docker cp "$id:/usr/local/bin/$binary" "$extracted/${binary}_linux_${arch}"
  docker rm "$id" >/dev/null
}

for arch in amd64 arm64; do
  extract "$gateway" luxd "$arch"
  extract "$stubs" lux-stubs "$arch"
done
go run ./tools/release images -dist "$dist" -extracted "$extracted"
