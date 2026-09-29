#!/usr/bin/env node
// Platform-resolver shim for the `mcp-local-hub` npm meta package.
//
// The meta package ships ZERO binaries. Instead it declares one
// `@applicate2628/mcp-local-hub-<platform>-<arch>` package per supported
// target in its `optionalDependencies`. npm installs ONLY the sub-package
// whose `os`/`cpu` fields match the host (esbuild / turbo / @swc pattern), so
// a Windows x64 host downloads only `@applicate2628/mcp-local-hub-win32-x64`,
// never the macOS or Linux binaries.
//
// This shim is the `bin` entry point. It:
//   1. maps `${process.platform}-${process.arch}` to the matching sub-package,
//   2. `require.resolve`s the platform binary inside that installed package,
//   3. `spawnSync`s it with our argv tail and inherited stdio,
//   4. propagates the child's exit code (and signal) verbatim.
//
// SECURITY: there is deliberately NO postinstall DOWNLOAD step — a postinstall
// that FETCHES a binary over the network is the top npm supply-chain attack
// vector. The platform binary arrives purely through the optionalDependency npm
// itself installed, and it still resolves under `npm install --ignore-scripts`
// (that flag skips lifecycle scripts, not optional dependencies).
//
// The package DOES ship one lifecycle script, scripts/postinstall.js, but it
// performs NO network I/O: it only COPIES the already-installed local platform
// binary into the canonical ~/.local/bin (via `mcphub canonicalize`) so a bare
// `mcphub` runs the version npm just delivered. A copy-only step is a
// different, far narrower risk profile than a downloading postinstall, and it
// is fully fail-safe — a copy failure never breaks `npm install`. Under
// `--ignore-scripts` it simply does not run, and the operator reconciles
// ~/.local with `mcphub setup`.

"use strict";

const { spawnSync } = require("node:child_process");
const { PACKAGE_BY_PLATFORM, binaryBasename } = require("../lib/platform-binary");

// PACKAGE_BY_PLATFORM and binaryBasename live in ../lib/platform-binary.js,
// shared with postinstall. The map is derived from the shipped meta manifest:
// a deferred host is rejected before looking for any older installed package.

const RELEASES_URL =
  "https://github.com/applicate2628/mcp-local-hub/releases";

function fail(message) {
  process.stderr.write(`mcphub: ${message}\n`);
  process.exit(1);
}

function fallbackHint() {
  return (
    `\nCheck available pinned package versions or release assets for this host:\n` +
    `  ${RELEASES_URL}\n`
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
    windowsHide: true,
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
