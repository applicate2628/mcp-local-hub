#!/usr/bin/env node
// Platform-resolver shim for the `mcp-local-hub` npm meta package.
//
// The meta package ships ZERO binaries. Instead it declares one
// `@applicate2628/mcp-local-hub-<platform>-<arch>` package per supported
// target in its `optionalDependencies`. npm installs ONLY the sub-package
// whose `os`/`cpu` fields match the host (esbuild / turbo / @swc pattern), so
// a Windows host downloads only `@applicate2628/mcp-local-hub-win32-x64`,
// never the macOS or Linux binaries.
//
// This shim is the `bin` entry point. It:
//   1. maps `${process.platform}-${process.arch}` to the matching sub-package,
//   2. `require.resolve`s the platform binary inside that installed package,
//   3. `spawnSync`s it with our argv tail and inherited stdio,
//   4. propagates the child's exit code (and signal) verbatim.
//
// SECURITY: this package ships no postinstall script. The platform binary
// arrives purely through the optionalDependency npm itself installs, and it
// still resolves under `npm install --ignore-scripts` (that flag skips
// lifecycle scripts, not optional dependencies). To update the canonical
// ~/.local/bin copy after a global npm upgrade, run `mcphub setup` explicitly.

"use strict";

const { spawnSync } = require("node:child_process");
const { PACKAGE_BY_PLATFORM, binaryBasename } = require("../lib/platform-binary");

// PACKAGE_BY_PLATFORM (the `${process.platform}-${process.arch}` -> sub-package
// map) and binaryBasename live in ../lib/platform-binary.js, their single
// owner. Support tiers are
// documented in npm/README.md and each sub-package description: win32-x64 is
// GA; the rest are best-effort (the CLI runs everywhere we cross-compile;
// supervisor lifecycle is Windows-GA / Linux-beta / macOS-preview).
//
// ROSETTA CAVEAT: Node reports Apple-Silicon-native as `arm64` and
// Intel/Rosetta as `x64`. On an arm64 Mac with no native arm64 install present
// (or when Node itself runs under Rosetta and reports `process.arch === "x64"`),
// the darwin-x64 binary runs fine under Rosetta 2 translation — an ACCEPTABLE
// fallback, not an error. We do not force-prefer the arm64 build; we trust the
// package npm actually installed for this host.

const RELEASES_URL =
  "https://github.com/applicate2628/mcp-local-hub/releases";

function fail(message) {
  process.stderr.write(`mcphub: ${message}\n`);
  process.exit(1);
}

function fallbackHint() {
  return (
    `\nFallback: download the matching binary from GitHub Releases\n` +
    `  ${RELEASES_URL}\n` +
    `and place it on your PATH, then keep it current with\n` +
    `  mcphub install --upgrade\n`
  );
}

function main() {
  const key = `${process.platform}-${process.arch}`;
  const pkg = PACKAGE_BY_PLATFORM[key];

  if (!pkg) {
    const supported = Object.keys(PACKAGE_BY_PLATFORM).join(", ");
    fail(
      `unsupported platform "${key}".\n` +
        `Supported targets: ${supported}.` +
        fallbackHint(),
    );
  }

  const binName = binaryBasename(process.platform);

  // Resolve the binary inside the installed sub-package. `require.resolve`
  // throws MODULE_NOT_FOUND when the optionalDependency was not installed
  // (host matched no `os`/`cpu`, `--no-optional`, `--ignore-scripts` does NOT
  // affect this since there is no install script, or a partial/offline
  // install). Treat that as a clear, actionable error — not a stack trace.
  let binPath;
  try {
    binPath = require.resolve(`${pkg}/bin/${binName}`);
  } catch (err) {
    fail(
      `the platform package "${pkg}" is not installed (could not resolve ` +
        `${pkg}/bin/${binName}).\n` +
        `This usually means the optional dependency failed to install for ` +
        `"${key}" (offline install, "--no-optional", or a registry that has ` +
        `not yet published this platform).` +
        fallbackHint(),
    );
    return; // unreachable; fail() exits. Keeps control flow obvious.
  }

  const result = spawnSync(binPath, process.argv.slice(2), {
    stdio: "inherit",
    // Do NOT shell-interpret; pass argv through verbatim.
    shell: false,
    windowsHide: false,
  });

  if (result.error) {
    if (result.error.code === "ENOENT") {
      fail(
        `platform binary not found at ${binPath} (package "${pkg}" is ` +
          `installed but its binary is missing).` +
          fallbackHint(),
      );
    }
    fail(`failed to launch ${binPath}: ${result.error.message}`);
  }

  // Mirror the child's termination. A signal-killed child has a null status;
  // re-raise the signal so callers (and shells) observe the real cause.
  if (result.signal) {
    process.kill(process.pid, result.signal);
    return;
  }
  process.exit(result.status === null ? 1 : result.status);
}

main();
