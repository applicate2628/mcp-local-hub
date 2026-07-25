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
// GLOBAL-INSTALL-ONLY GUARD (load-bearing): this hook does real work ONLY when
// npm is running a GLOBAL install (`npm install -g` / `npm i -g`). A local
// install of this package into a project's own node_modules, or a TRANSITIVE
// install where mcp-local-hub is merely a dependency of some other package,
// must never mutate the developer's canonical ~/.local/bin PATH binary — that
// would be a surprising supply-chain-adjacent side effect on a host that never
// asked for mcphub at all (PR #585 / bot review).
//
// Detection is TWO gates, both required (see `globalInstallSkipReason` below
// for the per-gate citations and the empirical probes behind them):
//   1. npm reports a global-install transaction — read from BOTH
//      `npm_config_global` and `npm_config_location`, because npm surfaces the
//      same intent under different keys for `-g` vs `--location=global`.
//   2. THIS package is that install's top-level target — its own directory
//      sits directly in the global node_modules root, not nested inside
//      another package. Gate 1 alone is insufficient: npm sets
//      npm_config_global=true for the lifecycle scripts of TRANSITIVE
//      dependencies of a `-g` install too, so a package that merely depends on
//      mcp-local-hub would otherwise silently replace the operator's PATH
//      binary (bot PR #586).
// Every unverifiable state FAILS CLOSED (skip + notice).
//
// Comparing `npm_config_prefix` against `npm_config_global_prefix` was
// considered and REJECTED as a detection signal — a probe showed both env vars
// hold the SAME value (the global prefix) during a LOCAL install too, so that
// comparison cannot tell the two cases apart. Gate 2 compares the PACKAGE'S
// OWN location against the global root instead, which does separate them.
//
// COPY-ONLY, NO DOWNLOAD: this script performs NO network I/O. It resolves the
// platform binary npm ALREADY installed (the optionalDependency) and asks THAT
// binary to copy ITSELF into the canonical path via `mcphub canonicalize` — a
// binary-only, non-interactive entry point that does NOT edit PATH, install
// scheduled tasks, prompt/elevate, or reap/restart the running fleet. This is a
// different, far narrower risk profile than a supply-chain-vector "postinstall
// that DOWNLOADS"; see the SECURITY note in bin/cli.js.
//
// FAIL-SAFE (load-bearing): this script ALWAYS exits 0. Any failure — not a
// global install, no platform package for this host, the optionalDependency
// not installed, the binary failing to launch, or `mcphub canonicalize`
// exiting non-zero because the running fleet holds the file — is reported as
// a one-line notice and then swallowed (the global-install skip notice does
// not point at `mcphub setup`; every other branch does). A canonicalize
// failure must NEVER break `npm install`.
//
// --ignore-scripts: npm skips this script entirely; the ~/.local/bin binary is
// then left as-is and the operator reconciles it manually with `mcphub setup`.

"use strict";

const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");
const { PACKAGE_BY_PLATFORM, binaryBasename } = require("../lib/platform-binary");

const SETUP_FALLBACK = "run `mcphub setup` to update ~/.local/bin manually";

function notice(msg) {
  // One line, non-fatal, on stderr so it is visible in npm's output.
  process.stderr.write(`mcp-local-hub: ${msg}\n`);
}

// GATE 1 — is npm running a global-install transaction at all?
//
// npm mirrors its resolved config into npm_config_<key> for every lifecycle
// script, but the "install globally" intent arrives under TWO DIFFERENT keys
// depending on how the operator spelled it. Probed on npm 11.17.0 (this
// repo's toolchain host, 2026-07-26) by dumping the env from a lifecycle
// script:
//     npm ... -g / --global      -> npm_config_global="true", location unset
//     npm ... --location=global  -> npm_config_global unset,  location="global"
// `--location=global` is a documented global-install mode
// (https://docs.npmjs.com/cli/v11/using-npm/config#location), so reading only
// npm_config_global silently takes the non-global branch for it and leaves the
// ~/.local/bin binary stale — defeating this hook for a supported invocation.
// Boolean forms are normalized rather than compared to the literal "true".
function npmReportsGlobalTransaction() {
  const flag = String(process.env.npm_config_global ?? "").trim().toLowerCase();
  if (flag === "true" || flag === "1") return true;
  return String(process.env.npm_config_location ?? "").trim().toLowerCase() === "global";
}

