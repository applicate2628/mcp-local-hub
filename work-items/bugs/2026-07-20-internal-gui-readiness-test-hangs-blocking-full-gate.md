---
title: TestReadinessHandler_AllServers hangs indefinitely, making `go test ./...` unrunnable as a gate
severity: medium
found-by: backend-engineer
found-in-phase: supervisor death-forensics lane (running the mandatory repo gate)
affected-surface: internal/gui (TestReadinessHandler_AllServers)
context: adjacent-finding
status: open
related-branch: feat/supervisor-death-forensics
---

## Symptom

`go test -count=1 -timeout 5m ./...` — gate 1 in CLAUDE.md's mandatory
pre-push checklist — cannot complete. `internal/gui` consumes the entire
timeout and is killed:

```
panic: test timed out after 5m0s
	running tests:
		TestReadinessHandler_AllServers (1s)
FAIL	mcp-local-hub/internal/gui	300.131s
```

The `(1s)` in the timeout report is the elapsed marker at the moment the
panic printed; the test itself never returns.

## Confirmed PRE-EXISTING, not branch-induced

Reproduced on pristine `master` @ `d8ab4777` with all branch changes stashed
(`git stash push --include-untracked`):

```
=== PRISTINE MASTER (d8ab4777, changes stashed) ===
panic: test timed out after 3m0s
	running tests:
		TestReadinessHandler_AllServers (3m0s)
FAIL	mcp-local-hub/internal/gui	180.124s
```

The death-forensics branch touches only `internal/api` and `internal/cli`
and has no code path into the gui readiness handler.

## Why it matters

This is a **gate-integrity** problem, not just a slow test. CLAUDE.md Step 1
makes `go test ./...` a hard pre-push gate and explicitly warns not to trust
a subagent's "all green". While this test hangs, that gate cannot be run to
completion by anyone, so every branch is currently forced to report the gate
as partial — which is exactly the condition under which a real regression
slips through unnoticed.

## Environment note (unverified hypothesis)

`ASSUMPTION (UNVERIFIED)`: the hang may be host-state dependent — this host
was running a full live fleet (38 `mcphub.exe` processes, supervisor PID
125232) during every observed run, and a readiness handler named
"AllServers" plausibly probes real endpoints. **Settling probe:** run
`go test -run TestReadinessHandler_AllServers ./internal/gui/` on a host with
no live mcphub fleet, and/or read the test to see whether it dials real
ports or is fully hermetic. Not investigated here — out of this lane's
change surface.

If it IS host-state dependent, the fix is a hermetic seam (the same problem
`MCPHUB_E2E_SCHEDULER=none` already solves for the scheduler), not a longer
timeout.
