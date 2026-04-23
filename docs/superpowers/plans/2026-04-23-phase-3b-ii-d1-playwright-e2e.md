# Phase 3B-II D1 — Playwright E2E Suite

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add headless-browser end-to-end tests for the Vite+Preact GUI that spawn a real `mcphub gui` backend, verify DOM behavior on the three existing screens (Servers / Dashboard / Logs), and run as a CI job separate from `go test`. Unlocks regression-safe delivery of the Phase 3B-II screens A1–A5.

**Architecture:** Tests live under `internal/gui/e2e/` — a standalone Node project distinct from `internal/gui/frontend/` (the Preact source) so they don't bloat the frontend dev loop or drag Playwright browsers into normal `npm install`. A Playwright global-setup first rebuilds the frontend bundle (`npm --prefix ../frontend run build`) to avoid stale assets, then compiles `cmd/mcphub` once to `e2e/bin/mcphub(.exe)` for the run. Each test spawns that binary with `--port 0` + a per-test temp dir pinned across all known state env vars (`HOME`, `USERPROFILE`, `LOCALAPPDATA`, `XDG_STATE_HOME`, `XDG_DATA_HOME`, `XDG_CONFIG_HOME`, `MCPHUB_GUI_TEST_PIDPORT_DIR`), parses stdout for `GUI listening on http://127.0.0.1:<port>`, waits until `/api/ping` answers, then drives Chromium against that URL. Teardown SIGTERMs the process and awaits its real exit.

**Tech Stack:** `@playwright/test` latest, TypeScript 5, Node 20+, headless Chromium. No Preact / Vite dep overlap — this project is test-tooling only.

**Platform:** Windows-only for now. The `/api/status` route calls `scheduler.New()` which returns "not implemented" on Linux/Darwin (see `internal/scheduler/scheduler_linux.go`), so Dashboard + Logs tests would hit 500s on non-Windows CI. Spec §2.2 makes Windows the production target; Linux/macOS E2E is out of scope until a backend test seam for scheduler-less status exists.

**Clean-home behavior:** On a fresh `tmpHome`, `/api/scan` returns an empty `entries` array because it only reads EXISTING client config files (it does NOT enumerate shipped manifests). Task 6 therefore asserts the **empty-state** Servers matrix (headers only, zero rows, Apply disabled) rather than seeding fake client configs. Richer matrix coverage is deferred to a later plan item that introduces a config-seed fixture helper.

**Deterministic scheduler state:** `/api/status` calls `scheduler.New()` which on Windows queries the system-global Task Scheduler. Redirecting `HOME` does NOT filter that output — a developer box with installed hub tasks returns those rows, while fresh CI runners return empty. Task 3 adds a Go-side env-seam: `MCPHUB_E2E_SCHEDULER=none` makes `scheduler.New()` return an empty noop scheduler so every Dashboard/Logs test sees zero daemons regardless of the host machine. The fixture sets that env var for every child. Guarded by prefix-convention (`MCPHUB_E2E_*`) + a startup warning so accidental production use is visible.

---

## File structure

```
internal/gui/e2e/
├── .gitignore                        (node_modules, test-results, playwright-report, bin)
├── package.json                      (playwright + TS only)
├── tsconfig.json
├── playwright.config.ts              (webServer: global-setup-spawned binary; projects: chromium)
├── global-setup.ts                   (builds cmd/mcphub → bin/mcphub on startup)
├── fixtures/
│   └── hub.ts                        (per-test fixture: spawn binary, parse port, wait ready, teardown)
├── tests/
│   ├── shell.spec.ts                 (sidebar + hash routing + nav highlight)
│   ├── servers.spec.ts               (matrix rows, disabled cells, Apply-disabled)
│   ├── dashboard.spec.ts             (empty cards, /api/events SSE opens)
│   └── logs.spec.ts                  (picker empty, notice text)
└── bin/                              (build artifact, gitignored)
```

Root-level additions:
- `CLAUDE.md` — new "E2E tests (Playwright)" subsection.
- `Makefile` OR npm script in root (no Makefile currently in repo — add an npm script inside `internal/gui/e2e/package.json` and document invocation in `CLAUDE.md`; no repo-root Makefile needed).

