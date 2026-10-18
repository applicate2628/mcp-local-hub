#!/usr/bin/env node
// Generator for the release's `mcphub-<platform>-<arch>` sub-packages.
//
// The platform sub-packages are NEVER hand-edited. This generator emits each
// `npm/packages/<platform>-<arch>/package.json` (+ a README and a .gitkeep)
// from a SINGLE source of truth:
//   * the GOOS/GOARCH -> Node os/cpu map below, and
//   * the `version` field of npm/package.json (the version authority).
//
// Re-run after bumping the version (or via npm/sync-version.js, which sets the
// version first) so the generated package.json files stay byte-identical to a
// fresh generation. CI re-runs this and asserts `git diff --exit-code` is
// clean, so a hand-edit or a missed regeneration fails the build.
//
// Native payloads are NOT committed and NOT produced here — the release job
// drops `mcphub` on POSIX, or admitted `mcphub.exe` plus its generated
// identity manifest on Windows, into each sub-package's bin/ at publish time.
// Until then each bin/ holds only a .gitkeep.

"use strict";

const fs = require("node:fs");
const path = require("node:path");

// Single source of truth: GOOS/GOARCH <-> Node platform/cpu, plus the
// support tier. `goos`/`goarch` are the names build.sh / build.ps1 cross-
// compile for; `nodeOs`/`nodeCpu` are what Node reports and what npm matches
// against the sub-package `os`/`cpu` arrays at install time.
const PACKAGE_SCOPE = "@applicate2628";

// The platform sub-package name. The meta package (`mcp-local-hub`) is
// UNSCOPED so the install command stays short; platform packages are
// SCOPED under the publisher namespace so a brand-new account publishing a
// family of near-identical names does not trip npm's spam-detection heuristic
// (the esbuild / swc / turbo layout). Single source of truth for the shape.
function platformPackageName(nodeOs, nodeCpu) {
  return `${PACKAGE_SCOPE}/mcp-local-hub-${nodeOs}-${nodeCpu}`;
}

const TARGETS = [
  { nodeOs: "win32", nodeCpu: "x64", goos: "windows", goarch: "amd64", tier: "GA" },
];

const NPM_DIR = __dirname;
const PACKAGES_DIR = path.join(NPM_DIR, "packages");
const META_PKG_PATH = path.join(NPM_DIR, "package.json");
const WINDOWS_ARTIFACT_CONTRACT_PATH = path.join(
  NPM_DIR,
  "..",
  "windows-product-artifacts.json",
);

function readMeta() {
  const raw = fs.readFileSync(META_PKG_PATH, "utf8");
  return JSON.parse(raw);
}

function binaryBasename(nodeOs) {
  return nodeOs === "win32" ? "mcphub.exe" : "mcphub";
}

function readWindowsArtifactContract() {
  const contract = JSON.parse(fs.readFileSync(WINDOWS_ARTIFACT_CONTRACT_PATH, "utf8"));
  const artifacts = Array.isArray(contract.artifacts) ? contract.artifacts : [];
  const npmPayload = artifacts.filter((artifact) => artifact.npmPayload === true);
  const npmBins = npmPayload.filter((artifact) => artifact.npmBin === true);
  const identityManifest = contract.identityManifest || {};
  if (
    contract.schemaVersion !== 2 ||
    artifacts.length !== 1 ||
    npmPayload.length !== 1 ||
    npmBins.length !== 0 ||
    npmPayload[0].role !== "product" ||
    npmPayload[0].filename !== "mcphub.exe" ||
    npmPayload[0].subsystem !== 2 ||
    identityManifest.filename !== "windows-product-identity.json" ||
    identityManifest.schema !== "windows-product-identity-v1"
  ) {
    throw new Error(
      "windows-product-artifacts.json must declare schemaVersion 2, one GUI product, no platform bin, and the V1 identity manifest",
    );
  }
  return { npmPayload, identityManifest };
}

function tierBlurb(tier) {
  return tier === "GA"
    ? "GA (generally available)"
    : "best-effort (the CLI runs; supervisor lifecycle is Windows-GA / Linux-beta / macOS-preview)";
}

function subPackageJson(meta, target) {
  const { nodeOs, nodeCpu, goos, goarch, tier } = target;
  const name = platformPackageName(nodeOs, nodeCpu);
  const windowsContract = nodeOs === "win32" ? readWindowsArtifactContract() : null;
  // Stable key order so output is deterministic across Node versions.
  const manifest = {
    name,
    version: meta.version,
    description:
      `mcphub binary for ${nodeOs}/${nodeCpu} (Go ${goos}/${goarch}) — ` +
      `${tierBlurb(tier)}. Platform sub-package of the "mcp-local-hub" meta package; ` +
      `not installed directly.`,
    license: meta.license,
    author: meta.author,
    homepage: meta.homepage,
    repository: {
      type: meta.repository.type,
      url: meta.repository.url,
      directory: `npm/packages/${nodeOs}-${nodeCpu}`,
    },
    // Scoped packages default to restricted (private) access; declare public
    // so the sub-package is publishable-public on its own, not solely via the
    // publish step's `--access public` flag (defense in depth).
    publishConfig: { access: "public" },
    os: [nodeOs],
    cpu: [nodeCpu],
  };
  if (!windowsContract) {
    manifest.bin = { mcphub: `bin/${binaryBasename(nodeOs)}` };
  }
  manifest.files = windowsContract
    ? [
        ...windowsContract.npmPayload.map((artifact) => `bin/${artifact.filename}`),
        `bin/${windowsContract.identityManifest.filename}`,
        "README.md",
      ]
    : [`bin/${binaryBasename(nodeOs)}`, "README.md"];
  manifest.engines = meta.engines;
  return manifest;
}

