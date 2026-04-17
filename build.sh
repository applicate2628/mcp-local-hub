#!/usr/bin/env bash
# Build script for mcp-local-hub. Git Bash / WSL / cross-platform friendly.
# See build.ps1 for the Windows-native equivalent.
set -euo pipefail

VERSION="0.1.0"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "==> Generating Windows version resource (cmd/mcp/resource.syso)"
go generate ./cmd/mcp

LDFLAGS="-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}"

echo "==> Building mcp.exe (version=${VERSION} commit=${COMMIT})"
go build -trimpath -ldflags "${LDFLAGS} -H windowsgui" -o mcp.exe ./cmd/mcp

if [ ! -f mcp.exe ]; then
  echo "ERROR: mcp.exe missing after build — check Defender exclusions (see INSTALL.md)." >&2
  exit 1
fi

echo "==> Done. Run './mcp.exe version' to print build info."
ls -la mcp.exe
