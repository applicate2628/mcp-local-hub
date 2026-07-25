"use strict";
// Black-box fail-safe test for scripts/postinstall.js: the canonicalize hook
// MUST exit 0 and point the operator at `mcphub setup` when the platform
// binary cannot be resolved — a canonicalize failure must never break
// `npm install`. It also MUST run its real-work path (resolve + spawn) ONLY
// when npm reports a GLOBAL install via `npm_config_global`, and MUST skip
// that work — without ever attempting to resolve or spawn the platform
// binary — for a local or transitive install (bot PR #585 concern; see the
// GLOBAL-INSTALL-ONLY GUARD comment in scripts/postinstall.js).

const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { PACKAGE_BY_PLATFORM } = require("./lib/platform-binary");

// Shared fixture builder: reproduces the published package's relative layout
// (lib/ + scripts/) so the script's `require("../lib/platform-binary")`
// resolves exactly as it would once installed, AND forces a deterministic
// resolve FAILURE on any host (a platform package dir with a package.json but
// NO bin binary) so that even if a test's guard assertion is wrong, the
// script can never resolve — let alone spawn — a real binary. This is the
// safety net that keeps every test in this file side-effect-free regardless
// of which branch the code under test actually takes.
function buildFixture() {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "mcphub-postinstall-"));
  // Mirror the published meta package: CommonJS, so the copied scripts load
  // as CommonJS regardless of any ancestor package.json on the test host.
  fs.writeFileSync(
    path.join(tmp, "package.json"),
    JSON.stringify({ name: "mcp-local-hub-postinstall-fixture", type: "commonjs" }) + "\n",
  );
  fs.mkdirSync(path.join(tmp, "lib"));
  fs.mkdirSync(path.join(tmp, "scripts"));
  fs.copyFileSync(
    path.join(__dirname, "lib", "platform-binary.js"),
    path.join(tmp, "lib", "platform-binary.js"),
  );
  fs.copyFileSync(
    path.join(__dirname, "scripts", "postinstall.js"),
    path.join(tmp, "scripts", "postinstall.js"),
  );

  // require.resolve(`<pkg>/bin/<name>`) then throws (file missing) AND this
  // local dir shadows any real platform package elsewhere on the machine, so
  // the test never spawns a real binary. On an unsupported host the map miss
  // takes the "no platform binary" branch — also exit 0 + fallback notice.
  const key = `${process.platform}-${process.arch}`;
  const pkg = PACKAGE_BY_PLATFORM[key];
  if (pkg) {
    const pkgDir = path.join(tmp, "node_modules", ...pkg.split("/"));
    fs.mkdirSync(pkgDir, { recursive: true });
    fs.writeFileSync(
      path.join(pkgDir, "package.json"),
      JSON.stringify({ name: pkg, version: "0.0.0" }) + "\n",
    );
  } else {
    fs.mkdirSync(path.join(tmp, "node_modules"));
  }
  return tmp;
}

function runPostinstall(tmp, extraEnv) {
  // Fresh HOME/USERPROFILE so nothing here can touch the real ~/.local.
  // `npm_config_global` is deleted from the inherited base FIRST (this test
  // file itself might be running under `npm test`, whose own env could carry
  // an inherited value) so the test's intent (set vs left unset via
  // `extraEnv`) is never accidentally satisfied by whatever launched this
  // test runner; `extraEnv` is applied afterward and wins when it sets it.
  const env = { ...process.env, HOME: tmp, USERPROFILE: tmp };
  delete env.npm_config_global;
  Object.assign(env, extraEnv);
  return spawnSync(process.execPath, [path.join(tmp, "scripts", "postinstall.js")], {
    encoding: "utf8",
    env,
  });
}

test("postinstall exits 0 with a `mcphub setup` fallback when the platform binary is unresolvable (global install)", () => {
  const tmp = buildFixture();
  try {
    // This is the resolve-failure branch, which is only reachable when the
    // global-install guard passes — so this test doubles as proof that a
    // global install proceeds past the guard into the resolve/spawn logic.
    const res = runPostinstall(tmp, { npm_config_global: "true" });

    assert.strictEqual(
      res.status,
      0,
      `postinstall must exit 0 on a resolve failure; got ${res.status}\nstderr:\n${res.stderr}`,
    );
    assert.match(
      res.stderr,
      /mcphub setup/,
      `fallback notice must point at \`mcphub setup\`; stderr:\n${res.stderr}`,
    );
    assert.doesNotMatch(
      res.stderr,
      /not a global install/,
      `a global install must not print the skip notice; stderr:\n${res.stderr}`,
    );
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test("postinstall skips ~/.local/bin canonicalize without resolving/spawning anything when npm_config_global is unset (local/transitive install)", () => {
  const tmp = buildFixture();
  try {
    const res = runPostinstall(tmp, {});

    assert.strictEqual(
      res.status,
      0,
      `postinstall must still exit 0 when skipping; got ${res.status}\nstderr:\n${res.stderr}`,
    );
    assert.match(
      res.stderr,
      /skipping ~\/\.local\/bin canonicalize \(not a global install\)/,
      `local/transitive install must print the skip notice; stderr:\n${res.stderr}`,
    );
    // Proves the guard fired BEFORE any resolve/spawn attempt: neither the
    // "no platform binary" nor the "platform binary not installed" fallback
    // notice (which only fire from inside the resolve logic) appears, and
    // the notice does not point at `mcphub setup` the way every resolve/spawn
    // failure notice does.
    assert.doesNotMatch(
      res.stderr,
      /platform binary|mcphub canonicalize|mcphub setup/,
      `local/transitive install must never reach resolve/spawn code; stderr:\n${res.stderr}`,
    );
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});

test("postinstall skips ~/.local/bin canonicalize when npm_config_global=false (npm's own negative form)", () => {
  const tmp = buildFixture();
  try {
    const res = runPostinstall(tmp, { npm_config_global: "false" });

    assert.strictEqual(res.status, 0, `postinstall must exit 0; got ${res.status}\nstderr:\n${res.stderr}`);
    assert.match(
      res.stderr,
      /skipping ~\/\.local\/bin canonicalize \(not a global install\)/,
      `npm_config_global=false must be treated as non-global; stderr:\n${res.stderr}`,
    );
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
