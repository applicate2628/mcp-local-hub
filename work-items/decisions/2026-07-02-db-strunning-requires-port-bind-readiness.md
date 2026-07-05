# D-B: StRunning requires port-bind readiness, not process-start

- **status:** proposed (deferred — not scheduled for implementation)
- **filed:** 2026-07-02
- **context:** supervisor health semantics (from the P1b design discussion in `.plans/2026-07/plan(main)-2026-07-02_01-49_supervisor-lost-child-fix-design.md`)

## Decision (proposed)

Change the supervisor's daemon-health definition so `StRunning` is entered on an observed port-bind by the spawned PID (readiness), not on `cmd.Start` success (process-start). Today `EvHealthOK` is posted on bare spawn success (`supervisor_controller.go` spawn side effect — "health = process-start" comment), and P1b (#488) compensates operationally with a first-bind deadline in the liveness sweep rather than changing the SM.

## Why deferred

Touches SM transition rows, `postManualRestartAndWaitRunning`'s StRunning wait, and the GUI/status meaning of "Running" (operator-visible semantics change). P1b removed the operational damage (slow-binding daemons are no longer killed mid-startup) without that surgery. Revisit if a future need requires true readiness semantics (e.g. auto-remediation keyed on Running, or SLO reporting).

## Interaction

- P1b (shipped in #488): per-descriptor `StartupBindDeadlineSeconds` — the operational stand-in.
- P2c (#489): LSP proxies stamp 120s; probation/warmed is the daemon-side readiness analog.
