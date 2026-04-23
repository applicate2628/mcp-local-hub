import { test as base } from "@playwright/test";
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));

// Matches the backend's "GUI listening on http://127.0.0.1:<port>" line
// (see internal/cli/gui.go ~ line 107). Capturing group 1 is the port.
const LISTEN_RE = /GUI listening on http:\/\/127\.0\.0\.1:(\d+)/;

export interface HubHandle {
  url: string;   // baseURL like "http://127.0.0.1:54321"
  port: number;
  home: string;  // per-test HOME/USERPROFILE directory
}

// hubFixture spawns a fresh mcphub gui on an OS-assigned port, pointed
// at a per-test temp home, waits for the "GUI listening on ..." banner
// on stdout, and exposes the base URL. Teardown SIGTERMs the child,
// awaits the real 'close' event (child.killed only reports "signal
// sent", not "process gone"), then cleans the temp home.
//
// Why fresh spawn per test: a clean HOME gives deterministic /api/scan
// output (empty, no leaked client configs). Parallel-safe — OS picks
// a free port per spawn.
export const test = base.extend<{ hub: HubHandle }>({
  hub: async ({}, use) => {
    const home = mkdtempSync(resolve(tmpdir(), "mcphub-e2e-"));
    const binPath = resolve(
      __dirname,
      "..",
      "bin",
      process.platform === "win32" ? "mcphub.exe" : "mcphub",
    );
    // Redirect every state-path env var at the temp home so registry,
    // logs, and pidport do not leak into the developer's real config.
    // See internal/api/workspace_registry.go, internal/api/logs.go,
    // and internal/gui/paths.go for the list of vars consulted.
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      HOME: home,
      USERPROFILE: home,              // Windows equivalent of $HOME
      LOCALAPPDATA: home,             // Windows logs/state base
      XDG_STATE_HOME: home,           // Linux state base
      XDG_DATA_HOME: home,            // Linux data base
      XDG_CONFIG_HOME: home,          // Linux config base
      MCPHUB_GUI_TEST_PIDPORT_DIR: home,
      // Task 3 seam: force scheduler.New() to return the noop impl so
      // /api/status returns [] regardless of the host's installed
      // mcp-local-hub-* scheduler tasks. Without this a dev box with
      // 13 installed daemons renders 13 Dashboard cards and empty-state
      // assertions fail locally even though CI passes.
      MCPHUB_E2E_SCHEDULER: "none",
    };
    const child = spawn(
      binPath,
      ["gui", "--no-browser", "--no-tray", "--port", "0"],
      { env, stdio: ["ignore", "pipe", "pipe"] },
    );

    // spawn's type union makes stdout/stderr nullable. They ARE present
    // because we passed "pipe" for both; assert once so the rest of
    // the fixture can use them without `!`.
    if (!child.stdout || !child.stderr) {
      throw new Error("hub fixture: spawned child has no stdout/stderr pipes");
    }
    const stdout = child.stdout;
    const stderr = child.stderr;

    // Drive lifecycle from 'close' + 'error', NOT 'exit'. Reason:
    // Node's spawn emits 'error' + 'close' on spawn failure (e.g.
    // ENOENT when the binary is missing) but NEVER 'exit'. An 'exit'-
    // only waiter would hang forever in that failure mode. 'close'
    // fires after stdio streams finish for both successful and failed
    // spawns, so it is the reliable "child is truly gone" signal.
    let closed = false;
    let spawnError: Error | null = null;
    const closePromise = new Promise<void>((res) => {
      const markClosed = () => {
        closed = true;
        res();
      };
      child.once("close", markClosed);
      // Spawn failure (ENOENT etc.) → 'error' fires, 'close' also fires
      // after it. We still mark closed here defensively in case 'close'
      // does not follow in some odd runtime path.
      child.once("error", (err) => {
        spawnError = err;
        markClosed();
      });
    });

    const cleanup = async () => {
      try {
        if (!closed) {
          child.kill("SIGTERM");
          const killed = await Promise.race([
            closePromise.then(() => true),
            new Promise<false>((res) => setTimeout(() => res(false), 3_000)),
          ]);
          if (!killed && !closed) {
            child.kill("SIGKILL");
            await Promise.race([
              closePromise,
              // Ultimate backstop — if SIGKILL still does not settle
              // in 3s (shouldn't happen, but Windows handles are
              // weird), stop waiting so cleanup can finish.
              new Promise<void>((res) => setTimeout(res, 3_000)),
            ]);
          }
        }
      } catch {
        // Defensive: never let cleanup throw during finally.
      }
      try {
        rmSync(home, { recursive: true, force: true });
      } catch {
        // Windows sometimes holds file handles briefly after process
        // termination; best-effort cleanup.
      }
    };

    try {
      // Await the banner or an early close/error, whichever comes first.
      const port = await new Promise<number>((resolveP, rejectP) => {
        let buf = "";
        const timer = setTimeout(() => {
          rejectP(new Error("hub fixture: timed out waiting for 'GUI listening on' banner"));
        }, 15_000);
        stdout.on("data", (chunk: Buffer) => {
          buf += chunk.toString("utf8");
          const m = buf.match(LISTEN_RE);
          if (m) {
            clearTimeout(timer);
            resolveP(Number(m[1]));
          }
        });
        stderr.on("data", (chunk: Buffer) => {
          // Surface stderr for debugging. Do not reject — pidport warnings
          // etc. land here and are not fatal.
          process.stderr.write("[hub stderr] " + chunk.toString("utf8"));
        });
        // If the process closes (including spawn-error ENOENT) before
        // we see a banner, reject startup. This is the catch for "the
        // binary is missing" and "the child crashed during Cobra init".
        closePromise.then(() => {
          clearTimeout(timer);
          if (spawnError) {
            rejectP(new Error(`hub fixture: child spawn error: ${spawnError.message}`));
          } else {
            rejectP(new Error(`hub fixture: child closed (code=${child.exitCode}) before banner`));
          }
        });
      });

      const handle: HubHandle = { url: `http://127.0.0.1:${port}`, port, home };

      // Wait for /api/ping to 200 before handing control to the test.
      // Poll up to 10s; if the loop exhausts without success, THROW
      // (cleanup runs via finally). 10s covers Windows TCP bind latency
      // right after a cold global-setup build — the banner fires as soon
      // as the listen socket is announced but the OS takes another beat
      // to actually accept connections when the machine is still
      // finishing go/npm compile I/O. 5s was too tight under that load.
      const deadline = Date.now() + 10_000;
      let pingOk = false;
      while (Date.now() < deadline && !closed) {
        try {
          const resp = await fetch(`${handle.url}/api/ping`);
          if (resp.ok) {
            pingOk = true;
            break;
          }
        } catch {
          // Connection refused during the race window — retry.
        }
        await new Promise((r) => setTimeout(r, 100));
      }
      if (!pingOk) {
        throw new Error(
          `hub fixture: /api/ping did not respond within 10s at ${handle.url}`,
        );
      }

      await use(handle);
    } finally {
      await cleanup();
    }
  },
});

export { expect } from "@playwright/test";
