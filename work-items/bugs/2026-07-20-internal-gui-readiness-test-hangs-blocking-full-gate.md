---
title: TestReadinessHandler_AllServers takes 338s via unbounded WMI probes; Gate 1 -timeout is per-package, not total
severity: medium
found-by: backend-engineer
found-in-phase: supervisor death-forensics lane (running the mandatory repo gate)
affected-surface: internal/gui (TestReadinessHandler_AllServers)
context: adjacent-finding
status: root-caused; fixed on fix/readiness-handler-unbounded-probe
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

---

## CORRECTION (2026-07-20, same day) — title was wrong, severity was understated

Two readings in this file looked contradictory and both turned out to be true.
Settled by isolated `-run`-filtered runs on pristine `d8ab4777`, where nothing
else could consume the budget:

```
-timeout 45s -run '^TestReadinessHandler_AllServers$'  -> panic; dump shows (45s): it burned the budget ALONE
-timeout 15m  same filter                              -> PASS (338.14s)
after the fix                                          -> PASS (37.06s)
```

**"Hangs indefinitely" is wrong** — it completes in 338s. **But the test is not
an innocent bystander either**: 338s exceeds the 5m per-package budget on its
own. The `(3m0s)` reading was the test genuinely blocking under a 3m budget; the
`(1s)` reading was a full-suite run where readiness merely started 1s before a
deadline the REST of the suite had already nearly exhausted. The suite is slow
AND this test is slow.

The Lead initially read the `(1s)` marker as proof the test was a bystander and
redirected the investigating lane away from it. That was half right — it
correctly identified the wrong title — and half wrong. The `-run`-filtered run is
what settles it, and it should have been the FIRST measurement taken by anyone,
including the Lead. Cost: one lane nearly re-scoped away from a real defect.

## Actual root cause — an unbounded WMI probe reachable from a live GUI route

```
syscall.WaitForSingleObject(0x4d0, 0xffffffff)          <- INFINITE wait, no recovery
os/exec.(*Cmd).Output -> runWmicNameParent               internal/api/processes.go:733
  -> procNameAndParent -> portHeldByOurDaemonForPortArm  internal/api/install.go:1945
  -> fixedPortStatus -> CheckServerReadinessWithScope    internal/api/readiness.go:395
  -> AllServerReadiness -> (*Server).readinessByName     internal/gui/readiness.go:65
```

Measured: `wmic` 4.1-6.7s per call, PowerShell fallback 10.5s, `netstat` 110ms,
`schtasks` 177ms. 13 of 14 manifest ports are in use on this host at ~18s each,
serially. `internal/api/main_test.go:82-88` independently records "~31s per wmic
call on Win11 24H2".

**This is a PRODUCTION defect, not a test defect.** `/api/server/readiness` is
the live GUI route (`internal/gui/readiness.go:28`). It is finite today only
because WMI happens to answer; `0xffffffff` is a genuine infinite wait with no
timeout, so a wedged Winmgmt hangs the operator's Dashboard with no recovery.

Fixed at both layers on `fix/readiness-handler-unbounded-probe`: per-probe and
per-chain deadlines in `internal/api/process_probe_exec.go`, plus a fan-out
budget in `AllServerReadiness`. Both mutation-proven. The fan-out mutation is the
important one — removing that budget reproduces
`mcp-language-server reported Ready=true after the probe budget was exhausted`,
i.e. a readiness endpoint that LIES. A readiness surface that reports ready on a
timed-out probe is worse than one that hangs.

## The Gate-1 mechanism nobody had named

`go help testflag`: *"If a test **binary** runs longer than duration d, panic"* —
`-timeout` is **per package**, not total. `go test ./...` runs one binary per
package and grants the full budget to EACH. CLAUDE.md Gate 1 specifies
`-timeout 5m ./...`; two packages (`internal/gui`, `internal/api`) exceed 5m on
their own, so the gate fails while naming a rotating cast of innocent tests —
which is exactly why multiple independent lanes each dismissed a different
victim as "a pre-existing flake" this session.

Tracked separately as
`2026-07-20-claude-md-gate1-timeout-misspecified-per-package.md`, with a
split fast-lane/slow-lane correction argued at 2x the measured worst package and
a note that the per-package semantic must be stated explicitly in CLAUDE.md.

Related slow-test instances of the same Gate-1 problem, not defects in
themselves: `TestDeAdoptPlanRouteNeverSerializesExecutionState` (121s),
`TestExecuteAdoptPersistsProvenanceAcrossFreshAPI` (85s).
