# Phase 3 — GUI Installer + CLI Parity Expansion — Design Spec

**Date:** 2026-04-17
**Author:** Dmitry (with Claude Opus 4.7 via superpowers:brainstorming)
**Status:** Design — pending implementation plan
**Supersedes:** adds to `docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md`
**Depends on:** Phase 2 complete (`docs/phase-2-verification.md`)

## 1. Problem

Phase 2 consolidated 7 MCP servers into shared daemons reachable via HTTP. Install/uninstall/migration is functional but strictly CLI-driven — every new host, every new server, every re-migration requires manual `mcp install --server X` invocations and hand-editing of YAML manifests.

Two concrete pain points followed from Phase 2:

1. **No unified view of what's actually managed vs what's still stdio.** `mcp status` shows scheduler tasks for servers we installed, but not the broader state: which clients route which servers through the hub, which still spawn their own stdio subprocess, which MCP servers exist on the machine that have no manifest yet. The user has to mentally overlay four client configs (`~/.claude.json`, `~/.codex/config.toml`, `~/.gemini/settings.json`, `~/.gemini/antigravity/mcp_config.json`) to answer "am I using the hub everywhere I could?"

2. **Operational friction for day-2.** Daemon failed → need to check `schtasks /Query`, tail the log file manually, restart via `mcp restart --server X`. Weekly refresh only runs for Serena (manifest-opt-in); other servers drift. Backup files accumulate unboundedly (20+ `.bak-mcp-local-hub-*` per client config in practice) and the truly-pristine pre-hub backup is lost in the noise.

## 2. Goal

Ship a **local GUI** — a browser window + system-tray icon served by `mcp.exe` itself — that:

1. Scans all four client configs and presents a unified matrix of "which servers route through the hub for which clients".
2. Offers one-click migration of stdio entries to hub HTTP for servers that have manifests.
3. Lets users create manifests from existing stdio entries for servers that don't have them yet ("I have cursor-mcp-fetch configured as stdio — wrap it into a hub daemon").
4. Shows live daemon status (running/stopped/failed, RAM, uptime, client connection count) with one-click restart.
5. Manages the encrypted secrets vault, client-config backups, and scheduler weekly-refresh without dropping to CLI.

**The GUI is additive to CLI, not a replacement.** Every operation reachable from the UI has an equivalent CLI command (and vice versa); both frontends call the same `internal/api/` functions. This keeps scripting, automation, and headless machines first-class.

### 2.1 Success criteria

1. A user who has just run `mcp install --all` on a fresh host sees the Servers matrix load with all 7 daemons marked "Via hub" across all four clients within one second.
2. Clicking a checkbox to flip `memory` from "Via hub" to "off" for Codex CLI, then Apply, rewrites `~/.codex/config.toml` to restore its original stdio entry without touching other clients and without leaving daemon 9123 in an inconsistent state.
3. A "Create manifest" action on an unknown stdio entry auto-populates `command`, `base_args`, and `env` from the existing client config, letting the user add a port + daemon name and save in under a minute.
4. With the GUI running, a user who kills `mcp.exe` from Task Manager sees tray update to "✕ error" within 10 seconds and UI eventually reconnect; a user who kills a daemon subprocess sees the Dashboard card flip red within the same 5-second auto-refresh cycle and scheduler auto-restart kicks in.
5. After an unexpected system reboot, the next `mcp gui` invocation detects stale pidport file, acquires mutex fresh, and starts normally — no manual cleanup.
6. `mcp.exe` double-clicked from Explorer opens the GUI window; `mcp.exe status` from a cmd.exe session prints to that cmd's console and returns the prompt cleanly; Scheduler-started daemons never flash a console window.
7. GUI backup management reliably preserves a "pristine pre-hub-ever" state per client config and prunes old timestamped backups to a user-configured maximum (default 5) on every install.

### 2.2 Non-goals

