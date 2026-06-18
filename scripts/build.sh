#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="$ROOT/dist/cli"
GOOS_VALUE="${GOOS:-linux}"
GOARCH_VALUE="${GOARCH:-amd64}"
EXT=""

if [ "$GOOS_VALUE" = "windows" ]; then
  EXT=".exe"
fi

mkdir -p "$OUT"

echo "building eutherpunk for $GOOS_VALUE/$GOARCH_VALUE"
GOOS="$GOOS_VALUE" GOARCH="$GOARCH_VALUE" CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o "$OUT/eutherpunk-$GOOS_VALUE-$GOARCH_VALUE$EXT" \
  ./cmd/eutherpunk

echo "building eutherpunkd for $GOOS_VALUE/$GOARCH_VALUE"
GOOS="$GOOS_VALUE" GOARCH="$GOARCH_VALUE" CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o "$OUT/eutherpunkd-$GOOS_VALUE-$GOARCH_VALUE$EXT" \
  ./cmd/eutherpunkd

echo "wrote $OUT"
