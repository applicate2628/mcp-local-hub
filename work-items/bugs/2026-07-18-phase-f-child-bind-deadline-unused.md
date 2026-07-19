# Bug: Phase-F restart child does not apply the bind deadline

- id: 2026-07-18-phase-f-child-bind-deadline-unused
- context: 2026-07-16-productization-gui-solidify
- status: open
- severity: medium
- area: internal/cli/gui.go
- found-by: qa-engineer

Reproduction: inspect `runRestartV3ChildStartup` after constructing the restart listener owner. At `internal/cli/gui.go:122-123`, the child passes the long-lived runtime context directly to `BindStandby`; no production call in the Phase-F path reads `cfg.Deadlines.Bind`. The accepted plan assigns the shared bind deadline to the Phase-F standby path (`work-items/active/2026-07-16-productization-gui-solidify/item3-unitB-plan.md:134,166-184`).

Expected: create a child bind context bounded by `cfg.Deadlines.Bind`, use it only for `BindStandby`, and keep `GUIReadHeaderTimeout` and the separate standby-close budget independent.

Actual: `GUIReadHeaderTimeout` is correctly 10 seconds and standby close correctly owns 2 seconds, but standby binding itself is not bounded by the configured bind deadline. `TestRestartV3_StandbyCloseUsesOwnBudgetNotBindDeadline` proves only that close ignores an injected 37-second bind value, so it lets this missing production bind-deadline use pass.

Classification: regression/contract omission in the Phase-F implementation. Gate remains `REVISE` until a deterministic test blocks the bind and proves timeout/cancellation at the configured deadline, followed by the full tagged guard.
