#!/usr/bin/env sh
set -eu

ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
OUT="$ROOT/dist/cli"
GOOS_VALUE="${GOOS:-linux}"
GOARCH_VALUE="${GOARCH:-amd64}"
EXT=""
CLI_ONLY="${EUTHERPUNK_CLI_ONLY:-0}"
VERSION="${EUTHERPUNK_VERSION:-$(git -C "$ROOT" describe --always --dirty 2>/dev/null || printf dev)}"
DEFAULT_URL="${EUTHERPUNK_DEFAULT_URL:-https://apothictech.se}"
DEFAULT_MODEL="${EUTHERPUNK_DEFAULT_MODEL:-supergemma4-26b-free:latest}"

if [ "$GOOS_VALUE" = "windows" ]; then
  EXT=".exe"
fi

mkdir -p "$OUT"

echo "building eutherpunk for $GOOS_VALUE/$GOARCH_VALUE"
CLI_LDFLAGS="-s -w -X main.version=$VERSION"
if [ -n "$DEFAULT_URL" ]; then
  CLI_LDFLAGS="$CLI_LDFLAGS -X main.defaultAPIURL=$DEFAULT_URL"
fi
if [ -n "$DEFAULT_MODEL" ]; then
  CLI_LDFLAGS="$CLI_LDFLAGS -X main.defaultModel=$DEFAULT_MODEL"
fi
GOOS="$GOOS_VALUE" GOARCH="$GOARCH_VALUE" CGO_ENABLED=0 \
  go build -trimpath -ldflags="$CLI_LDFLAGS" \
  -o "$OUT/eutherpunk-$GOOS_VALUE-$GOARCH_VALUE$EXT" \
  ./cmd/eutherpunk

if [ "$CLI_ONLY" = "1" ]; then
  echo "wrote $OUT/eutherpunk-$GOOS_VALUE-$GOARCH_VALUE$EXT"
  exit 0
fi

echo "building eutherpunkd for $GOOS_VALUE/$GOARCH_VALUE"
GOOS="$GOOS_VALUE" GOARCH="$GOARCH_VALUE" CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o "$OUT/eutherpunkd-$GOOS_VALUE-$GOARCH_VALUE$EXT" \
  ./cmd/eutherpunkd

echo "wrote $OUT"
