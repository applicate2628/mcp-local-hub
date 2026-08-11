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
// Detection is THREE gates, all required (see `globalInstallSkipReason` below
// for the per-gate citations and the empirical probes behind each):
//   1. the npm command actually DELIVERS a package (`install` / `update`).
//      A bare `npm rebuild -g` re-runs postinstall for every package in the
//      global tree with npm_config_global=true, delivering nothing — acting
//      there could downgrade a newer ~/.local/bin binary, and gate 3 cannot
//      catch it because we genuinely ARE the top-level package then.
//   2. npm reports a global install — read from BOTH `npm_config_global` and
//      `npm_config_location`, because npm surfaces the same intent under
//      different keys for `-g` vs `--location=global`, and using npm's own
//      boolean semantics rather than a hand-picked allowlist.
//   3. THIS package is that install's top-level target — <globalRoot>/<name>
//      resolves to the directory we are running in. Gates 1-2 alone are
//      insufficient: npm sets npm_config_global=true for the lifecycle scripts
//      of TRANSITIVE dependencies of a `-g` install too, so a package that
//      merely depends on mcp-local-hub would otherwise silently replace the
//      operator's PATH binary (bot PR #586).
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

// GATE 1 — is this an npm invocation that DELIVERS a package?
//
// npm sets `npm_command` to the running command for every lifecycle script
// (npm 7+; this package requires node>=18, so every supported npm sets it).
// Measured on npm 11.17.0, this host, 2026-07-26:
//     npm i -g <x> / npm add -g <x>  -> npm_command="install"
//     npm update -g                  -> npm_command="update"
//     npm rebuild -g                 -> npm_command="rebuild"
//
// A bare `npm rebuild -g` re-runs postinstall for EVERY package in the global
// tree — measured: our probe package's postinstall fired twice, with
// npm_config_global=true and cwd at the package root. Nothing is freshly
// delivered by a rebuild, so canonicalizing there is at best a no-op and at
// worst a DOWNGRADE: it would copy the currently-installed npm binary over a
// NEWER ~/.local/bin one placed by `mcphub install --upgrade`. Gate 3 cannot
// catch this — during a rebuild we genuinely ARE the top-level global package.
//
// Absent npm_command means we are not running under a supported npm lifecycle
// at all (the script was invoked directly, or by something imitating npm), so
// it fails CLOSED.
const PACKAGE_DELIVERING_NPM_COMMANDS = new Set(["install", "update"]);

function npmCommandDeliversPackage() {
  const cmd = String(process.env.npm_command ?? "").trim().toLowerCase();
  if (cmd === "") return false;
  return PACKAGE_DELIVERING_NPM_COMMANDS.has(cmd);
}

// GATE 2 — is npm installing GLOBALLY?
//
// npm mirrors its resolved config into npm_config_<key>, but the "install
// globally" intent arrives under TWO DIFFERENT keys depending on how the
// operator spelled it. Measured by dumping the env from a real lifecycle
// script (npm 11.17.0, this host, 2026-07-26):
//     npm ... -g / --global      -> npm_config_global="true", location unset
//     npm ... --location=global  -> npm_config_global unset,  location="global"
// `--location=global` is a documented global-install mode
// (https://docs.npmjs.com/cli/v11/using-npm/config#location), so reading only
// npm_config_global silently takes the non-global branch for it.
//
// The boolean handling is npm's, not a hand-picked allowlist. Passed on the
// COMMAND LINE npm normalizes every spelling to "true", but a value inherited
// from the PARENT ENVIRONMENT (CI wrappers, Docker images, shell profiles all
// do this) is passed through RAW. Measured which raw values npm itself then
// resolves to the global root, via `env npm_config_global=<v> npm root`:
//     GLOBAL: "true" "1" "yes" "on" "no" "off"      <- note "no"/"off" too
//     local : "false" "0" ""                          (and unset)
// "no"/"off" reading as global is counterintuitive but it is what npm does,
// and a hand-written {"true","1"} allowlist would reject four spellings npm
// treats as global — a false negative (stale ~/.local/bin) on every one.
// Matching npm exactly is safe here because gate 3 independently proves
// top-level global placement; this gate is a cheap pre-filter, not the
// load-bearing safety check.
const NPM_FALSE_CONFIG_VALUES = new Set(["false", "0", ""]);

