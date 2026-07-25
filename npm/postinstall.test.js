"use strict";
// Black-box fail-safe tests for scripts/postinstall.js.
//
// The canonicalize hook MUST exit 0 and point the operator at `mcphub setup`
// when the platform binary cannot be resolved — a canonicalize failure must
// never break `npm install`.
//
// It MUST also run its real-work path (resolve + spawn) ONLY for a global
// install whose TOP-LEVEL TARGET is this package, and skip that work — without
// ever attempting to resolve or spawn the platform binary — otherwise. Two
// gates enforce that (see the GLOBAL-INSTALL-ONLY GUARD comment in
// scripts/postinstall.js):
//
//   gate 1: npm reports a global transaction, spelled EITHER as
//           npm_config_global=true (`-g`) OR npm_config_location=global
//           (`--location=global`) — bot PR #586 finding 1.
//   gate 2: this package's own directory sits directly in the global
//           node_modules root, so a TRANSITIVE dependency of some other
//           `npm i -g` install (which npm also runs with
//           npm_config_global=true) is skipped — bot PR #586 finding 2.
//
// Unverifiable states fail CLOSED (skip), because leaving ~/.local/bin stale
// is recoverable with `mcphub setup` while clobbering a PATH binary nobody
// asked us to touch is not.

const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { PACKAGE_BY_PLATFORM } = require("./lib/platform-binary");

// Where npm puts globally-installed packages, per its folders documentation.
// Mirrored here so the fixture reproduces the REAL on-disk shape gate 2
// inspects; a fixture that ignored this would make every gate-2 test vacuous.
function globalRootFor(prefix) {
  return process.platform === "win32"
    ? path.join(prefix, "node_modules")
    : path.join(prefix, "lib", "node_modules");
}

// Copies the published package's relative layout (package.json + lib/ +
// scripts/) into `pkgDir` so the script's `require("../lib/platform-binary")`
// resolves exactly as it would once installed, AND plants a platform-package
// stub with a package.json but NO bin binary so `require.resolve` is FORCED to
// fail on any host. That stub is the safety net that keeps every test in this
// file side-effect-free: even if a guard assertion were wrong, the script can
// never resolve — let alone spawn — a real binary.
function materializePackage(pkgDir) {
  fs.mkdirSync(pkgDir, { recursive: true });
  // Mirror the published meta package: CommonJS, so the copied scripts load as
  // CommonJS regardless of any ancestor package.json on the test host.
  fs.writeFileSync(
    path.join(pkgDir, "package.json"),
    JSON.stringify({ name: "mcp-local-hub", type: "commonjs" }) + "\n",
  );
  fs.mkdirSync(path.join(pkgDir, "lib"));
  fs.mkdirSync(path.join(pkgDir, "scripts"));
  fs.copyFileSync(
    path.join(__dirname, "lib", "platform-binary.js"),
    path.join(pkgDir, "lib", "platform-binary.js"),
  );
  fs.copyFileSync(
    path.join(__dirname, "scripts", "postinstall.js"),
    path.join(pkgDir, "scripts", "postinstall.js"),
  );

  const key = `${process.platform}-${process.arch}`;
  const pkg = PACKAGE_BY_PLATFORM[key];
  if (pkg) {
    const stubDir = path.join(pkgDir, "node_modules", ...pkg.split("/"));
    fs.mkdirSync(stubDir, { recursive: true });
    fs.writeFileSync(
      path.join(stubDir, "package.json"),
      JSON.stringify({ name: pkg, version: "0.0.0" }) + "\n",
    );
  } else {
    // Unsupported host: the map miss takes the "no platform binary" branch,
    // which is also exit 0 + fallback notice.
    fs.mkdirSync(path.join(pkgDir, "node_modules"), { recursive: true });
  }
}

// `placement` decides where the package sits relative to the global root —
// which is exactly what gate 2 adjudicates:
//   "top-level"  -> <globalRoot>/mcp-local-hub          (a real `npm i -g mcp-local-hub`)
//   "scoped"     -> <globalRoot>/@acme/mcp-local-hub    (same, for a scoped name)
//   "transitive" -> <globalRoot>/parent-pkg/node_modules/mcp-local-hub
//                   (mcphub pulled in by someone else's `npm i -g parent-pkg`)
//   "local"      -> <base>/project/node_modules/mcp-local-hub (ordinary local install)
function buildFixture(placement = "top-level") {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "mcphub-postinstall-"));
  const prefix = path.join(base, "prefix");
  const globalRoot = globalRootFor(prefix);
  fs.mkdirSync(globalRoot, { recursive: true });

  // A HOME/USERPROFILE of its own, so nothing here can reach the real ~/.local.
  const home = path.join(base, "home");
  fs.mkdirSync(home, { recursive: true });

  let pkgDir;
  switch (placement) {
    case "top-level":
      pkgDir = path.join(globalRoot, "mcp-local-hub");
      break;
    case "scoped":
      pkgDir = path.join(globalRoot, "@acme", "mcp-local-hub");
      break;
    case "transitive":
      pkgDir = path.join(globalRoot, "parent-pkg", "node_modules", "mcp-local-hub");
      break;
    case "local":
      pkgDir = path.join(base, "project", "node_modules", "mcp-local-hub");
      break;
    default:
      throw new Error(`unknown placement: ${placement}`);
  }

  materializePackage(pkgDir);
  return { base, prefix, globalRoot, home, pkgDir };
}

