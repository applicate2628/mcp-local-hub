#!/usr/bin/env bash
# Build script for mcp-local-hub. Git Bash / WSL / cross-platform friendly.
# See build.ps1 for the Windows-native equivalent.
# Output goes to bin/ (standard Go project layout; gitignored).
set -euo pipefail

VERSION="0.1.0"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

OUT_DIR="bin"
OUT_FILE="${OUT_DIR}/mcphub.exe"

mkdir -p "${OUT_DIR}"

echo "==> Generating Windows version resource (cmd/mcphub/resource.syso)"
go generate ./cmd/mcphub

LDFLAGS="-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}"

echo "==> Building ${OUT_FILE} (version=${VERSION} commit=${COMMIT})"
go build -trimpath -ldflags "${LDFLAGS} -H windowsgui" -o "${OUT_FILE}" ./cmd/mcphub

if [ ! -f "${OUT_FILE}" ]; then
  echo "ERROR: ${OUT_FILE} missing after build — check Defender exclusions (see INSTALL.md)." >&2
  exit 1
fi

echo "==> Done. Run './${OUT_FILE} version' to print build info."
ls -la "${OUT_FILE}"
