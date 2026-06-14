#!/usr/bin/env node
// Version-sync: make npm/package.json the single version authority for the
// whole project, and close the version-skew gap the Go build scripts leave.
//
// Today the version literal is hand-duplicated in three places:
//   * npm/package.json   "version": "X"      (the authority)
//   * build.sh    line 7  VERSION="X"
//   * build.ps1   line 20 $version = "X"
// and the Windows upgrade guard only rejects version=="dev"/"" at runtime, so a
// semver DRIFT between these copies is never caught. This script makes the
// drift a hard build failure.
//
// Modes (exactly one):
//   --check
//       Assert build.sh + build.ps1 already carry npm/package.json's version.
//       Read-only; exits non-zero on any mismatch. This is the CI gate.
//   --inject
//       Rewrite build.sh + build.ps1 to npm/package.json's version (the
//       authority pushes its value down before `go build`). Mutates files.
//   --assert-binary <path-to-mcphub[.exe]>
//       Run `<binary> version`, parse the "mcp-local-hub <ver>" line, and
//       assert it equals npm/package.json's version. This proves the ldflags
//       actually baked the authority version into the artifact.
//
// ZERO Go source is touched in any mode. --inject only rewrites the two build
// scripts' version literal (the documented injection point); the committed
// tree already has them matching, so --inject is a no-op on a clean checkout.

"use strict";

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const NPM_DIR = __dirname;
const REPO_ROOT = path.resolve(NPM_DIR, "..");
const META_PKG_PATH = path.join(NPM_DIR, "package.json");
const BUILD_SH = path.join(REPO_ROOT, "build.sh");
const BUILD_PS1 = path.join(REPO_ROOT, "build.ps1");

