# mcp-local-hub — Claude Code notes

This file documents developer workflows and conventions for this repo that
are load-bearing enough to be worth surfacing to the agent by default. Add
new sections as they become necessary.

## GUI frontend (Phase 3B-II onward)

The web UI lives under `internal/gui/frontend/` (Vite + TypeScript +
Preact). Built artifacts land in `internal/gui/assets/` and are
committed — `go build` does not require Node.

### Day-to-day frontend dev (hot reload)

```bash
# Terminal 1: Go backend with a fixed port so Vite proxy can target it.
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125

# Terminal 2: Vite dev server on 5173 with /api → 9125 proxy.
cd internal/gui/frontend
npm install  # once
npm run dev
# Browse http://localhost:5173
```

### Build + smoke against embedded assets (what ships)

```bash
cd internal/gui/frontend
npm run build
cd ../../..
go run ./cmd/mcphub gui --no-browser --no-tray --port 9125
# Browse http://127.0.0.1:9125
```

### Regenerate the embedded bundle (CI + commits)

```bash
go generate ./internal/gui/...
```

This calls `npm run build` under `internal/gui/frontend/` and
overwrites `internal/gui/assets/{index.html,app.js,style.css}`. Always
rebuild before committing frontend changes so the embedded bundle
matches the source.

### Tests

- Frontend unit tests (pure helpers): `cd internal/gui/frontend && npm run test`
- Type-check: `cd internal/gui/frontend && npm run typecheck`
- Go-side embed smoke: `go test ./internal/gui/`

## GUI E2E tests (Phase 3B-II onward)

End-to-end browser tests live under `internal/gui/e2e/` (Playwright +
TypeScript, headless Chromium). They spawn a real `mcphub gui`
binary per-test with `HOME`/`USERPROFILE` redirected to a temp dir
so tests never touch the developer's real config, and drive the
Preact UI against the live Go backend. The backend scheduler is
redirected to an empty-noop via `MCPHUB_E2E_SCHEDULER=none` so
/api/status returns [] regardless of the host's installed
mcp-local-hub-* tasks.

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

The `global-setup.ts` rebuilds `internal/gui/assets/` then compiles
`cmd/mcphub` into `internal/gui/e2e/bin/` once per run. Each test
spawns that binary with `--port 0` so the OS picks a free port —
tests are parallel-safe.

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

