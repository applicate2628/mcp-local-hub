"use strict";

const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const ROOT = path.resolve(__dirname, "..");
const SCRATCH = path.join(ROOT, ".scratch");

function withScratch(prefix, fn) {
  fs.mkdirSync(SCRATCH, { recursive: true });
  const dir = fs.mkdtempSync(path.join(SCRATCH, prefix));
  try {
    return fn(dir);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function generateFixture(fixtureRoot) {
  const npmDir = path.join(fixtureRoot, "npm");
  fs.mkdirSync(npmDir, { recursive: true });
  fs.copyFileSync(
    path.join(__dirname, "generate-platform-packages.js"),
    path.join(npmDir, "generate-platform-packages.js"),
  );
  fs.copyFileSync(path.join(__dirname, "package.json"), path.join(npmDir, "package.json"));
  const contract = path.join(ROOT, "windows-product-artifacts.json");
  if (fs.existsSync(contract)) {
    fs.copyFileSync(contract, path.join(fixtureRoot, "windows-product-artifacts.json"));
  }
  const result = spawnSync(process.execPath, [path.join(npmDir, "generate-platform-packages.js")], {
    cwd: fixtureRoot,
    encoding: "utf8",
  });
  assert.strictEqual(result.status, 0, result.stderr || result.stdout);
  return npmDir;
}

test("generator emits the exact three-file Windows native payload and canonical npm bin", () => {
  withScratch("npm-platform-generator-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    for (const target of ["win32-x64", "win32-arm64"]) {
      const manifest = JSON.parse(
        fs.readFileSync(path.join(npmDir, "packages", target, "package.json"), "utf8"),
      );
      assert.deepStrictEqual(manifest.bin, { mcphub: "bin/mcphub.exe" });
      assert.deepStrictEqual(manifest.files, [
        "bin/mcphub.exe",
        "bin/mcphub-windowless.exe",
        "bin/mcphub-pe-admit.exe",
        "README.md",
      ]);
    }
  });
});

test("generator preserves the exact non-Windows payload shape", () => {
  withScratch("npm-platform-nonwindows-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    for (const target of ["darwin-x64", "darwin-arm64", "linux-x64", "linux-arm64"]) {
      const manifest = JSON.parse(
        fs.readFileSync(path.join(npmDir, "packages", target, "package.json"), "utf8"),
      );
      assert.deepStrictEqual(manifest.bin, { mcphub: "bin/mcphub" });
      assert.deepStrictEqual(manifest.files, ["bin/mcphub", "README.md"]);
    }
  });
});

test("npm pack dry-run exposes exactly the declared native payload for every target", () => {
  withScratch("npm-platform-pack-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    const npmCli = process.env.npm_execpath;
    assert.ok(npmCli, "npm_execpath is required for the package dry-run fixture");
    for (const target of [
      "win32-x64",
      "win32-arm64",
      "darwin-x64",
      "darwin-arm64",
      "linux-x64",
      "linux-arm64",
    ]) {
      const packageDir = path.join(npmDir, "packages", target);
      const manifest = JSON.parse(fs.readFileSync(path.join(packageDir, "package.json"), "utf8"));
      const expectedNative = manifest.files.filter((entry) => entry.startsWith("bin/")).sort();
      for (const relative of expectedNative) {
        const nativePath = path.join(packageDir, ...relative.split("/"));
        fs.mkdirSync(path.dirname(nativePath), { recursive: true });
        fs.writeFileSync(nativePath, `synthetic payload for ${target}/${relative}\n`);
      }
      const packed = spawnSync(process.execPath, [npmCli, "pack", "--dry-run", "--json"], {
        cwd: packageDir,
        encoding: "utf8",
      });
      assert.strictEqual(packed.status, 0, packed.stderr || packed.stdout);
      const result = JSON.parse(packed.stdout);
      assert.strictEqual(result.length, 1);
      const actualNative = result[0].files
        .map((entry) => entry.path)
        .filter((entry) => entry.startsWith("bin/"))
        .sort();
      assert.deepStrictEqual(actualNative, expectedNative, target);
    }
  });
});

test("Windows artifact contract freezes filenames, roles, sources, subsystems, and metadata policy", () => {
  const contract = JSON.parse(
    fs.readFileSync(path.join(ROOT, "windows-product-artifacts.json"), "utf8"),
  );
  assert.deepStrictEqual(contract, {
    schemaVersion: 1,
    artifacts: [
      {
        role: "cli",
        filename: "mcphub.exe",
        source: "./cmd/mcphub",
        subsystem: 3,
        productMetadata: true,
        npmPayload: true,
        npmBin: true,
      },
      {
        role: "windowless",
        filename: "mcphub-windowless.exe",
        source: "./cmd/mcphub-windowless",
        subsystem: 2,
        productMetadata: true,
        npmPayload: true,
        npmBin: false,
      },
      {
        role: "admission-helper",
        filename: "mcphub-pe-admit.exe",
        source: "./cmd/mcphub-pe-admit",
        subsystem: 3,
        productMetadata: false,
        npmPayload: true,
        npmBin: false,
      },
    ],
  });
});

test("npm shim preserves real child argv, stdout, stderr, and exit status", () => {
  withScratch("npm-shim-pass-through-", (fixtureRoot) => {
    const pkgDir = path.join(fixtureRoot, "mcp-local-hub");
    fs.mkdirSync(path.join(pkgDir, "bin"), { recursive: true });
    fs.mkdirSync(path.join(pkgDir, "lib"), { recursive: true });
    fs.copyFileSync(path.join(__dirname, "bin", "cli.js"), path.join(pkgDir, "bin", "cli.js"));
    fs.copyFileSync(
      path.join(__dirname, "lib", "platform-binary.js"),
      path.join(pkgDir, "lib", "platform-binary.js"),
    );

    const { PACKAGE_BY_PLATFORM, binaryBasename } = require("./lib/platform-binary");
    const platformPackage = PACKAGE_BY_PLATFORM[`${process.platform}-${process.arch}`];
    assert.ok(platformPackage, "test host must be one of the six packaged targets");
    const platformBinDir = path.join(pkgDir, "node_modules", ...platformPackage.split("/"), "bin");
    fs.mkdirSync(platformBinDir, { recursive: true });
    const candidate = path.join(platformBinDir, binaryBasename(process.platform));
    fs.copyFileSync(process.execPath, candidate);
    if (process.platform !== "win32") fs.chmodSync(candidate, 0o755);

    const childProgram =
      "process.stdout.write(JSON.stringify(process.argv.slice(1)));" +
      "process.stderr.write('stderr-marker');" +
      "process.exit(37);";
    const argv = ["-e", childProgram, "alpha", "two words", "--literal=\"quoted\""];
    const result = spawnSync(process.execPath, [path.join(pkgDir, "bin", "cli.js"), ...argv], {
      cwd: pkgDir,
      encoding: "utf8",
    });

    assert.strictEqual(result.status, 37);
    assert.strictEqual(result.stdout, '["alpha","two words","--literal=\\"quoted\\""]');
    assert.strictEqual(result.stderr, "stderr-marker");
  });
});
