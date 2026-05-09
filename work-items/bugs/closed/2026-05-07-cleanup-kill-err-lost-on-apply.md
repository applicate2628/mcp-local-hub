---
title: cleanup apply drops per-row kill_err on UI render
severity: medium
found-by: qa-engineer
found-in-phase: PR #131 cleanup-buttons xhigh review (commit 4e46eba)
affected-surface: internal/gui/frontend/src/components/settings/SectionMaintenance.tsx
context: standalone (PR #131 follow-up; not blocking merge per QA verdict)
status: closed
fixed-in: commit 1f59a65 (PR #131 follow-up landed 2026-05-07)
---

## Reproduction

Frontend code path `SectionMaintenance.tsx:84-97, 174-190`:

1. Click Preview on Orphan MCP / Orphan Log Watchers card.
2. Apply.
3. Observe banner reads "Done. Killed N, skipped M." with no per-row
   detail. The preview table also disappears.

`kill_err` values returned by the backend (e.g. `"skipped: PID reused
(identity mismatch)"` from `log_watchers.go:191`, `"skipped: process
exited between snapshot and kill"` from `log_watchers.go:185`,
or shell-out errors from `taskkill`/`os.Process.Kill`) never reach the
operator's eyes.

## Expected vs actual

**Expected:** the apply-mode result table surfaces per-row `kill_err`
when present, so operators can distinguish revalidation skips, access
denials, and lifecycle race skips. The PID-reuse revalidation feature
(kosyak #11) is otherwise invisible in production UI — only the unit
test at `internal/api/log_watchers_test.go:133` proves it works.

**Actual:** apply state replaces preview state, dropping the per-row
list. Even if it didn't, `OrphansTable` (line 142) and `WatchersTable`
(line 259) lack a `kill_err` column.

## Files involved

- `internal/gui/frontend/src/components/settings/SectionMaintenance.tsx:84-97`
  (CardOrphanMcpServers.apply)
- `internal/gui/frontend/src/components/settings/SectionMaintenance.tsx:174-190`
  (CardOrphanLogWatchers.apply)
- `internal/gui/frontend/src/components/settings/SectionMaintenance.tsx:130-152`
  (OrphansTable — no kill_err column)
- `internal/gui/frontend/src/components/settings/SectionMaintenance.tsx:247-271`
  (WatchersTable — no kill_err column)
- Backend producers: `internal/api/log_watchers.go:185,191,196`;
  `internal/api/cleanup.go:159`.

## Suggested fix

After apply, retain the row list with `kill_err` populated and render a
post-apply table (or annotate existing preview table). Add a `kill_err`
column displayed only when any row has a non-empty value.

## Resolution

**Status:** closed
**Date:** 2026-05-07
**Commit:** `1f59a65` — `fix(gui): Codex bot P2 on 72757c6 — surface per-row kill_err in apply result tables (both cleanup cards)`

The fix landed as a Codex Cloud bot P2 follow-up on PR #131 commit
`72757c6` (escalated from QA F1):

- Extended `ActionState["applied"]` with optional `orphans` and
  `watchers` row lists so post-kill data survives the state transition.
- Both apply paths (`cleanupOrphans`, `cleanupLogWatchers`) now retain
  the returned row list in the new state.
- `OrphansTable` + `WatchersTable` render in BOTH preview AND applied
  states (no longer gated on `state.kind === "preview"`).
- Conditional Result column appears only when at least one row has a
  non-empty `kill_err`: shows the kill_err message for skipped rows
  and "killed" for successful kills, with `.maintenance-error`
  highlighting on the failure cells.

Test coverage in `internal/gui/frontend/src/components/settings/SectionMaintenance.test.tsx`:

- kill_err visibility on apply for both cards
- no Result column on all-clean apply
- OS-friendly 501 error rendering
- disabled Clean(0) tooltip on log-watchers
- HTTP 207 partial-failure banner + per-daemon error column on Stop-All
- empty-result Done banner

Verified live in current `internal/gui/frontend/src/components/settings/SectionMaintenance.tsx`:

- `OrphansTable` lines ~247-291 — `showResult = orphans.some((o) => !!o.kill_err)`
- Apply path line ~166 — `setState({ kind: "applied", ..., orphans: r.orphans })`
- Render condition line ~205 — `(state.kind === "preview" || state.kind === "applied")`
