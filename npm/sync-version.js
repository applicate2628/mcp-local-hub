#!/usr/bin/env node
// Version-sync: make npm/package.json the single version authority for the
// whole project, and close the version-skew gap the Go build scripts (and
// the Windows PE resource) leave.
//
// Today the version literal is hand-duplicated in four places:
//   * npm/package.json          "version": "X"      (the authority)
//   * build.sh           line 7  VERSION="X"
//   * build.ps1          line 20 $version = "X"
//   * cmd/mcphub/versioninfo.json  FixedFileInfo.{FileVersion,ProductVersion}
//     (Major/Minor/Patch/Build quads) + StringFileInfo.{FileVersion,
//     ProductVersion} (X.Y.Z.0 strings) — the source goversioninfo compiles
//     into cmd/mcphub/resource.syso, which the Windows PE embeds as its
//     FileVersion/ProductVersion resource (visible in Explorer "Details" /
//     any SBOM tool, independent of `mcphub version`'s ldflags string).
// and the Windows upgrade guard only rejects version=="dev"/"" at runtime, so a
// semver DRIFT between these copies is never caught. This script makes the
// drift a hard build failure.
//
// Modes (exactly one):
//   --check
//       Assert build.sh + build.ps1 + versioninfo.json already carry
//       npm/package.json's version. Read-only; exits non-zero on any
//       mismatch. This is the CI gate.
//   --inject
//       Rewrite build.sh + build.ps1 + versioninfo.json to npm/package.json's
//       version (the authority pushes its value down before `go build`).
//       Mutates files. versioninfo.json changes still require a follow-up
//       `go generate ./cmd/mcphub` to regenerate resource.syso — this script
//       does not invoke goversioninfo itself.
//   --assert-binary <path-to-mcphub[.exe]>
//       Run `<binary> version`, parse the "mcp-local-hub <ver>" line, and
//       assert it equals npm/package.json's version. This proves the ldflags
//       actually baked the authority version into the artifact.
//
// ZERO Go source is touched in any mode. --inject only rewrites the two build
// scripts' version literal plus versioninfo.json's version fields (the
// documented injection points); the committed tree already has them
// matching, so --inject is a no-op on a clean checkout.

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
const VERSIONINFO_JSON = path.join(REPO_ROOT, "cmd", "mcphub", "versioninfo.json");

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

