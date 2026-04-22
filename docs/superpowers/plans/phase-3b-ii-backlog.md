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
| C2 | Browser focus on activate-window | Currently logs only. Wire `SetForegroundWindow` (Windows) on the Chrome app-mode window. Tray "Open dashboard" shares the limitation. | PR #5 final review |
| C3 | Tray icon state variants | MVP ships a single icon (`SetTooltip` only). Add 4-state icon switching (`healthy` / `degraded` / `down` / `migrating`) driven by SSE `daemon-state` events. | Spec §6 |
| C4 | Toast notifications | Windows toast on `daemon-failed` / `auto-restart-triggered` / `manual-action-done`. Events already in `/api/events` stream, UI does not render them. | Spec §6 |

### D. Testing infrastructure

| # | Item | Description |
|---|---|---|
| **D1** | **Playwright E2E suite** | Automate the "live browser smoke" gap that PR #5 left deferred. Covers: Servers matrix render + toggle, Dashboard live SSE updates, Logs picker + tail follow, Migration grouping (post-A1), AddServer form (post-A2). Runs against `go run ./cmd/mcphub gui --no-browser --no-tray --port 0` spawned by test fixture. Node.js + `@playwright/test` + headless Chromium. ~300-500 lines of test code. CI job separate from `go test`. **Recommended as the FIRST task of Phase 3B-II** — amortizes toolchain investment across all subsequent screens, provides regression safety before Migration/AddServer/Secrets/Settings land. |
| D2 | Live manual smoke | Remaining gaps Playwright does NOT cover: tray icon rendering, AttachConsole + windowsgui subsystem matrix (cmd/PowerShell/Git Bash/Scheduler/Explorer × status/install/gui), single-instance recovery through OS reboot, real daemon kill via Task Manager. Requires Windows desktop. |
| D3 | Multi-language workspace smoke (Phase 3 follow-up) | `mcphub register D:\dev\proj cpp python rust` — concurrent materialization of three real LSP backends (clangd, pyright-langserver, rust-analyzer). Currently unit-tested with fakes; no live multi-language verification. Belongs with D2 on the same Windows desktop session. |

### E. Spec success criteria deferred

| Criterion | Description | Depends on |
|---|---|---|
| §2.1 #3 | Create manifest from unknown stdio — "I have cursor-mcp-fetch configured as stdio, wrap it into a hub daemon" | A1 + A2 + B2 |
| §2.1 #7 | Backup management UX — list + restore + delete timestamped backups, keep-N enforcement UI | A4 Settings screen |

---

## Suggested sequencing

1. **D1** — Playwright E2E suite (foundational for everything downstream; unlocks regression-safe iteration on A1–A5)
2. **B1** — Reverse-migrate API (unblocks proper uncheck semantics in Servers matrix; small backend change)
3. **B2** — ExtractManifestFromClient API (unblocks A1 "Create manifest" action)
4. **A1** — Migration screen (primary deferred UX; depends on B2)
5. **A2** — Add/Edit manifest form (largest UI surface; depends on B2 for prefill)
6. **A3** — Secrets screen
7. **A4** — Settings screen
8. **A5** — About screen
9. **C3 + C4** — Tray icon state variants + toast notifications (polish after SSE event handling is mature)
10. **C1 + C2** — `--force` take-over + browser focus (CLI/UX polish, Windows-specific wiring)
11. **Release hardening** — D2 + D3 manual smoke matrix, write `docs/phase-3b-ii-verification.md`

**Estimated scope:** ~30-40 implementation tasks, similar shape to Phase 3B-I MVP (22 tasks). Playwright adds ~5-8 test-authoring tasks on top of UI tasks; budget accordingly.

**Not included here** (out of scope for 3B-II entirely):
- Cross-platform tray (Linux/macOS) — explicit non-goal per spec §2.2
- Multi-user / remote access — explicit non-goal per spec §2.2
- Real-time log search across daemons — explicit non-goal per spec §2.2
- JSON Schema inline validation in manifest form — explicit non-goal per spec §2.2 (save-time validation via `api.ManifestValidate` is sufficient)
