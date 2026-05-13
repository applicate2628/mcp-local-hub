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

### D2.4 Real daemon kill — recovery via watchdog (5-min cadence)

**Important:** Task Scheduler `RestartOnFailure` does NOT reliably fire
for user-issued End Task / `Stop-Process -Force` kills on Win11 24H2+
(see `work-items/bugs/2026-05-07-task-scheduler-restartonfailure-not-firing.md`).
Recovery is now handled by the watchdog scheduled task
(`\mcp-local-hub-watchdog`), which runs `mcphub watchdog --once` every
5 minutes. The full smoke for the watchdog flow lives in **D2.6**;
D2.4 is the focused parent-wrap regression check (DM-3b port release,
DM-3a launch-failure parent line).

| Step | Expectation | Result |
|---|---|---|
| 1. Launch a known-good daemon: `mcphub install memory` then check `mcphub status` | memory daemon Running with PID populated; `mcp-local-hub-watchdog` scheduled task installed (auto-installed during `mcphub setup` per Task 11) | |
| 2. Open Task Manager, find the memory daemon process by PID | Process visible with "mcphub.exe" name | |
| 3. End the process from Task Manager | Daemon exits non-zero; native `<RestartOnFailure>` may or may not fire (see bug tracker); watchdog tick within ~5 min restarts the daemon regardless | |
| 4. Wait up to ~5 min (watchdog cadence), run `mcphub status` again | memory daemon Running again, NEW PID; `watchdog.log` tail shows `restart-verified-running` for `memory-default` | |
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

### D2.6 Watchdog 5-min recovery loop (`mcphub watchdog`)

Plan: `docs/superpowers/plans/2026-05-07-mcphub-watchdog.md` (v13).

