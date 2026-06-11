import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdirSync, rmSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const execFileP = promisify(execFile);
// ESM does not expose __dirname; derive it from import.meta.url. The
// package.json sets "type": "module" so bare __dirname would throw
// at module load.
const __dirname = dirname(fileURLToPath(import.meta.url));

// globalSetup runs once before the test run. It first rebuilds the
// Vite frontend bundle so the Go embed serves current TSX, then
// compiles cmd/mcphub to internal/gui/e2e/bin/mcphub(.exe) so the
// per-test hub fixture can spawn it directly instead of re-running
// `go run` (~5s compile) each time. Both outputs are gitignored on
// CI; locally the committed internal/gui/assets/* is refreshed by
// the npm build.
export default async function globalSetup() {
  const repoRoot = resolve(__dirname, "..", "..", "..");
  const frontendDir = resolve(__dirname, "..", "frontend");
  const binDir = resolve(__dirname, "bin");
  mkdirSync(binDir, { recursive: true });

  // Wipe the servers/ directories that the mcphub binary populates when tests
  // call POST /api/manifest/create. The binary's defaultManifestDir() probes:
  //   1. exeDir/servers     = e2e/bin/servers/  (sibling, checked first)
  //   2. exeDir/../servers  = e2e/servers/      (parent)
  //   3. fallback           = e2e/bin/servers/  (after PR #11 removed CWD fallback)
  //
  // Whichever exists first is chosen. If bin/servers/ has stale content from a
  // previous run, the binary writes there; but edit-server.spec.ts seeds manifests
  // into e2e/servers/, causing a dir-mismatch where reads and writes target
  // different roots. Wiping BOTH keeps the resolution deterministic: after the
  // wipe, bin/servers doesn't exist → sibling check fails → parent is selected
  // as soon as any seed (or create) populates e2e/servers/.
  rmSync(resolve(__dirname, "servers"), { recursive: true, force: true });
  rmSync(resolve(__dirname, "bin", "servers"), { recursive: true, force: true });
  const binPath = resolve(binDir, process.platform === "win32" ? "mcphub.exe" : "mcphub");

  // 1) Rebuild Preact bundle → internal/gui/assets/. Then verify
  //    git didn't see any diff after the rebuild — if it did, the
  //    committed bundle was stale vs source and a fresh `go build`
  //    on CI would ship different code than what E2E just exercised.
  //    Fail loudly rather than silently masking the problem.
  console.log("[global-setup] npm run build (frontend)…");
  await execFileP("npm", ["run", "build"], {
    cwd: frontendDir,
    env: { ...process.env },
    maxBuffer: 10 * 1024 * 1024,
    shell: true, // npm resolves to npm.cmd on Windows via shell lookup
  });
  // git status --porcelain catches both modified-tracked files AND
  // untracked files (e.g. a newly split chunk or an imported font that
  // Vite emits for the first time). `git diff --name-only` misses the
  // untracked case — the E2E would pass while a clean-checkout go build
  // would be missing the new embed file.
  const { stdout: statusOut } = await execFileP(
    "git",
    ["status", "--porcelain", "--", "internal/gui/assets/"],
    { cwd: repoRoot, maxBuffer: 1024 * 1024 },
  );
  if (statusOut.trim().length > 0) {
    throw new Error(
      "[global-setup] internal/gui/assets/ changed after npm run build — " +
        "committed bundle was stale or a new asset was emitted. Run " +
        "`go generate ./internal/gui/...` and commit the updated assets. " +
        "Changed/new files:\n" +
        statusOut,
    );
  }

  // 2) Compile mcphub binary so the fixture can spawn it fast.
  console.log("[global-setup] go build ./cmd/mcphub…");
  const { stderr } = await execFileP("go", ["build", "-o", binPath, "./cmd/mcphub"], {
    cwd: repoRoot,
    env: { ...process.env },
    maxBuffer: 10 * 1024 * 1024,
  });
  if (stderr) {
    // `go build` writes nothing to stderr on success. Non-empty stderr
    // usually means deprecation warnings we can ignore; surface it for
    // visibility but do not fail — execFileP already throws on non-zero
    // exit.
    console.warn("[global-setup] go build stderr:\n" + stderr);
  }
  console.log(`[global-setup] built mcphub → ${binPath}`);
}
