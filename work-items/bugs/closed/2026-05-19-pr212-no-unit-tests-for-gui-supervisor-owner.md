---
title: gui_supervisor_owner.go has zero unit tests for its core functions
severity: medium
found-by: qa-engineer
found-in-phase: PR #212 r6 QA review
affected-surface: internal/cli/gui_supervisor_owner.go
context: feat/gui-supervisor-lifecycle
status: closed
closed-by: PR #212 r7 (commit 2833ee4)
closed-at: 2026-05-19
---

# Closure note

Closed by PR #212 r7. internal/cli/gui_supervisor_owner_test.go added with 7 tests:
- TestBoundedBuffer (4 sub-tests): empty / under-cap / over-cap / multi-write
- TestProbeSupervisor_ContextCanceled
- TestSupervisorMonitorStderr_SwapForTests
- TestSupervisorOwner_StopAdoptedIsNoOp
- TestSupervisorOwner_StopNilProcReturnsError
- TestSupervisorOwner_StopIdempotent

Test for startExitMonitor end-to-end (real subprocess + Wait wiring) is deliberately skipped with t.Skip + doc comment; that path is covered by integration smoke per the test rationale recorded in the file. Coverage is sufficient for the critical boundedBuffer + adopt/spawn classification + Stop() idempotency paths.
