# `internal/process` flakes under load — wall-clock upper bounds in `jobobject_windows_test.go`

Status: open
Severity: P3 (test-only; no production behavior affected)
Filed: 2026-07-17
Found: during the item-2 (`feat/gui-daemon-recovery`) pre-PR verification sweep.

## Symptom (observed once, exactly)

`go test -count=1 -timeout 10m ./internal/process/ ./internal/daemonrecovery/` reported
`FAIL` for `mcp-local-hub/internal/process` while `daemonrecovery` passed. Five subsequent
runs of the same package were green: 2 plain (`27.4s`, `28.2s`) and 3 under
`-race -count=3` (`82.5s`, no data race reported).

The machine was loaded at the time: 37 live `mcphub` daemons, 3 concurrent `codex`
processes, and a 200-second `./internal/cli/` test run had just completed.

## ASSUMPTION (UNVERIFIED) — the exact failing test was not captured

The failing run's output was piped through `tail -2`, which discarded the test name. The
identification below is a well-supported hypothesis, **not** a confirmed root cause.

**Verification step that would resolve it:** re-run the package under synthetic CPU load
(e.g. a busy-loop per core) with `-count=20` and full output captured to a file, then read
the failing test name. If `TestTerminateAllZeroTimeout*` (the `:189` assertion) is not the
one, this bug's diagnosis must be rewritten rather than patched.

## Hypothesis: the wall-clock upper bounds

`internal/process/jobobject_windows_test.go` carries wall-clock **upper-bound** assertions —
the only ones in the package that fail purely from scheduling delay:

- [`jobobject_windows_test.go:189`](../../internal/process/jobobject_windows_test.go#L189) —
  the tightest, and the prime suspect:

  ```go
  start := time.Now()
  if err := job.TerminateAll(0); err != nil { ... }
  if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
      t.Errorf("TerminateAll(0) took %v; want near-instant return (regression: zero-timeout path is waiting when it should not)", elapsed)
  }
  ```

- [`jobobject_windows_test.go:164`](../../internal/process/jobobject_windows_test.go#L164) —
  `if elapsed > 5*time.Second` (looser; less likely but the same class).

`TerminateAll(0)` on an **empty** job returns in microseconds, so 500 ms looks generous —
but it is a bound on WALL CLOCK, not on work done. A goroutine descheduled under heavy load
can exceed it while the code under test is perfectly correct.

## Why this is a design defect, not "just a flake"

The repo's race-window assertion discipline: *a test asserting a transient window must
engineer the window deterministically via a known-slow path or injection seam, not rely on
natural timing.* This test asserts the inverse (that a path is FAST), but the defect is the
same shape — the verdict depends on the host's scheduler rather than on the property under
test.

The assertion's **intent is good and must be preserved**: it pins that the zero-timeout path
opts out of waiting instead of blocking. Only the measurement is wrong.

## Right shape

Replace the wall-clock bound with a deterministic observation of the property itself — that
the zero-timeout path does not enter the wait — via an injection seam or a counter on the
wait call, so the assertion is about the code path taken, not about elapsed nanoseconds. If a
timing bound must remain, it belongs behind a load-tolerant margin or a `testing.Short()`
skip, not at 500 ms.

## Scope

Pre-existing and independent of daemon recovery. The file is untouched by the item-2 branch
(`git status` clean for it); last modified by `9828bf7e` (#241, per-task Job Object orphan
cleanup). Deliberately NOT fixed inside the item-2 PR — an unrelated test-only defect does
not belong in a destructive-path change.

## Companion observation 2026-07-17 (same load-induced class)
A second intermittent failure of the same class was observed during heavy parallel codex load
(design-B + consilium lanes running): `TestVerifyProxyReadyForServerNames_AcceptsJSONFiniteAndHeldOpenSSE`
(`internal/api/register_test.go`) failed once in an aggregate `./internal/api/ ./internal/cli/ ./internal/gui/`
run, then passed 3/3 in isolation and passed a subsequent full aggregate. Held-open SSE + temp-port under
CPU/port pressure. Same trigger (machine saturated by concurrent lanes), same non-reproducing signature.
Not blocking; same "wall-clock/load-dependent test" class as the jobobject finding above. If the
deterministic-window fix lands for jobobject, audit this SSE-readiness test for the same anti-pattern.