function runPostinstall(fixture, extraEnv) {
  // Every npm_config_* key this script reads is deleted from the inherited
  // base env FIRST — this test file may itself be running under `npm test`,
  // whose environment could otherwise satisfy a test's intent by accident.
  // `extraEnv` is applied afterward and wins; a key set to `undefined` there
  // is deleted, which is how the "npm_config_prefix unset" case is expressed.
  const env = { ...process.env, HOME: fixture.home, USERPROFILE: fixture.home };
  delete env.npm_config_global;
  delete env.npm_config_location;
  env.npm_config_prefix = fixture.prefix;
  for (const [key, value] of Object.entries(extraEnv || {})) {
    if (value === undefined) delete env[key];
    else env[key] = value;
  }

  return spawnSync(process.execPath, [path.join(fixture.pkgDir, "scripts", "postinstall.js")], {
    encoding: "utf8",
    env,
    // A lifecycle script runs with the package directory as cwd; gate 2 reads
    // process.cwd(), so this is load-bearing, not incidental.
    cwd: fixture.pkgDir,
  });
}

function withFixture(placement, fn) {
  const fixture = buildFixture(placement);
  try {
    fn(fixture);
  } finally {
    fs.rmSync(fixture.base, { recursive: true, force: true });
  }
}

// Asserts the guard PASSED: the script reached the resolve/spawn logic and
// failed there (the fixture guarantees resolution fails), exiting 0 with the
// `mcphub setup` fallback and no skip notice.
function assertReachedRealWork(res) {
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
  // The GUARD's skip notice is `skipping ... canonicalize (<reason>)`; the
  // resolve/spawn failure notices use `...; skipping ... canonicalize — run
  // \`mcphub setup\``. Match the parenthesized reason so this asserts the guard
  // did not fire, without also rejecting the resolve-failure branch we WANT.
  assert.doesNotMatch(
    res.stderr,
    /skipping ~\/\.local\/bin canonicalize \(/,
    `a top-level global install must not print the guard's skip notice; stderr:\n${res.stderr}`,
  );
}

// Asserts the guard FIRED before any resolve/spawn attempt. Neither the
// "no platform binary" nor the "platform binary not installed" notice (both of
// which only fire from INSIDE the resolve logic) may appear, and the notice
// must not point at `mcphub setup` the way every resolve/spawn failure does.
function assertSkippedBeforeRealWork(res, reasonPattern) {
  assert.strictEqual(
    res.status,
    0,
    `postinstall must still exit 0 when skipping; got ${res.status}\nstderr:\n${res.stderr}`,
  );
  assert.match(
    res.stderr,
    reasonPattern,
    `skip notice must state the right reason; stderr:\n${res.stderr}`,
  );
  assert.doesNotMatch(
    res.stderr,
    /platform binary|mcphub canonicalize|mcphub setup/,
    `a skipped install must never reach resolve/spawn code; stderr:\n${res.stderr}`,
  );
}

test("global install via `-g` (npm_config_global) reaches the resolve/spawn path", () => {
  withFixture("top-level", (fixture) => {
    assertReachedRealWork(runPostinstall(fixture, { npm_config_global: "true" }));
  });
});

// Bot PR #586 finding 1: `npm install --location=global` is a documented
// global-install mode that sets npm_config_location instead of
// npm_config_global. Probed on npm 11.17.0: `-g` sets global=true with
// location unset; `--location=global` sets location=global with global unset.
test("global install via `--location=global` reaches the resolve/spawn path", () => {
  withFixture("top-level", (fixture) => {
    assertReachedRealWork(
      runPostinstall(fixture, { npm_config_global: undefined, npm_config_location: "global" }),
    );
  });
});

test("a scoped top-level global install is still recognized as the target", () => {
  withFixture("scoped", (fixture) => {
    assertReachedRealWork(runPostinstall(fixture, { npm_config_global: "true" }));
  });
});

test("no global signal at all skips canonicalize without resolving or spawning", () => {
  withFixture("local", (fixture) => {
    assertSkippedBeforeRealWork(runPostinstall(fixture, {}), /not a global install/);
  });
});

test("npm_config_global=false (npm's own negative form) is treated as non-global", () => {
  withFixture("local", (fixture) => {
    assertSkippedBeforeRealWork(
      runPostinstall(fixture, { npm_config_global: "false" }),
      /not a global install/,
    );
  });
});

// Bot PR #586 finding 2: npm runs the lifecycle scripts of a `-g` install's
// TRANSITIVE dependencies with npm_config_global=true as well. Gate 1 alone
// would let that replace the PATH binary of a host that never asked for
// mcphub — the exact case this guard claims to prevent.
test("a transitive dependency of a global install is skipped even though npm reports global", () => {
  withFixture("transitive", (fixture) => {
    assertSkippedBeforeRealWork(
      runPostinstall(fixture, { npm_config_global: "true" }),
      /dependency of the global install, not its target/,
    );
  });
});

// Gate 2 also backstops gate 1: an operator carrying `location=global` in
// their .npmrc while running an ordinary local install lands in a project's
// node_modules, not the global root.
test("`location=global` from config does not authorize an ordinary local install", () => {
  withFixture("local", (fixture) => {
    assertSkippedBeforeRealWork(
      runPostinstall(fixture, { npm_config_location: "global" }),
      /dependency of the global install, not its target/,
    );
  });
});

test("an unverifiable global root fails CLOSED rather than canonicalizing", () => {
  withFixture("top-level", (fixture) => {
    assertSkippedBeforeRealWork(
      runPostinstall(fixture, { npm_config_global: "true", npm_config_prefix: undefined }),
      /cannot verify the global install root/,
    );
  });
});
