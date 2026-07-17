# Daemon Recovery Round 6 Implementation Plan

**Goal:** Close findings 1a-9 without widening kill authority or changing the held-generation and verdict designs.

**Architecture:** `internal/daemonrecovery` remains the single operator recovery owner. Before a verified-own kill it proves supervisor reachability and reserves a detached respawn slice; after a committed kill it reports confirmed and unconfirmed termination separately and dispatches respawn before adapter notifications. The shared classifier accepts only one parser-effective value for each task discriminator.

**Tech stack:** Go, existing supervisor status/respawn inter-process communication (IPC), injected hermetic process seams.

## Global Constraints

- No external/model helpers, graph tools, subagents, commits, or prohibited Go test targets.
- Preserve the held-generation primitive, classifier verdict model, destructive-boundary re-reads, and unrelated dirty-worktree changes.
- Never terminate unless a nonzero respawn reservation and a reachable supervisor have been established.
- A committed termination with an unconfirmed wait result still forces respawn but never claims `reaped`.

### Task 1: Pin destructive-path recovery failures

**Files:**
- Modify: `internal/daemonrecovery/recovery_test.go`

- [ ] Add a full-termination-bound test that requires respawn to receive a live detached deadline.
- [ ] Add an insufficient-reservation test that requires zero terminate and zero respawn.
- [ ] Add an unreachable-supervisor test that requires `FailureSupervisorUnavailable` before terminate.
- [ ] Add a committed-plus-wait-error test that requires one unconfirmed event with the wait error, zero reap claims, and one respawn.
- [ ] Add a panicking-notify test that proves respawn was dispatched first.
- [ ] Strengthen boundary-probe cancellation to require `stage=boundary_port_probe` and exactly one state read.

### Task 2: Implement the recovery invariant

**Files:**
- Modify: `internal/daemonrecovery/recovery.go`
- Modify: `internal/cli/daemon_recover.go`

- [ ] Inject the existing supervisor status probe with a bounded production timeout.
- [ ] Validate and reserve `RespawnReserve` before terminate; give port wait only the nonreserved remainder.
- [ ] Split confirmed reap from committed-but-unconfirmed termination and preserve the wait error in the audit event.
- [ ] Dispatch respawn before any post-commit adapter notification.

### Task 3: Reject ambiguous task discriminators

**Files:**
- Modify: `internal/daemonrecovery/classifier.go`
- Modify: `internal/daemonrecovery/recovery_test.go`

- [ ] Add duplicate/conflicting flag cases for `--task-name`, `--workspace`, `--language`, `--server`, and `--daemon`, asserting refusal for every candidate value and zero terminate.
- [ ] Parse one effective long-flag value (`--flag value` or `--flag=value`) up to `--`; duplicates fail closed.

### Task 4: Make the automatic event honest

**Files:**
- Modify: `internal/cli/supervise_squatter.go`
- Modify: `internal/cli/supervise_lostchild_f1_f3_test.go`

- [ ] Add a hermetic automatic-trigger test for `ErrProcessAlreadyExited` requiring zero `reaped` events and one `already-exited` event.
- [ ] Split the event while preserving the proceed-to-respawn outcome.

### Task 5: Reconcile documentation and verify

**Files:**
- Modify: `work-items/backlog/2026-07-16-daemon-recovery-followups.md`
- Modify: `work-items/active/2026-07-16-productization-gui-solidify/item2-recover-design.md`
- Create: `.reports/2026-07/report(backend-security)-2026-07-17_round6-daemon-recovery.md`

- [ ] Replace the false no-data-loss claim with the exact hard-restart bound.
- [ ] Describe the shared-operation migration and detached post-commit respawn context.
- [ ] Run focused red/green tests, format, inspect the diff, then run the exact authorized build/vet/test matrix.
- [ ] Record the single-pass backend/security result and final claim probes without committing.
