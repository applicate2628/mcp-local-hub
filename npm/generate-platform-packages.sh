#!/usr/bin/env bash
# Thin wrapper: regenerate the six mcphub-<platform>-<arch> sub-packages.
#
# The actual generation logic lives in generate-platform-packages.js (the
# single source of truth for the GOOS/GOARCH -> Node os/cpu map). This wrapper
# exists so a POSIX shell / CI step can run the generator without remembering
# the node invocation. Node 18+ required.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec node "${SCRIPT_DIR}/generate-platform-packages.js" "$@"
