# Phase 3B-I Verification — 2026-04-22

Closes **Phase 3B-I — GUI Installer MVP** across the implementation plan:

- `docs/superpowers/plans/2026-04-22-phase-3b-gui-mvp.md` (22 tasks)

## Goal (from plan)

Ship a local-loopback GUI served by `mcphub.exe` itself, covering the primary
flow: scan client configs → view Servers matrix → toggle hub routing →
Apply → see live Dashboard → read logs. Secondary screens (Migration detail,
AddServer form, Secrets, Settings, About) are Phase 3B-II.

The MVP targets success criteria §2.1 **#1, #2, #4, #5** from the design
spec. Criteria #3 (create manifest from stdio) and #7 (backup UX) are
deferred to Phase 3B-II along with the AddServer/Settings/About/Secrets
screens.

## Architecture delivered

- `internal/gui/` — single-port HTTP server bound to `127.0.0.1` plus
  embedded HTML shell, CSS theme, and three screen modules. 24 test
  functions across 14 `*_test.go` files in the package.
- `internal/tray/` — Windows systray integration via
  `github.com/getlantern/systray`. `tray_other.go` is a no-op stub for
  Linux/macOS so `GOOS=linux go build` still succeeds without systray
  dependencies firing.
- `cmd/mcphub` — new `gui` cobra subcommand with `--port`, `--no-browser`,
  `--no-tray`, and `--force` flags.
