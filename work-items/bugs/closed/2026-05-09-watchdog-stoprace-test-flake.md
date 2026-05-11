---
title: TestWatchdogOnce_ApplyRestartDecision_StopRace_NoRestart fails under machine load
severity: low
found-by: pre-tag smoke run on master HEAD 2e16711 (post-PR #145–#148 merges)
found-on: 2026-05-09
project: mcp-local-hub
related-pr: pre-existing — fails on PR #144 merge base 0a32264 too
status: closed
fixed-in: test-only seam fix (internal/cli/watchdog_test.go:602)
---

# Watchdog stop-race test fails when run under load

## Reproduction

```bash
go test -count=1 -tags=test_state_path_env -run TestWatchdogOnce_ApplyRestartDecision_StopRace_NoRestart ./internal/cli/
```

Reproducible result (seen on Windows 11, Go 1.26.2, ~7 background mcphub
processes + Vitest + Playwright concurrently):

```text
--- FAIL: TestWatchdogOnce_ApplyRestartDecision_StopRace_NoRestart (30.00s)
    watchdog_test.go:645: Restart MUST NOT be called when intent stop-race detected; calls=1
    watchdog_test.go:650: watchdog.log missing stop-race-aborted; got [{TS:... Action:restart-not-yet-running-after-30s ...}]
```

Verified pre-existing by checking out master pre-PR-#145 versions of
`internal/cli/watchdog{,_test}.go` + `gui_tray_state{,_test}.go` from
commit `0a32264` (PR #144 merge base): same failure mode.

## Root cause hypothesis

The test seeds `SetTestIntentReaderFn` to return `IntentDesiredStopped`
immediately, expecting `applyRestartDecision` to detect the stop-race
BEFORE invoking the mocked `SetTestRestartWithSnapshotFn`. Under load,
the order is reversed:

1. `applyRestartDecision` invokes the restart mock immediately
   (`restartCalls.Load() == 1`).
2. Watchdog goroutine then waits 30s for daemon to become Running.
3. Times out, logs `restart-not-yet-running-after-30s`.
4. Stop-race check never fires.

This suggests either:

- The intent-stop check is downstream of the restart invocation in
  `applyRestartDecision` — possibly only consulted when re-evaluating
  whether the in-flight restart should be aborted, not before kicking
  it off.
- OR the test setup expects `IntentReaderFn` to be consulted in a
  pre-restart guard that doesn't actually exist in the current code
  path.

## Why it was hidden until now

Running `go test ./...` (without `-tags=test_state_path_env`) hangs on
`internal/cli/` due to flock contention with a user-installed mcphub
binary holding `daemon-intent.json.lock` — see related bug
`work-items/bugs/2026-05-08-api-tests-flock-contention-with-user-binary.md`.
Result: this test was never executed in past CI runs because the
package containing it never reached test execution.

With `-tags=test_state_path_env` the state path redirects to a temp
dir, flock contention disappears, and the test finally runs — but
fails as documented above.

## Severity

**Low**:

- Test failure exposes potential watchdog logic gap, but no production
  user has reported a stuck restart-during-stop scenario.
- The `restart-not-yet-running-after-30s` action is a fail-safe — even
  if stop-race is missed, the restart eventually times out without
  damage.
- Not blocking v0.3.0 tag — pre-existing, not introduced by recent
  work.

## Fix candidates

1. **Add pre-restart intent re-read** — in `applyRestartDecision`,
   call `TryReadDaemonIntent` immediately before the
   `SetTestRestartWithSnapshotFn` invocation. If `IntentStillRunning`
   returns false, log `stop-race-aborted` and return without restart.

2. **Increase test timeout + retry** — under load, the 30s hard
   timeout in the watchdog itself elapses before the intent-reader
   goroutine fires. Bump the wait or add explicit synchronization.

3. **Extract pre-restart guard into a unit-testable helper** — split
   the apply logic so the test can verify the guard fires without
   needing the full restart-context-with-snapshot path.

Recommended: option 1. Smallest change with deterministic outcome.

## Plan

- Defer to v0.4.x.
- Coordinate with the existing `daemon-intent.json` user-stop marker
  pattern shipped in PR #134 (commit `982c366`) and the tray
  suppression in PR #142 (commit `d8e3999`) — they all consume the
  same intent file.
- Add a regression test that verifies the pre-restart guard fires
  deterministically (mock the intent reader to return stopped + verify
  zero restart calls within ~1 second, not 30 seconds).

## Resolution

**Status:** closed
**Date:** 2026-05-12
**Fix:** test-only — added `watchdogNowFn = func() time.Time { return now }` seam injection
**File:** `internal/cli/watchdog_test.go:602` (TestWatchdogOnce_ApplyRestartDecision_StopRace_NoRestart)

The test was incorrectly relying on real wall-clock time inside
`applyRestartDecision` (which calls `watchdogNow()` at line 699 to
TTL-check the fake intent). With wall-clock time several days past
the test's fixed `now = 2026-05-07T12:00:00Z`, the 24h
`StopIntentTTL` made the fake `IntentDesiredStopped` appear stale,
`IsActiveStop` returned false, `IntentStillRunning` returned true,
and the stop-race-aborted path was bypassed.

Other watchdog tests (lines ~957, ~1411, ~1524, ~1618) follow the
correct pattern of pinning `watchdogNowFn` to the test's `now`.
This test was the only one missing the seam.

After the fix, the test runs in 0.024s (was 30s timeout-then-fail)
and full `internal/cli/` package passes in 30.151s (test deadline
budget consumed by intentional context-budget tests, not flake).

Production `applyRestartDecision` code is correct — it must consult
wall clock for intent staleness because real elapsed time matters
for the TTL contract. No production code change.
