#!/usr/bin/env node
// Tests for npm/sync-version.js — run with `node --test npm/sync-version.test.js`
// (Node >=18, the package `engines.node` floor). No third-party deps.
//
// sync-version.js resolves every path relative to its own __dirname
// (NPM_DIR = __dirname; REPO_ROOT = NPM_DIR/..), and runs main() on load.
// So each case builds a throwaway repo layout in a temp dir:
//
//   <root>/npm/sync-version.js      (copy of the real script)
//   <root>/npm/package.json         (fixture authority version)
//   <root>/build.sh                 (fixture VERSION="…")
//   <root>/build.ps1                (fixture $version = "…")
//   <root>/cmd/mcphub/versioninfo.json
//
// then spawns `node <root>/npm/sync-version.js <mode>` and asserts on the
// exit code + the rewritten fixture files.

"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const REAL_SCRIPT = path.join(__dirname, "sync-version.js");

// buildScriptContent renders a build.sh / build.ps1 body carrying `version`
// in the exact assignment line shape the script's SH_PATTERN / PS1_PATTERN
// anchor on.
function buildSh(version) {
  return `#!/usr/bin/env bash\nset -euo pipefail\nVERSION="${version}"\n`;
}
function buildPs1(version) {
  return `$ErrorActionPreference = "Stop"\n$version = "${version}"\n`;
}

// versionInfoJSON renders a minimal-but-valid versioninfo.json with the given
// numeric quad + dotted string. `padded` reproduces the committed file's
// hand-aligned single-line quad objects so we can observe the re-serialize.
function versionInfoJSON(major, minor, patch, dotted) {
  return (
    "{\n" +
    '    "IconPath": "mcphub.ico",\n' +
    '    "FixedFileInfo": {\n' +
    `        "FileVersion":    { "Major": ${major}, "Minor": ${minor}, "Patch": ${patch}, "Build": 0 },\n` +
    `        "ProductVersion": { "Major": ${major}, "Minor": ${minor}, "Patch": ${patch}, "Build": 0 }\n` +
    "    },\n" +
    '    "StringFileInfo": {\n' +
    '        "CompanyName":      "Example",\n' +
    `        "FileVersion":      "${dotted}",\n` +
    `        "ProductVersion":   "${dotted}"\n` +
    "    }\n" +
    "}\n"
  );
}

// scaffold materializes a throwaway repo layout and returns its paths.
function scaffold({ pkgVersion, shVersion, ps1Version, viMajor, viMinor, viPatch, viDotted }) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "syncver-test-"));
  const npmDir = path.join(root, "npm");
  const cmdDir = path.join(root, "cmd", "mcphub");
  fs.mkdirSync(npmDir, { recursive: true });
  fs.mkdirSync(cmdDir, { recursive: true });

  const scriptPath = path.join(npmDir, "sync-version.js");
  fs.copyFileSync(REAL_SCRIPT, scriptPath);

  const pkgPath = path.join(npmDir, "package.json");
  fs.writeFileSync(pkgPath, JSON.stringify({ name: "fixture", version: pkgVersion }, null, 2) + "\n");

  const shPath = path.join(root, "build.sh");
  fs.writeFileSync(shPath, buildSh(shVersion));

  const ps1Path = path.join(root, "build.ps1");
  fs.writeFileSync(ps1Path, buildPs1(ps1Version));

  const viPath = path.join(cmdDir, "versioninfo.json");
  fs.writeFileSync(viPath, versionInfoJSON(viMajor, viMinor, viPatch, viDotted));

  return { root, scriptPath, pkgPath, shPath, ps1Path, viPath };
}

function run(scriptPath, ...args) {
  return spawnSync(process.execPath, [scriptPath, ...args], { encoding: "utf8" });
}

function cleanup(root) {
  fs.rmSync(root, { recursive: true, force: true });
}

