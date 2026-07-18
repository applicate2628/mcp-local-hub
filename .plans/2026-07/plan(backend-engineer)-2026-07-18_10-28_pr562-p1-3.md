# PR #562 P1-3 Reconcile-on-Rotation Plan

**Goal:** Ensure every successfully persisted adversarial Instance ID rotation immediately surfaces the existing reconcile-required operator state, even when the restart driver is then cancelled.

**Architecture:** Keep the change inside the hub-listener restart owner. After the existing rotation callback returns success, call the server's existing `hubHealth.markReconcilePending()` owner; retain the post-rebind event for its established audit fields. Document the accepted P1-1/P1-2 classifier residual without changing the gate.

**Tech Stack:** Go, existing GUI hub restart driver and health tracker.

## Global Constraints

- Do not use Graphify or the Claude command-line interface.
- Do not set `MCPHUB_GUI_SPAWN_TESTS`.
- All command-line interface and application programming interface tests use `-tags=test_state_path_env`.
- Do not commit.

## Implementation

- [x] Add `TestHubListenerRestartDriverRotationPersistsThenCancelStillNeedsReconcile` in `internal/gui/hub_listener_initial_bind_restart_test.go`.
- [x] Run that test with `-tags=test_state_path_env` and confirm it fails because health is not `needs-reconcile`.
- [x] Call `s.hubHealth.markReconcilePending()` immediately after `rotateHubInstanceIDFn` succeeds in `internal/gui/hub_listener.go`.
- [x] Add the operator-approved P1-1/P1-2 bounded residual comment beside `hubInitialBindPortNeedsInstanceIDRotationWithDeps`.
- [x] Re-run the focused test, formatting checks, `go build ./...`, `go vet ./...`, and the mandated tagged application programming interface and graphical user interface suite.

## Terms and Abbreviations

- API — Application Programming Interface.
- GUI — Graphical User Interface.
- P1 — Priority 1 review finding.
