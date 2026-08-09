---
title: TestEnsureAliveGUIRecovery_LateStandbyRejectsInterruptedMarker failed once in a combined-batch internal/cli run (not reproduced in 15 subsequent reruns)
severity: low
found-by: backend-engineer, gate verification for residuals 1/4/5 (2026-07-26 fix round)
affected-surface: internal/cli (supervise_ensure_alive_test.go)
context: adjacent-finding
status: open
---

## What happened

While running the mandatory gate for the residual 1/4/5 fixes
(`work-items/active/2026-07-25-liveness-headless-gui-recovery/`), one combined
run of

```
go test -tags=test_state_path_env ./internal/cli/ \
  -run 'TestEnsureAlive|TestProbeGUIOwnerAlive|TestRestartV3|TestAwaitGUIExitSignalReason|TestStartGUIExitSignalObserver' \
  -count=1 -timeout 180s
```

failed once:

```
--- FAIL: TestEnsureAliveGUIRecovery_LateStandbyRejectsInterruptedMarker (0.24s)
    supervise_ensure_alive_test.go:1679: ensure-alive marker = &{... Phase:reserved ...}, want interrupted
```

## Reproduction attempts (not reproduced)

- `go test ./internal/cli/ -run '^TestEnsureAliveGUIRecovery_LateStandbyRejectsInterruptedMarker$' -count=1` — 5/5 PASS in isolation.
- The same combined command above — 15/15 PASS across two follow-up batches
  (5 runs, then 10 more runs), all on the identical residual-1/2/3 diff.

Net: 1 failure observed out of 16 total combined-batch runs; 0 failures in 20
total runs once isolated or rerun. This is consistent with a transient
environmental flake, not a deterministic regression, though the sample is
small and this is not proof of absence.

## Why this is plausibly linked to (but not proven caused by) this round's diff

`TestEnsureAliveGUIRecovery_LateStandbyRejectsInterruptedMarker` does not fake
`guiOwnerAliveFn` before calling `runEnsureAlive`, so that call falls through
to the real `probeGUIOwnerAlive()`, which resolves `gui.PidportPath()`
(`<stateDir>/mcp-local-hub/gui.pidport`, DIFFERENT from the test's own pidport
path `filepath.Join(stateDir, gui.PidportFileLeaf)` used later at
`supervise_ensure_alive_test.go:1682` — no `mcp-local-hub` subdirectory) and
finds no pidport file, classifying `guiOwnerStateUnknown`. Residual 1(b)'s new
`runEnsureAliveGUIOwnerUnknownEscalation` (also unfaked in this test) then
calls `guiOwnerLockUnheldProbeFn()`, which performs a REAL flock
TryLock+Release against `<stateDir>/mcp-local-hub/gui.pidport.lock` — a file
this test never touched or asserted on before this round, in a directory
isolated from the test's actual pidport path. This adds one small filesystem
operation to this test's `runEnsureAlive` call that did not exist previously.

No direct path collision or shared-state mutation between this new operation
and the test's own assertions was found (they operate on distinct
directories), so this is filed as a plausible, unproven contributor to
generic timing/I/O pressure on an already-documented-flaky-under-load host
(`work-items/bugs/2026-07-25-full-suite-flakes-gui-and-cli-adjacent-finding.md`
already attributes a different pair of flakes on this same package to
Windows file-handle/flock-release timing sensitivity), not a root-caused
logic defect in the escalation path itself.

## Why this is filed as adjacent, not fixed here

Root-causing generic full-batch timing sensitivity in this package is outside
the approved change surface for the residual 1/4/5 fix round and would expand
scope beyond what was authorized. Filing here per the adjacent-findings
protocol so the orchestrator can fold it into the existing flake-investigation
backlog alongside the two flakes already tracked in
`work-items/bugs/2026-07-25-full-suite-flakes-gui-and-cli-adjacent-finding.md`.

## Suggested next step (not started)

- If this recurs with a higher frequency in CI or future sessions, consider
  faking `guiOwnerAliveFn` (or `guiOwnerLockUnheldProbeFn`) in
  `TestEnsureAliveGUIRecovery_LateStandbyRejectsInterruptedMarker` and its
  Phase-I sibling tests that don't already do so, matching the pattern
  `noLiveGUIOwner`/`liveGUIOwner`/`unknownGUIOwnerState` already establish for
  the Part-B tests — this would remove the incidental real-flock I/O from
  Phase-I-focused tests entirely, independent of whether it is the actual
  cause.

## Terms and Abbreviations

- Flock: the `gofrs/flock` OS-level advisory file lock this codebase uses for
  its single-instance and supervisor locks.
- GUI: graphical user interface.
