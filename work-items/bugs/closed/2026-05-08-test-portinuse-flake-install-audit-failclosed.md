---
title: TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation flakes when port 9128 is held
severity: low
found-by: qa-engineer
found-in-phase: PR #134 final QA gate
affected-surface: internal/api/install_intent_test.go:668
context: standalone
status: closed
closed-on: 2026-06-14
---

## Reproduction

1. Start `mcphub gui` (or any daemon binding port 9128, the manifest "time" daemon's external port).
2. Run `go test -count=1 -timeout 5m ./internal/api/ -run TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation`.
3. Observe failure:

```
install_intent_test.go:687: error chain: want ErrIdentityOversize, got port 9128 already in use (needed for daemon time/default)
```

When port 9128 is free at test invocation, the test passes.

## Expected vs actual

**Expected:** `Install()` short-circuits at the audit-first call (plan §62) and surfaces `ErrIdentityOversize` regardless of host environment.

**Actual:** `Install()` runs the preflight `portInUse(d.Port)` BEFORE the audit-first audit append, so a real running mcphub.exe holding port 9128 returns the port-conflict error before the audit path is ever reached.

The test depends on port 9128 being free, which is unstable in dev environments where mcphub is installed and running. CI runs on fresh runners so this should not flake there, but it is a footgun for local development and a fragile test design.

## Files involved

- internal/api/install_intent_test.go:668 — TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation
- internal/api/install.go:1213 — portInUse implementation (default `net.DialTimeout`)
- internal/api/install.go:1156-1170 — preflight portInUse check called BEFORE audit-first

## Suggested fix

Add a `portInUseFn` package-level seam in `install.go`, and stub it in the test so the preflight short-circuits to "free" regardless of host state. Same pattern as `lookupProcess`, `appendIntentAuditFn`, etc. already used.

```go
// install.go
var portInUseFn = portInUse
// ... use portInUseFn in preflight

// install_intent_test.go
prevPortInUse := portInUseFn
portInUseFn = func(int) bool { return false }
t.Cleanup(func() { portInUseFn = prevPortInUse })
```

## Severity rationale

Low: the test is correct in isolation and CI passes; this only affects local dev runs when mcphub is concurrently installed. Filed as known-flake post-merge follow-up per PR description.

## Closure (2026-06-14)

CLOSED — adversarially re-verified (refute-default skeptic) as FULLY fixed at
HEAD; residual hunted and not found.

FIXED by the `preflightPortInUse` seam (commit `5d91e42`) AND the v0.6
supervisor-intent port-ownership recognition: `internal/api/install.go:1760-1797`
checks supervisor-intent (line 1761) BEFORE the scheduler-task fallback, closing
the residual the triage-2026-05-28 Rows 6+10 note flagged (a v0.5.0 supervisor
child was not recognised as the legitimate port owner, so the gate could still
fire a spurious port-9128 collision on supervisor hosts). With the
supervisor-intent check ahead of the scheduler-task fallback, an own-daemon
holding the port is recognised regardless of whether it is a scheduled task or a
supervisor child.

Tests at HEAD (confirmed to exist and exercise the fix):

- `TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation`
  (`internal/api/install_intent_test.go:839`) — the originally-flaking test, now
  port-seam-isolated.
- `TestPreflight_AllowsSameSupervisorOwnedPortAndRejectsForeignIntentRow`
  (`internal/api/install_own_port_test.go:146`).
- `TestPortHeldBySupervisorIntentDaemonExternalRequiresPIDProof`
  (`internal/api/install_own_port_test.go:235`).

NOTE: this is the duplicate filing #6 of the port-9128 defect. The SEPARATE doc
`2026-05-12-install-test-port-9128-collision.md` stays OPEN — it tracks a
DIFFERENT residual (the audit-before-port-check ordering / Option-B preflight
reorder, still deferred), not the supervisor-child recognition this doc tracked.

Doc moved to `work-items/bugs/closed/` per repo convention.
