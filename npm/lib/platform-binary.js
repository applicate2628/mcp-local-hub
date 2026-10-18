"use strict";
// Runtime projection of the shipped meta package's optional dependencies.
// bin/cli.js consumes this lookup so it cannot advertise an unshipped release
// target. Node built-ins and the adjacent shipped manifest only; no installed
// dependency is loaded. The no-op lifecycle script does not use this module.
//
// The generator's TARGETS registry checks the meta optionalDependencies set.

const PACKAGE_PREFIX = "@applicate2628/mcp-local-hub-";
const optionalDependencies = require("../package.json").optionalDependencies || {};
// Keyed by `${process.platform}-${process.arch}` (Node's own identifiers).
const PACKAGE_BY_PLATFORM = Object.fromEntries(
  Object.keys(optionalDependencies).map((name) => {
    const key = name.startsWith(PACKAGE_PREFIX) ? name.slice(PACKAGE_PREFIX.length) : "";
    if (!/^[a-z0-9]+-[a-z0-9]+$/.test(key)) {
      throw new Error(`unsupported platform package name in meta manifest: ${name}`);
    }
    return [key, name];
  }),
);

// The binary basename inside each sub-package. Windows targets carry the .exe
// suffix; POSIX targets do not. Matches the `bin`/`files` entry the generator
// writes into each sub-package's package.json.
function binaryBasename(platform) {
  return platform === "win32" ? "mcphub.exe" : "mcphub";
}

// Windows candidates carry the repository-admitted identity beside the sole
// executable. POSIX packages have no such sidecar and preserve their existing
// one-binary payload.
function identityManifestBasename(platform) {
  return platform === "win32" ? "windows-product-identity.json" : null;
}

module.exports = { PACKAGE_BY_PLATFORM, binaryBasename, identityManifestBasename };