- **Multi-user / remote access.** HTTP server binds 127.0.0.1 only. No auth layer, no TLS.
- **MCP protocol exposure.** The GUI server is an administrative surface for `mcp-local-hub`; it does not implement MCP tools or proxy MCP traffic itself.
- **Cross-platform native desktop UX.** Tray + single-binary flow is Windows-specific in Phase 3. Linux/macOS ship the same GUI web server without tray; native integration is Phase 4+.
- **Real-time log search/aggregation across daemons.** Per-daemon tail + follow is in scope; unified log search is out.
- **Deep manifest editing like JSON Schema validation in-form.** The Manifest form validates on save via `api.ManifestValidate()`; inline live validation is a polish pass for later.

## 3. Architecture

### 3.1 Three-layer shape

```
┌──────────────────────────────────────────────────────────────────────┐
│  mcp.exe  —  single binary, Windows subsystem, AttachConsole at init │
│                                                                      │
│  ┌──────────────────────────────────────────────────────────────┐    │
│  │ internal/api/   — source of truth for all operations         │    │
│  │                                                              │    │
│  │   • Scan / Migrate / ExtractManifestFromClient               │    │
│  │   • Install / InstallAll / Uninstall / Restart / RestartAll  │    │
│  │   • Stop / Status / Logs / LogsStream                        │    │
│  │   • ManifestList / Get / Create / Edit / Validate / Delete   │    │
│  │   • SecretsList / Set / Delete                               │    │
│  │   • BackupsList / Clean / Show / Rollback / RollbackOriginal │    │
│  │   • SchedulerUpgrade / WeeklyRefreshSet                      │    │
│  │                                                              │    │
│  │   State struct (mutex-guarded) + SSE broadcast bus           │    │
│  └──────────────────────────────────────────────────────────────┘    │
│         ▲                                               ▲            │
│    ┌────┴────────┐                           ┌──────────┴──────┐     │
│    │ internal/cli│                           │ internal/gui    │     │
│    │  (cobra)    │                           │  (net/http +    │     │
│    │             │                           │   embedded HTML)│     │
│    └─────────────┘                           └─────────────────┘     │
│                                                       ▲              │
│                                              ┌────────┴────────┐     │
│                                              │ internal/tray   │     │
│                                              │  (getlantern/   │     │
│                                              │   systray)      │     │
│                                              └─────────────────┘     │
└──────────────────────────────────────────────────────────────────────┘
```

**Source-of-truth invariant:** `internal/cli` and `internal/gui` are both thin wrappers over `internal/api`. They do not reach around into `internal/clients`, `internal/scheduler`, `internal/config`, or `internal/daemon` directly. This gives us UI ≡ CLI by construction, not by convention.

### 3.2 Single binary with runtime console attachment

`mcp.exe` is compiled with `go build -ldflags="-H windowsgui ..."`, which places it in the Windows subsystem. Windows does not automatically allocate a console for such a process; a console only appears if we explicitly attach (`AttachConsole` / `AllocConsole`) or if we're a child of a console-owning parent and something else forces allocation — which it doesn't for us.

At `init()` in `cmd/mcp/console_windows.go` (build tag `windows`):

1. Call `AttachConsole(ATTACH_PARENT_PROCESS)`.
2. On success, reopen `os.Stdout`, `os.Stderr`, `os.Stdin` against the attached console's standard handles.
3. On failure (no parent console — Scheduler, Explorer double-click, detached spawn), proceed silently.
4. At `os.Exit`, call `FreeConsole` to flush properly.

Consequences:
- `mcp.exe status` from cmd.exe / PowerShell / Git Bash: attaches to parent, output goes to the terminal, prompt returns on exit.
- `mcp.exe status | findstr memory`: pipe handles predate AttachConsole; output flows through pipe untouched.
- Scheduler invokes `mcp.exe daemon --server X`: no parent console, no allocation, no window flash — ever.
- Explorer double-click `mcp.exe`: detect no-arg + no parent console case → auto-invoke `gui` subcommand.

A Linux/macOS build uses ordinary `//go:build !windows` stubs and runs the CLI normally. The subsystem consideration is Windows-only.

### 3.3 Process lifecycle and tray

`mcp gui [--port N] [--no-browser] [--no-tray]`:

