# Hub Health Correctness Implementation Plan

> **Execution:** Inline only. The user explicitly prohibited subagents, external review, commits, test runs, typechecking, and generation.

**Goal:** Make hub-health reporting honest across restart-driver death and asynchronous startup, suppress unavailable Group URLs immediately, and close the specified regression-test gaps.

**Architecture:** Keep health ownership in the existing Go tracker and restart-driver lifecycle. Use the existing Groups background-load sequencing plus local Server-Sent Events health state for immediate client-side suppression. Keep the reconcile command owned by the health data transfer object.

**Tech Stack:** Go, Preact, TypeScript, Vitest test sources (authored but not executed).

## Global Constraints

- Do not rename lifecycle states or touch the three explicitly out-of-scope debts.
- Run no tests, typecheck, or generation.
- Verification is limited to `go build ./...` and `go vet ./...`.
- Do not commit.

## Tasks

- [x] Audit the pre-existing overlapping worktree diff and preserve unrelated edits.
- [x] Finalize regression coverage for driver liveness, initial startup states, Groups Server-Sent Events behavior, watcher recovery callback, exact Group connection identity, and Dashboard interval resync.
- [x] Finalize the driver-liveness flag and `down` versus `recovering` publication rule.
- [x] Finalize gate-on startup `recovering` and post-publication `healthy` transitions.
- [x] Finalize Groups immediate URL suppression plus visibility/60-second background refresh.
- [x] Finalize Dashboard operator-action rendering and restart-clearance wording.
- [x] Inspect the full diff, run only the two permitted Go checks, and write the mandatory session report.
