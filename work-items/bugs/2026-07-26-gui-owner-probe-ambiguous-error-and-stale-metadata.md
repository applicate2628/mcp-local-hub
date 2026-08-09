---
title: GUI owner probe still relaunches on ambiguous platform errors and can suppress dead-owner recovery forever
severity: high
found-by: qa-engineer
found-in-phase: round-3 re-verification
affected-surface: internal/gui/probe_windows.go; internal/gui/probe_linux.go; internal/gui/single_instance.go; internal/cli/supervise_ensure_alive.go
context: blocking fix-round residual
status: open
related-work-item: 2026-07-25-liveness-headless-gui-recovery
---

## Defect

The tri-state consumer is safe only after a trustworthy verdict exists.
Windows still maps every `OpenProcess` error except access denied, and every
`GetExitCodeProcess` error, to `Alive:false`
(`internal/gui/probe_windows.go:39-57`). Linux maps every `kill(pid, 0)` error
except `EPERM` to `Alive:false` (`internal/gui/probe_linux.go:84-90`).
`probeOnce` converts those non-definitive results to `VerdictDeadPID`
(`internal/gui/single_instance.go:948-952`), which authorizes the destructive
headless-fleet relaunch.

The opposite failure is also present. Missing, unreadable, corrupt, or
out-of-range pidport data becomes `VerdictMalformed`
(`internal/gui/single_instance.go:840-870`) and then `Unknown`. With a running
supervisor, `Unknown` suppresses every tick with no independent bounded
confirmation path (`internal/cli/supervise_ensure_alive.go:998-1004`), so a
genuinely dead GUI can remain unrecovered indefinitely.

## Evidence

The exact current-head probe/consumer tests pass. Mutating the mapping, the
running-supervisor `Unknown` guard, or the supervisor-down routing made each
named test fail. Those proofs cover the explicit tri-state guards but not the
platform-error producer paths or eventual recovery after persistent malformed
metadata.

## Fix direction

Carry process-inspection failures as `Unknown` rather than coercing them to
`Alive:false`. Add a bounded independent confirmation/recovery mechanism for
persistent missing or malformed pidport state. Add tests for transient platform
identity errors and for eventual recovery of a genuinely dead owner with
missing/corrupt metadata.

## Terms and Abbreviations

- Pidport: the GUI metadata record containing its process identifier and port.
- GUI: graphical user interface.
