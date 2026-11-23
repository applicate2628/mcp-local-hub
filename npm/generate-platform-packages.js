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
// Native payloads are NOT committed and NOT produced here. The release job
// stages the admitted Windows entry, Product, identity, and runtime payload
// in the platform package before packing it.
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
const WINIO_LICENSE_PATH = path.join(NPM_DIR, "..", "third_party", "go-winio", "LICENSE");
// Same ordered descriptor table as the Go V4 registry contract.
const WINDOWS_RUNTIME_DESCRIPTOR_PATHS = [
  "runtimes/python/release.yaml",
  "runtimes/uv/release.yaml",
];
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
  const product = npmPayload.find((artifact) => artifact.role === "product");
  const launcher = npmPayload.find((artifact) => artifact.role === "launcher");
  if (
    artifacts.length !== 2 ||
    npmPayload.length !== 2 ||
    npmBins.length !== 0 ||
    !product || !launcher ||
    product.filename !== "mcphub-product/mcphub.exe" ||
    product.source !== "./cmd/mcphub" ||
    product.subsystem !== 2 || product.productMetadata !== true || product.npmBin !== false ||
    launcher.filename !== "mcphub.exe" ||
    launcher.source !== "./cmd/mcphub-launcher" ||
    launcher.subsystem !== 3 || launcher.productMetadata !== false || launcher.npmBin !== false ||
    identityManifest.filename !== "windows-product-identity.json"
  ) {
    throw new Error(
      "windows-product-artifacts.json must declare the CUI entry/nested GUI product, no platform bin, and the canonical identity filename",
    );
  }
  if (
    contract.schemaVersion === 3 &&
    identityManifest.schema === "windows-product-identity-v1" &&
    !Object.hasOwn(contract, "runtimePayload")
  ) {
    return { npmPayload, identityManifest, identityPath: identityManifest.filename, runtime: null };
  }

  const productDir = path.posix.dirname(product.filename);
  const runtime = contract.runtimePayload;
  const releaseDescriptors = runtime?.releaseDescriptors;
  if (
    contract.schemaVersion !== 4 ||
    identityManifest.schema !== "windows-product-identity-v2" ||
    !runtime ||
    runtime.schema !== "hub-runtime-payload-v1" ||
    runtime.filename !== path.posix.join(productDir, "runtime-payload.json") ||
    !Array.isArray(releaseDescriptors) ||
    releaseDescriptors.length !== WINDOWS_RUNTIME_DESCRIPTOR_PATHS.length ||
    WINDOWS_RUNTIME_DESCRIPTOR_PATHS.some((name, index) => releaseDescriptors[index] !== name)
  ) {
    throw new Error(
      "windows-product-artifacts.json must declare schemaVersion 3/V1 identity or schemaVersion 4/V2 identity with the canonical runtime payload and ordered release descriptors",
    );
  }
  return {
    npmPayload,
    identityManifest,
    identityPath: path.posix.join(productDir, identityManifest.filename),
    runtime: { indexPath: runtime.filename, treePath: productDir + "/runtimes/", releaseDescriptors },
    productPath: product.filename,
  };
}

function tierBlurb(tier) {
  return tier === "GA"
    ? "GA (generally available)"
    : "best-effort (the CLI runs; supervisor lifecycle is Windows-GA / Linux-beta / macOS-preview)";
}

