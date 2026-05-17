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

  // ── v0.5.0 phase 14 / task 14.1 — rollback TIME_WAIT fixture ───────
  //
  // Optional, opt-in only. The matching spec
  // (internal/gui/e2e/tests/rollback_time_wait.spec.ts) verifies that
  // `mcphub install --rollback-to-legacy` re-binds the restored v0.4.x
  // daemon's pinned port within 5s — the empirical check that consultant
  // reservation #2 asked for. The flow needs a pre-existing v0.4.x-style
  // daemon task to roll back FROM, so this block registers one under the
  // real Windows Task Scheduler (NOT the per-test tmpHome, which is the
  // hermetic boundary for every OTHER e2e test).
  //
  // Why opt-in only (NOT part of default `npm test`):
  //   * It manipulates the developer's real Task Scheduler — registering
  //     mcp-local-hub-time-default outside the per-test tmpHome
  //     hermetic boundary that every other e2e spec relies on. Running
  //     this by accident on a dev box with active mcp-local-hub-* tasks
  //     would be a kosyak (CLAUDE.md
  //     `feedback_kosyak_full_test_sweep_affects_real_scheduler.md`).
  //   * The CLI surface (`mcphub install --rollback-to-legacy`) ships
  //     across phase 10.4 + 14; until both land the spec would FAIL by
  //     design, which is fine when the suite explicitly opts in but
  //     would block unrelated `npm test` runs in the meantime.
  //
  // Enable in CI's Windows runner with:
  //   MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT=1 npm test
  //
  // The spec's afterAll hook deletes mcp-local-hub-time-default
  // unconditionally so a teardown of this fixture is idempotent even if
  // setup partially completed.
  if (process.env.MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT === "1") {
    if (process.platform !== "win32") {
      console.warn(
        "[global-setup] MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT=1 but platform " +
          "is not win32 — fixture not registered (spec will skip).",
      );
    } else {
      // Must match FIXTURE_TASK in rollback_time_wait.spec.ts.
      const fixtureTask = "mcp-local-hub-time-default";
      console.log(`[global-setup] registering rollback fixture task ${fixtureTask}…`);
      // Best-effort delete first so a re-run after a crashed spec
      // starts from a clean slate. Ignore exit code — task may not
      // exist yet.
      try {
        await execFileP("schtasks", ["/Delete", "/TN", fixtureTask, "/F"], {
          maxBuffer: 1024 * 1024,
        });
      } catch {
        // Expected when the task isn't already present. No-op.
      }
      // Register a v0.4.x-style placeholder daemon task. The action is
      // intentionally minimal: the rollback flow reads the task as XML,
      // matches it against the migration journal's deviation classifier,
      // and decides whether to roll it back. A `cmd /c exit 0` action
      // is enough to land a valid task entry the rollback can target;
      // the test asserts on the post-rollback bind behavior, not on
      // the fixture's action body.
      //
      // /SC ONLOGON keeps the task quiescent — it does NOT run on
      // schedule during setup; the rollback flow is what triggers the
      // actual spawn the test cares about.
      try {
        await execFileP(
          "schtasks",
          [
            "/Create",
            "/TN", fixtureTask,
            "/TR", "cmd /c exit 0",
            "/SC", "ONLOGON",
            "/RL", "LIMITED",
            "/F",
          ],
          { maxBuffer: 1024 * 1024 },
        );
        console.log(`[global-setup] rollback fixture task registered: ${fixtureTask}`);
      } catch (err) {
        // Don't fail the whole suite; the spec's own skip-when-not-ready
        // semantics + the explicit opt-in env var mean a failed fixture
        // registration produces a clean skip rather than a misleading
        // test failure. Surface the error so CI logs explain why.
        console.warn(
          `[global-setup] rollback fixture registration failed (spec will skip): ${(err as Error).message}`,
        );
      }
    }
  }
}
