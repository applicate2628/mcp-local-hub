#!/usr/bin/env bash
# Thin wrapper around sync-version.js (the actual logic + the single version
# authority lives there). Lets a POSIX shell / CI step invoke version-sync
# without remembering the node call. Node 18+ required.
#
# Usage:
#   npm/sync-version.sh --check
#   npm/sync-version.sh --inject
#   npm/sync-version.sh --assert-binary <path-to-mcphub[.exe]>
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec node "${SCRIPT_DIR}/sync-version.js" "$@"