**Why per-test fresh binary spawn (not a shared one):**
- Each test gets a clean `HOME` → deterministic `/api/scan` output (no leaked state from prior test).
- Parallel-safe out of the box (no port coordination needed — OS picks a fresh port per spawn).
- Cost: ~300ms per spawn. With 4–8 tests total, total overhead ~2-3s. Acceptable for a regression suite run in CI, not on every save.

**Why pre-built binary (not `go run`):**
- `go run` recompiles every spawn → adds ~5s × N tests. With N tests that's tens of seconds of wasted compile.
- Pre-build once in `global-setup.ts`, reuse for all spawns.

---

## Task 1: Scaffold Playwright project

**Files:**
- Create: `internal/gui/e2e/.gitignore`
- Create: `internal/gui/e2e/package.json`
- Create: `internal/gui/e2e/tsconfig.json`
- Create: `internal/gui/e2e/playwright.config.ts`

- [ ] **Step 1: Create `internal/gui/e2e/.gitignore`**

```
node_modules/
test-results/
playwright-report/
bin/
*.log
```

- [ ] **Step 2: Create `internal/gui/e2e/package.json`**

```json
{
  "name": "mcp-local-hub-e2e",
  "private": true,
  "version": "0.0.0",
  "type": "module",
  "scripts": {
    "test": "playwright test",
    "test:headed": "playwright test --headed",
    "test:debug": "playwright test --debug",
    "install-browsers": "playwright install chromium --with-deps"
  },
  "devDependencies": {
    "@playwright/test": "^1.48.0",
    "@types/node": "^20.0.0",
    "typescript": "^5.5.0"
  }
}
```

- [ ] **Step 3: Create `internal/gui/e2e/tsconfig.json`**

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "skipLibCheck": true,
    "esModuleInterop": true,
    "resolveJsonModule": true,
    "types": ["node"]
  },
  "include": ["**/*.ts"]
}
```

- [ ] **Step 4: Create `internal/gui/e2e/playwright.config.ts`**

```ts
import { defineConfig } from "@playwright/test";

// Playwright drives real Chromium against a per-test-spawned mcphub gui
// process. The global setup compiles the Go binary once before the run
// so individual tests can spawn it in ~300ms instead of re-running
// `go run` (~5s compile) on each test.
export default defineConfig({
  testDir: "./tests",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? "github" : [["list"], ["html", { open: "never" }]],
  globalSetup: "./global-setup.ts",
  use: {
    // baseURL is injected per-test via the `hub` fixture. Leave unset here.
    trace: "on-first-retry",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { browserName: "chromium" },
    },
  ],
  timeout: 30_000,
  expect: { timeout: 5_000 },
});
```

- [ ] **Step 5: Install dependencies and Chromium**

From repo root:

```bash
cd internal/gui/e2e
npm install
npx playwright install chromium
```

Expected: install succeeds. `~/.cache/ms-playwright/` (or Windows equivalent) gains a Chromium build.

**Do NOT run `npx playwright test` yet** — there are no tests and no fixture/global-setup files. Later tasks add those.

- [ ] **Step 6: Commit**

```bash
git add internal/gui/e2e/.gitignore internal/gui/e2e/package.json internal/gui/e2e/tsconfig.json internal/gui/e2e/playwright.config.ts internal/gui/e2e/package-lock.json
git commit -m "feat(gui/e2e): scaffold Playwright E2E project

