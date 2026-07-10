# Work items index

> Canonical open-bug truth: **`bugs/TRIAGE-2026-07-09.md`** (HEAD-reconciled against master
> `63b6a008`, amended for #525 at master `7898c148`, then amended for #526 at master
> `b5e6f6f3`). Individual bug-doc frontmatter used to lag reality; the top-level bug registry
> has now been normalized. Trust the TRIAGE + the "Truly open" list below, NOT a raw count of
> files in `bugs/` (33 files = 0 open + 28 fixed + 1 duplicate + 1 wontfix + 3 triage records —
> the last open bug was disposed open→fixed on 2026-07-10; see TRIAGE Amendment 3).

## Active (work-items/active/)

| Item | State | Remaining |
|---|---|---|
| [2026-07-05-adopt-npx-orphans](active/2026-07-05-adopt-npx-orphans/status.md) | PARTIALLY DELIVERED | adopt CLI/API + GUI + **reaper (all 3 kill-authority hardenings)** SHIPPED + DEPLOYED (#513/#520/#521/#522); anti-drift "unmanaged detected" GUI signal **LANDED** (#523, master `f7eaa1c8`). Remaining = phase-2 de-adopt (now the separate `2026-07-09-deadopt-hub-to-native` item, blocked) + D P2a/P2b GUI. |
| [2026-07-09-deadopt-hub-to-native](active/2026-07-09-deadopt-hub-to-native/status.md) | REVISE / BLOCKED | Blocked on adopt-side durable pre-adopt provenance (`active/2026-07-09-adopt-side-durable-pre-adopt-provenance/`, research done, untracked); implementation must not start until the design is revised and the prerequisite is delivered. |

## Reaper kill-authority hardening — COMPLETE (2026-07-08, all bot-PASS + deployed)

| Fix | PR | master | Status |
|---|---|---|---|
| A2 PR5 auto-reaper (config-absence gate + snapshot fail-closed + T3 source-fix + reap_verdict presentation + identity-bind + specificity) | #520 | c53d874a | deployed + live-verified |
| Walk-uncertainty P0 (3-state fail-closed walk: tri-state QueryPIDState + self-loop + depth-cap) | #521 | 509afa31 | deployed + live-verified |
| Aggressive-token identity binding (token+started_at + {pid,started_at} kill-bind both surfaces) | #522 | 669951e3 | deployed + live-verified |

## Truly-open bugs (everything else in bugs/ is fixed/closed — status just not flipped)

**None — truly-open = 0.** All 30 top-level `bugs/*.md` docs are fixed/closed at master
`66b80ece`. The last open bug,
[2026-07-07-lsp-router-relay-entries-ignore-per-client-disable](bugs/2026-07-07-lsp-router-relay-entries-ignore-per-client-disable.md),
was disposed open→fixed by `$lead` on 2026-07-10 (backend #512 `c3fc1801` + GUI Servers-tab
per-client disable #524 `22c91cab`; see `bugs/TRIAGE-2026-07-09.md` Amendment 3).

Three triage batch files (`bugs/TRIAGE-2026-05-28.md`, `bugs/TRIAGE-2026-07-08.md`,
`bugs/TRIAGE-2026-07-09.md`) are
reconciliation records, not open tasks.

## Epics

| Epic | Status | Closed | Outcome |
|---|---|---|---|
| [2026-06-23-desktop-app-mcp-catalog](epics/2026-06-23-desktop-app-mcp-catalog.md) | closed | 2026-06-29 | DONE — desktop/creative-app MCP catalog, 52 rows, shipped npm v0.4.9. |
| [2026-06-19-install-and-it-works-ux](epics/2026-06-19-install-and-it-works-ux.md) | closed | 2026-06-28 | DONE — clean-install + hub-launch + per-project UX (#377/#407/#408/#437/#428–#435/#439). |

## Reference / parked (loose root files — NOT open tasks)

- `2026-06-17-phase1-audit-findings.md` — Phase 1 audit source-of-truth (historical reference).
- `2026-06-18-clean-install-ux-vision.md` — PARKED user vision (post-dev-debug future).
- `2026-06-18-vendor-init-uninstalled-clients.md` — RESOLVED 2026-06-21 (secure-parent-create stack).
- `roadmap-scan-2026-06-14.md` — gitignored local raw-inventory scan the `ROADMAP.md` audit
  header cites; superseded by `ROADMAP.md` for all live tracking (not an open task).

## Archived (work-items/archive/<YYYY-MM>/)

| Item | Closed | Outcome |
|---|---|---|
| [2026-07-09-test-leftover-reaper](archive/2026-07/2026-07-09-test-leftover-reaper/closure.md) | 2026-07-10 | DELIVERED — v1 preview/diagnostics-only `mcphub cleanup test-leftovers`; #527 squash `436e4f58`, bot PASS (2 rounds). Non-destructive: enumerate + classify + hypothetical-refusal labels, no process termination. Destructive apply deferred to v2 (blocked on round-3 P2/P3 + value case); standalone-`supervise` stays manual-reap-only. |
| [2026-07-09-lsp-relay-per-client-disable-gui](archive/2026-07/2026-07-09-lsp-relay-per-client-disable-gui/closure.md) | 2026-07-09 | DELIVERED — per-client LSP-router enable/disable in the Servers matrix; #524 squash `22c91cab`, deployed + live-verified. |
| [2026-07-09-intent-collapse-stop-resurrection](archive/2026-07/2026-07-09-intent-collapse-stop-resurrection/closure.md) | 2026-07-09 | DELIVERED — absent-only legacy stop watermarks; #525 squash `5d8ab063`, deployed `7898c148`. |
| [2026-07-05-unify-port-resolution-owner](archive/2026-07/2026-07-05-unify-port-resolution-owner/closure.md) | 2026-07-05 | DELIVERED — daemon port+identity single-owned; #505, deployed. |
| [2026-06-15-dynamic-mcp-discovery](archive/2026-06/2026-06-15-dynamic-mcp-discovery/closure.md) | 2026-06-16 | DELIVERED — Discovery view + demigrate + marketplace mark-installed. |
| [2026-06-15-workspace-daemon-prune](archive/2026-06/2026-06-15-workspace-daemon-prune/closure.md) | 2026-06-16 | DELIVERED — auto-prune Phase 1 + idle + GUI toggle. |
| [2026-06-15-settings-redesign](archive/2026-06/2026-06-15-settings-redesign/closure.md) | 2026-06-15 | DELIVERED — Settings card redesign + tooltips + UX-audit fixes. |
| [2026-06-11-pr288-cumulative-review](archive/2026-06/2026-06-11-pr288-cumulative-review/closure.md) | 2026-06-14 | CLOSED — review-only; findings landed via #300–#303. |
