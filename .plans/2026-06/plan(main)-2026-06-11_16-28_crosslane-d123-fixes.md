# Crosslane D123 Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix D1, D2, and D3 with falsifying tests, required verification, and one local commit.

**Architecture:** Keep changes in the existing owners: `stop_force_supervisor.go` for force-stop kill classification, serena router session reconciliation for idle wake state, and `install_parsed_manifest.go` for supervisor-intent PID capture. Tests use existing package seams and temp state roots only.

**Tech Stack:** Go, existing package-level test seams, `gofrs/flock` test probes, repo-standard `go build`, `go vet`, `go test`, and `gofmt`.

---

### Task 1: Diagnostics And RED Tests

**Files:**
- Modify: `internal/api/lane_a_internal_review_test.go`
- Modify: `internal/gui/serena_idle_reconcile_test.go`
- Modify: `internal/api/install_parsed_manifest_lane_b_test.go`

- [x] Capture code evidence and live-state baseline.
- [x] Add D1 tests for PID identity refusal falling through to port classification and stale PID-kill error fallback.
- [x] Add D2 tests for post-idle wake grace and no-respawn expiry.
- [x] Add D3 flock-probe tests around IPC status capture.
- [x] Run focused tests and confirm each new failure matches the reported pre-fix defect.

### Task 2: Minimal Fixes

**Files:**
- Modify: `internal/api/stop_force_supervisor.go`
- Modify: `internal/gui/serena_router_session.go`
- Modify: `internal/gui/server.go`
- Modify: `internal/api/install_parsed_manifest.go`

- [x] D1: Fall through to the port classifier on PID identity refusal and narrow already-gone PID-kill errors; preserve genuine PID-kill failures as error rows and include PID context in later port errors.
- [x] D2: Promote `serenaBackendIdlePaths` from boolean marker to bounded remaining-grace ticks.
- [x] D3: Move removed-target PID capture before the supervisor-intent flock, while recomputing the authoritative removed set under the lock.

### Task 3: Verification And Commit

- [x] Run focused green tests.
- [x] Run the full requested verification matrix.
- [x] Verify touched files are formatted.
- [x] Verify live supervisor-intent state is byte-identical to the pre-test baseline.
- [x] Create one local commit and do not push.
