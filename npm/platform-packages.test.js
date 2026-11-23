"use strict";

const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

const ROOT = path.resolve(__dirname, "..");
const SCRATCH = path.join(ROOT, ".scratch");
const WINIO_LICENSE = ["third_party", "go-winio", "LICENSE"];

function withScratch(prefix, fn) {
  fs.mkdirSync(SCRATCH, { recursive: true });
  const dir = fs.mkdtempSync(path.join(SCRATCH, prefix));
  try {
    return fn(dir);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

function prepareGeneratorFixture(fixtureRoot, projectContract = (contract) => contract) {
  const npmDir = path.join(fixtureRoot, "npm");
  fs.mkdirSync(npmDir, { recursive: true });
  fs.copyFileSync(
    path.join(__dirname, "generate-platform-packages.js"),
    path.join(npmDir, "generate-platform-packages.js"),
  );
  fs.copyFileSync(path.join(__dirname, "package.json"), path.join(npmDir, "package.json"));
  const contract = path.join(ROOT, "windows-product-artifacts.json");
  const projection = projectContract(JSON.parse(fs.readFileSync(contract, "utf8")));
  fs.writeFileSync(path.join(fixtureRoot, "windows-product-artifacts.json"), JSON.stringify(projection));
  return npmDir;
}

function generateFixture(fixtureRoot, projectContract = (contract) => contract) {
  const npmDir = prepareGeneratorFixture(fixtureRoot, projectContract);
  const fixtureLicense = path.join(fixtureRoot, ...WINIO_LICENSE);
  fs.mkdirSync(path.dirname(fixtureLicense), { recursive: true });
  fs.copyFileSync(path.join(ROOT, ...WINIO_LICENSE), fixtureLicense);
  const result = spawnSync(process.execPath, [path.join(npmDir, "generate-platform-packages.js")], {
    cwd: fixtureRoot,
    encoding: "utf8",
  });
  assert.strictEqual(result.status, 0, result.stderr || result.stdout);
  return npmDir;
}

function assertWinIOLicense(npmDir) {
  const original = fs.readFileSync(path.join(ROOT, ...WINIO_LICENSE));
  const readme = fs.readFileSync(path.join(npmDir, "packages", "win32-x64", "README.md"));
  const notice = Buffer.concat([
    Buffer.from("The Windows Product includes go-winio under the MIT License.\n\n```text\n"),
    original,
    Buffer.from("```\n"),
  ]);
  assert.ok(readme.includes(notice), "Windows Product README must contain the original WinIO license bytes");
}

test("generator rejects a missing or invalid WinIO license before creating packages", () => {
  const cases = [
    { license: null, diagnostic: /ENOENT[^\n]*go-winio[\\/]LICENSE/ },
    { license: Buffer.from([0xff]), diagnostic: /go-winio LICENSE must contain valid UTF-8 text/ },
  ];
  for (const { license, diagnostic } of cases) {
    withScratch("npm-platform-license-preflight-", (fixtureRoot) => {
      const npmDir = prepareGeneratorFixture(fixtureRoot);
      if (license !== null) {
        const fixtureLicense = path.join(fixtureRoot, ...WINIO_LICENSE);
        fs.mkdirSync(path.dirname(fixtureLicense), { recursive: true });
        fs.writeFileSync(fixtureLicense, license);
      }
      const result = spawnSync(process.execPath, [path.join(npmDir, "generate-platform-packages.js")], {
        cwd: fixtureRoot,
        encoding: "utf8",
      });
      assert.notStrictEqual(result.status, 0);
      assert.match(result.stderr, diagnostic);
      assert.strictEqual(fs.existsSync(path.join(npmDir, "packages")), false);
    });
  }
});

test("generator emits the waiting entry and complete nested Product projection", () => {
  withScratch("npm-platform-generator-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    const manifest = JSON.parse(
      fs.readFileSync(path.join(npmDir, "packages", "win32-x64", "package.json"), "utf8"),
    );
    assert.deepStrictEqual(manifest.os, ["win32"]);
    assert.deepStrictEqual(manifest.cpu, ["x64"]);
    assert.strictEqual(Object.hasOwn(manifest, "bin"), false);
    assert.strictEqual(manifest.mcphubProduct, "bin/mcphub-product/mcphub.exe");
    assert.strictEqual(manifest.license, "SEE LICENSE IN README.md");
    assert.deepStrictEqual(manifest.files, [
      "bin/mcphub-product/mcphub.exe",
      "bin/mcphub.exe",
      "bin/mcphub-product/windows-product-identity.json",
      "bin/mcphub-product/runtime-payload.json",
      "bin/mcphub-product/runtimes/",
      "README.md",
    ]);
    assertWinIOLicense(npmDir);
  });
});

