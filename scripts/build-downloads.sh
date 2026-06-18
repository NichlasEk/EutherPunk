#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

build_target() {
  GOOS="$1" GOARCH="$2" "$ROOT/scripts/build.sh"
}

build_target linux amd64
build_target linux arm64
build_target windows amd64
build_target darwin amd64
build_target darwin arm64
