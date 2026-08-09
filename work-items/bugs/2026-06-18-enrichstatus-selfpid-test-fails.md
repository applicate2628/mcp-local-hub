---
title: TestEnrichStatusWithRegistry_SelfPIDIsNotAlive fails on master (self-PID skip not firing)
severity: medium
found-by: backend-engineer
found-in-phase: groups Phase 5b-1 (/api/groups CRUD endpoint) — full internal/api test sweep
affected-surface: internal/api/status_enrich.go (selfPID skip, lines ~154/184/193/205) consumed by internal/api/status_enrich_test.go:110
context: adjacent-finding
fixed: 2026-06-19
resolution: >
  Root cause CAPTURED at runtime (t.Logf probe): the fixture
  makeFakeManifest(dir/wolfram, "wolfram", 9125) is REJECTED by
  parseManifestForName — "daemons[0] declares port 9125, the reserved GUI
  listener port" — so manifestPortMap(dir) returns an EMPTY map, the
  wolfram row's Port never resolves (stays 0), enrichStatus hits the
  Port==0 early-continue (status_enrich.go:178) and SKIPS deriveState, so
  the raw "Running" leaks and the self-PID gate (:193) is never reached.
  STALE TEST, not a production bug: the self-PID skip is correct and a real
  global manifest can no longer declare 9125 at all. Fix: seed
  rows[0].Port=9125 directly (drop the now-rejected makeFakeManifest),
  faithfully exercising the DM-2 self-PID branch for a daemon whose resolved
  port equals the GUI listener (a collision that still arises via
  registry/dynamic ports). All 13 EnrichStatus tests PASS.
---

- **status:** fixed
- **fixed-by:** PR #21 (`e01e9113`) - self-PID skip test repaired at HEAD.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `triage-2026-07-09.md` for code/test evidence.

## Summary

`go test -tags=test_state_path_env ./internal/api/` fails on
`TestEnrichStatusWithRegistry_SelfPIDIsNotAlive` (status_enrich_test.go:154)
**on the clean foundation baseline (HEAD 8b9911b), independent of any
groups change.** Discovered while running the mandatory full-package sweep
for groups Phase 5b-1; it is NOT caused by that work.

## Proof it is pre-existing (not caused by groups Phase 5b-1)

The groups Phase 5b-1 diff touches only `internal/api/hub_mcp_groups.go`
(an additive exported `ValidateGroupName` wrapper) and `internal/gui/*`.
`git stash`-removing the tracked groups changes and re-running the test on
the bare foundation still fails:

```
git stash push -- internal/api/hub_mcp_groups.go internal/gui/server.go
go test -tags=test_state_path_env -count=1 \
  -run 'TestEnrichStatusWithRegistry_SelfPIDIsNotAlive$' ./internal/api/
--- FAIL: TestEnrichStatusWithRegistry_SelfPIDIsNotAlive
    status_enrich_test.go:154: State = "Running" after self-PID skip; must not be "Running" (alive should be false)
```

The sibling `TestEnrichStatusWithRegistry_ForeignPIDIsAlive` PASSES, so the
general enrich/registry path works; only the self-PID-skip assertion fails.

## Reproduction

```
cd internal/api
go test -tags=test_state_path_env -count=1 \
  -run 'TestEnrichStatusWithRegistry_SelfPIDIsNotAlive$' ./
```

Failure:

```
status_enrich_test.go:154: State = "Running" after self-PID skip;
    must not be "Running" (alive should be false)
```

## Expected vs actual

- Expected: with `lookupProcessBatch` stubbed to return `os.Getpid()` for
  the daemon's port, `enrichStatus` recognizes the self-PID (production
  `selfPIDFn = os.Getpid`) and does NOT mark the row alive → state is not
  "Running".
- Actual: the row is left "Running" — the self-PID skip branch did not fire
  on this (Windows) host.

## Suspected cause (unverified — needs the bugfix diagnostic gate)

The test calls `enrichStatus(rows, dir)` (the non-`WithRegistry` form) and
seeds the manifest with `dir+"/wolfram"` (forward-slash concatenation). On
Windows the registry-path resolution inside `enrichStatus` may not locate
the seeded manifest, so the row takes a different enrichment branch than
the self-PID-skip path the test asserts. Whether the defect is in the test
fixture (path join / wrong enrich entry point) or in `status_enrich.go`'s
self-PID gate must be settled by capturing the actual branch taken
(log the resolved registry path + whether `info.PID == selfPID`), per the
pre-fix diagnostic gate — do NOT patch on this theory.

## Impact

Pre-existing red test on master for the internal/api package. Does not
affect groups Phase 5b-1 (groups tests are green). Flagged so the
orchestrator can prioritize; left untouched to keep the groups diff scoped.

---

## SECOND adjacent finding (same sweep): internal/gui self-restart os.Exit kills the test binary

`go test -tags=test_state_path_env ./internal/gui/` aborts the WHOLE package
binary via `os.Exit(0)` from `internal/gui/gui_self_restart.go:153`
(`selfRestartExitFn = func() { os.Exit(0) }`), reached from
`TestGUISelfRestart_SpawnSuccess` (gui_self_restart_test.go:32). The handler
fires the real exit on a delayed goroutine
(`go func(){ time.Sleep(selfRestartExitDelay); selfRestartExitFn() }()`),
so if the test restores the `selfRestartExitFn` seam before that goroutine
runs (timing race), the REAL os.Exit fires and terminates the test binary —
masking every test scheduled after it.

PROVEN pre-existing: with the groups files moved aside AND the tracked
groups changes stashed, the bare foundation (HEAD 8b9911b) reproduces the
identical stack trace and `FAIL`. With this one test SKIPPED
(`-skip 'GuiSelfRestart|SelfRestart'`) the rest of internal/gui (including
all groups Phase 5b-1 tests) is GREEN (`ok ... 17.9s`).

Suspected cause (unverified — needs the diagnostic gate): the delayed-exit
goroutine outlives the test's seam-restore window, so the production
`os.Exit` seam is reinstated and then invoked. A fix likely waits for /
cancels the delayed goroutine before restoring the seam, or makes the exit
seam swap+goroutine-join atomic within the test. Do NOT patch on this theory
without capturing the actual ordering. Out of scope for groups Phase 5b-1;
flagged for the orchestrator.

Resolution update (2026-06-20): this adjacent finding was fixed while
verifying PR #385 r4 because the required
`go test -tags=test_state_path_env -race -run 'HubListener|HubRestart|Restart'
./internal/gui/ ./internal/api/` command selects `TestGUISelfRestart_*` via the
`Restart` regex. The captured stack showed the delayed goroutine calling
`selfRestartExitFn` after test cleanup restored the production `os.Exit` seam
(`internal/gui/gui_self_restart.go:142-153`,
`internal/gui/gui_self_restart_test.go:18-25`). The test now waits for the
delayed exit seam in the spawn-success case before cleanup restores production
hooks; `GuiSelfRestart|SelfRestart` and the full requested race subset pass.
