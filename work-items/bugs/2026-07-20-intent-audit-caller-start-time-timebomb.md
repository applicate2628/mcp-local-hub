# Bug: TestIntentAudit_CallerFieldsPopulated is a wall-clock time bomb — it fails whenever the `internal/api` package takes >2 min to reach it

Status: open
Filed: 2026-07-20
Severity: P3 (test-only; no production impact — but it makes the CLAUDE.md Step-1 pre-push gate unpassable on a loaded machine, which trains operators to ignore a red gate)
Source: $backend-engineer adjacent finding while running the mandatory repo gates for the serena readiness-timeout-inversion fix (`fix/serena-readiness-timeout-inversion`)
Context: adjacent-finding (independent of that fix — `internal/api` has no import edge to `internal/cli` or `internal/daemon`)

## What happens

`internal/api/intent_audit_test.go:152-155` asserts that the audit row's
`caller_start_time` is within ±2 minutes of `time.Now()`:

```go
// ±2 minutes window for fresh test process — start time is precise on
// modern OS but CI noise + virtualization can widen the gap.
delta := time.Since(parsed).Abs()
if delta > 2*time.Minute {
    t.Errorf("caller_start_time delta = %v; want within ±2min of now (process just started)", delta)
}
```

`caller_start_time` is the **test binary's own process start time**. The comment
("process just started") encodes an assumption that is only true when this test runs
early in the package. The window is not a tolerance on clock skew — it is a budget on
how long the whole `internal/api` test binary may run before reaching this test.

Observed on a loaded Windows host (2026-07-20, other agents + the operator's live fleet
running concurrently):

```
--- FAIL: TestIntentAudit_CallerFieldsPopulated (0.00s)
    intent_audit_test.go:154: caller_start_time delta = 4m10.1300759s; want within ±2min of now (process just started)
```

The test itself took 0.00s. It failed purely because ~4m10s of *other* `internal/api`
tests had already run inside the same binary.

## Why this matters beyond the one test

The CLAUDE.md Step-1 pre-push gate is:

```bash
go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...
```

On this host that gate currently cannot pass, for two independent reasons:

1. this time-bomb assertion, and
2. `internal/api` and `internal/gui` both exceed the **5 m per-package** timeout —
   measured in isolation, `internal/gui` passes but takes **690 s** (11.5 min).

Both surface as a red gate that has nothing to do with the change under test. A gate
that is red for unrelated reasons is worse than no gate: it is exactly the condition
that produced the `feedback_kosyak_subagent_summary_overstates` failure mode (green
claimed without verification), because the honest signal is indistinguishable from noise.

## Suggested fix (not implemented — outside the approved change surface)

For the time bomb, pick one:

- assert only that `caller_start_time` **precedes** `time.Now()` and parses as UTC
  RFC3339Nano (drop the upper bound — the field's contract is "when did the caller
  process start", not "recently"); or
- capture a package-level `processStartApprox = time.Now()` in `TestMain` and assert
  `caller_start_time` is within a small window of **that**, not of `time.Now()` — this
  keeps a real assertion while making it independent of package runtime.

For the gate timeout, either raise the documented `-timeout` in CLAUDE.md Step 1 to a
value that reflects `internal/gui`'s real cost, or split the two slow packages into
their own gate invocation. This is a docs/policy decision, not an implementer call.

## Evidence

- `internal/api/intent_audit_test.go:152-155` — the assertion.
- Full-suite run 2026-07-20: only `FAIL mcp-local-hub/internal/api 300.092s` and
  `FAIL mcp-local-hub/internal/gui 300.325s`; every other package clean. Both at ~300 s
  == the 5 m per-package timeout.
- `go test -count=1 -timeout 20m ./internal/gui/` → `ok  mcp-local-hub/internal/gui 690.784s`.
- `go list -deps ./internal/api` lists neither `mcp-local-hub/internal/cli` nor
  `mcp-local-hub/internal/daemon`, so the changed packages cannot influence this result.