- Shell: sidebar, eight nav links, hash routing, active-link highlight.
- Servers: matrix columns (Server + 4 clients + Port + State), empty-body state on clean tmpHome, Apply disabled with no dirty cells, uncheck-via-hub + Apply posts /api/demigrate narrowed to cell, mixed Apply dispatches demigrate-before-migrate ordering, demigrate failure always-reloads and retains failed entry for retry, via-hub tooltip describes Uncheck-and-Apply semantic (no more 'mcphub rollback --client' stale text), per-client gate + 3-outcome pruning (failed + gated retained, succeeded pruned) with second-Apply retry firing exactly the previously-gated migrate.
- Migration: h1, empty-state copy, group sections hidden on empty home, hashchange swap from Servers, full POST /api/dismiss → on-disk JSON → GET /api/dismissed round-trip, /api/scan-unfiltered regression guard (seed + dismiss + re-scan).
- Add server: empty-state + debounced YAML preview, live name-regex inline error, single-daemon flat bindings, cascade rename/delete with confirm, Save writes manifest, Save&Install port-conflict failure path with Retry Install banner, Paste YAML import, sidebar-intercept unsaved-changes guard, Advanced kind-toggle (workspace/global reveals/hides languages+port_pool), Advanced always-visible fields survive kind toggles.
- Edit server: #/edit-server?name= load from disk, name+kind locked, Save → Reinstall banner, Force Save with external-edit hash-mismatch preserving `_preservedRaw` top-level fields, nested-unknown read-only mode, load failure banner, sidebar-intercept when dirty, 4+-daemon matrix view, workspace-scoped Advanced (languages + port_pool), internal-ID cascade daemon rename, hashchange cancel/accept dirty-guard, Paste YAML → Save race (version-counter invariant).
- Dashboard: empty-cards state on fresh home, `/api/events` SSE connection opens on mount.
- Logs: picker + controls render, notice text on no-daemons state, controls disabled when no eligible entries.
- Secrets: empty-state init, Add modal, Used-by counts from manifest scan, ghost-refs for manifest-only keys, decrypt-failed degraded view, Rotate Save-without-restart with persistent CTA + Restart-now path via POST /restart, Rotate Save-and-restart with 207 partial-failure handling, Delete differential typed-confirm (single-click for unreferenced / typed DELETE for referenced) via D5 escalation flow, scan-incomplete fail-closed path, backend 409 guard verification, sidebar nav link, mcphub secrets edit banner.
- Add/Edit server env picker: affordance button, auto-open on `secret:` typing with substring-narrowing filter, sort-by-match with `matchTier`-based badge for exact-after-normalization, broken-ref inline marker (red `.broken` for missing, yellow `.unverified` for unverified), conditional compact summary line above env section when count > 1 or vault not ok, in-place AddSecretModal with snapshot revalidate-on-save (savedFiredRef dedup) and Retry-on-load-error, full ARIA combobox semantics with keyboard navigation (Tab/Esc/Arrow/Enter), create-contextual 409 conflict flow (modal stays open + Cancel triggers refresh + marker clears), `[data-action="create-disabled"]` rendering for vault not ok.
- Settings: sidebar link + 5 section headers + deep-link query-string (#/settings?section=backups) + sticky inner-nav active-on-scroll + Save Appearance round-trip to gui-preferences.yaml + port save validation (below min) + port pending-restart badge after Save (anchored to persisted, not draft — Codex r3+r4 P2.1) + Daemons read-only "Configured schedule (effective in A4-b)" wording + Backups 4-client groups + would-prune badge with locked Codex copy + disabled Clean-now tooltip + Open app-data folder POST (mocked, no real spawn — Codex r2 P2) + dirty-guard navigation prompt + per-section Save isolation + deferred tray field disabled + discard-key remount on intra-Settings confirmed-discard navigation (Codex r2 P1, memo §10.4).
- About (PR #22): /api/version network round-trip + heading + version/commit/build-date data-testids + external links carry target=_blank rel="noopener noreferrer" + sidebar nav highlights active.

95 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs + 14 secrets + 10 secret-picker + 16 settings + 3 about), ~50s wall-time on a warm machine.

### What's NOT covered (future)

- Populated-row matrix tests (needs a client-config seed fixture — deferred to a follow-up plan item).
- Real migrate/restart flows (needs populated client configs).
- Dashboard SSE cleanup on screen swap — the `useEffect` return is the implementation, but Playwright's request API cannot observe connection close. A future CDP-based test could.
- Workspace-scoped daemons (Phase 3B-II D3 — manual smoke per `docs/phase-3b-ii-verification.md`).
- Tray icon rendering (4-state variants + toast notifications wired in PR #22; native surface Playwright can't reach — manual smoke per `docs/phase-3b-ii-verification.md` D2.1 + D2.4).
- Browser focus on activate-window (PR #22 wires SetForegroundWindow; manual smoke per D2.1).
- Linux/macOS (blocked on scheduler test seam).

## Stuck-instance recovery

If `mcphub gui` exits with the structured "Cannot acquire mcphub gui
single-instance lock" block, run `mcphub gui --force` for the
diagnostic — it also opens the lock folder in your file manager so
the offending files are visible.

To recover automatically:

```bash
mcphub gui --force --kill              # prompts before killing
mcphub gui --force --kill --yes        # no prompt; for scripts
```

`--kill` only terminates the recorded PID after a three-part identity
gate: (1) image basename is `mcphub.exe` (Windows) or `mcphub` (POSIX);
(2) `argv[1]` (cobra subcommand token) equals `gui` exactly OR the
process was launched with no args (Explorer/Start-menu double-click,
which `cmd/mcphub/main.go:32` defaults to gui internally); (3) process
start time precedes pidport mtime. If any gate fails (e.g. PID
recycled to a `mcphub.exe daemon` Task Scheduler child), `--kill`
refuses with exit 7.

Manual recovery when `--kill` refuses:

```text
Windows: download Sysinternals `handle.exe`, then
         `handle.exe -a "<lock-path>"` (REQUIRES ELEVATED shell).
Linux:   `lsof "<lock-path>"` or `fuser "<lock-path>"` (use `sudo`
         for cross-user holders).
```

Then `kill -9 <pid>` (Linux) or Task Manager → End Task on that
PID (Windows). DO NOT delete the lock file — deleting under a live
holder splits ownership. If admin tooling isn't available, reboot
is the universally available recovery (stuck file handles survive
user-mode cleanup only across a session reset).

**macOS:** `--force --kill` is unsupported on macOS in this PR (the
identity probe relies on `/proc`, which is Linux-only). `mcphub gui
--force` for the diagnostic still works, but kill recovery on macOS
is not yet implemented; reboot is the recovery path. Tracked as
follow-up in phase-3b-ii-backlog.md.

Exit codes:

```text
0 — success
1 — non-force startup error
2 — bare --force exited after diagnostic
3 — race lost or pidport changed mid-prompt
4 — kill failed / pidport unrecoverable
5 — RESERVED (not emitted)
6 — non-interactive shell with --kill but no --yes
7 — --kill refused by identity gate
```
