---
title: cleanup apply drops per-row kill_err on UI render
severity: medium
found-by: qa-engineer
found-in-phase: PR #131 cleanup-buttons xhigh review (commit 4e46eba)
affected-surface: internal/gui/frontend/src/components/settings/SectionMaintenance.tsx
context: standalone (PR #131 follow-up; not blocking merge per QA verdict)
status: open
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
