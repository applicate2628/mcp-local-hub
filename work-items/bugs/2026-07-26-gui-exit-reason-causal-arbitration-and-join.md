---
title: GUI exit reason remains causally ambiguous and RunE join is not regression-guarded
severity: medium
found-by: qa-engineer
found-in-phase: round-3 re-verification
affected-surface: internal/gui/gui_exit_reason.go; internal/cli/gui_exit_signal.go; internal/cli/gui.go; internal/gui/gui_self_restart.go
context: blocking fix-round residual
status: open
related-work-item: 2026-07-25-liveness-headless-gui-recovery
---

## Defect

`sync.Once` chooses the first caller to reach the emitter, not necessarily the
causal exit trigger (`internal/gui/gui_exit_reason.go:68-71`). When a signal
and context cancellation are both ready, the signal observer's select has no
signal priority and may skip attribution
(`internal/cli/gui_exit_signal.go:42-54`). The current first-trigger test is
sequential rather than a simultaneous race
(`internal/gui/gui_exit_reason_test.go:108-131`).

Normal RunE returns join the observer (`internal/cli/gui.go:499-519`), but
removing all `WaitGroup` wiring left every selected relevant test green.
Self-restart calls an `os.Exit` seam and bypasses RunE defers, including the
join (`internal/gui/gui_self_restart.go:173-186`).

## Evidence

Removing `sync.Once` or the context-aware select made their named tests fail.
Removing `WaitGroup.Add`, `Done`, and `Wait` together left all four helper tests
and two GUI command-construction tests passing, so the join mutation claim does
not hold.

## Fix direction

Introduce explicit causal-trigger arbitration, make simultaneous
signal/context handling deterministic, add a process-free RunE seam test that
fails when the join wiring is removed, and directly test the self-restart
synchronous attribution contract.

## Terms and Abbreviations

- GUI: graphical user interface.
- RunE: the Cobra command execution callback used by the GUI command.
