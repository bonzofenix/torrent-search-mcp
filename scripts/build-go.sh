#!/bin/bash
# Cross-compile the Go stdio MCP server into static release binaries:
#   dist/torrent-search-mcp-<os>-<arch>  (linux/darwin x amd64/arm64)
# Usage: scripts/build-go.sh [version]   (defaults to `git describe`)
set -euo pipefail

cd "$(dirname "$0")/.."
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="${OUT_DIR:-dist}"
mkdir -p "$OUT_DIR"

for os in linux darwin; do
  for arch in amd64 arm64; do
    out="${OUT_DIR}/torrent-search-mcp-${os}-${arch}"
    echo "==> ${out} (${VERSION})"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" -o "$out" ./cmd/torrent-search-mcp
  done
done
