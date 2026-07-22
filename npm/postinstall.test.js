"use strict";
// Black-box fail-safe test for scripts/postinstall.js: the canonicalize hook
// MUST exit 0 and point the operator at `mcphub setup` when the platform
// binary cannot be resolved — a canonicalize failure must never break
// `npm install`.

const { test } = require("node:test");
const assert = require("node:assert");
const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const { PACKAGE_BY_PLATFORM } = require("./lib/platform-binary");

test("postinstall exits 0 with a `mcphub setup` fallback when the platform binary is unresolvable", () => {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "mcphub-postinstall-"));
  try {
    // Reproduce the published package's relative layout (lib/ + scripts/) so
    // the script's `require("../lib/platform-binary")` resolves exactly as it
    // would once installed.
    // Mirror the published meta package: CommonJS, so the copied scripts load
    // as CommonJS regardless of any ancestor package.json on the test host.
    fs.writeFileSync(
      path.join(tmp, "package.json"),
      JSON.stringify({ name: "mcp-local-hub-postinstall-fixture", type: "commonjs" }) + "\n",
    );
    fs.mkdirSync(path.join(tmp, "lib"));
    fs.mkdirSync(path.join(tmp, "scripts"));
    fs.copyFileSync(
      path.join(__dirname, "lib", "platform-binary.js"),
      path.join(tmp, "lib", "platform-binary.js"),
    );
    fs.copyFileSync(
      path.join(__dirname, "scripts", "postinstall.js"),
      path.join(tmp, "scripts", "postinstall.js"),
    );

    // Force a deterministic resolution FAILURE on any supported host: create
    // the matching platform package dir WITH a package.json but NO bin binary.
    // require.resolve(`<pkg>/bin/<name>`) then throws (file missing) AND this
    // local dir shadows any real platform package elsewhere on the machine, so
    // the test never spawns a real binary. On an unsupported host the map miss
    // takes the "no platform binary" branch — also exit 0 + fallback notice.
    const key = `${process.platform}-${process.arch}`;
    const pkg = PACKAGE_BY_PLATFORM[key];
    if (pkg) {
      const pkgDir = path.join(tmp, "node_modules", ...pkg.split("/"));
      fs.mkdirSync(pkgDir, { recursive: true });
      fs.writeFileSync(
        path.join(pkgDir, "package.json"),
        JSON.stringify({ name: pkg, version: "0.0.0" }) + "\n",
      );
    } else {
      fs.mkdirSync(path.join(tmp, "node_modules"));
    }

    const res = spawnSync(process.execPath, [path.join(tmp, "scripts", "postinstall.js")], {
      encoding: "utf8",
      // Fresh HOME/USERPROFILE so nothing here can touch the real ~/.local.
      env: { ...process.env, HOME: tmp, USERPROFILE: tmp },
    });

    assert.strictEqual(
      res.status,
      0,
      `postinstall must exit 0 on a resolve failure; got ${res.status}\nstderr:\n${res.stderr}`,
    );
    assert.match(
      res.stderr,
      /mcphub setup/,
      `fallback notice must point at \`mcphub setup\`; stderr:\n${res.stderr}`,
    );
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
