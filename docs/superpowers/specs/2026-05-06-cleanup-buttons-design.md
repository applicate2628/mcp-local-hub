# Cleanup Buttons — Settings → Maintenance section design

> **Status:** draft, awaiting user approval before implementation
> **Branch (planned):** `feat/cleanup-buttons` (NEW; not on `fix/stdio-childexited-deadlock`)
> **Out of scope here:** the daemon/host.go fix on PR #130 (separate concern)

## Goal

Surface every cleanup operation mcphub already supports — plus the new
orphan-watcher cleanup — as buttons in the GUI, one button per variant,
so an operator never has to drop to a terminal for routine reclamation.

User directive (verbatim):
- "сделай тогда отдельный cleanup для застрявших в вечном watcher сессий!"
- "и для этих cleanup должны быть кнопки в GUI"
- "сколько вариантов cleanup столько и кнопок!"

## Inventory of cleanup variants

| # | Variant | Backend exists? | Where |
|---|---|---|---|
| 1 | **Orphan MCP-server processes** (uvx/npx/python from dead client sessions) | Yes — CLI | `internal/cli/cleanup.go` (`mcphub cleanup`), `internal/api/cleanup.go` (`API.CleanupOrphans`) |
| 2 | **Orphan log-watchers** (`tail -F` + `grep` reparented to PID 1 after agent shells exit) | NEW — script only | `scripts/cleanup-orphan-watchers.ps1` (this PR) |
| 3 | **Backup pruning** (per-server `.mcp.json.backup-*` beyond `keep_n`) | Yes — UI already | Settings → Backups section, "Clean-now" button |
| 4 | **Stuck mcphub instance** (single-instance lock holder won't die) | Yes — CLI | `mcphub gui --force --kill --yes` |
| 5 | **Daemon zombie reset** (Stop all + cleanup uvx/npx tree) | Yes — CLI composition | `mcphub stop --all && mcphub cleanup --confirm` |

(3) is already a button. (1), (2), (4), (5) are CLI-only today.

## Proposed UI: Settings → Maintenance

New Settings section between **Backups** and **About**, mirroring
the Backups affordances (each button: dry-run preview → confirm dialog
→ apply → result toast).

```
┌── Maintenance ─────────────────────────────────────────────────┐
│                                                                │
│  Orphan MCP server processes                                   │
│    Reclaim uvx/npx/python children left behind by dead         │
│    client sessions (IDE restart, Ctrl-C didn't propagate).     │
│    Run: mcphub cleanup --confirm                               │
│    [ Preview ] [ Clean ]                  Last: 2026-05-04     │
│                                                                │
│  Orphan log watchers (tail/grep)                Windows only   │
│    Reclaim tail.exe + grep.exe pipelines left behind by        │
│    agent shell-snapshot launchers (Claude Code, codex CLI).    │
│    See: docs/orphaned-log-watchers.md                          │
│    [ Preview ] [ Clean (orphans only) ] [ Clean +live ]        │
│                                                                │
│  Stuck mcphub instance                                         │
│    Force-kill the recorded single-instance lock holder         │
│    after the 3-part identity gate (mcphub.exe, argv[1]=gui,    │
│    start-time precedes lock mtime).                            │
│    Run: mcphub gui --force --kill                              │
│    [ Diagnose ] [ Force-kill ]            Lock: held / free    │
│                                                                │
│  Reset all daemons                                             │
│    Stop every running daemon AND reclaim orphan processes.     │
│    Use after multi-daemon zombie scenarios.                    │
│    Run: mcphub stop --all && mcphub cleanup --confirm          │
│    [ Stop all + Clean ]                                        │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

Why "Maintenance" not extending "Backups": Backups is per-server,
keep_n-driven, and idempotent. Maintenance is workstation-wide,
manual-trigger, and destructive. Different mental model.

## Per-button contracts

### Button 1: Orphan MCP server processes

| Field | Value |
|---|---|
| Action | `POST /api/cleanup/orphans` (NEW endpoint, wraps existing `API.CleanupOrphans`) |
| Dry-run preview | `POST /api/cleanup/orphans?dry_run=true` returns the orphan list as JSON |
| Confirm gate | Modal lists candidates with PID, server, age, RAM; "Clean N processes" button |
| Failure UX | Toast on per-PID kill errors; surface `KillErr` field from response |
| Backend status | Already implemented in `internal/api/cleanup.go` |
| Test surface | Existing `internal/cli/cleanup_test.go`; add E2E for the GUI flow |

### Button 2: Orphan log watchers

| Field | Value |
|---|---|
| Action | `POST /api/cleanup/log-watchers` (NEW endpoint, shells out to `scripts/cleanup-orphan-watchers.ps1`) |
| Dry-run preview | `POST /api/cleanup/log-watchers?dry_run=true` returns the candidate table as JSON |
| Confirm gate | Modal lists `tail.exe`/`grep.exe`/`bash.exe` with PID, parent-alive, age, command excerpt |
| Two action modes | "Clean orphans only" (default, parent dead) and "Clean +live" (path-matched live parents — current sessions; warn explicitly) |
| Platform | Windows only initially (the PS1 script). Show "not yet on macOS/Linux" banner on POSIX builds |
| Backend status | Script exists; need Go HTTP handler to invoke it and stream JSON |
| Test surface | NEW: smoke test that lists candidates without killing |

### Button 3: Stuck mcphub instance

| Field | Value |
|---|---|
| Action | `POST /api/cleanup/force-kill-instance` (NEW endpoint) |
| Diagnose mode | Same as `mcphub gui --force` (no-kill diagnostic) — runs identity gate, shows result |
| Confirm gate | Modal warns "this kills another mcphub instance"; requires typed confirmation |
| Identity gate | Reuse the existing 3-part gate from `internal/gui/probe*.go` |
| Failure UX | Show exit-code-7 explanation if gate refuses |
| Backend status | Logic exists in `internal/cli/gui.go:runForceKill`; needs HTTP wrapper |

### Button 4: Reset all daemons

| Field | Value |
|---|---|
| Action | `POST /api/cleanup/reset-all` — sequence: stop all daemons → wait for childExited per-daemon → run orphan-MCP cleanup → return combined report |
| Dry-run preview | Lists daemons that would be stopped + orphan candidates |
| Confirm gate | Modal lists running daemons by name + orphan count |
| Backend status | Composes existing operations; new endpoint |

## Open questions for user approval

1. **Maintenance section placement**: between Backups and About, or in its own sidebar item?
2. **POSIX support for orphan log watchers**: ship Windows-only initially, or block on `pkill`-based POSIX equivalent?
3. **`/api/cleanup/log-watchers` invocation**: shell out to the PS1 script (simple, fragile to PowerShell version), OR re-implement detection in Go using `os.Process` + `gopsutil` or similar (heavier, portable)?
4. **Reset all daemons** — keep as a single combined button, or split into "Stop all" + "Clean orphans" so user can do them separately?
5. **Auth**: same CSRF + DNS-rebind protection as existing `/api/*`. Confirm.

## Implementation phases

| Phase | Scope | Branch |
|---|---|---|
| Cleanup-1 | `scripts/cleanup-orphan-watchers.ps1` (DONE) + this design memo | `feat/cleanup-orphan-watchers-script` |
| Cleanup-2 | Backend: `/api/cleanup/orphans` (wrap existing `API.CleanupOrphans`) | `feat/api-cleanup-orphans` |
| Cleanup-3 | Backend: `/api/cleanup/log-watchers` (wrap PS1 script) | `feat/api-cleanup-log-watchers` |
| Cleanup-4 | Backend: `/api/cleanup/force-kill-instance` + `/api/cleanup/reset-all` | `feat/api-cleanup-instance-reset` |
| Cleanup-5 | Frontend: Settings → Maintenance section + 4 buttons + dry-run modals | `feat/gui-maintenance-section` |
| Cleanup-6 | E2E tests covering each button + dry-run + confirm + result | `feat/e2e-maintenance` |

## Acceptance criteria

1. Running each button in dry-run mode produces a JSON list visible in the modal, no processes killed.
2. Confirming each button kills only the matched processes; never kills mcphub itself, em.exe, python.exe, codex, claude, cursor, code, antigravity.
3. The Maintenance section is keyboard-accessible (Tab order, Enter to confirm).
4. `mcphub cleanup --confirm` (existing CLI) and the new GUI button reach identical end-state.
5. The orphan-watcher script's NeverKill list is honoured by the API endpoint.
6. POSIX users see a "Windows only (Phase 3B-II)" notice on the log-watchers button rather than a 500.
7. The Codex bot reviews this PR cleanly (no P1/P2 findings on auth/permissions/destructive defaults).

## Terms and Abbreviations

- `dry-run`: report-only mode that lists candidates without taking destructive action.
- `keep_n`: backup-retention policy field that bounds how many `.mcp.json.backup-*` files survive per server.
- `orphan`: a process whose parent PID is no longer in the OS process table.
- `orphan log watcher`: a `tail -F` or `grep` process spawned by an agent's shell launcher (Claude Code, codex CLI) that survived the parent agent's exit; subject of `d:\dev\orphaned-log-watchers-report.md`.
- `single-instance lock`: the file/path mcphub uses to prevent two `mcphub gui` instances from racing on the loopback HTTP port.
- `3-part identity gate`: the safety check from `internal/gui/probe*.go` that a `--force --kill` target must satisfy (executable basename, argv[1] = `gui`, start-time precedes pidport mtime) before being killed.