function subPackageJson(meta, target, windowsContract) {
  const { nodeOs, nodeCpu, goos, goarch, tier } = target;
  const name = platformPackageName(nodeOs, nodeCpu);
  // Stable key order so output is deterministic across Node versions.
  const manifest = {
    name,
    version: meta.version,
    description:
      `mcphub binary for ${nodeOs}/${nodeCpu} (Go ${goos}/${goarch}) — ` +
      `${tierBlurb(tier)}. Platform sub-package of the "mcp-local-hub" meta package; ` +
      `not installed directly.`,
    license: windowsContract?.runtime ? "SEE LICENSE IN README.md" : meta.license,
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
  } else if (windowsContract.runtime) {
    manifest.mcphubProduct = `bin/${windowsContract.productPath}`;
  }
  manifest.files = windowsContract
    ? [
        ...windowsContract.npmPayload.map((artifact) => `bin/${artifact.filename}`),
        `bin/${windowsContract.identityPath}`,
        ...(windowsContract.runtime
          ? [`bin/${windowsContract.runtime.indexPath}`, `bin/${windowsContract.runtime.treePath}`]
          : []),
        "README.md",
      ]
    : [`bin/${binaryBasename(nodeOs)}`, "README.md"];
  manifest.engines = meta.engines;
  return manifest;
}

function winioLicenseNotice() {
  const original = fs.readFileSync(WINIO_LICENSE_PATH);
  const license = original.toString("utf8");
  if (license.trim() === "" || !original.equals(Buffer.from(license, "utf8"))) {
    throw new Error("go-winio LICENSE must contain valid UTF-8 text");
  }
  return (
    "The Windows Product includes go-winio under the MIT License.\n\n" +
    "```text\n" +
    license +
    (license.endsWith("\n") ? "" : "\n") +
    "```\n\n"
  );
}

function subPackageReadme(target, windowsContract) {
  const { nodeOs, nodeCpu, goos, goarch, tier } = target;
  const name = platformPackageName(nodeOs, nodeCpu);
  const bin = binaryBasename(nodeOs);
  const payloadDescription = windowsContract
    ? windowsContract.npmPayload
        .map(
          (artifact) =>
            `  - \`bin/${artifact.filename}\` (${artifact.role}, PE subsystem ${artifact.subsystem})\n`,
        )
        .join("") +
      `  - \`bin/${windowsContract.identityPath}\` ` +
      `(${windowsContract.identityManifest.schema}, non-executable admitted identity)\n` +
      (windowsContract.runtime
        ? `  - \`bin/${windowsContract.runtime.indexPath}\` and complete declared Python/uv generations\n`
        : "") +
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
    `Generated by \`npm/generate-platform-packages.js\` — do not edit by hand.\n` +
    (windowsContract
      ? `\n## Licenses\n\n` +
        `Hub code retains MPL-2.0.` +
        (windowsContract.runtime
          ? ` Bundled components retain their original licenses. ` +
            `The runtime index points to each immutable release descriptor and inventory; ` +
            `each descriptor lists the original license files shipped in its component root.\n\n`
          : "\n\n") +
        winioLicenseNotice() +
        (windowsContract.runtime
          ? `## Terms and Abbreviations\n\n` +
            `MCP stands for Model Context Protocol. npm distributes Node packages. PE is the Windows executable format.\n`
          : "")
      : "")
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

  const outputs = TARGETS.map((target) => {
    const directoryName = `${target.nodeOs}-${target.nodeCpu}`;
    const windowsContract = target.nodeOs === "win32" ? readWindowsArtifactContract() : null;
    const readme = subPackageReadme(target, windowsContract);
    return { directoryName, packageJson: stringify(subPackageJson(meta, target, windowsContract)), readme };
  });

  // A retired target directory is still a publishable package source. Fail
  // instead of silently leaving it beside the current generated release set.
  fs.mkdirSync(PACKAGES_DIR, { recursive: true });
  const expectedDirs = new Set(outputs.map(({ directoryName }) => directoryName));
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
  for (const { directoryName, packageJson, readme } of outputs) {
    const dir = path.join(PACKAGES_DIR, directoryName);
    const pkgPath = path.join(dir, "package.json");
    const readmePath = path.join(dir, "README.md");
    const gitkeepPath = path.join(dir, "bin", ".gitkeep");

    if (writeIfChanged(pkgPath, packageJson)) changed++;
    if (writeIfChanged(readmePath, readme)) changed++;
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

if (require.main === module) {
  main();
}

module.exports = { readWindowsArtifactContract };