1. Acquire named mutex `Local\mcp-local-hub-gui`.
   - **ERROR_ALREADY_EXISTS**: read `%LOCALAPPDATA%\mcp-local-hub\gui.pidport`, GET `/api/ping` on the stored port. If 200 OK, POST `/api/activate-window` and exit 0. If connection refused or timeout, retry 3× over 1.5s; if still failing, print error and exit 1 (user can `mcp gui --force` to take over).
   - **Fresh acquisition**: write pidport file with our PID+port (overwriting stale reboot residue), proceed.
2. Start HTTP server on `127.0.0.1:<port>` (default: free port from 9100-9110 range).
3. Start tray icon goroutine (`--no-tray` skips this).
4. If not `--no-browser`: spawn Chrome/Edge in app-mode (`--app=http://127.0.0.1:<port>`) or fall back to default browser.
5. Block until: tray "Quit" clicked, SIGINT, or HTTP server returns error.
6. On shutdown: release mutex (automatic via process exit), close HTTP server, remove pidport file.

Mutex behavior under OS reboot is correct by design: Windows releases named kernel objects on process termination regardless of cause, so a rebooted machine has no stale mutex holders. The pidport file on disk may be stale, but we only consult it when mutex acquisition tells us someone already holds it.

### 3.4 Weekly refresh — hub-wide

A single scheduler task `mcp-local-hub-weekly-refresh` replaces per-server weekly refreshes for Phase 3 onward. It runs `mcp restart --all` every Sunday at 03:00 local time by default; the day and time are user-configurable in Settings.

- `uvx`-launched daemons (Serena, paper-search-mcp) receive fresh git pulls on next start because their manifests use `uvx --refresh`.
- `npx -y`-launched daemons (memory, sequential-thinking, time) re-resolve the package which pulls any newer version from the npm registry cache.
- Local-binary daemons (wolfram, godbolt) restart unchanged; cost is a 10-second outage window per daemon, staggered serially.