Adds internal/gui/e2e/ with package.json, tsconfig, playwright.config.
Separate Node project from internal/gui/frontend/ so the Preact dev
loop does not pull Playwright browsers into every npm install.
Tests, fixtures, and global-setup land in later tasks."
```

---

## Task 2: Global setup — build the binary once

**Files:**
- Create: `internal/gui/e2e/global-setup.ts`

- [ ] **Step 1: Create `internal/gui/e2e/global-setup.ts`**

```ts
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
```

- [ ] **Step 2: Verify the config references global-setup.ts**

Check `internal/gui/e2e/playwright.config.ts` already has `globalSetup: "./global-setup.ts"` from Task 1. (It does — step 4 above.)

- [ ] **Step 3: Verify global setup runs by executing the build commands directly**

`npx playwright test --list` does NOT invoke globalSetup (it only lists tests). To actually run the build chain, either add a trivial placeholder test first or invoke the two commands directly. Easier and more honest is the direct path:

```bash
cd internal/gui/frontend
npm ci
npm run build
cd ../e2e
go build -o bin/mcphub ./../../../cmd/mcphub      # from e2e/, Go module root is the repo root
# On Windows, the binary name needs .exe — either let Go pick it or be explicit:
# go build -o bin/mcphub.exe ./../../../cmd/mcphub
ls bin/
```

Expected: `bin/mcphub` (or `mcphub.exe`) present and executable. This proves the commands globalSetup uses will succeed at test time. globalSetup itself fires on the first real test run (Task 5 onwards).

If `npm ci` fails because `internal/gui/frontend/node_modules` is absent in fresh clones — that is expected and Task 9 / CLAUDE.md setup step covers it. For THIS step, run `npm ci` first to have a known-good state.

If `go build` fails (e.g., `go: command not found`): report BLOCKED — the Go toolchain is a hard prereq.

- [ ] **Step 4: Commit**

```bash
git add internal/gui/e2e/global-setup.ts
git commit -m "feat(gui/e2e): global setup compiles mcphub binary once per run

Before any test spawns a hub, globalSetup builds cmd/mcphub into
internal/gui/e2e/bin/. Per-test spawn then invokes the binary
directly (~300ms) instead of re-running \`go run\` (~5s compile)
on every test."
```

---

## Task 3: Backend scheduler E2E seam

Adds a Go-side noop scheduler that `scheduler.New()` returns when
`MCPHUB_E2E_SCHEDULER=none` is set. The hub fixture (Task 4) sets
that env var for every spawned child so Dashboard/Logs tests see
zero daemons regardless of the developer's installed mcp-local-hub
scheduler tasks.

**Files:**
- Modify: `internal/scheduler/scheduler.go` (env-check in `New()`)
- Create: `internal/scheduler/scheduler_noop.go`
- Create: `internal/scheduler/scheduler_noop_test.go`

### Step 1: Modify `internal/scheduler/scheduler.go`

Find the existing:

```go
// New returns the platform-appropriate Scheduler implementation for the current OS.
// Defined per-OS in scheduler_<os>.go.
func New() (Scheduler, error) {
	return newPlatformScheduler()
}
```

Replace with:

```go
// e2eSchedulerEnv, when set to "none", swaps scheduler.New()'s return
// value for a noop scheduler that records no tasks and reports empty
// results. Used exclusively by the Playwright E2E fixture so tests
// run against a deterministic empty state regardless of whatever
// mcp-local-hub-* tasks the host Task Scheduler happens to have
// installed. The prefix is a convention so accidental production
// use is obvious both in code review and in the startup log.
const e2eSchedulerEnv = "MCPHUB_E2E_SCHEDULER"

// New returns the platform-appropriate Scheduler implementation for the current OS.
// Defined per-OS in scheduler_<os>.go. If MCPHUB_E2E_SCHEDULER=none is set,
// returns the noop scheduler instead — test-only; never set in production.
func New() (Scheduler, error) {
	if os.Getenv(e2eSchedulerEnv) == "none" {
		// Log to stderr so accidental production activation is visible
		// in daemon/hub logs the next time an operator investigates.
		fmt.Fprintf(os.Stderr,
			"warning: %s=none — scheduler returns empty/noop responses. This flag is for E2E tests only; never set it in production.\n",
			e2eSchedulerEnv)
		return &noopScheduler{}, nil
	}
	return newPlatformScheduler()
}
```

Add imports to the existing import block at the top of the file:

```go
import (
	"errors"
	"fmt"
	"os"
)
```

### Step 2: Create `internal/scheduler/scheduler_noop.go`

