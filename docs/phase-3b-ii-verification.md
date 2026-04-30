# Phase 3B-II Verification — D2/D3 Manual Smoke Matrix

This file is the operator-driven smoke checklist for surfaces that
Playwright cannot reach (Windows tray, console-subsystem matrix,
real Task Manager kill, multi-language LSP backends). Run before
tagging a Phase 3B-II release.

**Scope:** all items deferred from automated coverage in
`docs/superpowers/plans/phase-3b-ii-backlog.md` rows D2 and D3.

**Prerequisites:**

- Windows 10 or 11 desktop session (tray + Task Scheduler are
  Windows-only by design — see spec §2.2 non-goals).
- `mcphub.exe` built from the branch under test:
  `pwsh -ExecutionPolicy Bypass -File .\build.ps1` populates
  `~/.local/bin/mcphub.exe` with embedded version metadata.
- Clean state: no other `mcphub.exe` instances running, no scheduler
  tasks under `mcp-local-hub-*` from a different binary version.
- One MCP-capable client (Claude Code, Codex CLI, Gemini CLI, or
  Antigravity) installed for routing-end tests in D2.4 and D3.

**How to fill in:** mark each row PASS / FAIL / SKIP with a one-line
note. SKIP requires a reason (e.g., "no LSP installed for rust").
Capture the binary version at the bottom for the audit trail.

---

## D2 — Live manual smoke

### D2.1 Tray icon rendering and state variants

The tray icon is rendered by direct Win32 syscalls (user32 + shell32
via `golang.org/x/sys/windows` lazy DLL imports — no CGo, no
third-party tray library) running in a separate `mcphub tray`
subprocess spawned from `mcphub gui`. PR #24 replaced the prior
`getlantern/systray`-based path. Programmatically generated 16×16
PNG icons (`internal/tray/icons.go`) are pushed via stdin JSON IPC
at four state variants (healthy / partial / down / error). State
changes are driven by SSE `daemon-state` events the GUI poller
publishes every 5s.

| Step | Expectation | Result |
|---|---|---|
| 1. Launch `mcphub gui` | Tray icon appears in notification area; left-click brings the dashboard window to front (`gui.FocusBrowserWindow`); on no-window fallback opens a fresh `http://127.0.0.1:<port>/` | |
| 2. Hover the icon | Tooltip shows `mcp-local-hub: <state>` (one-word state label) | |
| 3. Right-click OR keyboard Apps-key / Shift+F10 on the focused icon | Two-item popup menu anchored at the icon's screen rect: "Open dashboard", "Quit (keep daemons)". A single right-click delivers BOTH `WM_RBUTTONUP` AND `WM_CONTEXTMENU` on Win11 — the 200ms `lastMenuShow` debounce in `tray_windows.go` absorbs the second event so the menu does NOT re-open after dismissal. | |
| 4. With all daemons running, observe the icon | "healthy" variant rendered (color-coded green) | |
| 5. Stop one daemon via `mcphub stop <server>` | Icon switches to "partial" within ~5s | |
| 6. Stop all daemons | Icon switches to "down" within ~5s | |
| 7. Trigger a daemon failure | Icon switches to "error" with a single failure-onset toast (`internal/tray/toast_windows.go::ShowToast`); subsequent identical failures do NOT re-toast | |
| 8. Click "Quit (keep daemons)" | GUI closes, tray icon disappears (NIM_DELETE before destroyWindow), scheduler tasks remain ACTIVE (`mcphub status` still shows them) | |
| 9. Re-launch `mcphub gui`, kill `mcphub tray` child via Task Manager | Parent GUI surfaces "tray subprocess exited" stderr line; GUI continues serving `http://127.0.0.1:<port>/`; tray child does NOT auto-respawn (deliberate — restart `mcphub gui` to recover the tray) | |
| 10. Restart Explorer (Task Manager → "Restart") | Tray icon re-appears at the same state via the `TaskbarCreated` re-register path; `versionV4` is reset to the new SETVERSION outcome (legacy mode if the new shell instance refuses V4) | |

### D2.2 AttachConsole + windowsgui subsystem matrix

