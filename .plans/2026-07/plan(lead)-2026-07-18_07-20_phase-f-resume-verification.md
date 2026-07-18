# Phase F Restart-Child Resume Verification Plan

Goal: finish the interrupted Phase-F correction without touching Phase I, committing, or changing gate-off behavior.

Classification: behavioral and security-sensitive correction inside approved owner seams.

## Completed steps

- [x] Restore the active work-item and inspect the current uncommitted diff.
- [x] Trace fixes 1-6 and the three Option-B corrections to production owners and tests.
- [x] Add failing tests for the unused bind budget, non-canonical nonce path, exact secret length, and Commit/runtime settlement race.
- [x] Implement the smallest owner-level corrections and pass focused tests plus the race detector.
- [x] Cross-compile tagged Linux API, CLI, and GUI test binaries to verify the POSIX files and fixtures compile.
- [x] Run formatting, build, vet, and the exact tagged package suite.
- [x] Reconcile task memory; leave independent QA/security re-gates open.

## Constraints preserved

- No graphify and no Claude command-line interface.
- No `MCPHUB_GUI_SPAWN_TESTS` environment setting.
- Every API/CLI test command used `-tags=test_state_path_env`.
- `internal/cli/supervise_ensure_alive.go` was not edited by this lane.
- No commit or publication action.

## Terms and Abbreviations

- API: Application Programming Interface.
- CLI: Command-Line Interface.
- POSIX: Portable Operating System Interface, used here for the non-Windows state-reader implementation.