```go
package scheduler

// noopScheduler is a test-only Scheduler that records no tasks and returns
// empty results for List/Status. Activated via the MCPHUB_E2E_SCHEDULER=none
// env var from the Playwright E2E fixture so tests run against deterministic
// empty scheduler state regardless of the developer's real Task Scheduler
// contents. Never construct this outside tests — New() is the only
// production-callable constructor, and it only returns noopScheduler when
// the env seam is explicit.
type noopScheduler struct{}

func (*noopScheduler) Create(TaskSpec) error                  { return nil }
func (*noopScheduler) Delete(string) error                    { return nil }
func (*noopScheduler) Run(string) error                       { return nil }
func (*noopScheduler) Stop(string) error                      { return nil }
func (*noopScheduler) Status(string) (TaskStatus, error)      { return TaskStatus{}, ErrTaskNotFound }
func (*noopScheduler) List(string) ([]TaskStatus, error)      { return nil, nil }
func (*noopScheduler) ExportXML(string) ([]byte, error)       { return nil, ErrTaskNotFound }
func (*noopScheduler) ImportXML(string, []byte) error         { return nil }
```

### Step 3: Create `internal/scheduler/scheduler_noop_test.go`

```go
package scheduler

import (
	"errors"
	"testing"
)

func TestNew_WithoutEnvReturnsPlatformImpl(t *testing.T) {
	t.Setenv(e2eSchedulerEnv, "")
	s, err := New()
	// On Linux/Darwin, newPlatformScheduler() errors out ("not
	// implemented") — (nil, err). Either way, default path must
	// NOT silently return the noop.
	if err == nil {
		if _, ok := s.(*noopScheduler); ok {
			t.Fatalf("default path must not return noopScheduler")
		}
	}
}

func TestNew_WithE2EEnvReturnsNoop(t *testing.T) {
	t.Setenv(e2eSchedulerEnv, "none")
	s, err := New()
	if err != nil {
		t.Fatalf("noop path must not error: %v", err)
	}
	if _, ok := s.(*noopScheduler); !ok {
		t.Fatalf("MCPHUB_E2E_SCHEDULER=none must return *noopScheduler, got %T", s)
	}
	tasks, err := s.List("mcp-local-hub-")
	if err != nil {
		t.Fatalf("noop List must not error: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("noop List must return empty, got %d entries", len(tasks))
	}
}

func TestNoopScheduler_StatusReturnsNotFound(t *testing.T) {
	var s Scheduler = &noopScheduler{}
	_, err := s.Status("anything")
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("noop Status must return ErrTaskNotFound, got %v", err)
	}
}

func TestNoopScheduler_CreateRunDeleteAreNoOps(t *testing.T) {
	var s Scheduler = &noopScheduler{}
	if err := s.Create(TaskSpec{Name: "x"}); err != nil {
		t.Errorf("Create: %v", err)
	}
	if err := s.Run("x"); err != nil {
		t.Errorf("Run: %v", err)
	}
	if err := s.Stop("x"); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := s.Delete("x"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := s.ImportXML("x", []byte("<Task/>")); err != nil {
		t.Errorf("ImportXML: %v", err)
	}
}
```

### Step 4: Verify

```bash
go test ./internal/scheduler/ -count=1 -run "TestNew_|TestNoopScheduler"
```

Expected: all 4 tests PASS. Also confirm the full scheduler suite still runs clean:

```bash
go test ./internal/scheduler/ -count=1
```

Expected: all prior scheduler tests + the 4 new ones PASS.

### Step 5: Confirm the seam is visible when active

```bash
MCPHUB_E2E_SCHEDULER=none go run ./cmd/mcphub gui --no-browser --no-tray --port 0 2>&1 | head -3
# On Windows PowerShell:
#   $env:MCPHUB_E2E_SCHEDULER="none"; go run ./cmd/mcphub gui --no-browser --no-tray --port 0
```

Expected output includes a `warning: MCPHUB_E2E_SCHEDULER=none — scheduler returns empty/noop responses. ...` line on stderr before the `GUI listening on ...` banner. Kill the process (Ctrl+C) after the banner appears.

### Step 6: Commit

