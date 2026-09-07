#!/usr/bin/env bash
# Build script for mcp-local-hub's supported non-Windows environments.
# Windows product payloads are exclusively owned by build.ps1.
# Output goes to bin/ (standard Go project layout; gitignored).
set -euo pipefail

VERSION="0.4.36"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if [ "$(go env GOOS)" = "windows" ]; then
  echo "==> Delegating the Windows product payload to build.ps1"
  exec pwsh -NoProfile -File ./build.ps1
fi

OUT_DIR="bin"
OUT_FILE="${OUT_DIR}/mcphub"

mkdir -p "${OUT_DIR}"

LDFLAGS="-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}"

echo "==> Building ${OUT_FILE} (version=${VERSION} commit=${COMMIT})"
go build -trimpath -ldflags "${LDFLAGS}" -o "${OUT_FILE}" ./cmd/mcphub

if [ ! -f "${OUT_FILE}" ]; then
  echo "ERROR: ${OUT_FILE} missing after build — check Defender exclusions (see INSTALL.md)." >&2
  exit 1
fi

echo "==> Done. Run './${OUT_FILE} version' to print build info."
ls -la "${OUT_FILE}"
