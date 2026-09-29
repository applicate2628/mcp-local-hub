"use strict";
// Black-box fail-safe tests for scripts/postinstall.js.
//
// The canonicalize hook MUST exit 0 and point the operator at `mcphub setup`
// when the platform binary cannot be resolved — a canonicalize failure must
// never break `npm install`.
//
// It MUST also run its real-work path (resolve + spawn) ONLY when all three
// gates in scripts/postinstall.js pass, and skip that work — without ever
// attempting to resolve or spawn the platform binary — otherwise:
//
//   gate 1: the npm command DELIVERS a package (`install` / `update`). A bare
//           `npm rebuild -g` re-runs postinstall across the whole global tree
//           delivering nothing — bot PR #586 follow-up.
//   gate 2: npm reports a global install, spelled as npm_config_global (using
//           npm's OWN boolean semantics) or npm_config_location=global.
//   gate 3: <globalRoot>/<our name> resolves to the directory we run in, so a
//           TRANSITIVE dependency of someone else's `npm i -g` is skipped
//           while a SYMLINKED top-level install (`npm i -g ./folder`) is not.
//
// Unverifiable states fail CLOSED (skip), because leaving ~/.local/bin stale
// is recoverable with `mcphub setup` while clobbering a PATH binary nobody
// asked us to touch is not.
//
// Every gate assertion below separates ACT-vs-SKIP from the skip REASON text.
// A mutation that only changes wording must not be able to masquerade as a
// behavioural proof — that conflation weakened an earlier revision of this
// file and was caught in review.

const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { PACKAGE_BY_PLATFORM } = require("./lib/platform-binary");

// The name the real npm/package.json declares; gate 3 derives the expected
// top-level global path from it, so the fixture must publish the same name.
const SELF_NAME = "mcp-local-hub";

// Where npm puts globally-installed packages, per its folders documentation
// and confirmed on this host (`npm root -g` == <prefix>\node_modules).
// Mirrored here so the fixture reproduces the REAL on-disk shape gate 3
// inspects; a fixture that ignored this would make every gate-3 test vacuous.
function globalRootFor(prefix) {
  return process.platform === "win32"
    ? path.join(prefix, "node_modules")
    : path.join(prefix, "lib", "node_modules");
}

// Copies the published package's relative layout (package.json + lib/ +
// scripts/) into `pkgDir`, AND plants a platform-package stub with a
// package.json but NO bin binary so `require.resolve` is FORCED to fail on any
// host. That stub is the safety net that keeps every test in this file
// side-effect-free: even if a gate assertion were wrong, the script can never
// resolve — let alone spawn — a real binary.
function materializePackage(pkgDir, name = SELF_NAME) {
  fs.mkdirSync(pkgDir, { recursive: true });
  // Mirror the published meta package: CommonJS, so the copied scripts load as
  // CommonJS regardless of any ancestor package.json on the test host.
  fs.writeFileSync(
    path.join(pkgDir, "package.json"),
    JSON.stringify({ ...require("./package.json"), name }) + "\n",
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

// Directory symlink that works on Windows without elevation.
function linkDir(target, linkPath) {
  fs.mkdirSync(path.dirname(linkPath), { recursive: true });
  fs.symlinkSync(target, linkPath, process.platform === "win32" ? "junction" : "dir");
}

// `placement` decides the on-disk shape gate 3 adjudicates. Each mirrors a
// real npm invocation, measured on npm 11.17.0:
//   "top-level"        `npm i -g mcp-local-hub` — COPIED to <globalRoot>/<name>,
//                      cwd is that directory.
//   "scoped"           same, for a scoped package name.
//   "linked"           `npm i -g ./checkout` — <globalRoot>/<name> is a SYMLINK
//                      to the source folder and cwd is the SOURCE FOLDER. Any
//                      parent-of-cwd comparison fails here; this is the shape
//                      that broke an earlier revision of the gate.
//   "transitive"       mcphub pulled in by someone else's `npm i -g parent-pkg`;
//                      cwd is <globalRoot>/parent-pkg/node_modules/<name>.
//   "transitive-decoy" same, but a genuine top-level <globalRoot>/<name> ALSO
//                      exists — it must not launder the transitive run.
//   "local"            ordinary local install into a project's node_modules.
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
      pkgDir = path.join(globalRoot, SELF_NAME);
      materializePackage(pkgDir);
      break;
    case "scoped": {
      const scoped = "@acme/mcp-local-hub";
      pkgDir = path.join(globalRoot, ...scoped.split("/"));
      materializePackage(pkgDir, scoped);
      break;
    }
    case "linked":
      // Source lives OUTSIDE the global root; the root holds only a link.
      pkgDir = path.join(base, "checkout", SELF_NAME);
      materializePackage(pkgDir);
      linkDir(pkgDir, path.join(globalRoot, SELF_NAME));
      break;
    case "transitive":
      pkgDir = path.join(globalRoot, "parent-pkg", "node_modules", SELF_NAME);
      materializePackage(pkgDir);
      break;
    case "transitive-decoy":
      // A real top-level install exists too — gate 3 must still reject the
      // nested run, because <globalRoot>/<name> resolves to the OTHER copy.
      materializePackage(path.join(globalRoot, SELF_NAME));
      pkgDir = path.join(globalRoot, "parent-pkg", "node_modules", SELF_NAME);
      materializePackage(pkgDir);
      break;
    case "local":
      pkgDir = path.join(base, "project", "node_modules", SELF_NAME);
      materializePackage(pkgDir);
      break;
    default:
      throw new Error(`unknown placement: ${placement}`);
  }

  return { base, prefix, globalRoot, home, pkgDir };
}

