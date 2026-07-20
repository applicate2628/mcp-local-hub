---
title: TestIntentAudit_CallerFieldsPopulated asserts a ±2min "process just started" window that the api suite now exceeds
severity: low
found-by: backend-engineer
found-in-phase: supervisor death-forensics lane (running the mandatory repo gate)
affected-surface: internal/api/intent_audit_test.go:152-155
context: adjacent-finding
status: open
related-branch: feat/supervisor-death-forensics
---

## Symptom

```
--- FAIL: TestIntentAudit_CallerFieldsPopulated (0.00s)
    intent_audit_test.go:154: caller_start_time delta = 3m47.2709327s;
        want within ±2min of now (process just started)
```

## Mechanism — a duration assertion, not a correctness assertion

`intent_audit_test.go:152-155`:

```go
delta := time.Since(parsed).Abs()
if delta > 2*time.Minute {
    t.Errorf("caller_start_time delta = %v; want within ±2min of now (process just started)", delta)
}
```

`parsed` is the **test process's own start time**. The assertion therefore
holds only while the test binary has been alive for under 2 minutes. It is
not testing `caller_start_time` correctness — it is testing how long the
`internal/api` package takes to reach this test.

The `internal/api` package now runs **393–720 s** on this host, so by the
time this test executes the process is 3–5 minutes old and the window is
structurally unsatisfiable. The test's own comment ("process just started")
encodes an assumption the suite outgrew.

## Confirmed PRE-EXISTING

- Passes in isolation (fresh process): `ok mcp-local-hub/internal/api 0.043s`
- Fails on pristine `master` @ `d8ab4777` with all branch changes stashed,
  full package: `caller_start_time delta = 4m47.4178017s` (that same master
  run also failed `TestVerifyProxyReadyForServerNames_AcceptsJSONFiniteAndHeldOpenSSE`
  and `TestVerifyProxyReady_GenericAllowlistDoesNotReadHeldOpenBody`, i.e.
  master currently fails MORE api tests on this host than the branch does)

The branch adds ~0.15 s of api tests; it cannot move a 3m47s delta under 2m.

## Suggested fix

Anchor the assertion to what it actually means to verify. Options, cheapest
first:

1. Capture `start := time.Now()` at the top of the test and assert
   `parsed` is within a window of **that**, not of `time.Now()` at assert
   time — this tests the field, not the suite duration.
2. Assert `parsed` is not in the future and not older than the process
   start time obtained independently, dropping the wall-clock window.

Do not "fix" it by widening the window to 10 minutes — that just defers the
same failure until the suite grows again, and the widened bound would no
longer detect a genuinely wrong `caller_start_time`.