function npmReportsGlobalTransaction() {
  if (process.env.npm_config_global !== undefined) {
    const flag = String(process.env.npm_config_global).trim().toLowerCase();
    if (!NPM_FALSE_CONFIG_VALUES.has(flag)) return true;
  }
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

// This package's own published name, read from the manifest that ships beside
// this script. Used by gate 3 to compute where a top-level global install of
// THIS package would live.
function selfPackageName() {
  try {
    return String(require("../package.json").name || "").trim();
  } catch {
    return "";
  }
}

// GATE 3 — is THIS package the top-level target of that global install?
//
// Gates 1-2 are necessary but NOT sufficient. When some OTHER package depends
// on mcp-local-hub and that parent is installed with `npm i -g`, npm runs the
// lifecycle scripts of its transitive dependencies with the SAME
// npm_config_global=true. Those gates alone would let a host that never asked
// for mcphub have its canonical PATH binary replaced — precisely the
// transitive case this guard exists to prevent (bot PR #586).
//
// The check asks the question directly: does <globalRoot>/<our name> resolve
// to the directory this lifecycle script is running in? Both sides are
// realpath'd, which is what makes it correct for BOTH install shapes —
// measured on npm 11.17.0, this host, 2026-07-26:
//   - registry install (`npm i -g mcp-local-hub`): the package is COPIED to
//     <globalRoot>/mcp-local-hub, and cwd is that same directory.
//   - folder install (`npm i -g ./some/checkout`): <globalRoot>/<name> is a
//     SYMLINK to the source folder, and cwd is the SOURCE FOLDER, not the link
//     — so any comparison of cwd's PARENT against the global root fails and
//     wrongly rejects this supported spelling. Resolving both sides through
//     realpath makes the two agree.
//   - transitive dependency: <globalRoot>/<our name> is either absent, or it
//     is some OTHER top-level copy that does not resolve to our cwd (which
//     lives under <globalRoot>/<parent>/node_modules/<name>). Rejected either
//     way.
//
// This is NOT the rejected npm_config_prefix-vs-npm_config_global_prefix probe
// described in the file header: that compared two PREFIXES (identical during a
// local install, hence useless). This resolves an actual directory ENTRY and
// compares it to where we are running, which separates exactly the cases we
// must tell apart. It also backstops gate 2: an operator with
// `location=global` in their .npmrc running an ordinary local install lands in
// a project's node_modules, where <globalRoot>/<name> does not resolve to us.
//
// Returns null when this IS a top-level global install of this package, or a
// human-readable skip reason otherwise. Unverifiable states FAIL CLOSED
// (skip): leaving ~/.local/bin stale is recoverable with `mcphub setup`,
// whereas clobbering a PATH binary nobody asked us to touch is not.
function globalInstallSkipReason() {
  if (!npmCommandDeliversPackage()) {
    const cmd = String(process.env.npm_command ?? "").trim().toLowerCase();
    return cmd === ""
      ? "not running under a recognized npm install lifecycle"
      : `\`npm ${cmd}\` delivers no new package`;
  }
  if (!npmReportsGlobalTransaction()) return "not a global install";

  const root = globalNodeModulesRoot();
  if (!root) return "cannot verify the global install root (npm_config_prefix is unset)";

  const name = selfPackageName();
  if (!name) return "cannot read this package's own name";

  // name may be scoped ("@scope/pkg"); npm lays that out as <root>/@scope/pkg.
  const expected = path.join(root, ...name.split("/"));

  let expectedDir;
  let selfDir;
  try {
    expectedDir = fs.realpathSync(expected);
  } catch {
    // <globalRoot>/<name> does not exist: we are not the top-level entry.
    return "this package is not the top-level entry of the global install";
  }
  try {
    selfDir = fs.realpathSync(process.cwd());
  } catch {
    return "cannot resolve this package's own directory";
  }

  if (samePath(expectedDir, selfDir)) return null;
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

  if (process.platform === "win32") {
    let admitPath;
    try {
      admitPath = require.resolve(`${pkg}/bin/mcphub-pe-admit.exe`);
    } catch {
      notice(
        `PE admission adapter is missing for "${key}"; ~/.local/bin left as-is — ${SETUP_FALLBACK}.`,
      );
      return;
    }
    const admission = spawnSync(admitPath, [binPath], {
      stdio: "inherit",
      shell: false,
      windowsHide: true,
    });
    if (admission.error || admission.signal || admission.status !== 0) {
      const detail = admission.error
        ? admission.error.code || admission.error.message
        : admission.signal
          ? `signal ${admission.signal}`
          : `exit ${admission.status}`;
      notice(
        `PE admission rejected the platform binary (${detail}); ~/.local/bin left as-is — ${SETUP_FALLBACK}.`,
      );
      return;
    }
  }

  const result = spawnSync(binPath, ["canonicalize"], {
    stdio: "inherit",
    // Do NOT shell-interpret; pass argv through verbatim.
    shell: false,
    windowsHide: true,
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
