# Phase 3B-II Backlog

Tracking document for Phase 3B-II — the "everything I cut from Phase 3B-I MVP" scope. NOT an implementation plan yet. A plan (via `superpowers:writing-plans`) is written when Phase 3B-II execution starts.

**Source documents:**

- [docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md](../specs/2026-04-17-phase-3-gui-installer-design.md) — original GUI design spec (full scope before MVP split)
- [docs/phase-3b-verification.md](../../phase-3b-verification.md) — Phase 3B-I MVP closeout, "Deferred to Phase 3B-II" section
- [docs/phase-3b-pr5-codex-walkthrough.md](../../phase-3b-pr5-codex-walkthrough.md) — PR #5 review walkthrough with R17 / final-review deferrals

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
| C1 | `--force` take-over flag | Currently hidden placeholder. Realize: detect stale single-instance mutex, confirm with user, delete `<pidport>.lock`, acquire. | PR #5 cleanup |
| C2 | Browser focus on activate-window | ✅ PR #22 — `gui.FocusBrowserWindow` enumerates visible top-level windows by title substring "mcp-local-hub", calls SW_RESTORE then SetForegroundWindow. Activate-window callback in cli/gui.go now invokes it instead of logging. Manual real-match smoke in `docs/phase-3b-ii-verification.md` D2.1. | PR #5 final review |
| C3 | Tray icon state variants | MVP ships a single icon (`SetTooltip` only). Add 4-state icon switching (`healthy` / `degraded` / `down` / `migrating`) driven by SSE `daemon-state` events. | Spec §6 |
| C4 | Toast notifications | Windows toast on `daemon-failed` / `auto-restart-triggered` / `manual-action-done`. Events already in `/api/events` stream, UI does not render them. | Spec §6 |

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
9b. **A4-b** — Settings lifecycle: tray, port live-rebind, weekly schedule edit, retry policy, Clean now confirm, export bundle.
10. **A5** — About screen
11. **C3 + C4** — Tray icon state variants + toast notifications (polish after SSE event handling is mature)
12. **C1 + C2** — `--force` take-over + browser focus (CLI/UX polish, Windows-specific wiring)
13. **Release hardening** — D2 + D3 manual smoke matrix, write `docs/phase-3b-ii-verification.md`

### Daemon-management hygiene follow-ups (post-A4-a, separate sprint)

Surfaced during A4-a local smoke (2026-04-28) and confirmed via Codex consult. Independent of A4-a; deferred to a dedicated sprint.

**Status:** DM-1 + DM-2 + DM-3 implemented and merged. PR #21, merge SHA `e01e9113` (squash, 2026-04-28).

- **DM-1: Status `Starting` forever when manifest missing.** ✅ Fixed in `internal/api/status_enrich.go` — when `Port=0 && !IsMaintenance`, `deriveState` is bypassed so the raw scheduler state (`Running`, `Ready`, …) survives instead of being mis-rendered as `Starting`. Maintenance rows (`weekly-refresh`) keep going through `deriveState` so `Ready+future trigger → Scheduled` and `Ready+no trigger → Stopped` still work. Codex r1 P2 narrowed the original guard from "all Port=0 rows" to "non-maintenance Port=0 rows."
- **DM-2: Self-PID false-positive `Running`.** ✅ Fixed in `internal/api/status_enrich.go` — `selfPIDFn` test seam returns `os.Getpid()` in production and a stub in tests; rows whose detected PID matches `selfPID` skip the alive/PID/RAM/Uptime population so the GUI's own listener can no longer masquerade as a daemon. Long-term: enforce disjoint port ranges for GUI vs daemon manifests (open).
- **DM-3a: Lost spawn diagnostics.** ✅ Fixed in `internal/cli/daemon.go` and `internal/cli/daemon_workspace.go` — both cobra `RunE` paths install a `defer` that calls `writeLaunchFailure(logPath, server, daemon, err)` when `err != nil`, appending `[mcphub-launch-failure <RFC3339-UTC> server=<s> daemon=<d>] <err>` to the per-daemon log file. That makes the cause of `last_result=1` discoverable instead of a Task Scheduler black hole. Codex r1 P2 (workspace mirror) moved the defer above `CanonicalWorkspacePath` so stale-workspace registrations also get diagnosed; `logPath` starts as `lazy-proxy-<lang>-pre.log` and is refined to `lsp-<wsKey>-<lang>.log` after canonicalization succeeds.
- **DM-3b: Restart race.** ✅ Fixed in `internal/api/install.go` — `Restart` and `RestartAll` now call `waitForPortFree(port, 5s)` between the stop and the `schtasks /Run` so the kernel's TIME_WAIT window can drain before the daemon tries to rebind. Without this, the second `bind` would race the first connection's TIME_WAIT and fail with `bind: address already in use`, leaving `last_result=1`.

**Out of scope of PR #21:**

- ~~Add `servers/gdb/manifest.yaml` to the repo.~~ **Withdrawn (2026-04-28):** gdb was intentionally retired in PR #13 — see `retiredServerNames` in `internal/api/install.go:707` (manifest-less uninstall fallback for stale state) AND `perSessionServers` in `internal/api/scan.go:34` (gdb is a debugger, always per-session, not hub-managed). Restoring the manifest contradicts both contracts. DM-1 narrowing is the correct production fix; users with stale Task Scheduler entries clear them via `mcphub uninstall gdb`.
- ~~`TestInstallAllInstallsEverything` flake (hardcoded 9130/9131).~~ **Closed (PR #22 commit 1):** test now uses `pickFreeLocalPort` so it survives TIME_WAIT residue and parallel daemon ownership.
- Enforce disjoint port ranges for GUI vs daemon manifests (DM-2 root cause — **open**). Today the GUI's `--port` default is `9125` and the wolfram manifest also declares 9125; the self-PID skip turns the symptom into a silent no-op but the collision is still bad operator UX (e.g. `mcphub status` reports the daemon as `Stopped` when the user's actual hub server IS bound to that port).

**Estimated scope:** ~35-45 implementation tasks. D0 adds ~9-10 migration tasks; Playwright adds ~5-8 test-authoring tasks on top of UI tasks; budget accordingly.

**Not included here** (out of scope for 3B-II entirely):
- Cross-platform tray (Linux/macOS) — explicit non-goal per spec §2.2
- Multi-user / remote access — explicit non-goal per spec §2.2
- Real-time log search across daemons — explicit non-goal per spec §2.2
- JSON Schema inline validation in manifest form — explicit non-goal per spec §2.2 (save-time validation via `api.ManifestValidate` is sufficient)