`mcphub.exe` is built with the `windowsgui` subsystem so launching
the tray doesn't pop a console window, but `mcphub status` /
`mcphub install` need to print to the host console when invoked
from a terminal. The binary uses `kernel32!AttachConsole` to attach
to the parent console when the parent has one.

| Host | Command | Expectation | Result |
|---|---|---|---|
| `cmd.exe` | `mcphub version` | Output appears in cmd, exit 0 | |
| `cmd.exe` | `mcphub status` | Output appears in cmd, exit 0 | |
| `cmd.exe` | `mcphub install memory` | Output appears in cmd, scheduler task created | |
| `cmd.exe` | `mcphub gui` | NO console window pops; tray icon appears | |
| PowerShell 7+ | `mcphub version` | Output appears in pwsh, exit 0 | |
| PowerShell 7+ | `mcphub status` | Output appears in pwsh, exit 0 | |
| PowerShell 7+ | `mcphub install memory` | Output appears in pwsh, scheduler task created | |
| PowerShell 7+ | `mcphub gui` | NO console window pops; tray icon appears | |
| Git Bash (MINGW64) | `mcphub version` | Output appears in git-bash, exit 0 | |
| Git Bash | `mcphub status` | Output appears in git-bash, exit 0 | |
| Git Bash | `mcphub install memory` | Output appears in git-bash, scheduler task created | |
| Git Bash | `mcphub gui` | NO console window pops; tray icon appears | |
| Task Scheduler (logon trigger) | `mcphub-local-hub-memory-default` task fires at logon | Daemon process starts; no console window flickers | |
| Explorer (double-click `mcphub.exe`) | mcphub.exe launched without args | Help text shown via fallback console allocation OR exit-with-stderr explanation | |

### D2.3 Single-instance recovery through OS reboot

The single-instance lock uses `gofrs/flock` against
`%LOCALAPPDATA%\mcp-local-hub\gui.pidport.lock`, with the adjacent
`gui.pidport` recording `<pid> <port>` for the second-instance
handshake. After an OS reboot both files persist but no process
holds the flock; the next `mcphub gui` must succeed cleanly without
manual intervention.

