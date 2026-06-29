import { test as base } from "@playwright/test";
import { spawn } from "node:child_process";
import { mkdirSync, rmSync } from "node:fs";
import { resolve, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { hardenedTempHome } from "./hardened-temp";

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
    const home = hardenedTempHome("mcphub-e2e-");
    // The production binary resolves the Windows state directory via
    // SHGetKnownFolderPath(FOLDERID_LocalAppData), which expands
    // %USERPROFILE%\AppData\Local — it deliberately IGNORES the
    // LOCALAPPDATA env var (that fallback is compiled out unless the
    // `test_state_path_env` build tag is set, which the e2e binary is
    // NOT). With USERPROFILE redirected to the temp home, that subdir
    // must EXIST or SHGetKnownFolderPath fails → the state dir is
    // unresolvable → api.readStrictModeFromIntentBestEffort fails closed
    // to STRICT → every hardened state-file write (/api/dismiss,
    // capabilities probe, etc.) is refused with HTTP 500. Pre-creating
    // <home>\AppData\Local steers the resolver fully inside the sandbox.
    // (internal/api/state_paths_prod.go + client_write_init.go.)
    const localAppData = join(home, "AppData", "Local");
    mkdirSync(localAppData, { recursive: true });
    // APPDATA (Roaming) must ALSO be redirected: several client-config
    // resolvers (vscode -> %APPDATA%\Code\User\mcp.json, cline, devin,
    // amp — see internal/clients/clients.go:789, cline.go, devin.go)
    // read os.Getenv("APPDATA") FIRST and only fall back to
    // <home>\AppData\Roaming when it is unset. The fixture inherits the
    // real APPDATA from process.env, so without this redirect /api/scan
    // reads the developer's REAL %APPDATA%\Code\User\mcp.json and leaks
    // the live fleet into every empty-home spec (Discovery empty-state,
    // etc.).
    const roamingAppData = join(home, "AppData", "Roaming");
    mkdirSync(roamingAppData, { recursive: true });
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
      LOCALAPPDATA: localAppData,     // Windows logs/state base (matches the SHGetKnownFolderPath expansion of USERPROFILE\AppData\Local)
      APPDATA: roamingAppData,        // Windows Roaming base (vscode/cline/devin/amp client-config resolvers read this first)
      XDG_STATE_HOME: home,           // Linux state base
      XDG_DATA_HOME: home,            // Linux data base
      XDG_CONFIG_HOME: home,          // Linux config base
      // Cross-process state-dir redirect, honored ONLY by a binary built
      // with -tags=test_state_path_env (global-setup.ts does this). It
      // wins BEFORE the platform resolver on every GOOS, so it fences the
      // Windows state dir off the developer's real %LOCALAPPDATA%\mcp-local-hub
      // fleet — without it, SHGetKnownFolderPath reads the real profile from
      // the token (ignoring HOME/USERPROFILE/LOCALAPPDATA) and /api/scan +
      // /api/status leak the live managed daemons into every empty-home spec.
      MCPHUB_STATE_DIR_OVERRIDE: join(localAppData, "mcp-local-hub"),
      MCPHUB_GUI_TEST_PIDPORT_DIR: home,
      // Strict-mode neutralization: a developer's corp-policy shell may
      // export MCPHUB_REQUIRE_SINGLE_USER_HOME=1, which would force the
      // strict parent-dir gate and refuse every state-file write under
      // the (often broadened-DACL) OS temp dir. Force it off for the
      // sandbox and explicitly opt into the documented relax lane so a
      // temp dir whose parent grants Authenticated Users (S-1-5-11) does
      // not trip the secondary TOCTOU write-bits check. The per-file
      // DACL still makes every written state file owner-only. See
      // CLAUDE.md "Hardened state-file writes".
      MCPHUB_REQUIRE_SINGLE_USER_HOME: "",
      MCPHUB_ALLOW_UNHARDENED_STATE_WRITE: "1",
      // Task 3 seam: force scheduler.New() to return the noop impl so
      // /api/status returns [] regardless of the host's installed
      // mcp-local-hub-* scheduler tasks. Without this a dev box with
      // 13 installed daemons renders 13 Dashboard cards and empty-state
      // assertions fail locally even though CI passes.
      MCPHUB_E2E_SCHEDULER: "none",
      // PR #212: GUI now spawns a supervisor child by default. Tests
      // run under a temp HOME with no supervisor-intent.json, so the
      // spawn would always time out 15s waiting for IPC bind that
      // can't happen. Suppress the spawn block parallel to the
      // scheduler seam above.
      MCPHUB_E2E_SUPERVISOR: "none",
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