test("generator preserves the legacy V1 package projection for a schema 3 registry", () => {
  withScratch("npm-platform-legacy-generator-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot, (contract) => {
      contract.schemaVersion = 3;
      contract.identityManifest.schema = "windows-product-identity-v1";
      delete contract.runtimePayload;
      return contract;
    });
    const manifest = JSON.parse(
      fs.readFileSync(path.join(npmDir, "packages", "win32-x64", "package.json"), "utf8"),
    );
    assert.strictEqual(Object.hasOwn(manifest, "mcphubProduct"), false);
    assert.strictEqual(manifest.license, require("./package.json").license);
    assert.deepStrictEqual(manifest.files, [
      "bin/mcphub-product/mcphub.exe",
      "bin/mcphub.exe",
      "bin/windows-product-identity.json",
      "README.md",
    ]);
    assertWinIOLicense(npmDir);
  });
});

test("generator, meta manifest, and runtime lookup expose only the shipped Windows x64 target", () => {
  withScratch("npm-platform-release-targets-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    assert.deepStrictEqual(fs.readdirSync(path.join(npmDir, "packages")).sort(), ["win32-x64"]);
    const meta = JSON.parse(fs.readFileSync(path.join(npmDir, "package.json"), "utf8"));
    assert.deepStrictEqual(meta.optionalDependencies, {
      "@applicate2628/mcp-local-hub-win32-x64": meta.version,
    });
    const manifest = JSON.parse(
      fs.readFileSync(path.join(npmDir, "packages", "win32-x64", "package.json"), "utf8"),
    );
    assert.strictEqual(manifest.version, meta.version);
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

test("npm pack includes the nested Windows x64 runtime and excludes unlisted payloads", () => {
  withScratch("npm-platform-pack-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    const npmCli = process.env.npm_execpath;
    assert.ok(npmCli, "npm_execpath is required for the package dry-run fixture");
    for (const target of ["win32-x64"]) {
      const packageDir = path.join(npmDir, "packages", target);
      const manifest = JSON.parse(fs.readFileSync(path.join(packageDir, "package.json"), "utf8"));
      const runtimeRoot = "bin/mcphub-product/runtimes";
      const expectedNative = [
        "bin/mcphub-product/mcphub.exe",
        "bin/mcphub.exe",
        `${runtimeRoot}/python/${"1".repeat(64)}/root/python.exe`,
        `${runtimeRoot}/uv/${"2".repeat(64)}/root/uv.exe`,
        "bin/mcphub-product/runtime-payload.json",
        "bin/mcphub-product/windows-product-identity.json",
      ].sort();
      for (const relative of expectedNative) {
        const nativePath = path.join(packageDir, ...relative.split("/"));
        fs.mkdirSync(path.dirname(nativePath), { recursive: true });
        fs.writeFileSync(nativePath, `synthetic payload for ${target}/${relative}\n`);
      }
      fs.writeFileSync(path.join(packageDir, "bin", "windows-product-identity.json"), "stale V1 identity\n");
      fs.writeFileSync(path.join(packageDir, "bin", "mcphub-product", "unlisted.txt"), "unlisted Product data\n");
      // Development SDKs and their bundled CLI must remain outside Product
      // supply even when a developer environment is present beside/in a package.
      for (const scope of [fixtureRoot, npmDir, packageDir]) {
        for (const relative of [
          "tools/codex/uv.lock",
          "tools/codex/.venv/Lib/site-packages/openai_codex/__init__.py",
          "tools/codex/.venv/Lib/site-packages/codex_cli_bin/codex.exe",
        ]) {
          const sentinel = path.join(scope, ...relative.split("/"));
          fs.mkdirSync(path.dirname(sentinel), { recursive: true });
          fs.writeFileSync(sentinel, "synthetic development-only SDK sentinel\n");
        }
      }
      const packed = spawnSync(process.execPath, [npmCli, "pack", "--dry-run", "--json", "--ignore-scripts"], {
        cwd: packageDir,
        encoding: "utf8",
      });
      assert.strictEqual(packed.status, 0, packed.stderr || packed.stdout);
      const packedResult = JSON.parse(packed.stdout);
      if (!Array.isArray(packedResult)) {
        assert.deepStrictEqual(Object.keys(packedResult), [manifest.name]);
      }
      const result = Array.isArray(packedResult)
        ? packedResult
        : [packedResult[manifest.name]];
      assert.strictEqual(result.length, 1);
      const actualNative = result[0].files
        .map((entry) => entry.path)
        .filter((entry) => entry.startsWith("bin/"))
        .sort();
      assert.deepStrictEqual(actualNative, expectedNative, target);
      assert.deepStrictEqual(
        result[0].files.map((entry) => entry.path).sort(),
        ["README.md", ...expectedNative, "package.json"],
      );
      const metaPack = spawnSync(process.execPath, [npmCli, "pack", "--dry-run", "--json", "--ignore-scripts"], {
        cwd: npmDir,
        encoding: "utf8",
      });
      assert.strictEqual(metaPack.status, 0, metaPack.stderr || metaPack.stdout);
      const metaResult = JSON.parse(metaPack.stdout);
      const metaManifest = JSON.parse(fs.readFileSync(path.join(npmDir, "package.json"), "utf8"));
      const meta = Array.isArray(metaResult) ? metaResult[0] : metaResult[metaManifest.name];
      // This fixture intentionally contains no shim payload; the exact Product
      // allowlist is checked separately from the fixture's available files.
      assert.deepStrictEqual(metaManifest.files, [
        "bin/cli.js", "lib/platform-binary.js", "scripts/postinstall.js", "README.md",
      ]);
      assert.deepStrictEqual(meta.files.map((entry) => entry.path), ["package.json"]);
    }
  });
});

test("Windows artifact contract freezes filenames, roles, sources, subsystems, and metadata policy", () => {
  const contract = JSON.parse(
    fs.readFileSync(path.join(ROOT, "windows-product-artifacts.json"), "utf8"),
  );
  assert.deepStrictEqual(contract, {
    schemaVersion: 4,
    identityManifest: {
      filename: "windows-product-identity.json",
      schema: "windows-product-identity-v2",
    },
    runtimePayload: {
      schema: "hub-runtime-payload-v1",
      releaseDescriptors: ["runtimes/python/release.yaml", "runtimes/uv/release.yaml"],
      filename: "mcphub-product/runtime-payload.json",
    },
    artifacts: [
      {
        role: "product",
        filename: "mcphub-product/mcphub.exe",
        source: "./cmd/mcphub",
        subsystem: 2,
        productMetadata: true,
        npmPayload: true,
        npmBin: false,
      },
      {
        role: "launcher",
        filename: "mcphub.exe",
        source: "./cmd/mcphub-launcher",
        subsystem: 3,
        productMetadata: false,
        npmPayload: true,
        npmBin: false,
      },
    ],
  });
});

test("generator refuses a swapped or incomplete Windows entry set", () => {
  withScratch("npm-platform-entry-contract-", (fixtureRoot) => {
    const npmDir = generateFixture(fixtureRoot);
    const contractPath = path.join(fixtureRoot, "windows-product-artifacts.json");
    const original = JSON.parse(fs.readFileSync(contractPath, "utf8"));
    for (const mutate of [
      (contract) => { contract.artifacts.pop(); },
      (contract) => { contract.artifacts[0].filename = "mcphub.exe"; },
      (contract) => { contract.artifacts[1].subsystem = 2; },
      (contract) => { contract.identityManifest.schema = "windows-product-identity-v1"; },
      (contract) => { delete contract.runtimePayload; },
      (contract) => { contract.runtimePayload.filename = "runtime-payload.json"; },
      (contract) => { delete contract.runtimePayload.releaseDescriptors; },
      (contract) => { contract.runtimePayload.releaseDescriptors = []; },
      (contract) => { contract.runtimePayload.releaseDescriptors.reverse(); },
      (contract) => { contract.runtimePayload.releaseDescriptors[1] = "runtimes/other/release.yaml"; },
    ]) {
      const contract = structuredClone(original);
      mutate(contract);
      fs.writeFileSync(contractPath, JSON.stringify(contract));
      const result = spawnSync(process.execPath, [path.join(npmDir, "generate-platform-packages.js")], {
        cwd: fixtureRoot, encoding: "utf8",
      });
      assert.notStrictEqual(result.status, 0);
      assert.match(result.stderr, /windows-product-artifacts\.json must declare/);
    }
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
