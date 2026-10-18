"use strict";
// Lifecycle delivery is non-mutating. The shim alone launches the npm payload.
const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { PACKAGE_BY_PLATFORM } = require("./lib/platform-binary");
const SELF_NAME = "mcp-local-hub";

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

test("npm delivery never promotes the native product into a second installation", () => {
  const fixture = buildForcedWindowsFixture();
  try {
    const result = spawnSync(process.execPath, [path.join(fixture.pkgDir, "scripts", "postinstall.js")], {
      encoding: "utf8",
      cwd: fixture.pkgDir,
      env: forcedWindowsEnv(fixture),
      timeout: 10000,
    });
    assert.strictEqual(result.status, 0, result.stderr);
    assert.deepStrictEqual(readSpawnTrace(fixture), [], "npm must not invoke canonicalize or launch a fleet");
    assert.strictEqual(fs.existsSync(path.join(fixture.home, ".local")), false);
  } finally {
    fs.rmSync(fixture.base, { recursive: true, force: true });
  }
});

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