// Global root layout per npm's folders documentation: <prefix>/node_modules on
// Windows, <prefix>/lib/node_modules everywhere else. Verified on the
// toolchain host: npm_config_prefix="C:\...\nodejs" and `npm root -g` reports
// "C:\...\nodejs\node_modules".
function globalNodeModulesRoot() {
  const prefix = process.env.npm_config_prefix;
  if (!prefix) return null;
  return process.platform === "win32"
    ? path.join(prefix, "node_modules")
    : path.join(prefix, "lib", "node_modules");
}

function samePath(a, b) {
  const left = path.resolve(a);
  const right = path.resolve(b);
  // Windows paths are case-insensitive; realpath does not normalize case.
  return process.platform === "win32"
    ? left.toLowerCase() === right.toLowerCase()
    : left === right;
}

// GATE 2 — is THIS package the top-level target of that global install?
//
// Gate 1 is necessary but NOT sufficient. When some OTHER package depends on
// mcp-local-hub and that parent is installed with `npm i -g`, npm runs the
// lifecycle scripts of its transitive dependencies with the SAME
// npm_config_global=true. Gate 1 alone would therefore let a host that never
// asked for mcphub have its canonical PATH binary replaced — precisely the
// transitive case this guard exists to prevent (bot PR #586).
//
// The two cases are distinguishable by LOCATION, verified on the toolchain
// host: a top-level global install lands directly at <globalRoot>/<name>
// ("C:\...\node_modules\mcp-local-hub"), while a nested dependency lands one
// level deeper, under its parent's own node_modules
// ("...\node_modules\mcp-local-hub\node_modules\@applicate2628\...").
// A lifecycle script's cwd is the package's own directory, so comparing that
// against the derived global root separates them.
//
// This is NOT the rejected npm_config_prefix-vs-npm_config_global_prefix probe
// described in the file header: that compared two PREFIXES (identical during a
// local install, hence useless). This compares the PACKAGE'S OWN LOCATION
// against the global root, which differs in exactly the cases we must tell
// apart. It also backstops gate 1: an operator with `location=global` in their
// .npmrc running an ordinary local install lands in a project node_modules and
// is correctly skipped here.
//
// Returns null when this IS a top-level global install of this package, or a
// human-readable skip reason otherwise. Unverifiable states FAIL CLOSED
// (skip): leaving ~/.local/bin stale is recoverable with `mcphub setup`,
// whereas clobbering a PATH binary nobody asked us to touch is not.
function globalInstallSkipReason() {
  if (!npmReportsGlobalTransaction()) return "not a global install";

  const root = globalNodeModulesRoot();
  if (!root) return "cannot verify the global install root (npm_config_prefix is unset)";

  let selfDir;
  let rootDir;
  try {
    selfDir = fs.realpathSync(process.cwd());
    rootDir = fs.realpathSync(root);
  } catch {
    // Either path is missing or unreadable — we cannot prove we are the
    // top-level target, so we must not act.
    return "cannot verify the global install root on disk";
  }

  const parent = path.dirname(selfDir);
  if (samePath(parent, rootDir)) return null;
  // Scoped package: <globalRoot>/@scope/<name>.
  if (path.basename(parent).startsWith("@") && samePath(path.dirname(parent), rootDir)) {
    return null;
  }
  return "this package is a dependency of the global install, not its target";
}

function canonicalize() {
  const skip = globalInstallSkipReason();
  if (skip) {
    notice(`skipping ~/.local/bin canonicalize (${skip})`);
    return;
  }

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
