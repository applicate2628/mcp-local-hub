// v0.5.0 phase 14 / task 14.1 — consultant reservation #2 (TIME_WAIT folklore).
//
// Consultant cross-validation (plan §2664-2666) flagged the 2-3s TIME_WAIT
// settle that rollback step 8 assumes as "folklore-level": Windows default
// TcpTimedWaitDelay is 240s, so a naive read of the docs would suggest the
// port stays unavailable for minutes. This test verifies empirically that
// the rollback flow's restored v0.4.x daemon does bind its pinned port
// within 5s on a real Windows host — i.e. TIME_WAIT does NOT block the
// rebind because the kernel rebinds via SO_REUSEADDR / dual-stack
// behavior. If this assumption breaks on AV-instrumented Windows, this
// spec is where we will see it first.
//
// Why opt-in only (NOT part of `npm test`):
//   1. It registers a REAL Windows Scheduled Task (`mcp-local-hub-time-default`)
//      against the developer's host — not the fixture's tmp HOME. This
//      cannot be hermetic the way the other e2e tests are.
//   2. It runs `mcphub install --rollback-to-legacy` which mutates the
//      caller's Task Scheduler state. Running this by accident on a dev
//      box with active mcp-local-hub-* tasks would be a kosyak
//      (CLAUDE.md `feedback_kosyak_full_test_sweep_affects_real_scheduler.md`).
//   3. The implementation it exercises (`internal/migration.RunRollback`
//      driven by `mcphub install --rollback-to-legacy`) is delivered
//      across phases 10.4 + 14; the CLI surface lands later in v0.5.0.
//
// Enable in CI's Windows runner with:
//   MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT=1 npm test
//
// Global-setup's matching MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT block
// registers the fixture task before the suite runs. Cleanup lives here
// in `afterAll` so a failed setup still tears the task down.

import { test, expect } from "@playwright/test";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const execFileP = promisify(execFile);
const __dirname = dirname(fileURLToPath(import.meta.url));

// Must match the task name used by global-setup.ts when
// MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT=1. Both files independently use
// this constant to avoid a cross-file import (Playwright spec files are
// usually self-contained; cross-importing from global-setup would make
// the spec harder to lift/move).
const FIXTURE_TASK = "mcp-local-hub-time-default";
const FIXTURE_PORT = 9128;

// OS guard: rollback is Windows-only. The migration journal returns
// `ErrPosixNotSupported` on Linux/macOS — exit code is non-zero, the
// test would fail spuriously.
test.skip(process.platform !== "win32", "rollback is Windows-only");

// Opt-in guard: only run when explicitly enabled. See file header for
// why this is not in the default suite.
test.skip(
  process.env.MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT !== "1",
  "skipped by default; set MCPHUB_E2E_INCLUDE_ROLLBACK_TIME_WAIT=1 to enable",
);

// Compiled binary produced by global-setup.ts (`go build -o bin/mcphub`).
// If global-setup failed to produce it the whole suite would have
// aborted there, so we can assume the path is valid by the time the
// spec body runs.
const BIN_PATH = resolve(
  __dirname,
  "..",
  "bin",
  process.platform === "win32" ? "mcphub.exe" : "mcphub",
);

// afterAll deletes the fixture task whether or not the test ran. Place
// it OUTSIDE the test() block (Playwright treats top-level afterAll as
// suite-level cleanup) so a failed test still tears the task down. Also
// fires when both skip guards above evict the test — preserving
// idempotency on Linux/macOS or non-opted-in runs where setup never
// touched Task Scheduler at all (the schtasks /Delete in such cases
// fails because the task isn't there; the catch swallows that).
test.afterAll(async () => {
  if (process.platform !== "win32") return;
  try {
    await execFileP("schtasks", ["/Delete", "/TN", FIXTURE_TASK, "/F"], {
      maxBuffer: 1024 * 1024,
    });
  } catch {
    // Task wasn't registered or already gone — fine. Surface nothing
    // because afterAll noise pollutes test output.
  }
});

test("rollback step 8 TIME_WAIT settle: legacy daemon binds port within 5s", async () => {
  // Step 1: invoke mcphub install --rollback-to-legacy. The migration
  // journal's RunRollback path (internal/migration/journal.go) is the
  // production implementation; we exercise it through the CLI surface.
  //
  // success-with-warnings is exit 0 — the rollback flow may emit
  // warnings for orphaned state but still succeed. exit != 0 means a
  // hard failure (token mismatch, PowerShell-locked, foreign-owner
  // task, etc.) and should fail the test.
  const rollback = spawn(BIN_PATH, ["install", "--rollback-to-legacy"], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  let stdoutBuf = "";
  let stderrBuf = "";
  rollback.stdout?.on("data", (c: Buffer) => {
    stdoutBuf += c.toString("utf8");
  });
  rollback.stderr?.on("data", (c: Buffer) => {
    stderrBuf += c.toString("utf8");
  });
  const exitCode = await new Promise<number>((res) => {
    rollback.on("exit", (c) => res(c ?? -1));
    rollback.on("error", () => res(-1));
  });
  // Surface CLI output on failure for diagnosis — without this the
  // bare exit-code mismatch is unactionable. The diagnostic lands in
  // the Playwright HTML report under the failing test.
  if (exitCode !== 0) {
    // eslint-disable-next-line no-console
    console.error(
      `[rollback_time_wait] mcphub install --rollback-to-legacy exited ${exitCode}\n` +
        `stdout:\n${stdoutBuf}\nstderr:\n${stderrBuf}`,
    );
  }
  expect(exitCode).toBe(0);

  // Step 2: verify the restored v0.4.x daemon binds its pinned port
  // within 5s. The fixture daemon is `mcp-local-hub-time-default`
  // pinned to FIXTURE_PORT; on rollback step 8 it is re-spawned and
  // should accept connections after at most ~2-3s of TIME_WAIT
  // settle. 5s gives slack for spawn + first-accept latency on a
  // busy Windows host without making the test sluggish.
  //
  // If TIME_WAIT really did block the rebind for 240s (the folklore
  // reading of TcpTimedWaitDelay), this loop would exhaust and the
  // assertion would fail — which is exactly the empirical signal
  // consultant reservation #2 asked for.
  const start = Date.now();
  let allBound = false;
  let lastErr: string | null = null;
  while (Date.now() - start < 5000) {
    try {
      const result = await fetch(`http://127.0.0.1:${FIXTURE_PORT}/health`);
      if (result.ok) {
        allBound = true;
        break;
      }
      lastErr = `HTTP ${result.status}`;
    } catch (e) {
      lastErr = (e as Error).message;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  if (!allBound) {
    // eslint-disable-next-line no-console
    console.error(
      `[rollback_time_wait] port ${FIXTURE_PORT} did not bind within 5s; lastErr=${lastErr}`,
    );
  }
  expect(allBound).toBe(true);
});
