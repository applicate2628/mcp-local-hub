---
title: installPlanCore superviseGlobal path self-deadlocks on the supervisor-intent flock
severity: high
found-by: Phase 4-F done-gate test (first exercise of the global supervisor-intent install path)
found-on: 2026-06-11
project: mcp-local-hub
context: adjacent-finding
status: closed
closed-on: 2026-06-14
related-pr: (Phase 4-F worktree — fix/serena-supervisor-robustness, branch worktree-agent-afb253946147c19eb)
---

# installPlanCore (global install) self-deadlocks holding the supervisor-intent flock

## Symptom

A fresh `mcphub install` of any GLOBAL manifest with daemons (memory, time,
wolfram, …) hangs forever. The first Phase 4-F done-gate test
(`TestInstallPlanCore_GlobalFreshInstall_WritesSupervisorIntent_NoSchedulerTask`)
hit the Go test 600s deadline. Captured goroutine stack (verbatim, the
load-bearing diagnostic):

```text
github.com/gofrs/flock.(*Flock).Lock(...)
mcp-local-hub/internal/api.mutateStopSubBlock(...) internal/api/stop_intent_subblock.go:164
mcp-local-hub/internal/api.(*API).WriteStopIntent(...) internal/api/stop_intent_subblock.go:86
mcp-local-hub/internal/api.(*API).recordInstallIntentPostSuccess(...) internal/api/install_intent.go:433
mcp-local-hub/internal/api.(*API).installPlanCore(...) internal/api/install_parsed_manifest.go:589
```

## Root cause

`installPlanCore`'s `superviseGlobal` branch acquired the supervisor-intent
flock (`flock.New(intentPath + supervisorIntentLockSuffix).Lock()`) with a
`defer lock.Unlock()`. Because `defer` releases at FUNCTION return, the lock
stayed held through the fall-through to `recordInstallIntentPostSuccess` at the
end of `installPlanCore`. That call chains
`recordInstallIntentPostSuccess → WriteStopIntent → mutateStopSubBlock`, which
re-acquires the SAME `supervisor-intent.json.lock` leaf with a BLOCKING
`Lock()`. `gofrs/flock` on Windows `LockFileEx` is non-reentrant, so the
process blocks on a lock it already owns — a self-deadlock.

The workspace-scoped sister path (`InstallParsedManifest`) does NOT hit this:
it returns immediately after `installPlan` and never calls
`recordInstallIntentPostSuccess` inside the locked scope. The deadlock was
introduced when `installPlanCore` was added (Phase 4-F) and placed the
post-success stop-subblock write inside the same function whose `defer` still
held the lock.

## Fix

Wrapped the locked read-merge-write critical section (state-dir resolve →
flock → buildMergedSupervisorIntent → preflight → installPlan-with-intermediate)
in an inline closure `runLocked := func() error { … }`. Its `defer
lock.Unlock()` now fires at the CLOSURE's return — i.e. still inside
`installPlanCore` but BEFORE `recordInstallIntentPostSuccess`. The flock is
released before the stop-subblock write re-acquires it, breaking the
self-deadlock. No behavior change to the locked critical section itself (same
merge, same rollback scope, same flock window over exactly the read-merge-write).

File: `internal/api/install_parsed_manifest.go` (the `superviseGlobal` branch of
`installPlanCore`).

## Verification

- `TestInstallPlanCore_GlobalFreshInstall_WritesSupervisorIntent_NoSchedulerTask`
  and `TestInstallPlanCore_GlobalFreshInstall_NoPerDaemonSchedulerTaskCreated`
  now PASS in 0.05s (were 600s timeout before the fix).
- `go build ./...` + `go vet ./internal/api/` clean.

## Notes

This is a real production deadlock, not a test artifact: `(*API).Install` →
`installPlanCore` reaches this exact branch for every global daemon manifest.
The Phase 4-F done-gate test was the first code to exercise the global
supervisor-intent install path, which is why it surfaced only now.

## Closure (2026-06-14)

CLOSED — adversarially re-verified (refute-default skeptic) as FULLY fixed at
HEAD; residual hunted and not found.

FIXED (commit `ab4ea23a`): the locked critical section is wrapped in a
`runLocked` closure (`internal/api/install_parsed_manifest.go:564-668`) whose
`defer lock.Unlock()` (line 577) fires at the CLOSURE's return (line 672) —
still inside `installPlanCore` but BEFORE `recordInstallIntentPostSuccess` (line
697) re-acquires the same `supervisor-intent.json.lock` leaf. The non-reentrant
`gofrs/flock` `LockFileEx` self-deadlock is broken because the flock is released
before the stop-subblock write re-takes it. All error paths return INSIDE the
closure with the `defer` intact, so the lock is released on every exit path, not
just the happy path. No behavior change to the locked critical section (same
merge, same rollback scope, same flock window over exactly the
read-merge-write).

Tests at HEAD (confirmed to exist and exercise the fix — these were the 600s
timeout repros, now passing):

- `TestInstallPlanCore_GlobalFreshInstall_WritesSupervisorIntent_NoSchedulerTask`
  (`internal/api/install_parsed_manifest_test.go:1820`).
- `TestInstallPlanCore_GlobalFreshInstall_NoPerDaemonSchedulerTaskCreated`
  (`internal/api/install_parsed_manifest_test.go:2422`).

Doc moved to `work-items/bugs/closed/` per repo convention.