// Exact line shapes confirmed at build.sh:7 and build.ps1:20.
//   build.sh:7   VERSION="0.4.5"
//   build.ps1:20 $version = "0.4.5"
// Capture group 1 = the version literal. The patterns are deliberately strict
// (anchored to the assignment) so they match exactly one line and never a
// stray occurrence elsewhere in the script.
const SH_PATTERN = /^(VERSION=")([^"]*)(")$/m;
const PS1_PATTERN = /^(\$version = ")([^"]*)(")$/m;

function authorityVersion() {
  const meta = JSON.parse(fs.readFileSync(META_PKG_PATH, "utf8"));
  if (typeof meta.version !== "string" || meta.version.trim() === "") {
    fail(`npm/package.json has no usable "version" field`);
  }
  return meta.version;
}

function fail(message) {
  process.stderr.write(`sync-version: ${message}\n`);
  process.exit(1);
}

function ok(message) {
  process.stdout.write(`sync-version: ${message}\n`);
}

// Returns the version literal found in `file` via `pattern`, or fails if the
// expected assignment line is absent (the injection point moved — a real
// breakage we want loud, not silent).
function readScriptVersion(file, pattern, label) {
  const text = fs.readFileSync(file, "utf8");
  const m = text.match(pattern);
  if (!m) {
    fail(
      `could not find the version assignment in ${label} ` +
        `(expected a line matching ${pattern}). The injection point moved; ` +
        `update sync-version.js's pattern.`,
    );
  }
  return { text, version: m[2], pattern };
}

function doCheck() {
  const want = authorityVersion();
  const sh = readScriptVersion(BUILD_SH, SH_PATTERN, "build.sh");
  const ps1 = readScriptVersion(BUILD_PS1, PS1_PATTERN, "build.ps1");

  const problems = [];
  if (sh.version !== want) {
    problems.push(`build.sh has VERSION="${sh.version}" (want "${want}")`);
  }
  if (ps1.version !== want) {
    problems.push(`build.ps1 has $version = "${ps1.version}" (want "${want}")`);
  }
  if (problems.length > 0) {
    fail(
      `version drift vs npm/package.json ("${want}"):\n  - ` +
        problems.join("\n  - ") +
        `\nRun \`node npm/sync-version.js --inject\` to realign, ` +
        `or fix npm/package.json.`,
    );
  }
  ok(`build.sh + build.ps1 both match npm/package.json version "${want}"`);
}

function injectInto(file, pattern, label, want) {
  const { text, version } = readScriptVersion(file, pattern, label);
  if (version === want) {
    return false;
  }
  const next = text.replace(pattern, `$1${want}$3`);
  fs.writeFileSync(file, next);
  ok(`${label}: ${version} -> ${want}`);
  return true;
}

function doInject() {
  const want = authorityVersion();
  const a = injectInto(BUILD_SH, SH_PATTERN, "build.sh", want);
  const b = injectInto(BUILD_PS1, PS1_PATTERN, "build.ps1", want);
  if (!a && !b) {
    ok(`build scripts already at "${want}" — nothing to inject`);
  }
}

// Capture `<binary> version` output. On Windows the shipped binary is linked
// `-H windowsgui` and calls attachParentConsoleIfAvailable() at startup
// (cmd/mcphub/main.go); it then writes to the *attached console device*, NOT
// to an anonymous stdout pipe. So a piped capture (execFileSync default) comes
// back EMPTY on Windows even though the text is visible on the terminal. A
// real file handle, however, DOES receive the bytes. So we redirect the
// child's stdout+stderr to a temp file and read it back — this works for both
// the console-subsystem and the windowsgui-subsystem binary, on every OS.
function captureVersionOutput(binPath) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "mcphub-ver-"));
  const outFile = path.join(tmp, "out.txt");
  let fd;
  try {
    fd = fs.openSync(outFile, "w");
    const result = spawnSync(binPath, ["version"], {
      stdio: ["ignore", fd, fd],
      shell: false,
      windowsHide: true,
    });
    fs.closeSync(fd);
    fd = undefined;
    if (result.error) {
      fail(`failed to run "${binPath} version": ${result.error.message}`);
    }
    return fs.readFileSync(outFile, "utf8");
  } finally {
    if (fd !== undefined) {
      try {
        fs.closeSync(fd);
      } catch {
        /* already closed */
      }
    }
    fs.rmSync(tmp, { recursive: true, force: true });
  }
}

function doAssertBinary(binPath) {
  if (!binPath) {
    fail(`--assert-binary requires a path to the built mcphub binary`);
  }
  const want = authorityVersion();
  const out = captureVersionOutput(binPath);
  // First line of `mcphub version` is `mcp-local-hub <ver>` (see
  // internal/cli/version.go: cmd.Printf("mcp-local-hub %s\n", version)).
  const m = out.match(/^mcp-local-hub\s+(\S+)/m);
  if (!m) {
    fail(
      `could not parse a version from the binary output:\n${out}\n` +
        `(expected a line "mcp-local-hub <version>")`,
    );
  }
  const got = m[1];
  if (got === "dev") {
    fail(
      `binary reports version "dev" — it was built without the ldflags ` +
        `injection (run build.sh / build.ps1, not a bare \`go build\`).`,
    );
  }
  if (got !== want) {
    fail(
      `binary version "${got}" != npm/package.json authority "${want}". ` +
        `The build scripts were not synced before \`go build\` ` +
        `(run \`node npm/sync-version.js --inject\` first).`,
    );
  }
  ok(`binary version "${got}" matches npm/package.json authority "${want}"`);
}

function main() {
  const args = process.argv.slice(2);
  const mode = args[0];
  switch (mode) {
    case "--check":
      doCheck();
      break;
    case "--inject":
      doInject();
      break;
    case "--assert-binary":
      doAssertBinary(args[1]);
      break;
    default:
      process.stderr.write(
        "usage: node npm/sync-version.js (--check | --inject | " +
          "--assert-binary <path>)\n",
      );
      process.exit(2);
  }
}

main();
