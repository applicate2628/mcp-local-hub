# Daemon Recovery Destructive-Path Implementation Plan

**Goal:** Close the three reviewed destructive-path defects without changing the held-process primitive or widening kill authority.

**Architecture:** `internal/daemonrecovery` remains the single recovery orchestrator. Its injected port-owner dependency accepts the active context, the pre-commit path uses the request context, and a committed kill creates separate bounded port-wait and respawn phases. The held generation remains the sole identity/termination authority.

**Tech stack:** Go 1.26.2, `context`, existing `api.LoopbackPortOwnerPIDContext`, existing `process.HeldPIDGeneration`.

## Global Constraints

- No `internal/process` redesign or signature change.
- No external/model helpers, multi-agent work, graph tools, commits, or prohibited Go test targets.
- Preserve every existing refusal and audit path; boundary order is identity, context, ownership, terminate.
- Once termination commits, port-wait expiry is reported but cannot skip the single bounded respawn attempt.

### Task 1: Context-govern every owner probe

**Files:**
- Modify: `internal/daemonrecovery/recovery.go`
- Modify: `internal/daemonrecovery/recovery_test.go`
- Modify only if required by the existing CLI test seam: `internal/cli/daemon_recover.go`

**Interface:** `Dependencies.PortOwner` becomes `func(context.Context, int) (pid int, ok bool, err error)` and production wiring uses `api.LoopbackPortOwnerPIDContext`.

- [ ] Add `TestExecuteBlockingPortProbeReturnsOnContextDeadlineAndClosesHeldGeneration` with a blocking boundary probe that returns only when its received context is done; assert deadline return, zero terminate, zero respawn, and one generation close.
- [ ] Run the focused test and verify RED from the old context-free dependency shape/behavior.
- [ ] Thread the current governing context through initial, boundary, and wait probes; do not create a background context for a probe.
- [ ] Run the focused test and verify GREEN.

### Task 2: Separate post-commit wait and respawn budgets

**Files:**
- Modify: `internal/daemonrecovery/recovery.go`
- Modify: `internal/daemonrecovery/recovery_test.go`
- Modify: `internal/gui/daemon_recover.go`
- Modify GUI/CLI tests only where the newly reported result field changes their contract assertions.

**Interface:** `Result` gains a validated, safe enum describing the post-kill port-wait outcome; the GUI response includes it. Port wait gets `PortWaitTimeout`; respawn gets an independent `PostKillTimeout` context created after the wait.

- [ ] Add `TestExecuteCommittedKillPortNeverFreesStillRespawnsOnceAndReportsPortWaitOutcome`; assert one committed terminate, one live-context respawn, success result, and surfaced still-bound outcome/notification.
- [ ] Run the focused test and verify RED because the old wait deadline returns before respawn.
- [ ] Make wait expiry best-effort, preserve its notification, then create a fresh bounded detached respawn context and attempt respawn exactly once.
- [ ] Run the focused test and verify GREEN.

### Task 3: Reorder the kill boundary

**Files:**
- Modify: `internal/daemonrecovery/recovery.go`
- Modify: `internal/daemonrecovery/recovery_test.go`

**Interface:** No new public process surface. The destructive boundary order becomes `HeldPIDGeneration.VerifyIdentity` -> request-context check -> contextual owner probe -> `HeldPIDGeneration.Terminate`.

- [ ] Strengthen `TestExecutePortOwnerChangedAtKillBoundaryDoesNotTerminate` to preserve zero termination after the reorder.
- [ ] Add `TestExecuteIdentityMismatchAtKillBoundarySkipsOwnershipProbeAndTerminate`; assert only the initial classification probe occurs.
- [ ] Run the focused tests and verify RED on the ownership-probe count/order assertion.
- [ ] Reorder the existing checks without changing refusal/audit semantics.
- [ ] Run the focused tests and verify GREEN.

### Task 4: Verification and reconciliation

- [ ] Run all focused `internal/daemonrecovery` tests.
- [ ] Inspect the final diff for scope, background probe contexts, destructive call order, and unchanged `internal/process` signatures.
- [ ] Run `go build ./...`.
- [ ] Run `go vet ./...`.
- [ ] Run `go test -count=1 ./internal/process/ ./internal/daemonrecovery/ ./internal/gui/`.
- [ ] Record the mandatory session report and final file:line/test evidence. Do not commit.

## Security Claims

1. `{ guarantee: every recovery port-owner probe is canceled by its governing phase context; single-owner: daemonrecovery Dependencies.PortOwner contract; enforcement-probe: TestExecuteBlockingPortProbeReturnsOnContextDeadlineAndClosesHeldGeneration }`
2. `{ guarantee: identity mismatch or canceled request cannot reach ownership recheck or termination; single-owner: daemonrecovery kill-boundary ordering; enforcement-probe: TestExecuteIdentityMismatchAtKillBoundarySkipsOwnershipProbeAndTerminate plus TestExecuteCanceledBeforeKillDoesNotTerminate }`
3. `{ guarantee: once termination commits, port-wait expiry cannot suppress the one bounded respawn attempt; single-owner: daemonrecovery post-commit phase orchestration; enforcement-probe: TestExecuteCommittedKillPortNeverFreesStillRespawnsOnceAndReportsPortWaitOutcome }`