// Baseline env = a plain `npm i -g` of this package. Individual tests override
// exactly the key under test; a key set to `undefined` is DELETED, which is how
// "npm_command absent" and "prefix unset" are expressed.
function runPostinstall(fixture, extraEnv) {
  // Every npm_config_* / npm_command key this script reads is cleared from the
  // inherited base env FIRST — this file may itself run under `npm test`, whose
  // environment could otherwise satisfy a test's intent by accident.
  const env = { ...process.env, HOME: fixture.home, USERPROFILE: fixture.home };
  delete env.npm_config_global;
  delete env.npm_config_location;
  env.npm_command = "install";
  env.npm_config_global = "true";
  env.npm_config_prefix = fixture.prefix;
  for (const [key, value] of Object.entries(extraEnv || {})) {
    if (value === undefined) delete env[key];
    else env[key] = value;
  }

  return spawnSync(process.execPath, [path.join(fixture.pkgDir, "scripts", "postinstall.js")], {
    encoding: "utf8",
    env,
    // A lifecycle script runs with the package directory as cwd; gate 3 reads
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

// ACT: the gates passed and the script entered the resolve/spawn logic, where
// the fixture guarantees resolution fails — exit 0 plus the `mcphub setup`
// fallback, and no guard skip notice. The guard's notice is
// `skipping ... canonicalize (<reason>)`; the resolve-failure notices read
// `...; skipping ... canonicalize — run \`mcphub setup\``, so matching the
// parenthesized form distinguishes them.
function assertActed(res) {
  assert.strictEqual(
    res.status,
    0,
    `postinstall must exit 0 on a resolve failure; got ${res.status}\nstderr:\n${res.stderr}`,
  );
  assert.match(
    res.stderr,
    /mcphub setup/,
    `must have reached the resolve/spawn path; stderr:\n${res.stderr}`,
  );
  assert.doesNotMatch(
    res.stderr,
    /skipping ~\/\.local\/bin canonicalize \(/,
    `the guard must not have fired; stderr:\n${res.stderr}`,
  );
}

// SKIP: a gate fired BEFORE any resolve/spawn attempt. Neither the "no platform
// binary" nor the "platform binary not installed" notice (both of which only
// fire from INSIDE the resolve logic) may appear. Deliberately says nothing
// about WHICH gate fired — see assertSkipReason.
function assertSkipped(res) {
  assert.strictEqual(
    res.status,
    0,
    `postinstall must still exit 0 when skipping; got ${res.status}\nstderr:\n${res.stderr}`,
  );
  assert.match(
    res.stderr,
    /skipping ~\/\.local\/bin canonicalize \(/,
    `expected the guard's skip notice; stderr:\n${res.stderr}`,
  );
  assert.doesNotMatch(
    res.stderr,
    /platform binary|mcphub canonicalize|mcphub setup/,
    `a skipped install must never reach resolve/spawn code; stderr:\n${res.stderr}`,
  );
}

// Attribution only. Separate from assertSkipped so a wording change can never
// be mistaken for a behavioural regression, nor the reverse.
function assertSkipReason(res, pattern) {
  assert.match(res.stderr, pattern, `wrong skip reason; stderr:\n${res.stderr}`);
}

test("plain `npm i -g` of this package reaches the resolve/spawn path", () => {
  withFixture("top-level", (f) => assertActed(runPostinstall(f, {})));
});

// `npm install --location=global` sets npm_config_location instead of
// npm_config_global (measured: `-g` sets global=true with location unset;
// `--location=global` sets location=global with global unset).
test("`--location=global` reaches the resolve/spawn path", () => {
  withFixture("top-level", (f) =>
    assertActed(runPostinstall(f, { npm_config_global: undefined, npm_config_location: "global" })),
  );
});

test("a scoped top-level global install is recognized as the target", () => {
  withFixture("scoped", (f) => assertActed(runPostinstall(f, {})));
});

// `npm i -g ./checkout` symlinks <globalRoot>/<name> to the source folder and
// runs the lifecycle with cwd at the SOURCE. An earlier revision compared
// cwd's PARENT against the global root and wrongly skipped this.
test("a SYMLINKED top-level global install (`npm i -g ./folder`) is recognized", () => {
  withFixture("linked", (f) => assertActed(runPostinstall(f, {})));
});

test("`npm update -g` also delivers a package and is allowed", () => {
  withFixture("top-level", (f) => assertActed(runPostinstall(f, { npm_command: "update" })));
});

// npm runs the lifecycle scripts of a `-g` install's TRANSITIVE dependencies
// with npm_config_global=true too. Acting there would replace the PATH binary
// of a host that never asked for mcphub.
test("a transitive dependency of a global install is skipped", () => {
  withFixture("transitive", (f) => {
    const res = runPostinstall(f, {});
    assertSkipped(res);
    assertSkipReason(res, /not the top-level entry|not its target/);
  });
});

test("a transitive run is still skipped when a real top-level copy also exists", () => {
  withFixture("transitive-decoy", (f) => {
    const res = runPostinstall(f, {});
    assertSkipped(res);
    assertSkipReason(res, /dependency of the global install, not its target/);
  });
});

// A bare `npm rebuild -g` re-runs postinstall for EVERY global package (it
// fired twice for the probe package) with npm_config_global=true and cwd at
// the package root — so gate 3 cannot catch it: we really are top-level.
// Nothing is delivered, and canonicalizing could downgrade a newer
// ~/.local/bin binary placed by `mcphub install --upgrade`.
test("`npm rebuild -g` delivers nothing and is skipped even at top level", () => {
  withFixture("top-level", (f) => {
    const res = runPostinstall(f, { npm_command: "rebuild" });
    assertSkipped(res);
    assertSkipReason(res, /delivers no new package/);
  });
});

test("an absent npm_command fails CLOSED", () => {
  withFixture("top-level", (f) => {
    const res = runPostinstall(f, { npm_command: undefined });
    assertSkipped(res);
    assertSkipReason(res, /not running under a recognized npm install lifecycle/);
  });
});

// npm normalizes command-line spellings to "true", but a value inherited from
// the PARENT ENVIRONMENT passes through RAW. Measured via
// `env npm_config_global=<v> npm root`: these select the GLOBAL root.
for (const value of ["true", "1", "yes", "on", "no", "off"]) {
  test(`npm_config_global=${JSON.stringify(value)} is global (npm's own semantics)`, () => {
    withFixture("top-level", (f) => assertActed(runPostinstall(f, { npm_config_global: value })));
  });
}

// ...and these select the LOCAL root, so they must not authorize anything.
for (const value of ["false", "0", ""]) {
  test(`npm_config_global=${JSON.stringify(value)} is local`, () => {
    withFixture("local", (f) => {
      const res = runPostinstall(f, { npm_config_global: value });
      assertSkipped(res);
      assertSkipReason(res, /not a global install/);
    });
  });
}

test("no global signal at all skips canonicalize", () => {
  withFixture("local", (f) => {
    const res = runPostinstall(f, { npm_config_global: undefined });
    assertSkipped(res);
    assertSkipReason(res, /not a global install/);
  });
});

function buildForcedWindowsFixture() {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "mcphub-postinstall-win-"));
  const prefix = path.join(base, "prefix");
  const globalRoot = path.join(prefix, "node_modules");
  const pkgDir = path.join(globalRoot, SELF_NAME);
  const home = path.join(base, "home");
  fs.mkdirSync(home, { recursive: true });
  materializePackage(pkgDir);
  fs.mkdirSync(path.join(pkgDir, "bin"), { recursive: true });
  fs.copyFileSync(path.join(__dirname, "bin", "cli.js"), path.join(pkgDir, "bin", "cli.js"));

  const platformPkg = PACKAGE_BY_PLATFORM["win32-x64"];
  const platformBin = path.join(pkgDir, "node_modules", ...platformPkg.split("/"), "bin");
  fs.mkdirSync(platformBin, { recursive: true });
  const candidatePath = path.join(platformBin, "mcphub.exe");
  const candidateBytes = Buffer.from("candidate fixture\n");
  fs.writeFileSync(candidatePath, candidateBytes);
  fs.writeFileSync(
    path.join(platformBin, "windows-product-identity.json"),
    JSON.stringify({
      schema: "windows-product-identity-v1",
      role: "product",
      filename: "mcphub.exe",
      subsystem: 2,
      machine: 0x8664,
      version: "0.4.37",
      commit: "0123456789abcdef0123456789abcdef01234567",
      build_date: "2026-09-11T00:00:00Z",
      sha256: crypto.createHash("sha256").update(candidateBytes).digest("hex"),
    }) + "\n",
  );

  const tracePath = path.join(base, "spawn-trace.jsonl");
  const preloadPath = path.join(base, "spawn-preload.cjs");
  fs.writeFileSync(
    preloadPath,
    `Object.defineProperty(process, "platform", {value: "win32"});\n` +
      `Object.defineProperty(process, "arch", {value: process.env.MCPHUB_TEST_ARCH || "x64"});\n` +
      `const fs = require("node:fs");\n` +
      `const cp = require("node:child_process");\n` +
      `cp.spawnSync = (file, args, opts) => {\n` +
      `  fs.appendFileSync(process.env.MCPHUB_TEST_SPAWN_TRACE, JSON.stringify({file,args,stdio:opts.stdio,shell:opts.shell,windowsHide:opts.windowsHide,hasEnv:Object.prototype.hasOwnProperty.call(opts,"env")}) + "\\n");\n` +
      `  const status = Number(process.env.MCPHUB_TEST_CANDIDATE_STATUS || 0);\n` +
      `  return {status, signal:null, error:null};\n` +
      `};\n`,
  );
  return { base, prefix, globalRoot, home, pkgDir, tracePath, preloadPath };
}

function forcedWindowsEnv(fixture, extra = {}) {
  const env = { ...process.env, HOME: fixture.home, USERPROFILE: fixture.home };
  delete env.npm_config_location;
  env.npm_command = "install";
  env.npm_config_global = "true";
  env.npm_config_prefix = fixture.prefix;
  env.MCPHUB_TEST_SPAWN_TRACE = fixture.tracePath;
  env.NODE_OPTIONS = `--require=${fixture.preloadPath}`;
  return { ...env, ...extra };
}

function readSpawnTrace(fixture) {
  if (!fs.existsSync(fixture.tracePath)) return [];
  return fs
    .readFileSync(fixture.tracePath, "utf8")
    .trim()
    .split(/\r?\n/)
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

test("Windows platform payload has one PE plus the identity manifest and no bin owner", () => {
  for (const target of ["win32-x64"]) {
    const manifest = require("./packages/" + target + "/package.json");
    assert.deepStrictEqual(
      [...manifest.files].sort(),
      [
        "bin/mcphub.exe",
        "bin/windows-product-identity.json",
        "README.md",
      ].sort(),
    );
    assert.strictEqual(Object.hasOwn(manifest, "bin"), false);
  }
});

test("Postinstall verifies the Windows identity manifest before canonicalize", () => {
  const fixture = buildForcedWindowsFixture();
  try {
    const script = path.join(fixture.pkgDir, "scripts", "postinstall.js");
    const identityPath = path.join(
      fixture.pkgDir,
      "node_modules",
      ...PACKAGE_BY_PLATFORM["win32-x64"].split("/"),
      "bin",
      "windows-product-identity.json",
    );
    const originalIdentity = fs.readFileSync(identityPath, "utf8");

    fs.rmSync(identityPath);
    const missing = spawnSync(process.execPath, [script], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture),
    });
    assert.strictEqual(missing.status, 0, missing.stderr);
    assert.match(missing.stderr, /identity manifest is missing.*left as-is/);
    let trace = readSpawnTrace(fixture);
    assert.strictEqual(trace.length, 0, "missing-manifest trace=" + JSON.stringify(trace));

    fs.writeFileSync(
      identityPath,
      originalIdentity.replace("windows-product-identity-v1", "unknown-schema"),
    );
    const unknownSchema = spawnSync(process.execPath, [script], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture),
    });
    assert.strictEqual(unknownSchema.status, 0, unknownSchema.stderr);
    assert.match(unknownSchema.stderr, /identity manifest.*schema.*left as-is/);
    trace = readSpawnTrace(fixture);
    assert.strictEqual(trace.length, 0, "unknown-schema trace=" + JSON.stringify(trace));

    const wrongRole = JSON.parse(originalIdentity);
    wrongRole.role = "windowless";
    fs.writeFileSync(identityPath, JSON.stringify(wrongRole) + "\n");
    const unknownRole = spawnSync(process.execPath, [script], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture),
    });
    assert.strictEqual(unknownRole.status, 0, unknownRole.stderr);
    assert.match(unknownRole.stderr, /identity manifest fields are invalid.*left as-is/);
    trace = readSpawnTrace(fixture);
    assert.strictEqual(trace.length, 0, "unknown-role trace=" + JSON.stringify(trace));

    fs.writeFileSync(identityPath, originalIdentity.replace(/[0-9a-f]{64}/, "0".repeat(64)));
    const hashRejected = spawnSync(process.execPath, [script], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture),
    });
    assert.strictEqual(hashRejected.status, 0, hashRejected.stderr);
    assert.match(hashRejected.stderr, /identity manifest.*SHA-256.*left as-is/);
    trace = readSpawnTrace(fixture);
    assert.strictEqual(trace.length, 0, "hash-rejected trace=" + JSON.stringify(trace));

    fs.writeFileSync(identityPath, originalIdentity);
    const admitted = spawnSync(process.execPath, [script], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture),
    });
    assert.strictEqual(admitted.status, 0, admitted.stderr);
    trace = readSpawnTrace(fixture);
    assert.strictEqual(trace.length, 1, "admitted trace=" + JSON.stringify(trace));
    assert.strictEqual(path.basename(trace[0].file).toLowerCase(), "mcphub.exe");
    assert.deepStrictEqual(trace[0].args, ["canonicalize"]);
    assert.strictEqual(trace[0].windowsHide, true);
    assert.strictEqual(trace[0].stdio, "inherit");
    assert.strictEqual(trace[0].shell, false);
    assert.strictEqual(trace[0].hasEnv, false);
  } finally {
    fs.rmSync(fixture.base, { recursive: true, force: true });
  }
});

