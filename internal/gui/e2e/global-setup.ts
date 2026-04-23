import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { mkdirSync } from "node:fs";
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
  const binPath = resolve(binDir, process.platform === "win32" ? "mcphub.exe" : "mcphub");

  // 1) Rebuild Preact bundle → internal/gui/assets/. Otherwise tests
  //    could pass against stale assets after a frontend source change
  //    that was never rebuilt locally.
  console.log("[global-setup] npm run build (frontend)…");
  await execFileP("npm", ["run", "build"], {
    cwd: frontendDir,
    env: { ...process.env },
    maxBuffer: 10 * 1024 * 1024,
    shell: true, // npm resolves to npm.cmd on Windows via shell lookup
  });

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