// authorityVersionQuad parses the authority's X.Y.Z[-prerelease][+build]
// semver into the {Major, Minor, Patch, Build} quad goversioninfo's
// FixedFileInfo expects, plus the X.Y.Z.0 dotted-string form StringFileInfo
// expects. Build is always 0 — the authority version has no fourth numeric
// component and none of the existing three duplicate copies
// (build.sh/build.ps1/package.json) carry one either.
//
// A prerelease/build-metadata suffix (e.g. "0.5.0-beta.1") is TOLERATED: the
// publish workflow tags prereleases as `vX.Y.Z-beta.N` and routes them to the
// `beta` dist-tag, so `--check`/`--inject` must not reject them. A Windows
// FILEVERSION resource can only encode four 16-bit integers — it cannot carry
// a `-beta.N` suffix — so the quad and the dotted string use the numeric
// X.Y.Z core (Build stays 0). This mirrors build.sh/build.ps1, which write
// the full version string verbatim into their ldflags-only VERSION literal
// (accepting any suffix) while the PE resource comes from this numeric core.
// The `mcphub version` ldflags string still carries the full "-beta.N"
// suffix; only the Explorer-"Details"/SBOM PE resource is the numeric core.
// Fails loudly only when the X.Y.Z core itself is absent/malformed.
function authorityVersionQuad(want) {
  // Match the leading numeric X.Y.Z core; ignore any `-prerelease`/`+build`
  // suffix (SemVer 2.0 §9/§10) — the PE resource cannot encode it.
  const m = want.match(/^(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/);
  if (!m) {
    fail(
      `npm/package.json version "${want}" has no parseable X.Y.Z core; ` +
        `cannot derive a Windows FixedFileInfo quad for versioninfo.json. ` +
        `(A leading numeric "MAJOR.MINOR.PATCH" is required; an optional ` +
        `"-prerelease"/"+build" suffix is tolerated and contributes nothing ` +
        `to the PE resource version.)`,
    );
  }
  const [, major, minor, patch] = m;
  return {
    major: Number(major),
    minor: Number(minor),
    patch: Number(patch),
    build: 0,
    dotted: `${major}.${minor}.${patch}.0`,
  };
}

// readVersionInfoJSON parses cmd/mcphub/versioninfo.json and returns both
// the raw parsed object (for --inject's write-back) and the four version
// sub-fields --check/--inject care about: the two FixedFileInfo quads
// (FileVersion/ProductVersion) and the two StringFileInfo dotted strings.
function readVersionInfoJSON() {
  let raw;
  try {
    raw = fs.readFileSync(VERSIONINFO_JSON, "utf8");
  } catch (e) {
    fail(`could not read ${VERSIONINFO_JSON}: ${e.message}`);
  }
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch (e) {
    fail(`could not parse ${VERSIONINFO_JSON} as JSON: ${e.message}`);
  }
  const fixed = parsed.FixedFileInfo;
  const strings_ = parsed.StringFileInfo;
  if (!fixed || !fixed.FileVersion || !fixed.ProductVersion) {
    fail(`${VERSIONINFO_JSON} is missing FixedFileInfo.FileVersion/ProductVersion`);
  }
  if (!strings_ || typeof strings_.FileVersion !== "string" || typeof strings_.ProductVersion !== "string") {
    fail(`${VERSIONINFO_JSON} is missing StringFileInfo.FileVersion/ProductVersion`);
  }
  return { parsed, fixed, strings: strings_ };
}

function quadEquals(quad, want) {
  return (
    quad.Major === want.major &&
    quad.Minor === want.minor &&
    quad.Patch === want.patch &&
    quad.Build === want.build
  );
}

function doCheck() {
  const want = authorityVersion();
  const wantQuad = authorityVersionQuad(want);
  const sh = readScriptVersion(BUILD_SH, SH_PATTERN, "build.sh");
  const ps1 = readScriptVersion(BUILD_PS1, PS1_PATTERN, "build.ps1");
  const { fixed, strings: strFileInfo } = readVersionInfoJSON();

  const problems = [];
  if (sh.version !== want) {
    problems.push(`build.sh has VERSION="${sh.version}" (want "${want}")`);
  }
  if (ps1.version !== want) {
    problems.push(`build.ps1 has $version = "${ps1.version}" (want "${want}")`);
  }
  if (!quadEquals(fixed.FileVersion, wantQuad)) {
    problems.push(
      `versioninfo.json FixedFileInfo.FileVersion is ` +
        `${JSON.stringify(fixed.FileVersion)} (want Major/Minor/Patch/Build ` +
        `${wantQuad.major}/${wantQuad.minor}/${wantQuad.patch}/${wantQuad.build})`,
    );
  }
  if (!quadEquals(fixed.ProductVersion, wantQuad)) {
    problems.push(
      `versioninfo.json FixedFileInfo.ProductVersion is ` +
        `${JSON.stringify(fixed.ProductVersion)} (want Major/Minor/Patch/Build ` +
        `${wantQuad.major}/${wantQuad.minor}/${wantQuad.patch}/${wantQuad.build})`,
    );
  }
  if (strFileInfo.FileVersion !== wantQuad.dotted) {
    problems.push(
      `versioninfo.json StringFileInfo.FileVersion is "${strFileInfo.FileVersion}" ` +
        `(want "${wantQuad.dotted}")`,
    );
  }
  if (strFileInfo.ProductVersion !== wantQuad.dotted) {
    problems.push(
      `versioninfo.json StringFileInfo.ProductVersion is "${strFileInfo.ProductVersion}" ` +
        `(want "${wantQuad.dotted}")`,
    );
  }
  if (problems.length > 0) {
    fail(
      `version drift vs npm/package.json ("${want}"):\n  - ` +
        problems.join("\n  - ") +
        `\nRun \`node npm/sync-version.js --inject\` to realign, ` +
        `or fix npm/package.json. After --inject touches versioninfo.json, ` +
        `also run \`go generate ./cmd/mcphub\` to regenerate resource.syso.`,
    );
  }
  ok(
    `build.sh + build.ps1 + versioninfo.json all match npm/package.json ` +
      `version "${want}"`,
  );
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

// injectVersionInfoJSON rewrites versioninfo.json's four version fields
// (FixedFileInfo.{FileVersion,ProductVersion} quads + StringFileInfo.
// {FileVersion,ProductVersion} dotted strings) to the authority version.
// Every other field's VALUE (IconPath, FileFlags, CompanyName,
// FileDescription, etc.) is preserved, and key ORDER is preserved because
// JSON.stringify emits keys in the object's insertion order (which JSON.parse
// preserves from the source). It does NOT preserve the committed file's
// hand-alignment/whitespace: it FULLY re-serializes the parsed object via
// `JSON.stringify(parsed, null, 4)`, so the single-line quad objects in the
// committed source (`"FileVersion":    { "Major": 0, ... }`) are expanded into
// multi-line 4-space-indented JSON. That is fine — versioninfo.json is a
// build input consumed by goversioninfo, not a hand-formatted artifact.
// Returns true if anything changed. Mirrors injectInto's shape: read,
// compare, write only on drift, ok()-log the from->to transition.
function injectVersionInfoJSON(want, wantQuad) {
  const { parsed, fixed, strings: strFileInfo } = readVersionInfoJSON();
  let changed = false;
  const before = {
    fileVersionQuad: { ...fixed.FileVersion },
    productVersionQuad: { ...fixed.ProductVersion },
    fileVersionStr: strFileInfo.FileVersion,
    productVersionStr: strFileInfo.ProductVersion,
  };

  if (!quadEquals(fixed.FileVersion, wantQuad)) {
    fixed.FileVersion = { Major: wantQuad.major, Minor: wantQuad.minor, Patch: wantQuad.patch, Build: wantQuad.build };
    changed = true;
  }
  if (!quadEquals(fixed.ProductVersion, wantQuad)) {
    fixed.ProductVersion = { Major: wantQuad.major, Minor: wantQuad.minor, Patch: wantQuad.patch, Build: wantQuad.build };
    changed = true;
  }
  if (strFileInfo.FileVersion !== wantQuad.dotted) {
    strFileInfo.FileVersion = wantQuad.dotted;
    changed = true;
  }
  if (strFileInfo.ProductVersion !== wantQuad.dotted) {
    strFileInfo.ProductVersion = wantQuad.dotted;
    changed = true;
  }

  if (!changed) {
    return false;
  }

  fs.writeFileSync(VERSIONINFO_JSON, JSON.stringify(parsed, null, 4) + "\n");
  ok(
    `versioninfo.json: FileVersion ${JSON.stringify(before.fileVersionQuad)}/"${before.fileVersionStr}" -> ` +
      `${JSON.stringify(fixed.FileVersion)}/"${strFileInfo.FileVersion}"`,
  );
  return true;
}

function doInject() {
  const want = authorityVersion();
  const wantQuad = authorityVersionQuad(want);
  const a = injectInto(BUILD_SH, SH_PATTERN, "build.sh", want);
  const b = injectInto(BUILD_PS1, PS1_PATTERN, "build.ps1", want);
  const c = injectVersionInfoJSON(want, wantQuad);
  if (!a && !b && !c) {
    ok(`build scripts + versioninfo.json already at "${want}" — nothing to inject`);
  }
  if (c) {
    ok(
      `versioninfo.json changed — run \`go generate ./cmd/mcphub\` to ` +
        `regenerate resource.syso before building`,
    );
  }
}

// Capture `<binary> version` output through real redirected file handles. The
// Windows product is GUI-subsystem and ordinary launches never attach or
// allocate a console, so preserving inherited redirected handles is the stable
// machine-readable contract across all hosts.
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