function subPackageReadme(meta, target) {
  const { nodeOs, nodeCpu, goos, goarch, tier } = target;
  const name = platformPackageName(nodeOs, nodeCpu);
  const windowsContract = nodeOs === "win32" ? readWindowsArtifactContract() : null;
  const bin = binaryBasename(nodeOs);
  const payloadDescription = windowsContract
    ? windowsContract.npmPayload
        .map(
          (artifact) =>
            `  - \`bin/${artifact.filename}\` (${artifact.role}, PE subsystem ${artifact.subsystem})\n`,
        )
        .join("") +
      `  - \`bin/${windowsContract.identityManifest.filename}\` ` +
      `(${windowsContract.identityManifest.schema}, non-executable admitted identity)\n` +
      `- This Windows platform package deliberately declares no npm \`bin\`; ` +
      `the meta package alone owns the public \`mcphub\` Node shim so it can wait, ` +
      `relay standard streams, and return the native child's status.\n\n`
    : `- Binary: \`bin/${bin}\` — injected by the release job at publish time, ` +
      `NOT committed to git.\n\n`;
  return (
    `# ${name}\n\n` +
    `Platform binary sub-package for [\`mcp-local-hub\`](https://www.npmjs.com/package/mcp-local-hub).\n\n` +
    `- Target: \`${nodeOs}/${nodeCpu}\` (Go \`${goos}/${goarch}\`)\n` +
    `- Support tier: **${tierBlurb(tier)}**\n` +
    (windowsContract
      ? `- Native payload (injected by the release job; NOT committed to git):\n`
      : "") +
    payloadDescription +
    `You do not install this package directly. The \`mcp-local-hub\` meta package ` +
    `declares it in \`optionalDependencies\`; npm installs only the ` +
    `sub-package matching your host's \`os\`/\`cpu\`.\n\n` +
    `Generated by \`npm/generate-platform-packages.js\` — do not edit by hand.\n`
  );
}

// Deterministic JSON: 2-space indent + trailing newline (matches what
// Prettier / npm emit, so `git diff` stays clean).
function stringify(obj) {
  return JSON.stringify(obj, null, 2) + "\n";
}

function writeIfChanged(filePath, contents) {
  let prior = null;
  try {
    prior = fs.readFileSync(filePath, "utf8");
  } catch {
    prior = null;
  }
  if (prior === contents) {
    return false;
  }
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, contents);
  return true;
}

function main() {
  const meta = readMeta();

  // Guard: the meta's optionalDependencies must list exactly the targets we
  // generate, all pinned to the meta version. Drift here is a packaging bug.
  const expectedDeps = {};
  for (const t of TARGETS) {
    expectedDeps[platformPackageName(t.nodeOs, t.nodeCpu)] = meta.version;
  }
  const actualDeps = meta.optionalDependencies || {};
  const expectedKeys = Object.keys(expectedDeps).sort();
  const actualKeys = Object.keys(actualDeps).sort();
  const mismatch =
    expectedKeys.join(",") !== actualKeys.join(",") ||
    expectedKeys.some((k) => actualDeps[k] !== expectedDeps[k]);
  if (mismatch) {
    process.stderr.write(
      "generate-platform-packages: meta package.json optionalDependencies " +
        "do not match the generated targets/version.\n" +
        `  expected: ${JSON.stringify(expectedDeps)}\n` +
        `  actual:   ${JSON.stringify(actualDeps)}\n`,
    );
    process.exit(1);
  }

  // A retired target directory is still a publishable package source. Fail
  // instead of silently leaving it beside the current generated release set.
  fs.mkdirSync(PACKAGES_DIR, { recursive: true });
  const expectedDirs = new Set(TARGETS.map((t) => `${t.nodeOs}-${t.nodeCpu}`));
  const unexpectedDirs = fs
    .readdirSync(PACKAGES_DIR, { withFileTypes: true })
    .filter((entry) => entry.isDirectory() && !expectedDirs.has(entry.name))
    .map((entry) => entry.name)
    .sort();
  if (unexpectedDirs.length > 0) {
    process.stderr.write(
      `generate-platform-packages: unexpected generated target directory: ${unexpectedDirs.join(", ")}\n`,
    );
    process.exit(1);
  }

  let changed = 0;
  for (const target of TARGETS) {
    const dir = path.join(PACKAGES_DIR, `${target.nodeOs}-${target.nodeCpu}`);
    const pkgPath = path.join(dir, "package.json");
    const readmePath = path.join(dir, "README.md");
    const gitkeepPath = path.join(dir, "bin", ".gitkeep");

    if (writeIfChanged(pkgPath, stringify(subPackageJson(meta, target)))) changed++;
    if (writeIfChanged(readmePath, subPackageReadme(meta, target))) changed++;
    if (
      writeIfChanged(
        gitkeepPath,
        "# Binary injected by the release job at publish time; not committed.\n",
      )
    )
      changed++;
  }

  process.stdout.write(
    `generate-platform-packages: ${TARGETS.length} targets, ` +
      `${changed} file(s) written (version ${meta.version}).\n`,
  );
}

main();
