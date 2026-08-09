---
title: Daemon recovery can duplicate or block an audit after EmitWithTimeout returns
severity: high
found-by: qa-engineer
found-in-phase: round-3 re-verification
affected-surface: internal/api/supervisor_events.go; internal/daemonrecovery/recovery.go
context: blocking fix-round residual
status: open
related-work-item: 2026-07-25-liveness-headless-gui-recovery
---

## Defect

`EmitWithTimeout` now returns at the outer deadline, but its abandoned worker
continues holding both event-log locks and may append later
(`internal/api/supervisor_events.go:408-430`). Daemon recovery treats the
timeout as permission to queue a later blocking audit
(`internal/daemonrecovery/recovery.go:511-517`) and executes that fallback after
respawn (`internal/daemonrecovery/recovery.go:546-552`).

If the original worker later completes, the same committed audit can be
written twice. If it remains stalled, the blocking fallback can wait behind
the worker indefinitely. This is a behavior change in one of the shared
helper's six callers.

## Evidence

The exact stalled-write test passes, and replacing the outer worker/select with
a synchronous write makes that test time out. The existing daemon-recovery
tests do not exercise a timed-out worker that later completes, so they do not
falsify duplicate or blocking fallback behavior.

## Fix direction

Define completion semantics for timed-out emission and make daemon recovery
idempotent against a late first write. Add a deterministic test that stalls the
worker, observes timeout and respawn, releases the worker, and then asserts
bounded completion and exactly one audit row.

## Terms and Abbreviations

- Audit: a durable supervisor event describing the recovery action.