The watchdog is a per-user scheduled task (`\mcp-local-hub-watchdog`)
that runs `mcphub watchdog --once` every 5 min. Each tick walks the
daemon registry, classifies failures (`IsRealFailure`), and restarts
eligible daemons under a strictly-pure recovery state machine. State
files live in `%LOCALAPPDATA%\mcp-local-hub\` (Windows) — see CLAUDE.md
"Watchdog (Phase 3B-II onward)" for the full path manifest.

**Prerequisites (in addition to the file-level prerequisites above):**

- `mcphub setup` run on this host (auto-installs `\mcp-local-hub-watchdog`
  per Task 11; install is idempotent and refuses elevated unless
  `--allow-elevated`).
- `mcphub watchdog status` reports `Scheduled task: installed`.
- A known-good `memory` daemon registered + Running.
- For sub-case 17, build a synthetic-name binary in a scratch folder;
  do NOT pollute the real registry.

**Tick-evidence sources:**

- `%LOCALAPPDATA%\mcp-local-hub\watchdog.log` — JSON Lines decision log.
- `%LOCALAPPDATA%\mcp-local-hub\intent-audit.log` — JSON Lines audit log.
- `mcphub watchdog status` (or `mcphub watchdog status --json`) — live
  observability output (cooldown, three sliding windows, abs paths,
  recent events, recent audit tail with redaction).

**Sub-cases.** Mark each PASS / FAIL / SKIP with a one-line note. SKIP
requires a reason. Most sub-cases need a real Windows desktop (Task
Scheduler + flock semantics).

- [ ] **D2.6.1 — baseline reinstall.** Run `mcphub install memory`
  (Task 10 wires audit-first install). `mcphub status` shows memory
  Running. `intent-audit.log` shows a `server-installed` entry; intent
  file `daemon-intent.json` carries `Desired=running` for `memory`.
  XML for `mcp-local-hub-memory-default` parses through
  `watchdog_xml_validator` cleanly.
- [ ] **D2.6.2 — force-kill recovery.** Task Manager → End Task on the
  memory daemon process. Within 5 min, watchdog tick fires;
  `mcphub status` shows memory Running with a NEW PID. `watchdog.log`
  contains `restart-verified-running` for the daemon's task name in
  the recovery tick.
- [ ] **D2.6.3 — graceful stop suppresses revival.** `mcphub stop --server memory`.
  `daemon-intent.json` updated to `Desired=stopped, Reason=user-stop`.
  Wait 5 min (one full watchdog tick) — daemon stays Stopped. Repeat
  observation 24 h later (or fast-forward intent TTL by editing
  `UpdatedAt` to >7 d ago in a scratch run): permanent intents do not
  age out within 24 h.
- [ ] **D2.6.4 — `--force` stop, intent unwritable, audit OK.** Acquire
  exclusive lock on `daemon-intent.json` (e.g., open in another process
  with `FILE_SHARE_NONE`). Run `mcphub stop --server memory --force`.
  Audit log records `forced-stop-without-intent` (Priority=high). Daemon
  killed. Status reports the audit entry in tail.
- [ ] **D2.6.5 — `--force` stop, both intent + audit unwritable.**
  Lock both `daemon-intent.json` and `intent-audit.log`. Run
  `mcphub stop --server memory --force`. Command fails closed
  (non-zero exit, daemon NOT killed). Stderr explains audit-required
  fail-closed semantic.
- [ ] **D2.6.6 — chronic-failure intent skips.** Edit
  `daemon-intent.json` to set memory's `Desired=stopped, Reason=chronic-failure`.
  Watchdog tick logs `chronic-failure` for that daemon and does not
  attempt restart. (Real chronic auto-disable is exercised by
  sub-case 9 below.)
- [ ] **D2.6.7 — `suspicious-xml` rejection.** Hand-edit the scheduled
  task XML for `mcp-local-hub-memory-default` (via `schtasks /Change`
  or registry tooling) to point its `<Command>` at `notepad.exe`. Next
  watchdog tick logs `suspicious-xml` for that task name and SKIPS the
  restart. The validator's structural-ownership rejection (§5 v6) is
  the security gate.
- [ ] **D2.6.8 — structural-ownership rejection (manifest mismatch).**
  Add a fake server entry to the manifest whose `name` does NOT match
  any installed scheduled task; OR install a task with a name that
  doesn't resolve back to a manifest entry (e.g., manually create
  `mcp-local-hub-bogus-default` via `schtasks /Create`). Next watchdog
  tick rejects the task — `validator.IsOwnedAndValid` returns false,
  decision is `suspicious-xml`. Note: v6 removed the manifest-hash
  mechanism; the XML validator + structural ownership is the gate.
- [ ] **D2.6.9 — chronic auto-disable (~2 h).** Install a synthetic
  always-fail daemon (e.g., a wrapper that `exit 1`s immediately).
  Allow watchdog to run for ~2 h (~24 ticks). Verify cooldown grows
  per backoff schedule, then `ChronicLimitReached` fires and watchdog
  writes `Desired=stopped, Reason=chronic-failure` to the intent file.
  Audit log carries the `chronic-failure-auto-disabled` entry. Status
  surfaces the chronic state.
- [ ] **D2.6.10 — wall-clock jump suppression.** Set system clock 1 day
  forward (Settings → Date & Time, or `w32tm`-equivalent). Next
  watchdog tick: `watchdog.log` records `wall-clock-jump-suspect` and
  recovery is suppressed for that tick. Restore clock; subsequent
  tick proceeds normally. Verify `mcphub watchdog status` shows
  `LastWallClockSeen` and the delta.
- [ ] **D2.6.11 — corrupt-strike self-quarantine + manual recovery.**
  Corrupt `daemon-intent.json` 4 times within 30 min (write garbage
  bytes; let watchdog tick consume + quarantine each strike). On the
  4th strike, watchdog calls `UninstallWatchdogTaskInternal(QuarantineFourStrikes30Min)`
  and exits 9. Verify: `\mcp-local-hub-watchdog` scheduled task is
  GONE; `intent-audit.log` shows `Action="watchdog-self-quarantined"`
  with `Reason="4-strikes-30min"`; `mcphub watchdog status` reports
  `WATCHDOG SELF-QUARANTINED`. Manually verify `.corrupt-*`
  quarantine files (5 newest retained) under the state dir, confirm
  clean state files, then run `mcphub watchdog install` to resume.
- [ ] **D2.6.12 — singleton lock contention.** From two terminals,
  invoke `mcphub watchdog --once` simultaneously. The second process
  acquires `--once.lock` (flock) FAILS, reads `--once.lock.owner.json`
  best-effort, logs `already-running-skipped` to `watchdog.log` with
  the first process's PID/started_at/hostname, and exits 0.
- [ ] **D2.6.13 — `mcphub watchdog status` rich output.** Run
  `mcphub watchdog status` and verify the output includes: scheduled
  task installed-state, cadence (5 min), `LastWallClockSeen` with
  delta, `CorruptStrikeWindow` count + age of oldest entry, recent
  events tail (last 20 from `watchdog.log`), audit tail (last 20 from
  `intent-audit.log`) WITH redaction (any `caller_user` not matching
  the OS-current user shows `<redacted-non-owner>`), and absolute
  paths to all state files (per §57 — see CLAUDE.md "Watchdog (Phase
  3B-II onward) → State files"). `mcphub watchdog status --json`
  emits the same data with the same redaction rules.
- [ ] **D2.6.14 — pre-restart-persist durability (v9 §30).** Start
  `mcphub watchdog --once` manually from a TTY. While `RestartContext`
  is in flight, kill the watchdog process via Task Manager. Next
  scheduled tick (5 min later) reads `watchdog-state.json`, finds
  `RestartPendingAt != 0` for that task, and emits
  `restart-pending-skipped`. After ~6 min (TTL aging), a subsequent
  tick attempts a fresh restart and emits
  `restart-pending-stale-cleared` to `watchdog.log` (visible in
  `mcphub watchdog status`).
- [ ] **D2.6.15 — owner JSON in flock skip.** Start
  `mcphub watchdog --once` manually (long-running on a synthetic
  busy daemon set, OR pause the binary in a debugger to hold the
  lock). Concurrently invoke another `mcphub watchdog --once`. The
  second run logs `already-running-skipped` AND surfaces the first
  run's PID, `started_at`, and hostname (read best-effort from
  `--once.lock.owner.json`). `mcphub watchdog status` "Last flock
  skip" line shows the same identity.
- [ ] **D2.6.16 — status redaction.** Trigger an audit entry where
  `caller_user` is set to a value other than the OS-current user
  (e.g., stage a synthetic entry by appending a JSON line directly to
  `intent-audit.log` with `caller_user="test-user"`). Run
  `mcphub watchdog status` — the audit-tail display replaces that
  field with `<redacted-non-owner>`. The actual file content is NOT
  modified. System entries (`SystemEntry=true`, e.g., `<rotation-system>`,
  self-quarantine sentinel) are NEVER redacted (§37).
- [ ] **D2.6.17 — 16KB log cap.** Install a synthetic daemon whose
  task name exceeds 1KB (testing only — DO NOT ship; legitimate task
  names are <100 bytes). Watchdog rejects audit writes with
  `ErrIdentityOversize` (identity-preserving 16KB cap, plan §35).
  Next, install a daemon whose `note`/`err` field would exceed 16KB
  but whose `task` is <1KB; verify audit/log entries fit ≤16KB with
  `_truncated:true`, `_truncated_field:"<name>"`, and a 12-hex
  `_task_hash` for forensic correlation. Cleanup: remove the
  synthetic tasks via `schtasks /Delete`.

After completing D2.6, run `mcphub watchdog status` once more and
attach a copy of the recent-events tail to the audit-trail entry at
the bottom of this doc.

### D2.8 — Marketplace draft-import (G5, v2 per codex r1)

1. `mcphub marketplace refresh` → "Refreshed catalog: N entries." on stdout.
2. `mcphub marketplace search filesystem` → row for `filesystem` entry.
3. `mcphub marketplace show filesystem` → metadata block + `Readme URL: <url>` (NO README body — open the URL yourself).
4. `mcphub marketplace generate filesystem > /tmp/draft.yaml`
5. **Operator-edit step (load-bearing):** open `/tmp/draft.yaml` and:
   - change `name: filesystem` to a unique server id, e.g. `name: filesystem-test`
   - replace `port: 0` with a free port, e.g. `port: 9200`
   - inspect `command` + `base_args` + `env`; replace any verbatim `${env:*}` placeholders with the values you want persisted
   - the leading comment block reminds you of these three steps
6. `mcphub manifest create filesystem-test < /tmp/draft.yaml` → manifest accepted; `mcphub manifest list` shows `filesystem-test`.
7. `mcphub install --server filesystem-test --clients claude-code` → install succeeds; the daemon registers.
8. `mcphub marketplace generate context7` → non-zero exit; stdout empty; stderr contains "G6" + "wait" + "workaround".
9. Disconnect network; `mcphub marketplace search filesystem` → WARN line on stderr; cached output on stdout still works.
10. Manually plant a future `fetched_at` in `<state-dir>/marketplace-cache.json` (overwrite the field to `2099-01-01T00:00:00Z`) and re-run search → expect a fresh fetch attempt (revalidate is forced).

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
