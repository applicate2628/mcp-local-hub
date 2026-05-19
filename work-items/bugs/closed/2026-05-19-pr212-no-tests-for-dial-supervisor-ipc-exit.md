---
title: DialSupervisorIPCExit has no tests (happy path, ID mismatch, graceful_exit_initiated=false)
severity: low
found-by: qa-engineer
found-in-phase: PR #212 r6 QA review
affected-surface: internal/api/supervisor_ipc_exit_client.go
context: feat/gui-supervisor-lifecycle
status: closed
closed-by: PR #214
closed-at: 2026-05-19
---

# Closure note

Closed by PR #214 (commit added internal/api/supervisor_ipc_exit_client_test.go).

Four tests added:
- TestDialSupervisorIPCExit_HappyPath — drives happy-path "exit" cmd → graceful_exit_initiated=true
- TestDialSupervisorIPCExit_NoSupervisor — no lock owner sidecar → ErrSupervisorIPCUnavailable
- TestDialSupervisorIPCExit_HandshakeMismatch — wrong hello PID/StartedAt → error before sending request
- TestDialSupervisorIPCExit_NotInitiatedReturnsError — supervisor reply missing graceful_exit_initiated → client surfaces error

Inherits the same named-pipe-elevation constraint as TestDialSupervisorIPCStatus_HappyPath — runs cleanly on interactive Windows session, sandboxed CI may report Access is denied per pre-existing pattern (work-items/bugs/2026-05-12-internal-api-suite-hangs-on-windows.md scope).
