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

test("generator emits one Windows executable plus its non-executable identity manifest", () => {
  withScratch("npm-platform-generator-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    const manifest = JSON.parse(
      fs.readFileSync(path.join(npmDir, "packages", "win32-x64", "package.json"), "utf8"),
    );
    assert.deepStrictEqual(manifest.os, ["win32"]);
    assert.deepStrictEqual(manifest.cpu, ["x64"]);
    assert.strictEqual(Object.hasOwn(manifest, "bin"), false);
    assert.deepStrictEqual(manifest.files, [
      "bin/mcphub.exe",
      "bin/windows-product-identity.json",
      "README.md",
    ]);
  });
});

test("generator, meta manifest, and runtime lookup expose only the shipped Windows x64 target", () => {
  withScratch("npm-platform-release-targets-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    assert.deepStrictEqual(fs.readdirSync(path.join(npmDir, "packages")).sort(), ["win32-x64"]);
    const meta = JSON.parse(fs.readFileSync(path.join(npmDir, "package.json"), "utf8"));
    assert.deepStrictEqual(meta.optionalDependencies, {
      "@applicate2628/mcp-local-hub-win32-x64": "0.4.37",
    });
    const { PACKAGE_BY_PLATFORM } = require("./lib/platform-binary");
    assert.deepStrictEqual(PACKAGE_BY_PLATFORM, {
      "win32-x64": "@applicate2628/mcp-local-hub-win32-x64",
    });
  });
});

test("generator rejects a stale generated target directory", () => {
  withScratch("npm-platform-stale-target-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    fs.mkdirSync(path.join(npmDir, "packages", "freebsd-x64"));
    const result = spawnSync(process.execPath, [path.join(npmDir, "generate-platform-packages.js")], {
      cwd: fixtureRoot,
      encoding: "utf8",
    });
    assert.notStrictEqual(result.status, 0);
    assert.match(result.stderr, /unexpected generated target directory.*freebsd-x64/);
  });
});

test("npm pack dry-run exposes only the Windows x64 native payload", () => {
  withScratch("npm-platform-pack-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    const npmCli = process.env.npm_execpath;
    assert.ok(npmCli, "npm_execpath is required for the package dry-run fixture");
    for (const target of ["win32-x64"]) {
      const packageDir = path.join(npmDir, "packages", target);
      const manifest = JSON.parse(fs.readFileSync(path.join(packageDir, "package.json"), "utf8"));
      const expectedNative = ["bin/mcphub.exe", "bin/windows-product-identity.json"];
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
      assert.deepStrictEqual(
        result[0].files.map((entry) => entry.path).sort(),
        ["README.md", "bin/mcphub.exe", "bin/windows-product-identity.json", "package.json"],
      );
    }
  });
});

test("Windows artifact contract freezes filenames, roles, sources, subsystems, and metadata policy", () => {
  const contract = JSON.parse(
    fs.readFileSync(path.join(ROOT, "windows-product-artifacts.json"), "utf8"),
  );
  assert.deepStrictEqual(contract, {
    schemaVersion: 2,
    identityManifest: {
      filename: "windows-product-identity.json",
      schema: "windows-product-identity-v1",
    },
    artifacts: [
      {
        role: "product",
        filename: "mcphub.exe",
        source: "./cmd/mcphub",
        subsystem: 2,
        productMetadata: true,
        npmPayload: true,
        npmBin: false,
      },
    ],
  });
});