| Step | Expectation | Result |
|---|---|---|
| 1. Launch `mcphub gui`; verify tray + dashboard | Process active, port 9125 bound, lock held | |
| 2. Reboot the OS via Start menu (clean shutdown) | Windows shuts down without hang from mcphub | |
| 3. After reboot, log back in | Logon-triggered scheduler tasks fire daemons | |
| 4. Check `gui.pidport` + `gui.pidport.lock` exist in `%LOCALAPPDATA%\mcp-local-hub\` | Both files present (leftover markers; lock is unheld) | |
| 5. Launch `mcphub gui` from cmd | Tray + dashboard appear; flock is re-acquired without error; pidport is rewritten with the new PID/port | |
| 6. Force-kill mcphub gui via Task Manager | Process gone, port released, files remain (kernel releases flock as a side effect) | |
| 7. Launch `mcphub gui` again | Tray + dashboard appear; second-instance handshake handles the leftover; new instance acquires lock cleanly | |

### D2.4 Real daemon kill via Task Manager

DM-3b added `waitForPortFree` so Restart can survive TIME_WAIT after
a kill. Verify the full kill → recover loop with real Task Scheduler
RestartOnFailure semantics.

| Step | Expectation | Result |
|---|---|---|
| 1. Launch a known-good daemon: `mcphub install memory` then check `mcphub status` | memory daemon Running with PID populated | |
| 2. Open Task Manager, find the memory daemon process by PID | Process visible with "mcphub.exe" name | |
| 3. End the process from Task Manager | Daemon exits non-zero; Task Scheduler RestartOnFailure (3 retries × 1 min) kicks in | |
| 4. Wait ~70s, run `mcphub status` again | memory daemon Running again, NEW PID; uptime reset; State=Running | |
| 5. Within the GUI Dashboard during the recovery window | "Restarting" event appears in recent activity; toast notification fires (when D2.1 step 7 verified the toast wiring) | |
| 6. End memory process again, but observe `mcphub.exe daemon` log file | Log shows clean child exit notice; no `[mcphub-launch-failure ...]` line because the parent wrap (DM-3a) only fires on launch failures, not steady-state crashes | |
| 7. Trigger a real launch failure (`mcphub install memory` after deleting `npx` from PATH) | scheduler task fires, daemon command exits non-zero, `[mcphub-launch-failure ...]` line appears in `%LOCALAPPDATA%\mcp-local-hub\logs\memory-default.log` | |
| 8. Run `mcphub restart memory` from cmd | Restart waits for port release (DM-3b), `schtasks /Run` succeeds, daemon Running again | |

### D2.5 — `mcphub gui --force` stuck-instance recovery (PR #23)

**Test:** Reproduce a stuck single-instance lock via debugger pause:

1. `mcphub gui` (binds default port; tray icon visible).
2. Attach a debugger (e.g. `dlv attach <PID>`) and pause the gui
   process.
3. From a second terminal: `mcphub gui --force`.
4. Verify: structured diagnostic prints (Lock file path, recorded
   PID, port, alive=true, /api/ping=connection refused).
   Explorer/Finder window opens at the pidport directory.
5. From the same terminal: `mcphub gui --force --kill --yes`.
6. Verify: "force-killed previous incumbent PID `<pid>` and acquired
   lock" prints; new gui starts on a fresh port.
7. Detach the debugger. The original gui is gone (TerminateProcess'd).

**Expected outcomes:**

- Step 4: exit code 2 (bare diagnostic).
- Step 6: exit code 0 (kill succeeded).

**If exit 7:** the recorded PID is a different mcphub subcommand
(e.g. `mcphub daemon`); rerun without `--kill` and identify the
actual flock holder via `handle.exe` (admin shell) or reboot.

---

## D3 — Multi-language workspace smoke

`mcphub register <workspace> <lang>...` materializes per-(workspace,
language) lazy proxies that route LSP traffic. Phase 3 unit tests
cover one language at a time with mocked backends; this is the live
multi-language test.

**Prerequisites for D3:**

- A real workspace directory: e.g. `D:\dev\demo-multi-lang` containing
  `main.cpp`, `main.py`, and `Cargo.toml` (any small files).
- LSP backends installed:
  - clangd (cpp): typically via Visual Studio Build Tools or LLVM release
  - pyright-langserver (python): `npm install -g pyright`
  - rust-analyzer: `rustup component add rust-analyzer`
- `mcp-language-server` wrapper: see `servers/mcp-language-server/manifest.yaml`

| Step | Expectation | Result |
|---|---|---|
| 1. Run `mcphub register D:\dev\demo-multi-lang cpp python rust` | Three scheduler tasks created (`mcphub-local-hub-lsp-<wsKey>-cpp/python/rust`); registry entry has all three rows; ports allocated from PortPool | |
| 2. Run `mcphub workspaces` | Workspace listed with all 3 languages, lifecycle=Configured | |
| 3. Run `mcphub status` | All three lsp- tasks visible, State=Scheduled (lazy — proxies bind on first tools/call, not at registration) | |
| 4. Open workspace in a Claude Code session and trigger a language server call (e.g. hover over a symbol in main.cpp) | clangd lazy-proxy materializes, port binds, status shows Running for cpp lsp; python and rust still Scheduled | |
| 5. Trigger calls in main.py and Cargo.toml | pyright + rust-analyzer materialize independently; no port conflicts | |
| 6. Run `mcphub status` after all 3 are warm | All three lsp- rows Running with distinct PIDs and ports | |
| 7. Kill one backend (e.g. clangd.exe) via Task Manager | mcphub status shows the cpp row as Stopped/Starting; the lazy proxy auto-respawns the backend on the next tools/call | |
| 8. Run `mcphub unregister D:\dev\demo-multi-lang cpp python rust` | All three scheduler tasks removed; registry entries cleared; ports returned to pool | |
| 9. Verify `mcphub workspaces` is empty | No leftover entries | |

---

## Audit trail

| Field | Value |
|---|---|
| Branch | |
| Commit SHA | |
| `mcphub version` output | |
| Operator | |
| Date | |
| Result summary (X / Y PASS) | |

Filed checklists go to `docs/verifications/` with the date prefix
`YYYY-MM-DD-phase-3b-ii.md` so historical runs are searchable.
