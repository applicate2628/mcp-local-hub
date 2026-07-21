# Bug: internal/process test suite flaked once in five runs (failing test unidentified)

- id: 2026-07-20-internal-process-suite-flake-unidentified
- context: adjacent-finding
- status: open
- severity: low
- area: internal/process (pre-existing tests; NOT the new releaseconsole tests)
- found-by: qa-engineer

During QA of commit `27f42953`, `go test -count=1 -v ./internal/process/`
FAILED on run 1 of 5 (85.7s wall) and PASSED on runs 2-5 (113.7s / 102.6s /
94.6s / 89.5s, 50 tests each, 0 skips). Raw outputs for runs 2-5 preserved in
the session scratchpad (`process-test-run{2..5}.txt`); run 1's output was
truncated by the capturing command (only the tail survives), so the failing
test is UNIDENTIFIED — that identification gap is the reason this entry
exists.

What the surviving run-1 tail proves: both NEW tests
(`TestReleaseParentConsole_DetachesAnAttachedConsole`,
`TestReleaseParentConsole_NoConsoleIsSafeNoOp`) PASSED in the failing run,
and every test executing after them passed too — the failure occurred earlier
in the suite, before the new console tests ran, so it cannot have been caused
by their process-global console mutation. The package has no `t.Parallel()`
usage. Commit `27f42953`'s only production change in this package is a new,
nowhere-else-called function with a lazy DLL handle (no init side effects),
so the flake is classified pre-existing with high confidence — but per
flake-is-real-bug discipline it must be root-caused, not dismissed.

Next step: re-run the suite in a loop with full output capture
(`go test -count=1 -v ./internal/process/ 2>&1 | tee run-N.txt`) until the
failure reproduces; identify the test; engineer its race window
deterministically. Suspects: the timing-sensitive process-identity /
netstat / wmic tests (the suite's 85-113s wall-time variance lives there).

## Addendum (2026-07-20, backend-engineer — recorded, NOT chased)

Per the revision brief this was recorded, not root-caused. What the
revision run adds:

- `go test -count=1 -timeout 5m ./internal/process/` PASSED, 105.7s wall.
  That is a sixth observation and it sits inside the previously recorded
  85.7-113.7s band, so the band itself is unchanged and still wide.
- The revision ADDS a test to this package
  (`TestReleaseParentConsole_StdErrHandleIsNotRecycledByALaterSpawn`) which
  spawns three short-lived `cmd /c exit` helpers. That is new process-churn
  in a package whose wall-time variance is already suspected to live in the
  timing-sensitive process-identity / netstat / wmic tests. If the flake
  rate rises after this lands, this test is the first thing to bisect
  against — noted here so that link is not rediscovered from scratch.
- The new test mutates process-global console state (AllocConsole /
  FreeConsole), like the two existing releaseconsole tests. The package has
  no `t.Parallel()`, so this is sequential, but it is a second reason to
  suspect ordering effects in this package specifically.

Nothing here identifies the failing test; the next step in the original
entry (loop with full output capture until it reproduces) still stands.