```bash
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_noop.go internal/scheduler/scheduler_noop_test.go
git commit -m "feat(scheduler): add noop seam for E2E testing (MCPHUB_E2E_SCHEDULER=none)

Phase 3B-II D1 Playwright tests need a deterministic empty scheduler
state on every machine. Windows Task Scheduler is system-global —
HOME-override does not filter its output — so a developer with 13
installed mcp-local-hub-* tasks saw 13 Dashboard cards while CI with
0 tasks saw 0, and empty-state assertions could not work on both.

Add a noopScheduler that Create/Delete/Run/Stop no-op and List/Status
return empty/ErrTaskNotFound. scheduler.New() returns it when
MCPHUB_E2E_SCHEDULER=none is set and logs a visible warning to stderr.
The MCPHUB_E2E_* prefix + the warning log make accidental production
activation obvious on review and in logs."
```

---

## Task 4: Hub fixture — spawn, parse port, wait ready, teardown

**Files:**
- Create: `internal/gui/e2e/fixtures/hub.ts`

- [ ] **Step 1: Create `internal/gui/e2e/fixtures/hub.ts`**

```ts
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
// awaits the real 'exit' event (child.killed only reports "signal
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
      // Poll up to 5s; if the loop exhausts without success, THROW
      // (cleanup runs via finally).
      const deadline = Date.now() + 5_000;
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
          `hub fixture: /api/ping did not respond within 5s at ${handle.url}`,
        );
      }

      await use(handle);
    } finally {
      await cleanup();
    }
  },
});

export { expect } from "@playwright/test";
```

- [ ] **Step 2: Commit (no test yet, fixture compiles in isolation)**

```bash
cd internal/gui/e2e
npx tsc --noEmit
```

Expected: exit 0 (no TS errors).

```bash
git add internal/gui/e2e/fixtures/
git commit -m "feat(gui/e2e): hub fixture spawns binary with per-test HOME

Each test gets its own tmpHome (HOME + USERPROFILE + pidport dir)
and a fresh port (OS-assigned via --port 0). Fixture parses the
'GUI listening on ...' banner from stdout, polls /api/ping until
ready, hands the test a {url, port, home} handle, and SIGTERMs
on teardown."
```

---

## Task 5: Shell smoke test — sidebar + hash routing

**Files:**
- Create: `internal/gui/e2e/tests/shell.spec.ts`

- [ ] **Step 1: Create `internal/gui/e2e/tests/shell.spec.ts`**

```ts
import { test, expect } from "../fixtures/hub";

test.describe("shell", () => {
  test("renders sidebar with brand + three nav links", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    // Brand
    await expect(page.locator(".sidebar .brand")).toHaveText("mcp-local-hub");
    // Three nav links
    const links = page.locator(".sidebar nav a");
    await expect(links).toHaveCount(3);
    await expect(links.nth(0)).toHaveText("Servers");
    await expect(links.nth(1)).toHaveText("Dashboard");
    await expect(links.nth(2)).toHaveText("Logs");
  });

  test("default route is Servers and nav highlights on click", async ({ page, hub }) => {
    await page.goto(`${hub.url}/`);
    // Default → Servers active
    const serversLink = page.locator(".sidebar nav a", { hasText: "Servers" });
    await expect(serversLink).toHaveClass(/active/);
    // Click Dashboard
    await page.locator(".sidebar nav a", { hasText: "Dashboard" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Dashboard" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    // Click Logs
    await page.locator(".sidebar nav a", { hasText: "Logs" }).click();
    await expect(page.locator(".sidebar nav a", { hasText: "Logs" })).toHaveClass(/active/);
    await expect(page.locator("h1")).toHaveText("Logs");
  });

  test("hashchange triggers screen swap (browser back/forward)", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("h1")).toHaveText("Logs");
    await page.goBack();
    await expect(page.locator("h1")).toHaveText("Dashboard");
  });
});
```

- [ ] **Step 2: Run this test file**

```bash
cd internal/gui/e2e
npx playwright test tests/shell.spec.ts
```

Expected: 3 passed. If any fails, inspect the HTML screenshot in `test-results/` — the fixture may have timed out waiting for the banner (Go build issue) or the Preact shell may not have rendered the sidebar.

- [ ] **Step 3: Commit**

```bash
git add internal/gui/e2e/tests/shell.spec.ts
git commit -m "test(gui/e2e): shell smoke — sidebar + hash routing"
```

---

## Task 6: Servers smoke test — empty-state matrix + disabled Apply