test("--check passes on a plain X.Y.Z in sync", () => {
  const s = scaffold({
    pkgVersion: "0.5.0",
    shVersion: "0.5.0",
    ps1Version: "0.5.0",
    viMajor: 0,
    viMinor: 5,
    viPatch: 0,
    viDotted: "0.5.0.0",
  });
  try {
    const r = run(s.scriptPath, "--check");
    assert.equal(r.status, 0, `--check should pass; stderr:\n${r.stderr}`);
  } finally {
    cleanup(s.root);
  }
});

test("--check ACCEPTS a prerelease authority version (0.5.0-beta.1) whose quad core is in sync", () => {
  // build.sh/build.ps1 carry the FULL prerelease string; versioninfo.json
  // carries the numeric X.Y.Z core (Windows FILEVERSION cannot encode -beta.N).
  const s = scaffold({
    pkgVersion: "0.5.0-beta.1",
    shVersion: "0.5.0-beta.1",
    ps1Version: "0.5.0-beta.1",
    viMajor: 0,
    viMinor: 5,
    viPatch: 0,
    viDotted: "0.5.0.0",
  });
  try {
    const r = run(s.scriptPath, "--check");
    assert.equal(
      r.status,
      0,
      `--check must NOT reject a prerelease tag; stdout:\n${r.stdout}\nstderr:\n${r.stderr}`,
    );
  } finally {
    cleanup(s.root);
  }
});

test("--inject on a prerelease authority version writes the numeric quad + full string into build scripts", () => {
  const s = scaffold({
    pkgVersion: "0.5.0-beta.1",
    // Start the build scripts + versioninfo drifted, so --inject must rewrite them.
    shVersion: "0.4.0",
    ps1Version: "0.4.0",
    viMajor: 0,
    viMinor: 4,
    viPatch: 0,
    viDotted: "0.4.0.0",
  });
  try {
    const r = run(s.scriptPath, "--inject");
    assert.equal(r.status, 0, `--inject should succeed on a prerelease; stderr:\n${r.stderr}`);

    // build.sh / build.ps1 get the FULL prerelease string (ldflags-only literal).
    const sh = fs.readFileSync(s.shPath, "utf8");
    assert.match(sh, /^VERSION="0\.5\.0-beta\.1"$/m, `build.sh:\n${sh}`);
    const ps1 = fs.readFileSync(s.ps1Path, "utf8");
    assert.match(ps1, /^\$version = "0\.5\.0-beta\.1"$/m, `build.ps1:\n${ps1}`);

    // versioninfo.json gets the numeric X.Y.Z core: quad 0/5/0/0, dotted 0.5.0.0.
    const vi = JSON.parse(fs.readFileSync(s.viPath, "utf8"));
    assert.deepEqual(vi.FixedFileInfo.FileVersion, { Major: 0, Minor: 5, Patch: 0, Build: 0 });
    assert.deepEqual(vi.FixedFileInfo.ProductVersion, { Major: 0, Minor: 5, Patch: 0, Build: 0 });
    assert.equal(vi.StringFileInfo.FileVersion, "0.5.0.0");
    assert.equal(vi.StringFileInfo.ProductVersion, "0.5.0.0");

    // After --inject, --check must now pass (round-trip).
    const rc = run(s.scriptPath, "--check");
    assert.equal(rc.status, 0, `post-inject --check should pass; stderr:\n${rc.stderr}`);
  } finally {
    cleanup(s.root);
  }
});

test("--check still FAILS when a version has no parseable X.Y.Z core", () => {
  const s = scaffold({
    pkgVersion: "beta", // no numeric core at all
    shVersion: "beta",
    ps1Version: "beta",
    viMajor: 0,
    viMinor: 0,
    viPatch: 0,
    viDotted: "0.0.0.0",
  });
  try {
    const r = run(s.scriptPath, "--check");
    assert.notEqual(r.status, 0, `--check must reject a version with no X.Y.Z core`);
    assert.match(r.stderr, /no parseable X\.Y\.Z core/, `stderr:\n${r.stderr}`);
  } finally {
    cleanup(s.root);
  }
});
