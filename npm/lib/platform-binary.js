"use strict";
// Single source of truth mapping the host to its platform sub-package and the
// binary basename inside it. Shared by:
//   * bin/cli.js           — the runtime platform-resolver shim, and
//   * scripts/postinstall.js — the ~/.local/bin canonicalize hook,
// so the PACKAGE_BY_PLATFORM map + basename rule have exactly ONE owner instead
// of drifting copies. Node built-ins only; no runtime dependencies — requiring
// this file can never pull in a package that failed to install.
//
// A new platform target is added in three coordinated places: the TARGETS
// array in generate-platform-packages.js, the meta package.json
// optionalDependencies, and the map below.

// Keyed by `${process.platform}-${process.arch}` (Node's own identifiers).
const PACKAGE_BY_PLATFORM = {
  "win32-x64": "@applicate2628/mcp-local-hub-win32-x64",
  "win32-arm64": "@applicate2628/mcp-local-hub-win32-arm64",
  "darwin-x64": "@applicate2628/mcp-local-hub-darwin-x64",
  "darwin-arm64": "@applicate2628/mcp-local-hub-darwin-arm64",
  "linux-x64": "@applicate2628/mcp-local-hub-linux-x64",
  "linux-arm64": "@applicate2628/mcp-local-hub-linux-arm64",
};

// The binary basename inside each sub-package. Windows targets carry the .exe
// suffix; POSIX targets do not. Matches the `bin`/`files` entry the generator
// writes into each sub-package's package.json.
function binaryBasename(platform) {
  return platform === "win32" ? "mcphub.exe" : "mcphub";
}

module.exports = { PACKAGE_BY_PLATFORM, binaryBasename };