`/api/scan` only reads EXISTING client configs (see `internal/api/scan.go`). On the fixture's clean `tmpHome`, no configs exist, so `entries` is empty and the matrix renders with headers and zero rows. Tests lock that empty-state behavior down — richer populated-row tests are deferred to a follow-up plan item that introduces a config-seed helper.

**Files:**
- Create: `internal/gui/e2e/tests/servers.spec.ts`

- [ ] **Step 1: Create `internal/gui/e2e/tests/servers.spec.ts`**

```ts
import { test, expect } from "../fixtures/hub";

test.describe("servers", () => {
  test("matrix renders headers with correct column set", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    // Title
    await expect(page.locator("h1")).toHaveText("Servers");
    // Matrix table
    const matrix = page.locator("table.servers-matrix");
    await expect(matrix).toBeVisible();
    // Header: Server + 4 clients + Port + State = 7 columns
    const headerCells = matrix.locator("thead th");
    await expect(headerCells).toHaveCount(7);
    await expect(headerCells.nth(0)).toHaveText("Server");
    await expect(headerCells.nth(1)).toHaveText("claude-code");
    await expect(headerCells.nth(2)).toHaveText("codex-cli");
    await expect(headerCells.nth(3)).toHaveText("gemini-cli");
    await expect(headerCells.nth(4)).toHaveText("antigravity");
    await expect(headerCells.nth(5)).toHaveText("Port");
    await expect(headerCells.nth(6)).toHaveText("State");
  });

  test("empty body when tmpHome has no client configs", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    // On a fresh tmpHome, /api/scan returns no entries (it only
    // enumerates EXISTING client configs). The matrix body should
    // render zero rows. Wait for the "Loading…" state to clear.
    const matrix = page.locator("table.servers-matrix");
    await expect(matrix).toBeVisible();
    await expect(matrix.locator("tbody tr")).toHaveCount(0);
    // No "Loading…" text left hanging around.
    await expect(page.getByText("Loading…")).toHaveCount(0);
  });

  test("Apply button is disabled with no dirty cells", async ({ page, hub }) => {
    await page.goto(`${hub.url}/#/servers`);
    const applyBtn = page.getByRole("button", { name: "Apply changes" });
    await expect(applyBtn).toBeVisible();
    await expect(applyBtn).toBeDisabled();
  });
});
```

- [ ] **Step 2: Run**

```bash
cd internal/gui/e2e
npx playwright test tests/servers.spec.ts
```

Expected: 3 passed.

- [ ] **Step 3: Commit**

```bash
git add internal/gui/e2e/tests/servers.spec.ts
git commit -m "test(gui/e2e): servers headers + empty-state body + disabled Apply"
```

---

## Task 7: Dashboard smoke test — empty cards + SSE connect

**Files:**
- Create: `internal/gui/e2e/tests/dashboard.spec.ts`

- [ ] **Step 1: Create `internal/gui/e2e/tests/dashboard.spec.ts`**

```ts
import { test, expect } from "../fixtures/hub";

test.describe("dashboard", () => {
  test("renders Dashboard heading and empty cards container on fresh home", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/dashboard`);
    await expect(page.locator("h1")).toHaveText("Dashboard");
    // .cards container exists. On a fresh tmpHome no daemons run, so
    // no card elements should be present.
    const cards = page.locator(".cards");
    await expect(cards).toBeVisible();
    await expect(cards.locator(".card")).toHaveCount(0);
  });

  test("opens an EventSource to /api/events on mount", async ({ page, hub }) => {
    // Register the listener BEFORE navigating so a fast-firing request
    // during the initial mount is not missed. Then navigate and await
    // the promise. Using waitForRequest AFTER goto is racy: if the
    // EventSource opens between the goto() resolving and waitForRequest
    // subscribing, the request never shows up.
    const reqPromise = page.waitForRequest(
      (r) => r.url() === `${hub.url}/api/events`,
      { timeout: 5_000 },
    );
    await page.goto(`${hub.url}/#/dashboard`);
    const req = await reqPromise;
    expect(req.method()).toBe("GET");
  });
});
```

Notes:
- A third test that asserted "no new /api/events requests for 500ms after navigating away" was removed — that pattern cannot actually prove the prior SSE stream closed (it only proves no new request was made, which was true regardless). Cleanup on screen swap is exercised by the `useEventSource` hook's `useEffect` return; a real regression test would need Chrome DevTools Protocol access to observe connection state, which is out of D1 scope.

- [ ] **Step 2: Run**

```bash
cd internal/gui/e2e
npx playwright test tests/dashboard.spec.ts
```

Expected: 2 passed.

- [ ] **Step 3: Commit**

```bash
git add internal/gui/e2e/tests/dashboard.spec.ts
git commit -m "test(gui/e2e): dashboard empty state + SSE connect"
```

---

## Task 8: Logs smoke test — empty picker + notice

**Files:**
- Create: `internal/gui/e2e/tests/logs.spec.ts`

- [ ] **Step 1: Create `internal/gui/e2e/tests/logs.spec.ts`**

```ts
import { test, expect } from "../fixtures/hub";