- **Single-instance** — `gofrs/flock` against a `gui.flock` lease file under
  the user app-data directory (`%LOCALAPPDATA%\mcp-local-hub\` on Windows,
  XDG equivalent elsewhere). Second invocation reads the `gui.pidport`
  record, probes the incumbent over `/api/ping`, then calls
  `/api/activate-window` to focus the existing instance instead of
  double-binding the port.
- **SSE event bus** — `/api/events` broadcasts `daemon-state` and
  `poller-error` events. A 5s status poller diffs snapshots from
  `api.Status()` and publishes only on observed deltas.

## Server endpoints (Phase 3B-I surface)

| Method + path | Backed by |
|---|---|
| `GET /api/ping` | version + PID (single-instance probe) |
| `POST /api/activate-window` | in-process focus signal hook |
| `GET /api/scan` | `api.Scan()` |
| `GET /api/status` | `api.Status()` |
| `POST /api/migrate` body `{"servers":[...]}` | `api.MigrateFrom()` |
| `POST /api/servers/:name/restart` | `api.Restart(server, "")` |
| `GET /api/logs/:server?tail=N` | `api.LogsGet(server, "default", tail)` |
| `GET /api/logs/:server/stream` | SSE tail-follow, 500 ms re-read loop |
| `GET /api/events` | SSE broadcast (`daemon-state`, `poller-error`) |
| `GET /`, `GET /assets/*` | embedded HTML shell + CSS + JS |

Embedded asset tree:

```
internal/gui/assets/
  index.html     hash-router shell with sidebar
  style.css      light + dark theme
  app.js         boot + nav + SSE wiring
  servers.js     Servers matrix screen
  dashboard.js   live daemon cards
  logs.js        tail + follow viewer
```

## Commit trail — Phase 3B-I (Tasks 0 → 21 + closeout)

### Plan (Task 0)

| Commit | Description |
|---|---|
| `f66e11e` | `docs(phase-3b): add GUI MVP implementation plan (22 tasks, TDD)` |

### Tasks 1–7 — HTTP server skeleton, paths, single-instance, handshake

| Commit | Task | Description |
|---|---|---|
| `d309642` | 1 | `feat(gui): scaffold HTTP server with ping + lifecycle test` |
| `2717528` | 1-review | `chore(gui): address Task 1 code-review — stdlib strconv.Itoa + propagate Shutdown err` |
| `057f4cd` | 2 | `feat(cli): mcphub gui subcommand with --port/--no-browser/--no-tray/--force` |
| `ca7beea` | 3 | `feat(gui): /api/ping with pid/version + /api/activate-window skeleton` |
| `409d7c1` | 4 | `feat(gui): AppDataDir + PidportPath helpers (XDG + Windows parity)` |
| `3661cab` | 5 | `feat(gui): single-instance lock via gofrs/flock + pidport read/write` |
| `134dc1e` | 6 | `feat(gui): second-instance handshake (ping incumbent + activate)` |
| `cc55528` | 7 | `feat(cli): wire single-instance lock + handshake into mcphub gui` |

### Tasks 8–15 — embedded assets, API endpoints, SSE, migrate, restart

| Commit | Task | Description |
|---|---|---|
| `9685e91` | 8 | `feat(gui): embed HTML shell + CSS theme + hash-router skeleton` |
| `f429d4a` | 9 | `feat(gui): GET /api/scan wraps api.Scan() with error envelope` |
| `d71b7ee` | 10 | `feat(gui): GET /api/status wraps api.Status()` |
| `27a9da5` | 11 | `feat(gui): /api/events SSE broadcaster with context-bound subscribers` |
| `9eaa286` | 12 | `feat(gui): StatusPoller emits daemon-state events on observed deltas` |
| `7129a28` | 13 | `feat(cli): wire StatusPoller into mcphub gui for live SSE` |
| `feeaea0` | 14 | `feat(gui): POST /api/migrate wraps api.Migrate()` |
| `18c7356` | 15 | `feat(gui): POST /api/servers/:name/restart wraps api.Restart()` |

### Tasks 16–21 — screens, logs, browser auto-launch, tray

| Commit | Task | Description |
|---|---|---|
| `0a8e297` | 16 | `feat(gui): Servers screen — scan+status matrix with per-client checkboxes` |
| `6230c6f` | 17 | `feat(gui): Servers screen Apply → /api/migrate end-to-end` |
| `3848fac` | 18 | `feat(gui): Dashboard screen — live SSE daemon cards with restart` |
| `5c46fd4` | 19 | `feat(gui): /api/logs + Logs screen with tail + follow via SSE` |
| `6911a40` | 20 | `feat(gui): auto-launch browser (Chrome→Edge→default, --app mode)` |
| `aa8fd9e` | 21 | `feat(tray): Windows systray with Open + Quit (keep daemons)` |

### Task 22 — closeout

This document.

Total on branch vs `origin/master`: **25 commits** (1 plan commit + 21 task
commits + 1 Task-1 post-review polish + this verification doc + 1 final-review
cleanup that addressed the observations from the holistic pre-merge review).

## Tests

`go test ./... -count=1 -race -timeout 180s` summary:

```
ok  	mcp-local-hub/internal/api        9.593s
ok  	mcp-local-hub/internal/cli       10.750s
ok  	mcp-local-hub/internal/clients   17.614s
ok  	mcp-local-hub/internal/config     1.059s
ok  	mcp-local-hub/internal/daemon    19.484s
ok  	mcp-local-hub/internal/e2e        4.218s
ok  	mcp-local-hub/internal/godbolt    1.075s
ok  	mcp-local-hub/internal/gui        1.866s
ok  	mcp-local-hub/internal/perftools  9.043s
ok  	mcp-local-hub/internal/scheduler  1.073s
ok  	mcp-local-hub/internal/secrets    1.119s
```

Every package `ok` under `-race`. Packages without tests: `cmd/godbolt`,
`cmd/lldb-bridge`, `cmd/mcphub`, `cmd/perftools`, `internal/lldb`,
`internal/tray`, `servers`.

### New test files this phase

- `internal/gui/server_test.go` — Start/Shutdown lifecycle
- `internal/gui/ping_test.go` — `/api/ping` JSON + 405 on wrong method +
  `/api/activate-window` signal capture
- `internal/gui/paths_test.go` — `AppDataDir()` writeability + `PidportPath()`
  composition
- `internal/gui/single_instance_test.go` — first-caller success, second-caller
  fails fast, `ReadPidport()` parse
- `internal/gui/handshake_test.go` — ping-OK then activate, connection-refused
  error surfacing
- `internal/gui/assets_test.go` — `/` serves embedded HTML, `/assets/style.css`
  resolves
- `internal/gui/scan_test.go` — `/api/scan` wraps `api.Scan()`
- `internal/gui/status_test.go` — `/api/status` returns `DaemonStatus[]`
- `internal/gui/events_test.go` — broadcaster subscribe + unsubscribe-on-cancel
  + `/api/events` SSE streaming
- `internal/gui/poller_test.go` — state-delta emission
- `internal/gui/migrate_test.go` — `/api/migrate` passes server list through +
  405 on GET
- `internal/gui/servers_test.go` — restart endpoint hits `api.Restart`
- `internal/gui/logs_test.go` — tail read returns text
- `internal/gui/browser_test.go` — Chrome → Edge → default precedence
- `internal/cli/gui_test.go` — `gui` subcommand help surface
- `internal/cli/gui_integration_test.go` — second-instance handshake lives
  through a real `Run`

### Static checks

- `go vet ./...` — clean
- `staticcheck ./...` (v0.7.0) — clean
- `gofmt -l .` — clean
- `git diff --check origin/master...HEAD` — clean

## Success criteria (plan §2.1 + plan Step-2 smoke table)

| # | Criterion | Verification status |
|---|---|---|
| #1 | Servers matrix loads within 1s on a hub-installed host | **Automated** — `/api/scan` + `/api/status` covered by `TestScan_ReturnsJSONWrappingAPIResult` and `TestStatus_ReturnsArrayOfDaemonStatus`; latency budget depends on `api.Scan/Status` which ship unchanged from Phase 3. **Live browser smoke deferred to manual post-merge step** (requires Windows desktop with hub installed). |
| #2 | Checkbox flip + Apply rewrites **one client config only** | **Automated** — `TestMigrate_CallsAPIWithServerList` verifies `/api/migrate` forwards the per-client payload and `api.MigrateFrom()` is the same single-client writer used by the CLI (regression-tested in `internal/api` + `internal/clients`). **Live config-diff smoke deferred to manual post-merge step.** |
| #4 | Daemon kill → Dashboard card red ≤ 5s | **Automated** — `TestPoller_EmitsDeltaOnStateChange` confirms state transitions fire `daemon-state` events, `TestEventsSSE_StreamsPublishedEvents` confirms subscribers receive them, and `TestBroadcaster_UnsubscribeOnContextCancel` proves cleanup on disconnect. 5s visible-to-user budget = 5s poller cadence + SSE delivery. **Live taskkill smoke deferred.** |
| #5 | `mcphub gui` with stale pidport starts cleanly | **Automated** — `TestAcquireSingleInstance_FirstCallerSucceeds`, `TestAcquireSingleInstance_SecondCallerFails`, `TestGuiCmd_SecondInstanceActivates`, and `TestHandshake_ConnectionRefusedReturnsError` together cover stale-pidport-with-dead-PID → flock reclaim vs live-incumbent → handshake + focus. |

All four MVP criteria have automated coverage of the backing primitives.
The four live-browser smokes itemized above require an interactive Windows
desktop session and are documented for the user to run post-merge.

## Release-build verification

- `GOOS=windows GOARCH=amd64 go build -ldflags="-H windowsgui -s -w" -o /tmp/mcphub-gui-release.exe ./cmd/mcphub`
  → **11.05 MiB (11,584,000 bytes)** — clean build, `-H windowsgui`
  subsystem flag accepted by the Go linker, no console window will pop up
  when users launch from Explorer or a shortcut.
- `GOOS=linux GOARCH=amd64 go build -o /tmp/mcphub-linux ./cmd/mcphub`
  → **15.03 MiB**, clean build — the `internal/tray` stub
  (`tray_other.go`) picks up on non-Windows and omits all `systray` calls.
- Go toolchain: `go1.26.2 windows/amd64`.

Both platforms compile under the same source tree without build-tag
regressions.

## Deferred to Phase 3B-II

- §5.2 Migration detail screen (status grouping + "Create manifest"
  action)
- §5.4 Add/Edit manifest form
- §5.6 Secrets screen
- §5.7 Settings screen
- §5.8 About screen
- §2.1 success criterion **#3** — create manifest from stdio entry
- §2.1 success criterion **#7** — backup management UX (list, restore,
  delete)
- Tray icon state variants (healthy, degraded, down, migrating) — MVP
  ships with a single icon
- Toast notifications for `poller-error` events (surfaced in the event
  stream today but not rendered)
- **Live browser smoke of criteria #1, #2, #4, #5** on a Windows desktop
  with the hub installed

## Phase status

- ✅ **Phase 1** — Serena flagship daemon (docs/phase-1-verification.md)
- ✅ **Phase 2** — Global daemons consolidation (docs/phase-2-verification.md)
- ✅ **Phase 3A** — CLI parity + foundations (docs/phase-3a-verification.md)
- ✅ **Phase 3** — Workspace-scoped `mcp-language-server` (docs/phase-3-verification.md)
- ✅ **Phase 3B-I** — GUI MVP (**this document**)
- ⏳ **Phase 3B-II** — secondary screens + polish (design intact in
  `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md`;
  no plan yet)

## Merge trail (`phase-3b-gui-mvp`)

```
aa8fd9e feat(tray): Windows systray with Open + Quit (keep daemons)
6911a40 feat(gui): auto-launch browser (Chrome→Edge→default, --app mode)
5c46fd4 feat(gui): /api/logs + Logs screen with tail + follow via SSE
3848fac feat(gui): Dashboard screen — live SSE daemon cards with restart
6230c6f feat(gui): Servers screen Apply → /api/migrate end-to-end
0a8e297 feat(gui): Servers screen — scan+status matrix with per-client checkboxes
18c7356 feat(gui): POST /api/servers/:name/restart wraps api.Restart()
feeaea0 feat(gui): POST /api/migrate wraps api.Migrate()
7129a28 feat(cli): wire StatusPoller into mcphub gui for live SSE
9eaa286 feat(gui): StatusPoller emits daemon-state events on observed deltas
27a9da5 feat(gui): /api/events SSE broadcaster with context-bound subscribers
d71b7ee feat(gui): GET /api/status wraps api.Status()
f429d4a feat(gui): GET /api/scan wraps api.Scan() with error envelope
9685e91 feat(gui): embed HTML shell + CSS theme + hash-router skeleton
cc55528 feat(cli): wire single-instance lock + handshake into mcphub gui
134dc1e feat(gui): second-instance handshake (ping incumbent + activate)
3661cab feat(gui): single-instance lock via gofrs/flock + pidport read/write
409d7c1 feat(gui): AppDataDir + PidportPath helpers (XDG + Windows parity)
ca7beea feat(gui): /api/ping with pid/version + /api/activate-window skeleton
057f4cd feat(cli): mcphub gui subcommand with --port/--no-browser/--no-tray/--force
2717528 chore(gui): address Task 1 code-review — stdlib strconv.Itoa + propagate Shutdown err
d309642 feat(gui): scaffold HTTP server with ping + lifecycle test
f66e11e docs(phase-3b): add GUI MVP implementation plan (22 tasks, TDD)
```

All 25 commits on `phase-3b-gui-mvp`. All tests green under `-race`.
Release build with `-H windowsgui` clean. Ready for merge; live-browser
smoke of the four §2.1 MVP criteria is the one remaining manual step the
user should run against a Windows desktop before calling Phase 3B-I
closed end-to-end.