test("global postinstall on deferred Windows arm64 never canonicalizes an older package", () => {
  const fixture = buildForcedWindowsFixture();
  try {
    const older = path.join(
      fixture.pkgDir,
      "node_modules",
      "@applicate2628",
      "mcp-local-hub-win32-arm64",
      "bin",
    );
    fs.mkdirSync(older, { recursive: true });
    fs.writeFileSync(path.join(older, "mcphub.exe"), "older native payload");
    const result = spawnSync(
      process.execPath,
      [path.join(fixture.pkgDir, "scripts", "postinstall.js")],
      {
        encoding: "utf8",
        cwd: fixture.pkgDir,
        env: forcedWindowsEnv(fixture, { MCPHUB_TEST_ARCH: "arm64" }),
      },
    );
    assert.strictEqual(result.status, 0, result.stderr);
    assert.match(result.stderr, /no platform binary for "win32-arm64"/);
    assert.deepStrictEqual(readSpawnTrace(fixture), []);
  } finally {
    fs.rmSync(fixture.base, { recursive: true, force: true });
  }
});

test("npm launcher preserves argv and inherited stdio without console policy", () => {
  const fixture = buildForcedWindowsFixture();
  try {
    const argv = ["--debug-console", "status", "--", "x y", "--debug-console=false"];
    const result = spawnSync(process.execPath, [path.join(fixture.pkgDir, "bin", "cli.js"), ...argv], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture, { MCPHUB_TEST_CANDIDATE_STATUS: "37" }),
    });
    assert.strictEqual(result.status, 37, result.stderr);
    const trace = readSpawnTrace(fixture);
    assert.strictEqual(trace.length, 1, JSON.stringify(trace));
    assert.strictEqual(path.basename(trace[0].file).toLowerCase(), "mcphub.exe");
    assert.deepStrictEqual(trace[0].args, argv);
    assert.strictEqual(trace[0].stdio, "inherit");
    assert.strictEqual(trace[0].shell, false);
    assert.strictEqual(trace[0].windowsHide, true);
    assert.strictEqual(trace[0].hasEnv, false);
  } finally {
    fs.rmSync(fixture.base, { recursive: true, force: true });
  }
});

// Gate 3 also backstops gate 2: `location=global` carried in an operator's
// .npmrc during an ordinary local install must not authorize the write.
test("`location=global` from config does not authorize an ordinary local install", () => {
  withFixture("local", (f) => {
    const res = runPostinstall(f, {
      npm_config_global: undefined,
      npm_config_location: "global",
    });
    assertSkipped(res);
    assertSkipReason(res, /not the top-level entry|not its target/);
  });
});

test("an unverifiable global root fails CLOSED", () => {
  withFixture("top-level", (f) => {
    const res = runPostinstall(f, { npm_config_prefix: undefined });
    assertSkipped(res);
    assertSkipReason(res, /cannot verify the global install root/);
  });
});