test.describe("logs", () => {
  test("renders heading + controls + notice when no daemons are running", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/logs`);
    await expect(page.locator("h1")).toHaveText("Logs");
    // Controls container exists with a select, tail input, follow box,
    // and refresh button.
    const controls = page.locator("#logs-controls");
    await expect(controls).toBeVisible();
    await expect(controls.locator("select")).toBeVisible();
    await expect(controls.locator("input[type=number]")).toBeVisible();
    await expect(controls.locator("input[type=checkbox]")).toBeVisible();
    await expect(controls.locator("button", { hasText: "Refresh" })).toBeVisible();
  });

  test("picker is empty + notice explains no daemons running", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/logs`);
    // On a fresh tmpHome, /api/status returns an empty DaemonStatus[].
    // The component renders a notice in <pre id="logs-body">.
    const body = page.locator("#logs-body");
    await expect(body).toBeVisible();
    // Wait briefly for the status fetch to resolve and the notice to
    // populate.
    await expect(body).toContainText(/no daemons running|no global-server logs/i, {
      timeout: 5_000,
    });
  });

  test("controls are disabled when no daemons are eligible", async ({
    page,
    hub,
  }) => {
    await page.goto(`${hub.url}/#/logs`);
    // Wait for the notice to appear (proxy for "status fetch done").
    await expect(page.locator("#logs-body")).toContainText(
      /no daemons running|no global-server logs/i,
      { timeout: 5_000 },
    );
    const controls = page.locator("#logs-controls");
    await expect(controls.locator("select")).toBeDisabled();
    await expect(controls.locator("input[type=number]")).toBeDisabled();
    await expect(controls.locator("input[type=checkbox]")).toBeDisabled();
    await expect(controls.locator("button", { hasText: "Refresh" })).toBeDisabled();
  });
});
```

- [ ] **Step 2: Run**

```bash
cd internal/gui/e2e
npx playwright test tests/logs.spec.ts
```

Expected: 3 passed.

- [ ] **Step 3: Commit**

```bash
git add internal/gui/e2e/tests/logs.spec.ts
git commit -m "test(gui/e2e): logs empty-picker + notice + disabled controls"
```

---

## Task 9: CLAUDE.md documentation + full-suite run

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Read `CLAUDE.md` at repo root and append the E2E section**

Append this block after the existing "GUI frontend (Phase 3B-II onward)" section:

```markdown
## GUI E2E tests (Phase 3B-II onward)

End-to-end browser tests live under `internal/gui/e2e/` (Playwright +
TypeScript, headless Chromium). They spawn a real `mcphub gui`
binary per-test with `HOME`/`USERPROFILE` redirected to a temp dir
so tests never touch the developer's real config, and drive the
Preact UI against the live Go backend.

### One-time setup

```bash
# Frontend deps are required because global-setup.ts runs `npm run build`
# on the frontend before building the Go binary. Fresh clones need this
# step first.
cd internal/gui/frontend
npm ci

cd ../e2e
npm ci
npx playwright install chromium --with-deps
```

### Running

```bash
cd internal/gui/e2e
npm test                # headless
npm run test:headed     # see the browser
npm run test:debug      # Playwright Inspector step-through
```