The existing per-manifest `weekly_refresh: true` field stays for backward compatibility (serena's manifest keeps it) and acts as a safety net if the hub-wide task is disabled.

### 3.5 Backup strategy

Every client config owned by this tool has up to N+1 backups living next to the live config:

- `~/.claude.json.bak-mcp-local-hub-original` — a sentinel, written exactly once on the first `Install()` that touches this config. Never overwritten afterward.
- `~/.claude.json.bak-mcp-local-hub-YYYYMMDD-HHMMSS` — timestamped snapshot of the pre-install state, written on every install. Cleaned on each install down to the N most recent (default N=5, configurable in Settings).

`api.Rollback()` restores from the most recent timestamped backup. `api.RollbackOriginal()` restores from the sentinel. Both operations are idempotent and safe: they write the restored content via the same atomic-rename path used by `Install()`, so a crash mid-restore leaves the live file in a valid state.

### 3.6 Real-time update bus

`GET /api/events` is a single SSE endpoint shared by:
- The main UI window (one subscriber per open tab/app-window).
- The tray process (persistent subscriber, updates icon/tooltip on relevant events).
- Future `mcp status --follow` CLI (out of Phase 3 scope).

State mutations inside `internal/api` broadcast typed events:

```jsonc
{"type":"daemon-state",    "server":"memory", "state":"Running", "pid":8948, "port":9123, "ram_mb":48}
{"type":"daemon-failed",   "server":"wolfram", "reason":"exit 1", "will_retry":true, "retry_count":2}
{"type":"install-progress","server":"memory", "step":"creating-task", "percent":25}
{"type":"install-done",    "server":"memory", "result":"ok"}
{"type":"log-line",        "server":"memory", "level":"info", "line":"tool call: add_entity"}
{"type":"scan-result",     "added":["filesystem"], "removed":[], "unchanged":7}
{"type":"client-config-changed", "client":"claude-code", "path":"~/.claude.json"}
```

A poller goroutine samples `schtasks /Query`, port-listening checks, and process RSS every 5 seconds and emits `daemon-state` events on observed deltas. An `fsnotify` watcher on the four client config files emits `client-config-changed` within ~100ms of a manual edit, which in turn triggers a rescan and `scan-result` event.

Subscribers reconnect with 1-second backoff on disconnect; the tray flips to `✕ error` during disconnected intervals longer than ~5 seconds and restores icon state on reconnect.

## 4. CLI and HTTP API surface

### 4.1 New CLI commands

| Command | Purpose |
|---|---|
| `mcp scan [--json]` | List all MCP servers across the four client configs with classification (`via-hub` \| `can-migrate` \| `unknown` \| `per-session` \| `not-installed`). |
| `mcp migrate <server>... [--all-eligible]` | Flip stdio entries to HTTP entries (or relay for Antigravity) for servers that already have manifests. |
| `mcp install --all [--dry-run]` | Install every manifest under `servers/`. |
| `mcp restart --all` | Restart every running daemon. |
| `mcp stop --server <name>` | Stop a running daemon without uninstalling. |
| `mcp logs <server> [--tail N] [--follow]` | Print or stream a daemon's log. |
| `mcp manifest list` | List existing manifests under `servers/`. |
| `mcp manifest show <name>` | Print a manifest's YAML. |
| `mcp manifest create <name> [--from-file F]` | Create a new manifest interactively or from a file. |
| `mcp manifest edit <name>` | Open the manifest in `$EDITOR`. |
| `mcp manifest validate <name>` | Run static validation; print warnings. |
| `mcp manifest delete <name>` | Remove the manifest (does not uninstall; user should uninstall first). |
| `mcp manifest extract --client <c> --server <s>` | Print a draft manifest YAML derived from an existing stdio entry in a client config. |
| `mcp backups list [--client <c>]` | Show all backups with timestamps and sizes. |
| `mcp backups clean [--keep N]` | Prune timestamped backups older than the N most recent. |
| `mcp backups show <path>` | Print a backup's contents (for diagnostics). |
| `mcp rollback [--original] [--client <c>]` | Restore a client config from backup (most recent, or the sentinel). |
| `mcp scheduler upgrade` | Regenerate all scheduler tasks with the current executable path. |
| `mcp scheduler weekly-refresh --set "<DAY HH:MM>"` / `--disable` | Manage the hub-wide weekly-refresh task. |
| `mcp gui [--port N] [--no-browser] [--no-tray] [--dev] [--force]` | Launch the GUI server + tray. |
| `mcp settings get/set/list` | Read/write `%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml`. |

### 4.2 Updated CLI commands

| Command | Change |
|---|---|
| `mcp status` | Adds `Port`, `PID`, `RAM`, `Uptime` columns. JSON output via `--json` matches `DaemonStatus` struct. |
| `mcp install --server X` | Scheduler `<Command>` field resolves to the current `mcp.exe` path via `os.Executable()` (Phase 3 switches that binary to the Windows subsystem, so no separate daemon binary is needed). |

### 4.3 HTTP API

All endpoints bind to `127.0.0.1` only. No auth, no CORS allowed.

| Method + path | Maps to |
|---|---|
| `GET  /api/ping` | `{"ok":true,"pid":N,"version":"..."}` — used by single-instance probe. |
| `POST /api/activate-window` | Signal main window to come to front (for second-instance handoff). |
| `GET  /api/scan` | `api.Scan()` |
| `POST /api/migrate` body: `{"servers":[...]}` | `api.Migrate()` |
| `GET  /api/status` | `api.Status()` |
| `POST /api/install` body: `{"server":"X","daemon":"Y"}` | `api.Install()` |
| `POST /api/install-all` | `api.InstallAll()` |
| `DELETE /api/install/:server` | `api.Uninstall()` |
| `POST /api/servers/:server/restart` | `api.Restart()` |
| `POST /api/servers/restart-all` | `api.RestartAll()` |
| `POST /api/servers/:server/stop` | `api.Stop()` |
| `GET  /api/manifests` | `api.ManifestList()` |
| `GET  /api/manifests/:name` | `api.ManifestGet()` |
| `POST /api/manifests` body: full spec | `api.ManifestCreate()` |
| `PUT  /api/manifests/:name` body: full spec | `api.ManifestEdit()` |
| `DELETE /api/manifests/:name` | `api.ManifestDelete()` |
| `POST /api/manifests/validate` body: spec | `api.ManifestValidate()` |
| `GET  /api/manifests/extract?client=X&server=Y` | `api.ExtractManifestFromClient()` |
| `GET  /api/logs/:server?tail=N` | `api.LogsGet()` |
| `GET  /api/logs/:server/stream` | `api.LogsStream()` — SSE, one line per event |
| `GET  /api/secrets` | Keys only, values never exposed. |
| `POST /api/secrets` body: `{"key":"X","value":"Y"}` | `api.SecretsSet()` |
| `DELETE /api/secrets/:key` | `api.SecretsDelete()` |
| `GET  /api/backups` | `api.BackupsList()` |
| `POST /api/backups/clean` body: `{"keep":N}` | `api.BackupsClean()` |
| `GET  /api/backups/content?path=X` | `api.BackupShow()` |
| `POST /api/rollback` body: `{"original":bool,"client":"X"}` | `api.Rollback()` |
| `POST /api/scheduler/upgrade` | `api.SchedulerUpgrade()` |
| `POST /api/scheduler/weekly-refresh` body: `{"enabled":true,"day":"SUN","time":"03:00"}` | `api.WeeklyRefreshSet()` |
| `GET  /api/settings` | Read GUI preferences file. |
| `PUT  /api/settings` body: settings map | Write GUI preferences file. |
| `GET  /api/events` | Server-Sent Events stream (see §3.6). |

Error envelope for non-2xx: `{"error":"human-readable message","code":"MACHINE_CODE"}`.

## 5. GUI screens

Seven primary screens plus Settings and About, navigated via a left sidebar (default) or top tabs (Settings-switchable). All screens honor the theme and density preferences.

### 5.1 Servers — default home

Matrix table: rows are servers (managed + can-migrate + unknown + per-session), columns are the four clients, cells are checkboxes meaning "route through the hub for this client". Additional columns: Port, RAM, Uptime, Status. Disabled cells where a client binding is not supported (e.g., Antigravity relay for a per-session server, or Gemini CLI not configured for a given stdio entry).

Clicking a checkbox toggles without applying. An "Apply changes" button becomes active when any diff exists and commits all changes atomically via `api.Migrate()`. Clicking a row opens a drawer with manifest preview, lifetime stats, and Stop / Restart controls.

### 5.2 Migration

Scan-driven view grouping entries by status: `Via hub` (green, readonly), `Can migrate` (yellow, pre-checked with "Migrate selected"), `Unknown servers` (purple, each with "Create manifest" and "Dismiss"), `Per-session / not shareable` (gray, readonly). "Create manifest" pre-fills the Add-server form with values extracted via `api.ExtractManifestFromClient()`.

### 5.3 Dashboard

Live grid of cards, one per running daemon. Each card shows name, green/red dot, Port, RAM, Uptime, connected clients, requests/min, a RAM sparkline, and Restart / View-logs actions. Auto-refreshes via `/api/events` without polling. Failed daemons render in red with retry count.

### 5.4 Add server / Edit manifest

Accordion form: Basics (name, kind, transport) → Command (command, args, cwd) → Environment (key-value rows with secret/file/env prefix selectors) → Daemons (name, port, extra args) → Client bindings (checkbox per client) → Advanced (weekly refresh, idle timeout). YAML preview updates live on the side. "Validate" runs `api.ManifestValidate()`; "Save & Install" writes the manifest and runs install immediately; "Save only" writes without installing.

### 5.5 Logs

Server dropdown, tail size selector, Follow toggle (SSE-backed live stream), regex/substring filter, "Open folder" to reveal the logs directory in Explorer. Stderr lines prefixed with `[subproc stderr]` highlight in amber; error-level lines in red.

### 5.6 Secrets

Table of key names with "Used by" counts (manifests referencing each key). Values are never displayed in list form. "Add" opens a modal with a masked value field. "Rotate" warns how many daemons will restart. "Delete" warns on references. "Edit vault" shells out to `mcp secrets edit` for advanced cases.

### 5.7 Settings

Sections: Appearance (theme, shell, default home, density), GUI server (port, browser, tray), Daemons (weekly refresh schedule, retry policy), Backups (keep-N slider, "Clean now"), Advanced (export config bundle, open app-data folder). All settings persist to `%LOCALAPPDATA%\mcp-local-hub\gui-preferences.yaml` and round-trip via `mcp settings get/set`.

### 5.8 About

Version, commit, build date, GitHub link, Apache 2.0 license, credits. Links to README, INSTALL, verification docs.

## 6. Tray

The tray icon is a monochrome 16×16 PNG, embedded into the binary at four state variants (healthy / partial / down / error). The icon redraws on state change. The tooltip is generated from current state, capped at 127 characters.

Left click opens or activates the main window via `/api/activate-window`. Right click produces a menu with a disabled status header, "Open dashboard" (same as left click), "Restart all daemons", "Rescan client configs", a "Show recent activity" submenu listing the last five events, "Open logs folder", "Open data folder", and two quit variants: "Quit (keep daemons)" closes only the GUI + tray and leaves scheduler tasks active; "Quit and stop all" additionally stops daemons for the current user session.

Windows toast notifications fire on daemon failed / auto-restart / manual action completed events. They are disabled globally via a Settings toggle.

## 7. Theme and icon system

CSS variables define Light and Dark palettes with a System-preference default. Light is GitHub-Light-based with slightly elevated contrast (`--text-base: #1f2328`) to address the earlier too-soft appearance. Dark uses GitHub-Dark palette unchanged. A `data-theme` attribute on `<html>` drives the switch without page reload.

Running daemons render in `--success` (green #1a7f37 light / #3fb950 dark); stopped or failed daemons in `--danger` (red #cf222e / #f85149). Status cells include a shape indicator (● vs ○) so color is not the sole carrier of meaning.

Icons are a single monochrome SVG symbol set (Lucide/Feather-style, 1.8px strokes, 24×24 viewBox) defined once per page as `<symbol>` and referenced via `<use href="#id">`. All icons inherit `currentColor`, so they track theme automatically. Canonical Phase 3 set:

| Symbol ID | Used for |
|---|---|
| `i-migration` | Migration view (two-way arrows) |
| `i-dashboard` | Dashboard view (grid cells) |
| `i-servers` | Servers view (list with bullets) |
| `i-add` | Add server (plus) |
| `i-logs` | Logs view (document with lines) |
| `i-secrets` | Secrets view (key) |
| `i-settings` | Settings (gear) |
| `i-info` | About (i-circle) |
| `i-check` | Success states, selected checkbox |
| `i-x` | Delete/close actions |
| `i-play` | Start/Run daemon |
| `i-square` | Stop daemon |
| `i-refresh` | Restart daemon, rescan |
| `i-folder` | Open data folder |
| `i-external-link` | External link indicator |
| `i-chevron-down` / `i-chevron-right` | Accordion, dropdown |
| `i-copy` | Copy-to-clipboard button |
| `i-eye` / `i-eye-off` | Show/hide secret value |

Frontend-design polish (final spacing, micro-interactions, focus states, WCAG AA audit) is deferred to implementation.

## 8. Testing strategy

### 8.1 Test layers

| Layer | Scope |
|---|---|
| **internal/api/** unit | Business logic — `Scan`, `Migrate`, `ManifestValidate`, `BackupsClean`, mutex/pidport recovery. Real `t.TempDir()` fixtures; mocks only around `schtasks` and `exec.Cmd`. |
| **internal/gui/** HTTP | Every endpoint through `httptest.NewServer`. Table-driven: expected request → expected response shape and status. |
| **internal/gui/** single-instance | Spawn two concurrent `mcp gui` subprocesses; assert exactly one becomes the leader, the other sends activate. Simulate reboot by removing the mutex before the second launch and reusing a stale pidport file. |
| **internal/cli/** | New commands via `cobra.Command.SetArgs()` + captured stdout. Verify exit codes, flag parsing, and `--help` output. |
| **internal/tray/** | Pure functions (icon state computation, tooltip text). Interactive rendering is verified manually. |
| **Subsystem / AttachConsole** | Matrix of {cmd, PowerShell, Git Bash, Scheduler, Explorer} × {status, install, gui}. Verified empirically and captured in `docs/phase-3-subsystem-verification.md`. |

### 8.2 Explicit focus

- `api.Scan()` parsing of realistic config fixtures from all four clients. Regression risk is high because migration view correctness depends entirely on scan output.
- `api.Migrate()` partial-failure behavior: if writing `~/.codex/config.toml` fails (lock, permissions), state for the other clients must not change.
- `api.BackupsClean(keep=N)` as a property test: after any install sequence, exactly one `*-original` + at most N timestamped backups remain.
- Single-instance lock under contention and stale pidport file.
- Subsystem + AttachConsole across the full launch matrix. This cannot be covered by `go test`; it is a manual smoke list gated on release.

### 8.3 Out of scope for automated testing

Pixel-perfect UI rendering (deferred to frontend-design skill + manual acceptance), Chrome/Edge app-mode launch quirks (user-environment-dependent), systray rendering on Windows (requires display + interaction), and browser DOM interaction (covered indirectly by HTTP API tests).

### 8.4 Release checklist

`docs/phase-3-verification.md` (to be written during implementation) holds the gate list. Representative entries:

- `go test ./... -race` all green.
- `go build -ldflags="-H windowsgui ..." -o mcp.exe ./cmd/mcp` — single binary ~14 MB.
- Scheduler-started daemons produce no console window flash across 10 consecutive reboots.
- `mcp gui` double-click from Explorer opens the window; `mcp.exe status | findstr memory` pipes cleanly.
- Second `mcp gui` invocation activates the first window rather than starting a second server.
- `mcp install --all` installs all seven servers successfully; `mcp rollback --original` restores all four clients to pre-hub state.
- Light theme passes WCAG AA contrast on the primary text and status cells.

## 9. Open questions

These are explicitly deferred for now; resolve during implementation:

1. **Icon asset pipeline.** Do we hand-author the Phase 3 SVG sprite, or adopt a subset from Lucide with license attribution? Decision affects repo structure (`internal/gui/assets/` vs pulled from `web/lucide/`).
2. **Browser-launch fallback ordering.** Current plan: Chrome → Edge → default browser. Does the order need a Settings-exposed preference, or is auto-detect enough?
3. **`mcp gui --dev` live-reload mechanism.** Watch-and-rebuild + SSE-triggered page reload is proposed; simpler approach is to just restart the whole Go process on file change via `air` or similar. Decision during implementation.
4. **Rollback semantics when client config has been hand-edited after an install.** Does `mcp rollback` overwrite unconditionally, or diff and warn? Default plan: unconditional with a Y/N confirm in CLI and a modal in GUI.
5. **Backup encryption.** Client configs may contain API keys. Should backups inherit the secret-vault encryption pattern, or stay plain JSON/TOML with the same trust boundary as the live file? Default: plain, matching current behavior; revisit if users flag concerns.
6. **Antigravity relay-entry coverage in Migration view.** The relay-stdio entry is structurally different from other stdio entries (it *is* our own wrapper). Need to decide whether scan classifies it as "Via hub" (semantically correct, because it routes to the daemon) or as a separate fourth state. Default: "Via hub", with a tooltip explaining the mechanism.

## 10. Implementation order (for the follow-on plan)

This design is one spec producing one plan. Sequencing hints for the plan:

1. Subsystem + AttachConsole change — foundational, affects every downstream test.
2. `internal/api/` scaffolding — extract existing `scan`-equivalent logic from `internal/clients` into `api.Scan()` first, then layer Install/Migrate on top.
3. New CLI commands (`scan`, `migrate`, `install --all`, `backups *`, `rollback --original`, `manifest *`, `scheduler upgrade`) — reach CLI parity before starting GUI work.
4. Backup strategy rework (sentinel + keep-N) — required before any GUI rollback flow is meaningful.
5. HTTP API surface + SSE event bus.
6. GUI static assets: HTML templates, CSS theme, SVG sprite, client JS.
7. Tray integration.
8. Single-instance lock + pidport handling.
9. Release hardening: verification doc, release checklist, Phase 3 verification run.
