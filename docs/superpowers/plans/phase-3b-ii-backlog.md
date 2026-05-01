# Phase 3B-II Backlog

Tracking document for Phase 3B-II — the "everything I cut from Phase 3B-I MVP" scope. NOT an implementation plan yet. A plan (via `superpowers:writing-plans`) is written when Phase 3B-II execution starts.

**Source documents:**

- [docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md](../specs/2026-04-17-phase-3-gui-installer-design.md) — original GUI design spec (full scope before MVP split)
- [docs/phase-3b-verification.md](../../phase-3b-verification.md) — Phase 3B-I MVP closeout, "Deferred to Phase 3B-II" section
- [docs/phase-3b-pr5-codex-walkthrough.md](../../phase-3b-pr5-codex-walkthrough.md) — PR #5 review walkthrough with R17 / final-review deferrals

---

## Outstanding gaps vs spec — comprehensive audit (2026-05-01)

Codex CLI read-only audit comparing spec sections vs shipped code. Item rows below are NEW additions on top of the original A/B/C/D/E/F sections farther down. Severity:

- **blocker** — substantial spec capability missing or misshapen
- **nice-to-have** — secondary affordance or polish
- **cosmetic** — visual-only divergence

### Screen-specific gaps

| Sev | Screen | Gap | Location |
|---|---|---|---|
| blocker | Servers | Matrix has Server + 4 clients + Port + State only. Spec wants RAM/Uptime/Status columns and a row drawer with manifest preview / lifetime stats / Stop & Restart. | [Servers.tsx](../../../internal/gui/frontend/src/screens/Servers.tsx) |
| blocker | Dashboard | Cards show Port/PID/State + Restart/Stop only. Spec wants green/red dot, RAM, Uptime, connected-clients count, requests-per-minute, RAM sparkline, View logs link, retry count. | [Dashboard.tsx](../../../internal/gui/frontend/src/screens/Dashboard.tsx) |
| blocker | Logs | Server dropdown + tail + Follow SSE present. Missing regex/substring filter, Open folder action, amber/red line highlighting on warn/error. | [Logs.tsx](../../../internal/gui/frontend/src/screens/Logs.tsx) |
| blocker | Settings | `appearance.layout` registry key + sidebar/tabs switcher absent. tray, daemons.weekly_schedule, daemons.retry_policy, backups.clean_now, advanced.export_config_bundle still Deferred. | [settings_registry.go](../../../internal/api/settings_registry.go), [SectionBackups.tsx](../../../internal/gui/frontend/src/components/settings/SectionBackups.tsx) |
| nice-to-have | Servers | "Servers — default home" per spec; we ship Dashboard as default per user request — keep, document override. | — |
| nice-to-have | Servers | `unsupported` UI state checked but routing mapper outputs only `via-hub/direct/not-installed`. | [routing.ts](../../../internal/gui/frontend/src/lib/routing.ts) |
| nice-to-have | Migration | "Via hub" should be readonly; we expose Edit-manifest + Demigrate. | [Migration.tsx](../../../internal/gui/frontend/src/screens/Migration.tsx) |
| nice-to-have | Add/Edit | `cwd` missing in manifest model; env supports literal/secret only — no file/env prefix selectors. | [manifest.go](../../../internal/config/manifest.go), [SecretPicker.tsx](../../../internal/gui/frontend/src/components/SecretPicker.tsx) |
| nice-to-have | Secrets | "Edit vault shells out" not implemented — UI only displays the `mcphub secrets edit` command. | [Secrets.tsx](../../../internal/gui/frontend/src/screens/Secrets.tsx) |
| nice-to-have | About | No README / INSTALL / verification docs links. | [About.tsx](../../../internal/gui/frontend/src/screens/About.tsx) |

### Cross-section gaps

