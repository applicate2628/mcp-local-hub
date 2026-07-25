---
title: Two pre-existing test flakes surfaced while verifying the liveness headless-fleet + GUI exit-reason PR (unrelated to that diff)
severity: low
found-by: backend-engineer, gate verification for work-items/active/2026-07-25-liveness-headless-gui-recovery
affected-surface: internal/gui (full-package test ordering), internal/cli (Windows t.TempDir cleanup)
context: adjacent-finding
status: open
---

## What happened

While running the mandatory gate (`go build`, `go vet`, `go test`) for the
liveness headless-fleet-recovery + GUI exit-reason work, two test failures
appeared that are **not caused by that diff** — confirmed by reproducing them
on the base commit (`git stash` the diff, re-run) and by reproducing them
reliably in isolation (they pass every time alone; they only sometimes fail
as part of the full package run).

### 1. `internal/gui`: `TestBroadcaster_Publish_PersistsToGUIEventLog` intermittently fails in the full-package run

```
--- FAIL: TestBroadcaster_Publish_PersistsToGUIEventLog (0.00s)
    events_test.go:76: tail len = 5, want 4
```

- `go test ./internal/gui/... -run TestBroadcaster_Publish_PersistsToGUIEventLog -count=20` — 20/20 PASS in isolation.
- `go test ./internal/gui/...` (full package) — observed BOTH `FAIL` (this test)
  and `ok` across repeated runs, on the SAME commit (both with and without the
  liveness/exit-reason diff applied). This is order/timing-dependent
  contamination from another test in the package (most likely something else
  appending to the same shared `gui-events.log` test fixture path, or a
  timing race in the tail-length assertion), not a regression from this PR.

### 2. `internal/cli`: `TestRestartV3_ActivatedChildAcceptsSecondRestart` fails cleanup, not the test body

```
testing.go:1464: TempDir RemoveAll cleanup: unlinkat R:\Temp\TestRestartV3_ActivatedChildAcceptsSecondRestart2510411442\001\hardened-parent: The directory is not empty.
--- FAIL: TestRestartV3_ActivatedChildAcceptsSecondRestart (0.05s)
```

- `go test ./internal/cli/... -run TestRestartV3_ActivatedChildAcceptsSecondRestart -count=5` — 5/5 PASS in isolation.
- Only manifests when run alongside the rest of the `internal/cli` gui-test
  slice. The failure is `testing.go`'s own `t.TempDir()` cleanup complaining
  the directory isn't empty (Windows file-handle-still-open race — almost
  certainly a `gofrs/flock` lock file not yet released at `t.Cleanup` time),
  not an assertion in the test body. CLAUDE.md already documents the
  `internal/cli` full sweep as "flaky/crashy on this host" for this reason
  class; this is the same class, just newly pinned to a specific test name.

## Why this is filed as adjacent, not fixed here

Root-causing either flake (test isolation / fixture-path sharing for #1;
flock-release-ordering for #2) is outside the approved change surface for the
liveness headless-fleet-recovery + GUI exit-reason-attribution work and would
expand scope beyond what was authorized. Filing here per the adjacent-findings
protocol so the orchestrator can prioritize a dedicated investigation.

## Suggested next step (not started)

- For #1: audit `internal/gui/events_test.go` and neighboring tests for a
  shared `gui-events.log` path or global broadcaster state that isn't
  per-test-isolated (`t.TempDir()` + env override, matching the pattern
  `ensureAliveTestStateDir` uses in `internal/cli`).
- For #2: check whether `TestRestartV3_ActivatedChildAcceptsSecondRestart` (or
  a sibling test running just before/after it) leaves a `*.lock` file handle
  open past its own cleanup, and whether releasing that lock needs an
  explicit `t.Cleanup` ordering fix or a short bounded retry in the test's own
  teardown.
