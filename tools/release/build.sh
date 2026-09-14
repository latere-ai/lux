#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: Apache-2.0
#
# Builds what a release carries from one checkout (spec 017): luxd and
# lux for the four os/arch pairs the project tests on, with the -ldflags
# of spec 002 setting internal/version, and the eight binary archives;
# lux-stubs for Linux alone, since it ships in its image and in no
# archive. The two Linux luxd binaries are also left unarchived under
# dist/, the very bytes the archives carry, for Dockerfile.release to
# copy, so the image and the archive are one compilation:
#
#	tools/release/build.sh v0.1.0 [DIST]
#
# Every build is CGO_ENABLED=0 and -trimpath, so the bytes depend on the
# source and the toolchain and on nothing else.
set -euo pipefail

tag=${1:?usage: build.sh TAG [DIST]}
dist=${2:-dist}
module=$(go list -m)
commit=$(git rev-parse --short HEAD 2>/dev/null || echo none)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
ldflags="-X $module/internal/version.Version=$tag -X $module/internal/version.Commit=$commit -X $module/internal/version.Date=$date"

mkdir -p "$dist"
build() {
  local name=$1 os=$2 arch=$3 out=$4
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out" "./cmd/$name"
}

for os in linux darwin; do
  for arch in amd64 arm64; do
    for name in luxd lux; do
      build "$name" "$os" "$arch" "$dist/$name"
      if [ "$os" = linux ] && [ "$name" = luxd ]; then
        cp "$dist/luxd" "$dist/luxd_linux_$arch"
      fi
      tar -C "$dist" -czf "$dist/${name}_${tag}_${os}_${arch}.tar.gz" "$name"
      rm "$dist/$name"
      echo "build: $dist/${name}_${tag}_${os}_${arch}.tar.gz"
    done
  done
done
for arch in amd64 arm64; do
  build lux-stubs linux "$arch" "$dist/lux-stubs_linux_$arch"
  echo "build: $dist/lux-stubs_linux_$arch"
done