| Sev | Section | Gap | Location |
|---|---|---|---|
| blocker | §3.5 Backups | `BackupKeep` + `BackupsClean` exist but install/migrate paths call `Backup()` without enforcing keep-N — retention not automatic. | [clients.go](../../../internal/clients/clients.go), [install.go](../../../internal/api/install.go), [migrate.go](../../../internal/api/migrate.go) |
| blocker | §3.6 Event bus | Production publishes only `daemon-state`, `bulk-action`, `log-line`, `poller-error`. Missing: `daemon-failed`, `install-progress`, `install-done`, `scan-result`, `client-config-changed`. No fsnotify watcher. | [events.go](../../../internal/gui/events.go), [poller.go](../../../internal/gui/poller.go) |
| blocker | §4.3 HTTP API | Drift: missing `/api/install-all`, `DELETE /api/install/:server`, REST `/api/manifests*`, `/api/backups/clean`, `/api/backups/content`, `/api/rollback`, scheduler endpoints, bulk `PUT /api/settings`. | [install.go](../../../internal/gui/install.go), [manifest.go](../../../internal/gui/manifest.go), [backups.go](../../../internal/gui/backups.go), [settings.go](../../../internal/gui/settings.go) |
| blocker | §6 Tray menu | Actual menu: Open dashboard, Run/Stop all, Quit variants. Missing: disabled status header, Rescan client configs, recent-activity submenu, Open logs folder, Open data folder. | [tray_windows.go](../../../internal/tray/tray_windows.go) |
| nice-to-have | §5 Layout | Sidebar-only; spec wants top-tabs alternative, Settings-switchable. | [app.tsx](../../../internal/gui/frontend/src/app.tsx), [settings_registry.go](../../../internal/api/settings_registry.go) |
| nice-to-have | §6 Toasts | Failure-onset toast wired; auto-restart and manual-action-completed toasts plus Settings on/off toggle missing. | [gui_tray_state.go](../../../internal/cli/gui_tray_state.go) |
| nice-to-have | §7 Density | `data-density` applied at root, but variables consumed only in Settings/Backups; main screens still hardcode spacing. | [app.tsx](../../../internal/gui/frontend/src/app.tsx), [style.css](../../../internal/gui/frontend/src/styles/style.css) |
| nice-to-have | §7 Status shapes | Status is text + color only; no `●/○` shape indicator (color-blind affordance). | [Dashboard.tsx](../../../internal/gui/frontend/src/screens/Dashboard.tsx) |
| cosmetic | §7 Icons | No SVG `<symbol>` sprite; UI uses unicode/emoji (🔑, ×, ⚠) instead of canonical Lucide/Feather set. | [SecretPicker.tsx](../../../internal/gui/frontend/src/components/SecretPicker.tsx), [AddServer.tsx](../../../internal/gui/frontend/src/screens/AddServer.tsx) |

### Solo-closeable subset (no design decisions, ship before user manual smoke)

Tracked as "Round 1" items I can land without UI verification beyond Codex bot review:

