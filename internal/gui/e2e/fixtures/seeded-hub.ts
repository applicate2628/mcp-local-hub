import { test as base } from "@playwright/test";
import { spawn } from "node:child_process";
import { mkdirSync, rmSync } from "node:fs";
import { resolve, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { hardenedTempHome } from "./hardened-temp";

const __dirname = dirname(fileURLToPath(import.meta.url));

// Matches the backend's "GUI listening on http://127.0.0.1:<port>" line
// (see internal/cli/gui.go). Capturing group 1 is the port. Kept in sync
// with the same regex in hub.ts.
const LISTEN_RE = /GUI listening on http:\/\/127\.0\.0\.1:(\d+)/;

export interface SeededHubHandle {
  url: string; // baseURL like "http://127.0.0.1:54321"
  port: number;
  home: string; // per-test HOME/USERPROFILE directory the seed wrote into
}

// SeedFn writes client-config files (and any other on-disk state) into the
// per-test temp home BEFORE the mcphub gui binary starts. Because the
// backend's /api/scan reads the real OS-default config paths resolved
// against $HOME (see internal/clients/clients.go ConfigPathForName), a file
// seeded here is observed by the live scanner exactly as a real installed
// client config would be — exercising the full embed-bundle + Go-backend
// roundtrip that page.route mocks cannot reach.
//
// `home` is the per-test temp directory already wired into HOME / USERPROFILE
// / LOCALAPPDATA / XDG_*. The seed runs synchronously after the dir is made
// and before spawn, so the binary's first scan sees the seeded layout.
export type SeedFn = (home: string) => void;

// seededHubFor builds a Playwright `test` whose `hub` fixture spawns mcphub
// gui against a temp home that has been populated by `seed` first. It mirrors
// the spawn / banner-wait / ping-poll / teardown mechanics of fixtures/hub.ts
// exactly; the ONLY behavioral difference is the synchronous `seed(home)`
// call between mkdtemp and spawn. hub.ts is left untouched so the 100+
// clean-home specs that depend on its empty-scan contract keep their exact
// behavior.
export function seededHubFor(seed: SeedFn) {
  return base.extend<{ hub: SeededHubHandle }>({
    hub: async ({}, use) => {
      const home = hardenedTempHome("mcphub-e2e-seeded-");

      // Populate the temp home with real client-config files BEFORE spawn so
      // the binary's first /api/scan observes them. A throw here fails the
      // test deterministically (no half-seeded run); clean up the dir first.
      try {
        seed(home);
      } catch (err) {
        try {
          rmSync(home, { recursive: true, force: true });
        } catch {
          // best-effort cleanup before rethrow
        }
        throw err;
      }

      // SHGetKnownFolderPath(FOLDERID_LocalAppData) expands
      // %USERPROFILE%\AppData\Local; with USERPROFILE redirected to the
      // temp home that subdir must exist or the state dir is unresolvable
      // → strict-mode fail-closed → hardened state-file writes 500. Same
      // rationale + refs as fixtures/hub.ts.
      const localAppData = join(home, "AppData", "Local");
      mkdirSync(localAppData, { recursive: true });
      // APPDATA (Roaming) redirect — same rationale as hub.ts: vscode/
      // cline/devin/amp resolvers read os.Getenv("APPDATA") first.
      const roamingAppData = join(home, "AppData", "Roaming");
      mkdirSync(roamingAppData, { recursive: true });
      const binPath = resolve(
        __dirname,
        "..",
        "bin",
        process.platform === "win32" ? "mcphub.exe" : "mcphub",
      );
      // Same state-path redirection as hub.ts: every env var the backend
      // consults for registry / logs / pidport is pinned at the temp home.
      const env: NodeJS.ProcessEnv = {
        ...process.env,
        HOME: home,
        USERPROFILE: home,
        LOCALAPPDATA: localAppData, // matches SHGetKnownFolderPath(USERPROFILE\AppData\Local)
        APPDATA: roamingAppData,    // Windows Roaming base (vscode/cline/devin/amp resolvers read this first)
        XDG_STATE_HOME: home,
        XDG_DATA_HOME: home,
        XDG_CONFIG_HOME: home,
        // Cross-process state-dir redirect (test_state_path_env binary only)
        // — fences the Windows state dir off the live fleet. See hub.ts.
        MCPHUB_STATE_DIR_OVERRIDE: join(localAppData, "mcp-local-hub"),
        MCPHUB_GUI_TEST_PIDPORT_DIR: home,
        // Same seams as hub.ts: noop scheduler so /api/status is [] and noop
        // supervisor so the GUI does not spend 15s waiting for an IPC bind
        // that cannot happen under a temp home with no supervisor-intent.json.
        MCPHUB_E2E_SCHEDULER: "none",
        MCPHUB_E2E_SUPERVISOR: "none",
        // Strict-mode neutralization + relax-lane opt-in — see hub.ts.
        MCPHUB_REQUIRE_SINGLE_USER_HOME: "",
        MCPHUB_ALLOW_UNHARDENED_STATE_WRITE: "1",
      };
      const child = spawn(
        binPath,
        ["gui", "--no-browser", "--no-tray", "--port", "0"],
        { env, stdio: ["ignore", "pipe", "pipe"] },
      );

      if (!child.stdout || !child.stderr) {
        throw new Error(
          "seeded-hub fixture: spawned child has no stdout/stderr pipes",
        );
      }
      const stdout = child.stdout;
      const stderr = child.stderr;

      // Drive lifecycle from 'close' + 'error', NOT 'exit' — identical
      // rationale to hub.ts (spawn failure emits 'error'+'close', never
      // 'exit'; an exit-only waiter would hang on ENOENT).
      let closed = false;
      let spawnError: Error | null = null;
      const closePromise = new Promise<void>((res) => {
        const markClosed = () => {
          closed = true;
          res();
        };
        child.once("close", markClosed);
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
          // Windows sometimes holds file handles briefly after termination.
        }
      };

      try {
        const port = await new Promise<number>((resolveP, rejectP) => {
          let buf = "";
          const timer = setTimeout(() => {
            rejectP(
              new Error(
                "seeded-hub fixture: timed out waiting for 'GUI listening on' banner",
              ),
            );
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
            process.stderr.write(
              "[seeded-hub stderr] " + chunk.toString("utf8"),
            );
          });
          closePromise.then(() => {
            clearTimeout(timer);
            if (spawnError) {
              rejectP(
                new Error(
                  `seeded-hub fixture: child spawn error: ${spawnError.message}`,
                ),
              );
            } else {
              rejectP(
                new Error(
                  `seeded-hub fixture: child closed (code=${child.exitCode}) before banner`,
                ),
              );
            }
          });
        });

        const handle: SeededHubHandle = {
          url: `http://127.0.0.1:${port}`,
          port,
          home,
        };

        // Wait for /api/ping to 200 before handing control to the test —
        // same 10s budget + close-aware loop as hub.ts.
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
            `seeded-hub fixture: /api/ping did not respond within 10s at ${handle.url}`,
          );
        }

        await use(handle);
      } finally {
        await cleanup();
      }
    },
  });
}

export { expect } from "@playwright/test";
