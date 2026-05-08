---
title: TestInstall_AuditFailsErrIdentityOversize_NoSchedulerMutation flakes when port 9128 is held
severity: low
found-by: qa-engineer
found-in-phase: PR #134 final QA gate
affected-surface: internal/api/install_intent_test.go:668
context: standalone
status: open
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