1. `appearance.layout` registry + Settings dropdown + sidebar/tabs switcher (PR #29-style follow-up)
2. Status shapes (●/○) on Dashboard cards + Servers state column
3. Density variables consumed across all screens (audit grep, expand var usage)
4. Backups keep-N enforcement on install/migrate paths
5. Logs regex/substring filter + amber/red highlight + Open folder action
6. Tray menu items: status header (disabled), Rescan client configs, Open logs folder, Open data folder
7. About: README/INSTALL/verification docs links

### Design-decision subset (parked until user weighs in)

Each requires a design memo (per-metric Dashboard fields, Servers drawer layout, event bus shape, HTTP API contract reconciliation). Not closing solo:

- Dashboard expanded card (RAM, Uptime, sparkline, requests/min, retry count, View logs)
- Servers row drawer (manifest preview, lifetime stats)
- §3.6 event bus completion + fsnotify watcher
- §4.3 HTTP API contract reconciliation (decide: spec is canonical → rewrite handlers, or current code is canonical → spec gets aligned)
- A4-b Settings lifecycle Action items (Clean now / export bundle / weekly schedule / retry policy / tray toggle backend wiring)

---

## Scope items

### A. Secondary screens (spec §5)

| # | Screen | Spec | Description |
|---|---|---|---|
| A1 | **Migration** | §5.2 | Scan-driven view grouping entries by status (`Via hub` / `Can migrate` / `Unknown servers` / `Per-session`). "Create manifest" action on unknown stdio entries pre-fills AddServer form via `ExtractManifestFromClient()`. |
| A2 | **Add/Edit manifest** | §5.4 | Accordion form: Basics → Command → Environment → Daemons → Client bindings → Advanced. Live YAML preview. Validate + Save & Install / Save only actions. |
| A3 | **Secrets** | §5.6 | Key names table with "Used by" counts. Values never displayed. Add/Rotate/Delete flows. |
| A4 | **Settings** | §5.7 | Appearance / GUI server / Daemons / Backups / Advanced sections. Persists to `gui-preferences.yaml`. |
| A5 | **About** | §5.8 | Version, commit, build date, GitHub link, Apache 2.0 license, credits. |

### B. Backend API gaps

| # | API | Description |
|---|---|---|
| B1 | **Reverse-migrate** — `api.Demigrate(server, clients)` | HTTP hub entry → original stdio entry. Unlocks uncheck semantics in Servers matrix (currently disabled per PR #5 R17 fix). |
| B2 | **ExtractManifestFromClient** — `api.ExtractManifestFromClient(client, server)` | Reads stdio entry from a client config, returns draft manifest YAML. Unlocks §5.2 "Create manifest" action. |

### C. Polish / UX gaps (from PR #5 Codex review)

| # | Area | Description | Source |
|---|---|---|---|
| C1 | `--force` take-over flag | ✅ PR #23 — bare `--force` prints structured Verdict diagnostic + opens lock folder + exit 2. `--force --kill` enforces a three-part identity gate (image basename + argv[1]=="gui"\|\|len(argv)==1 + start-time vs pidport mtime), SIGKILL/TerminateProcess the recorded PID, and acquire-polls TryLock until success or 2s deadline. **MUST NOT delete `<pidport>.lock`** — flock is the source of truth; deletion would race a successor's pidport rewrite (`internal/gui/single_instance.go::Release` invariant). The PR #5-era proposal to "delete `<pidport>.lock`, acquire" was rewritten during the C1 design memo; current contract is kill-and-wait, not delete-and-acquire. | PR #5 cleanup |
| C2 | Browser focus on activate-window | ✅ PR #22 — `gui.FocusBrowserWindow` enumerates visible top-level windows by title substring "mcp-local-hub", calls SW_RESTORE then SetForegroundWindow. Activate-window callback in cli/gui.go now invokes it instead of logging. Manual real-match smoke in `docs/phase-3b-ii-verification.md` D2.1. | PR #5 final review |
| C3 | Tray icon state variants | ✅ PR #22 — `internal/tray/state.go::Aggregate` maps DaemonStatus rows to one of 4 spec-§6 variants (`healthy / partial / down / error`). `internal/tray/icons.go` programmatically generates 4 colored 16×16 PNG icons via `image/draw`+`image/png`, lazily cached. `StatusPoller.SetSnapshotChannel` feeds an `aggregateTrayState` goroutine in cli/gui.go that coalesces duplicate-state forwards. tray_windows.go selects on the new `Config.StateCh` and calls `systray.SetIcon`+`SetTooltip` per transition. | Spec §6 |
| C4 | Toast notifications | ✅ PR #22 — `internal/tray/toast_windows.go::ShowToast` invokes Windows.UI.Notifications via PowerShell (no extra Go deps). Failure-onset detection inside `aggregateTrayStateWithToast` (cli/gui_tray_state.go) compares per-row `(LastResult != 0 OR state-contains-fail)` between adjacent snapshots; fires a toast on each new failure key, never on repeats. `auto-restart-triggered` and `manual-action-done` events are not yet emitted by any publisher — when they are added, the same listener pattern can subscribe to broadcaster directly. | Spec §6 |

### D. Testing & frontend infrastructure

| # | Item | Description |
|---|---|---|
| **D0** | **Frontend toolchain migration → Vite + TS + React** | Phase 3B-I shipped vanilla JS (~700 LOC across `app.js`/`servers.js`/`dashboard.js`/`logs.js`). The five Phase 3B-II screens — especially A2 (AddServer form with accordion + live YAML preview + save-time validation) — grow this to ~2000+ LOC of hand-rolled `innerHTML` templating, which does not scale. Migrate the existing 3 screens to a Vite + TypeScript + React (or Preact for smaller bundle) stack BEFORE adding new screens. Build output stays `go:embed`-consumable (static HTML/JS/CSS under `internal/gui/assets/`), Go side untouched. No runtime change for end users; dev-time adds Node.js + npm. **Must come before D1 and before any A-series screen** — otherwise we rewrite twice. |
| **D1** | **Playwright E2E suite** | Automate the "live browser smoke" gap that PR #5 left deferred. Covers: Servers matrix render + toggle, Dashboard live SSE updates, Logs picker + tail follow, Migration grouping (post-A1), AddServer form (post-A2). Runs against `go run ./cmd/mcphub gui --no-browser --no-tray --port 0` spawned by test fixture. Node.js + `@playwright/test` + headless Chromium. ~300-500 lines of test code. CI job separate from `go test`. Follows D0 so the toolchain is already in place. |
| D2 | Live manual smoke | Remaining gaps Playwright does NOT cover: tray icon rendering, AttachConsole + windowsgui subsystem matrix (cmd/PowerShell/Git Bash/Scheduler/Explorer × status/install/gui), single-instance recovery through OS reboot, real daemon kill via Task Manager. Requires Windows desktop. |
| D3 | Multi-language workspace smoke (Phase 3 follow-up) | `mcphub register D:\dev\proj cpp python rust` — concurrent materialization of three real LSP backends (clangd, pyright-langserver, rust-analyzer). Currently unit-tested with fakes; no live multi-language verification. Belongs with D2 on the same Windows desktop session. |

### E. Spec success criteria deferred

| Criterion | Description | Depends on |
|---|---|---|
| §2.1 #3 | Create manifest from unknown stdio — "I have cursor-mcp-fetch configured as stdio, wrap it into a hub daemon" | A1 + A2 + B2 |
| §2.1 #7 | Backup management UX — list + restore + delete timestamped backups, keep-N enforcement UI | A4 Settings screen |

### F. Linux-server readiness (post-Phase 3B-II)

**Strategic goal:** project ships first on Windows desktop, but should be deployable on a **headless Linux server** without rewriting cross-cutting subsystems. Scoped under "server" deliberately — Linux DESKTOP (tray icons, browser focus, GNOME-tray-extension territory) is OUT of scope; on a server these are no-ops by design.

| # | Item | Effort | Description |
|---|---|---|---|
| F1 | **Platform-lane refactor** | ~1d | Move Windows-specific implementations into `internal/platform/windows/` behind a `Platform` interface; `internal/platform/linux/` and `internal/platform/darwin/` start as stubs. No behavior change. Goal: future Linux/macOS work touches ONE directory, not 17 scattered build-tagged files. Pre-req for everything else in F. |
| F2 | **Linux scheduler — systemd user units** | ~3-5d | Replace `not implemented` stub with a real systemd backend in `internal/scheduler/scheduler_linux.go` (or via the F1 lane). Generate `~/.config/systemd/user/mcp-local-hub-<server>-<daemon>.service` units, `systemctl --user enable/start/stop/status` plumbing, log forwarding. CI: spawn ubuntu-latest container, run `mcphub install --server time` smoke test. |
| F3 | **`mcphub setup --server`** | ~1d | Server-mode setup that registers the binary path AND enables `systemctl --user start mcp-local-hub-*.service` on boot via `loginctl enable-linger <user>`. Skip the GUI auto-launch + tray + browser config emitted by desktop setup. |
| F4 | **Headless guards in GUI subsystem** | ~half-day | `mcphub gui` on Linux server should: skip `LaunchBrowser` if `$DISPLAY`/`$WAYLAND_DISPLAY` is unset, skip tray spawn (already no-op via `tray_other.go`), still bind HTTP server on `127.0.0.1` so admin can SSH-tunnel and use the dashboard remotely. |
| F5 | **journald logging adapter** | ~1d | Optional: when running under systemd, write structured logs to journald (via `systemd-cat` pipe or `sd_journal_send`-style in pure Go) instead of file-rotated logs. Not a blocker; the file-based logs work fine on a server. |
| F6 | **macOS process probe (libproc / sysctl)** | ~half-day | Currently `probe_darwin.go` returns "macOS not supported" for `--force --kill`. Implement via `libproc.proc_pidpath` (image), `sysctl(KERN_PROCARGS2)` (cmdline), and `sysctl(KERN_PROC_PID).kp_proc.p_starttime` (start-time). Earned during F1 lane refactor — needed on any Mac contributor / dev machine. Tracked as iter-2 P2 #3 follow-up from PR #23. |
| F7 | **CI Linux build matrix** | ~1d | Add `GOOS=linux` build + `go test ./...` on `ubuntu-latest` to GitHub Actions, alongside existing `windows-latest` E2E job. Catches Linux regressions early; F1-F4 enable green CI status on Linux. |

**Explicitly OUT of scope for F (Linux desktop, not server):**

- Linux tray icon (DBus StatusNotifierItem + AppIndicator + GNOME-without-tray edge case + Wayland constraints — substantial sub-project, no value on a server)
- Linux browser focus (X11 `xdotool`, Wayland blocks foreground steal entirely — no value on a server)
- macOS desktop tray (NSStatusItem via CGo to Cocoa — no value on a server)

These remain documented as "Cross-platform tray (Linux/macOS) — explicit non-goal per spec §2.2" further down.

**Sequencing within F:** F1 → F7 (so future work has CI safety net) → F2/F3 in either order → F4/F5/F6 as needed.

---

## Suggested sequencing

1. **D0** — Frontend toolchain migration → Vite + TS + React (must come first; a later migration forces rewriting every new screen)
2. **D1** — Playwright E2E suite (foundational for everything downstream; unlocks regression-safe iteration on A1–A5)
3. **B1** — Reverse-migrate API (unblocks proper uncheck semantics in Servers matrix; small backend change)
4. **B2** — ExtractManifestFromClient API (unblocks A1 "Create manifest" action)
5. **A1** — Migration screen (primary deferred UX; depends on B2)
6. **A2** — Add/Edit manifest form (largest UI surface; depends on B2 for prefill)
7. **A3-a** — Secrets registry screen ✅ — see [docs/superpowers/plans/2026-04-25-phase-3b-ii-a3a-secrets-screen.md](2026-04-25-phase-3b-ii-a3a-secrets-screen.md). Memo: [docs/superpowers/specs/2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md](../specs/2026-04-25-phase-3b-ii-a3a-secrets-screen-design.md). PR pending user review.
8. **A3-b** — env.secret picker in AddServer/EditServer forms ✅ — see [docs/superpowers/plans/2026-04-26-phase-3b-ii-a3b-env-secret-picker.md](2026-04-26-phase-3b-ii-a3b-env-secret-picker.md). Memo: [docs/superpowers/specs/2026-04-26-phase-3b-ii-a3b-env-secret-picker-design.md](../specs/2026-04-26-phase-3b-ii-a3b-env-secret-picker-design.md).
9. **A4-a** — Settings screen ✅ — see [docs/superpowers/plans/2026-04-27-phase-3b-ii-a4-settings.md](2026-04-27-phase-3b-ii-a4-settings.md). Memo: [docs/superpowers/specs/2026-04-27-phase-3b-ii-a4-settings-design.md](../specs/2026-04-27-phase-3b-ii-a4-settings-design.md). Merge SHA: `2529c33d` (PR #20, 14 Codex bot review rounds).
9b. **A4-b** — Settings lifecycle: tray, port live-rebind, weekly schedule edit, retry policy, Clean now confirm, export bundle, **per-workspace weekly-refresh membership UI**.
   - **Forward-ref to PR #23 C1:** A4-b's "Recover stuck instance" Settings UI button posts to a new `POST /api/force-kill` HTTP handler that returns the `gui.Verdict` JSON contract from PR #23 (`internal/gui/single_instance.go::Verdict`). Diagnose/Hint are not on the wire (`json:"-"`); UI formats from the structured fields.
   - **Per-workspace weekly-refresh membership (decided 2026-05-01):** today `WorkspaceEntry.WeeklyRefresh` is hardcoded `true` at `mcphub register` time and never editable. A4-b PR #1 flips the registration default to `false` (opt-in) and adds a `WeeklyMembershipTable` to `SectionDaemons` listing every `(workspaceKey, lang)` tuple with a checkbox bound to the registry's current value. **No migration:** existing entries keep whatever `WeeklyRefresh` value they carry today (`true` or `false`); the table renders that state and the user toggles it manually. Backend: `PUT /api/daemons/weekly-refresh-membership` with body `[{workspaceKey, lang, enabled}, ...]` applied atomically to `workspaces.yaml`; idempotent partial update (entries not in the body are unchanged). Frontend: the table participates in the same `useSectionSaveFlow` as `daemons.weekly_schedule` + `daemons.retry_policy`, so one Save button writes cron + retry + membership in one transaction. `Select all` / `Clear all` affordances above the table. Pagination/filter: YAGNI for typical single-digit workspace×language counts.
10. **A5** — About screen ✅ PR #22 (cleanup + reliability harness + A5 + C2 + C3 + C4 + D2/D3 docs).
11. **C3 + C4** — Tray icon state variants + toast notifications ✅ PR #22.
12. **C1** — `--force` take-over (single-instance lock recovery) — **PR #23 (next).** C2 browser focus closed in PR #22.
13. **A4-b** — Settings lifecycle (tray toggle, weekly schedule edit, retry policy, port live-rebind, Clean-now confirm, export bundle, per-workspace weekly-refresh membership) — **PR #24 (last).**
14. **Release hardening** — execute `docs/phase-3b-ii-verification.md` D2 + D3 manual smoke on a real Windows desktop session before tagging.
15. **F1** — Platform-lane refactor (move Windows-specific impl'ы into `internal/platform/windows/`; pre-req for any Linux/macOS work). See § F.
16. **F7** — CI Linux build matrix (catches regressions before F2-F6 land). See § F.
17. **F2 + F3** — Linux scheduler (systemd user units) + `mcphub setup --server`. Unlocks `mcphub install` on a Linux server. See § F.
18. **F4 + F5 + F6** — Headless GUI guards, journald adapter, macOS probe. Polish + Mac dev machine support. See § F.

### Daemon-management hygiene follow-ups (post-A4-a, separate sprint)

Surfaced during A4-a local smoke (2026-04-28) and confirmed via Codex consult. Independent of A4-a; deferred to a dedicated sprint.

**Status:** DM-1 + DM-2 + DM-3 implemented and merged. PR #21, merge SHA `e01e9113` (squash, 2026-04-28).

- **DM-1: Status `Starting` forever when manifest missing.** ✅ Fixed in `internal/api/status_enrich.go` — when `Port=0 && !IsMaintenance`, `deriveState` is bypassed so the raw scheduler state (`Running`, `Ready`, …) survives instead of being mis-rendered as `Starting`. Maintenance rows (`weekly-refresh`) keep going through `deriveState` so `Ready+future trigger → Scheduled` and `Ready+no trigger → Stopped` still work. Codex r1 P2 narrowed the original guard from "all Port=0 rows" to "non-maintenance Port=0 rows."
- **DM-2: Self-PID false-positive `Running`.** ✅ Fixed in `internal/api/status_enrich.go` — `selfPIDFn` test seam returns `os.Getpid()` in production and a stub in tests; rows whose detected PID matches `selfPID` skip the alive/PID/RAM/Uptime population so the GUI's own listener can no longer masquerade as a daemon. Long-term: enforce disjoint port ranges for GUI vs daemon manifests (open).
- **DM-3a: Lost spawn diagnostics.** ✅ Fixed in `internal/cli/daemon.go` and `internal/cli/daemon_workspace.go` — both cobra `RunE` paths install a `defer` that calls `writeLaunchFailure(logPath, server, daemon, err)` when `err != nil`, appending `[mcphub-launch-failure <RFC3339-UTC> server=<s> daemon=<d>] <err>` to the per-daemon log file. That makes the cause of `last_result=1` discoverable instead of a Task Scheduler black hole. Codex r1 P2 (workspace mirror) moved the defer above `CanonicalWorkspacePath` so stale-workspace registrations also get diagnosed; `logPath` starts as `lazy-proxy-<lang>-pre.log` and is refined to `lsp-<wsKey>-<lang>.log` after canonicalization succeeds.
- **DM-3b: Restart race.** ✅ Fixed in `internal/api/install.go` — `Restart` and `RestartAll` now call `waitForPortFree(port, 5s)` between the stop and the `schtasks /Run` so the kernel's TIME_WAIT window can drain before the daemon tries to rebind. Without this, the second `bind` would race the first connection's TIME_WAIT and fail with `bind: address already in use`, leaving `last_result=1`.

**Out of scope of PR #21:**

- ~~Add `servers/gdb/manifest.yaml` to the repo.~~ **Withdrawn (2026-04-28):** gdb was intentionally retired in PR #13 — see `retiredServerNames` in `internal/api/install.go:707` (manifest-less uninstall fallback for stale state) AND `perSessionServers` in `internal/api/scan.go:34` (gdb is a debugger, always per-session, not hub-managed). Restoring the manifest contradicts both contracts. DM-1 narrowing is the correct production fix; users with stale Task Scheduler entries clear them via `mcphub uninstall gdb`. **Reversed (2026-04-30):** gdb manifest IS now in the repo at `servers/gdb/manifest.yaml` — re-added as part of PR #24 because the GDB-MCP project has built-in session management (`modules/gdb/sessionManager.py` + `modules/lldb/sessionManager.py`) where each client call carries a `session_id`, so one daemon serves N concurrent debug sessions on the hub. That breaks the original "always per-session" assumption that motivated the withdrawal. The hub-managed daemon model fits this server. Restoration ships gdb at port 9129 with session management server-side.
- ~~`TestInstallAllInstallsEverything` flake (hardcoded 9130/9131).~~ **Closed (PR #22 commit 1):** test now uses `pickFreeLocalPort` so it survives TIME_WAIT residue and parallel daemon ownership.
- Enforce disjoint port ranges for GUI vs daemon manifests (DM-2 root cause — **open**). Today the GUI's `--port` default is `9125` and the wolfram manifest also declares 9125; the self-PID skip turns the symptom into a silent no-op but the collision is still bad operator UX (e.g. `mcphub status` reports the daemon as `Stopped` when the user's actual hub server IS bound to that port).

### Cross-platform follow-ups

- **macOS `--force --kill` probe (libproc / sysctl-based identity).** PR #23 ships a Linux+Windows identity probe; `probe_darwin.go` is a stub that surfaces "not supported on macOS" via Verdict.Diagnose. Implement the same three-part identity gate on darwin via `libproc.proc_pidpath` (image), `sysctl(KERN_PROCARGS2)` (cmdline), and `sysctl(KERN_PROC_PID).kp_proc.p_starttime` (start-time). Tracks the iter-2 review's P2 #3 follow-up.

### Long-runtime stability follow-ups

- **Tray menu hangs after long uptime / state-event flood.** Reported 2026-04-30 (post PR #22): after ~hours of runtime with the daemon-status poller pushing state every 5s, the systray right-click menu stops appearing entirely (the icon still shows the correct partial/healthy/error color via `SetIcon`). Restart of `mcphub gui` clears it. Root-cause hypothesis: `getlantern/systray`'s message-pump goroutine starves under continuous `SetIcon` + `SetTooltip` traffic, OR the goroutine reading `cfg.StateCh` blocks on a Win32 callback inside SetIcon and starves `mOpen.ClickedCh`/`mQuit.ClickedCh` consumption.
  - Likely fixes (pick after profiling):
    1. Throttle: only call `SetIcon`/`SetTooltip` when `state` actually changed (track previous value in the goroutine).
    2. Decouple: state updates drain `cfg.StateCh` on a separate goroutine and post via a sync.Mutex-protected last-state field; the click-loop only reads it.
    3. Audit `getlantern/systray` versions for upstream fixes; consider switching to a different tray library if the message-pump issue is structural.
  - Acceptance: tray menu remains responsive after 24h+ continuous uptime with state churn (drive via a synthetic test that flips fake daemon states ~once/sec).
  - Cross-cuts: should ship before A4-b's "Recover stuck instance" Settings UI button so users have a working escape hatch from the tray.

**Estimated scope:** ~35-45 implementation tasks. D0 adds ~9-10 migration tasks; Playwright adds ~5-8 test-authoring tasks on top of UI tasks; budget accordingly.

**Not included here** (out of scope for 3B-II entirely):
- Cross-platform tray (Linux/macOS) — explicit non-goal per spec §2.2
- Multi-user / remote access — explicit non-goal per spec §2.2
- Real-time log search across daemons — explicit non-goal per spec §2.2
- JSON Schema inline validation in manifest form — explicit non-goal per spec §2.2 (save-time validation via `api.ManifestValidate` is sufficient)

### Closed by PR #24 (tray rewrite, 2026-04-30)

✅ **Tray subprocess + direct-Win32**: PR #24 spawns the tray as a separate `mcphub tray` child process and implements it via direct `golang.org/x/sys/windows` syscalls (no CGo, no third-party tray library). Click handler uses `Shell_NotifyIconGetRect` for deterministic icon-anchored popup placement; `NIM_SETVERSION(4)` + `MonitorFromPoint`/`GetMonitorInfoW` for multi-monitor-correct alignment; `TaskbarCreated` re-add survives explorer restart; `SetProcessDpiAwarenessContext` aligns coord spaces on scaled monitors. Supersedes any earlier `getlantern/systray` → `fyne.io/systray` → `energye/systray` migration plans — direct-Win32 is the chosen end state.