The `global-setup.ts` compiles `cmd/mcphub` into `internal/gui/e2e/bin/`
once per run. Each test spawns that binary with `--port 0` so the OS
picks a free port — tests are parallel-safe.

### CI (Windows-only)

Run E2E as a separate job from `go test` on a Windows runner. The GUI's
`/api/status` route goes through the real scheduler; `scheduler.New()`
on Linux/macOS returns "not implemented" and the status route 500s, so
Dashboard/Logs tests would fail on non-Windows runners. Pin this job
to `windows-latest` until a scheduler-less test seam exists.

```yaml
jobs:
  e2e:
    runs-on: windows-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
      - uses: actions/setup-node@v4
        with: { node-version: 20 }
      - run: cd internal/gui/frontend && npm ci
      - run: cd internal/gui/e2e && npm ci
      - run: cd internal/gui/e2e && npx playwright install chromium
      - run: cd internal/gui/e2e && npm test
```

### What's covered

- Shell: sidebar, three nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells.
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.

### What's NOT covered (future)

- Populated-row matrix tests (needs a client-config seed fixture — deferred to a follow-up plan item).
- Real migrate/restart flows (needs populated client configs).
- Dashboard SSE cleanup on screen swap — the `useEffect` return is the implementation, but Playwright's request API cannot observe connection close. A future CDP-based test could.
- Workspace-scoped daemons (Phase 3B-II D3).
- Tray icon (Windows-only, native surface Playwright can't reach — manual smoke per D2).
- Linux/macOS (blocked on scheduler test seam).
```

- [ ] **Step 2: Run the full suite from scratch**

Starting from a cold state (no binary built, no browsers installed — if you've already done this once, skip `install-browsers`):

```bash
cd internal/gui/e2e
npm test
```

Expected: 11 tests pass (3 shell + 3 servers + 2 dashboard + 3 logs). If any fail, inspect `playwright-report/` via `npx playwright show-report`.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: document Phase 3B-II D1 Playwright E2E workflow in CLAUDE.md

One-time install, run commands, what's covered, what's NOT yet,
and a minimal CI recipe."
```

---

## Task 10: End-to-end verification and merge-readiness check

**Files:** none (verification only)

- [ ] **Step 1: Clean rebuild from scratch**

```bash
# Rebuild frontend deps too — globalSetup depends on them.
cd internal/gui/frontend
rm -rf node_modules/
npm ci

cd ../e2e
rm -rf bin/ node_modules/ test-results/ playwright-report/
npm ci
npx playwright install chromium
npm test
```

Expected: all 11 tests green. Total wall-time should be under 60s.

- [ ] **Step 2: Confirm the e2e suite does not interfere with Go tests**

From repo root:

```bash
go test ./... -count=1
```

Expected: all Go tests still pass. If `internal/gui/` tests fail: the e2e suite should not have touched Go code; investigate separately.

- [ ] **Step 3: Confirm the e2e `bin/` is gitignored**

```bash
git status internal/gui/e2e/bin
```

Expected: no output (bin/ is gitignored).

- [ ] **Step 4: Tag the milestone (optional, local only)**

```bash
git tag d1-playwright-complete
```

- [ ] **Step 5: No final commit needed if git status is clean**

```bash
git status
```

Expected: `nothing to commit, working tree clean`.

---

## Dependency order summary

Task 1 (scaffold) → Task 2 (global-setup binary build) → Task 3 (backend scheduler E2E seam) → Task 4 (hub fixture) → Tasks 5-8 (four smoke specs — can run in any order after Task 4) → Task 9 (docs + full run) → Task 10 (verify).

- Task 1 produces a Node project; nothing depends on it besides later e2e tasks.
- Task 2 requires Task 1's `playwright.config.ts` referencing `./global-setup.ts`.
- Task 3 (backend seam) is independent of Tasks 1-2 in code but lands before the fixture so the fixture's `MCPHUB_E2E_SCHEDULER=none` env actually does something.
- Task 4 requires Task 2's binary build AND Task 3's backend seam so the fixture can spawn a scheduler-free hub.
- Tasks 5-8 each require Task 4 but are independent of each other.
- Task 9 documents the surface assuming 5-8 are green.
- Task 10 is the closeout verification.