test("platform binary owner names the Windows identity manifest without changing POSIX", () => {
  const { binaryBasename, identityManifestBasename } = require("./lib/platform-binary");
  const meta = require("./package.json");
  assert.deepStrictEqual(meta.bin, { mcphub: "bin/cli.js" });
  assert.strictEqual(binaryBasename("win32"), "mcphub.exe");
  assert.strictEqual(identityManifestBasename("win32"), "windows-product-identity.json");
  for (const platform of ["linux", "darwin"]) {
    assert.strictEqual(binaryBasename(platform), "mcphub");
    assert.strictEqual(identityManifestBasename(platform), null);
  }
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
    fs.copyFileSync(path.join(__dirname, "package.json"), path.join(pkgDir, "package.json"));

    const { PACKAGE_BY_PLATFORM, binaryBasename } = require("./lib/platform-binary");
    const platformPackage = PACKAGE_BY_PLATFORM["win32-x64"];
    assert.ok(platformPackage, "Windows x64 is the shipped target");
    const platformBinDir = path.join(pkgDir, "node_modules", ...platformPackage.split("/"), "bin");
    fs.mkdirSync(platformBinDir, { recursive: true });
    const candidate = path.join(platformBinDir, binaryBasename("win32"));
    fs.copyFileSync(process.execPath, candidate);
    if (process.platform !== "win32") fs.chmodSync(candidate, 0o755);

    const preload = path.join(fixtureRoot, "force-win32-x64.cjs");
    fs.writeFileSync(
      preload,
      'Object.defineProperty(process, "platform", {value: "win32"});\n' +
        'Object.defineProperty(process, "arch", {value: "x64"});\n',
    );

    const childProgram =
      "process.stdout.write(JSON.stringify(process.argv.slice(1)));" +
      "process.stderr.write('stderr-marker');" +
      "process.exit(37);";
    const argv = ["-e", childProgram, "alpha", "two words", "--literal=\"quoted\""];
    const result = spawnSync(process.execPath, [path.join(pkgDir, "bin", "cli.js"), ...argv], {
      cwd: pkgDir,
      encoding: "utf8",
      env: { ...process.env, NODE_OPTIONS: `--require=${preload}` },
    });

    assert.strictEqual(result.status, 37);
    assert.strictEqual(result.stdout, '["alpha","two words","--literal=\\"quoted\\""]');
    assert.strictEqual(result.stderr, "stderr-marker");
  });
});

test("deferred hosts reject even when an older platform package is available", () => {
  withScratch("npm-shim-deferred-host-", (fixtureRoot) => {
    const pkgDir = path.join(fixtureRoot, "mcp-local-hub");
    fs.mkdirSync(path.join(pkgDir, "bin"), { recursive: true });
    fs.mkdirSync(path.join(pkgDir, "lib"), { recursive: true });
    for (const relative of ["package.json", "bin/cli.js", "lib/platform-binary.js"]) {
      const destination = path.join(pkgDir, ...relative.split("/"));
      fs.copyFileSync(path.join(__dirname, ...relative.split("/")), destination);
    }
    const trace = path.join(fixtureRoot, "spawn-trace.txt");
    const preload = path.join(fixtureRoot, "force-host.cjs");
    fs.writeFileSync(
      preload,
      'Object.defineProperty(process, "platform", {value: process.env.MCPHUB_TEST_OS});\n' +
        'Object.defineProperty(process, "arch", {value: process.env.MCPHUB_TEST_CPU});\n' +
        'require("node:child_process").spawnSync = () => { ' +
        'require("node:fs").writeFileSync(process.env.MCPHUB_TEST_TRACE, "spawned"); ' +
        'return {status: 0, signal: null}; };\n',
    );

    for (const [osName, cpu, binary] of [
      ["linux", "x64", "mcphub"],
      ["win32", "arm64", "mcphub.exe"],
    ]) {
      const oldPackage = `@applicate2628/mcp-local-hub-${osName}-${cpu}`;
      const oldDir = path.join(pkgDir, "node_modules", ...oldPackage.split("/"));
      fs.mkdirSync(path.join(oldDir, "bin"), { recursive: true });
      fs.writeFileSync(path.join(oldDir, "package.json"), JSON.stringify({ name: oldPackage, version: "0.4.36" }));
      fs.writeFileSync(path.join(oldDir, "bin", binary), "older native payload");

      const result = spawnSync(process.execPath, [path.join(pkgDir, "bin", "cli.js"), "version"], {
        cwd: pkgDir,
        encoding: "utf8",
        env: {
          ...process.env,
          NODE_OPTIONS: `--require=${preload}`,
          MCPHUB_TEST_OS: osName,
          MCPHUB_TEST_CPU: cpu,
          MCPHUB_TEST_TRACE: trace,
        },
      });
      assert.strictEqual(result.status, 1, result.stderr);
      assert.match(result.stderr, new RegExp(`unsupported platform "${osName}-${cpu}"`));
      assert.match(result.stderr, /Supported targets: win32-x64\./);
      assert.doesNotMatch(result.stderr, /download the matching binary/);
      assert.strictEqual(fs.existsSync(trace), false, "an older native package was launched");
    }
  });
});
