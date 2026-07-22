#!/usr/bin/env node
// postinstall.js — canonicalize the freshly-installed platform binary into the
// canonical ~/.local/bin so a bare `mcphub` runs the version npm just
// delivered.
//
// WHY: ~/.local/bin is EARLIER than the npm global bin on the operator's PATH
// by design — the scheduler tasks (\mcp-local-hub-supervisor,
// \mcp-local-hub-liveness), the supervisor's own-child spawn, and
// `install --upgrade`'s rename-aside target all point at ~/.local/bin; npm is
// only the delivery vehicle. So `npm install -g mcp-local-hub@<newer>` used to
// leave a stale ~/.local/bin binary shadowing the fresh npm one, and nothing
// propagated the update (bug 2026-07-22, operator decision: "npm должен ставить
// в local тоже"). This hook propagates the fresh binary into ~/.local/bin at
// install time.
//
// COPY-ONLY, NO DOWNLOAD: this script performs NO network I/O. It resolves the
// platform binary npm ALREADY installed (the optionalDependency) and asks THAT
// binary to copy ITSELF into the canonical path via `mcphub canonicalize` — a
// binary-only, non-interactive entry point that does NOT edit PATH, install
// scheduled tasks, prompt/elevate, or reap/restart the running fleet. This is a
// different, far narrower risk profile than a supply-chain-vector "postinstall
// that DOWNLOADS"; see the SECURITY note in bin/cli.js.
//
// FAIL-SAFE (load-bearing): this script ALWAYS exits 0. Any failure — no
// platform package for this host, the optionalDependency not installed, the
// binary failing to launch, or `mcphub canonicalize` exiting non-zero because
// the running fleet holds the file — is reported as a one-line notice pointing
// at `mcphub setup` and then swallowed. A canonicalize failure must NEVER break
// `npm install`.
//
// --ignore-scripts: npm skips this script entirely; the ~/.local/bin binary is
// then left as-is and the operator reconciles it manually with `mcphub setup`.

"use strict";

const { spawnSync } = require("node:child_process");
const { PACKAGE_BY_PLATFORM, binaryBasename } = require("../lib/platform-binary");

const SETUP_FALLBACK = "run `mcphub setup` to update ~/.local/bin manually";

function notice(msg) {
  // One line, non-fatal, on stderr so it is visible in npm's output.
  process.stderr.write(`mcp-local-hub: ${msg}\n`);
}

function canonicalize() {
  const key = `${process.platform}-${process.arch}`;
  const pkg = PACKAGE_BY_PLATFORM[key];
  if (!pkg) {
    notice(
      `no platform binary for "${key}"; skipping ~/.local/bin canonicalize — ${SETUP_FALLBACK}.`,
    );
    return;
  }

  const binName = binaryBasename(process.platform);
  let binPath;
  try {
    binPath = require.resolve(`${pkg}/bin/${binName}`);
  } catch {
    // optionalDependency not installed (offline, --no-optional, partial or
    // unsupported install). The runtime shim surfaces the same condition.
    notice(
      `platform binary not installed for "${key}"; skipping ~/.local/bin canonicalize — ${SETUP_FALLBACK}.`,
    );
    return;
  }

  const result = spawnSync(binPath, ["canonicalize"], {
    stdio: "inherit",
    // Do NOT shell-interpret; pass argv through verbatim.
    shell: false,
    windowsHide: true,
    // A one-shot canonicalize never needs the parent console; suppressing the
    // attach also routes the windowsgui binary's output to the inherited stdio
    // instead of a detached console device.
    env: { ...process.env, MCPHUB_NO_CONSOLE_ATTACH: "1" },
  });

  if (result.error) {
    const code = result.error.code || result.error.message;
    notice(
      `could not launch \`mcphub canonicalize\` (${code}); ~/.local/bin left as-is — ${SETUP_FALLBACK}.`,
    );
    return;
  }
  if (result.signal) {
    notice(
      `\`mcphub canonicalize\` terminated by signal ${result.signal}; ~/.local/bin left as-is — ${SETUP_FALLBACK}.`,
    );
    return;
  }
  if (result.status !== 0) {
    notice(
      `\`mcphub canonicalize\` exited ${result.status}; ~/.local/bin not updated ` +
        `(the running fleet may hold the file) — stop the fleet and ${SETUP_FALLBACK}.`,
    );
    return;
  }
  // Success: the binary printed its own ✓ line via inherited stdio.
}

try {
  canonicalize();
} catch (err) {
  notice(
    `unexpected error during canonicalize (${(err && err.message) || err}); ` +
      `~/.local/bin left as-is — ${SETUP_FALLBACK}.`,
  );
}

// FAIL-SAFE hard floor: never propagate a non-zero exit to `npm install`.
// spawnSync is synchronous, so nothing is pending on the event loop and this is
// the effective process exit code.
process.exitCode = 0;
