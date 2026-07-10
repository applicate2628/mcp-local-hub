# Reaper V1 Commission Fix Plan

**Role:** backend-engineer

**Goal:** Correct four operator-facing diagnostic inaccuracies in the uncommitted V1 test-leftover preview without adding any process action path.

## Checklist

- [x] Verify each commission finding against the actual worktree and run the permitted focused baseline.
- [x] Add regression tests for candidate-local apply-floor age, unavailable protected scopes, and supervise parent liveness.
- [x] Strengthen the destructive-flag test so it identifies Cobra/pflag unknown-flag rejection before removing the dead inherited-parent guard.
- [x] Apply the smallest production changes in `internal/api/test_leftover_preview.go` and `internal/cli/cleanup_test_leftovers.go`.
- [x] Run `go build ./...`.
- [x] Run `go vet ./internal/api/ ./internal/cli/ ./internal/process/`.
- [x] Run the two requested focused test commands only.
- [x] Inspect the final diff for terminate, reap, Process Environment Block, apply, and confirmation call paths; confirm the fail-on-call seam remains at zero calls.
- [x] Write the required session report and hand off without commit or push.

## Constraints

- Work only in the user-provided linked worktree and preserve all pre-existing uncommitted changes.
- Keep census tests synthetic and dependencies injected; do not enumerate or act on host processes.
- Do not run the full API or CLI suites.
- Do not commit, push, terminate, signal, or reap a process.

## Terms and Abbreviations

- **API:** Application Programming Interface.
- **CLI:** Command-Line Interface.
- **V1:** Version 1, the preview-only implementation.
