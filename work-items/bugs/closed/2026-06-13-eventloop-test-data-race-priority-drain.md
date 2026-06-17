---
title: TestEventLoop_PriorityDrainsSelfBeforeMain has a data race (test-side, pre-existing) flagged under -race
severity: low
found-by: backend-engineer
found-in-phase: PR #302 r3 (root-orphan-reap follow-up)
affected-surface: internal/api/supervisor_event_loop_test.go
context: adjacent-finding
status: closed
fixed-in: acf9f7f (PR #315) — mutex barrier added around the order-tracking slice
---

## Resolution

Fixed in commit `acf9f7f` (PR #315, "test,ci: harden test-infra
isolation + add CI tagged-test/symbol/bundle gates"). The suggested
fix below was applied: the order-tracking slice `got` is now guarded
by a `sync.Mutex`, the handler appends under the lock, and the test
goroutine reads via a `snapshot()` helper that copies the slice under
the same lock. This installs the missing happens-before barrier
between the loop goroutine's writes and the assertion reads. The
priority-drain assertion (`g[0] != "self-1"`) is unchanged — the fix
is synchronization-only.

Verified clean:

```bash
go test -tags=test_state_path_env -run TestEventLoop -race -count=20 ./internal/api/
# ok  mcp-local-hub/internal/api
```

## Reproduction

```bash
go test -run 'TestEventLoop_PriorityDrainsSelfBeforeMain' -count=1 -race ./internal/api/
```

Observe `WARNING: DATA RACE` / `--- FAIL: ... race detected`.

## Root cause

The test's registered handler (`supervisor_event_loop_test.go:75`, inside
`(*EventLoop).Run`'s handler goroutine) WRITES a slice/order-tracking
variable while the test goroutine READS it at lines 92 and 100 WITHOUT
any happens-before synchronization (no channel/mutex barrier between the
loop goroutine's writes and the test goroutine's assertion reads). This
is a TEST-HARNESS bug, not a defect in production `EventLoop` code
(`supervisor_event_loop.go` is correct — the race is between the test's
own observation slice append and its read).

## Evidence it is pre-existing and NOT introduced by PR #302 r3

- `internal/api/` is UNMODIFIED by the r3 change (`git diff --stat
  internal/api/` is empty).
- The race reproduces on the base tree with the r3 changes stashed
  (verified by `git stash` + re-run).
- The race anchors are `supervisor_event_loop_test.go:75/92/100`, a file
  the r3 change never touches.

## Why not fixed in PR #302 r3

Out of the approved change surface (the orphan-reap controller fix, which
touches only `internal/cli/`). Filed per the adjacent-findings protocol;
the orchestrator decides priority. The r3 orphan-reap tests (which DO run
their EventLoop concurrently) are `-race`-clean — this is purely a
sibling test's own synchronization gap.

## Suggested fix (for whoever picks this up)

Add a synchronization barrier in the test between the handler's writes and
the assertion reads — e.g. have the handler signal completion on a channel
the test waits on before reading the order slice, or guard the slice with a
mutex. Do not weaken the priority-drain assertion.
