---
title: gui-events.log cross-process flock release failure is discarded — a stranded lock blocks every GUI event write with no signal
severity: medium
found-by: $architect, class sweep while closing the REVISE on PR feat/liveness-headless-gui-recovery
affected-surface: internal/api/gui_event_log.go:164 (AppendGUIEventLog)
context: adjacent-finding
status: open
---

## What is wrong

`(*API).AppendGUIEventLog` acquires a cross-process flock on `gui-events.log` and
releases it with:

```go
defer func() { _ = lock.Unlock() }()
```

If `UnlockFileEx` fails, the process keeps the flock for the rest of its lifetime and
every other emitter into `gui-events.log` blocks — with the verdict discarded, so
nothing on any channel reports it. This is the same defect class as the
`supervisor-events.log` one fixed on 2026-07-27, at the same height, on a different
lock.

## Why it was NOT fixed in that change

The unit of the invariant is the LOCK. `supervisor-events.log`, `gui-events.log`, and
`intent-audit.log` are three distinct flocks with three distinct blast radiuses and
three distinct reporting surfaces, so each needs its own owner. Folding all three into
a PR whose admitted scope was headless GUI recovery would have been scope expansion.

Decision that established the pattern to follow:
`work-items/decisions/2026-07-27-supervisor-event-flock-release-single-owner.md`.

## Suggested shape

Mirror `internal/api/supervisor_event_lock_health.go`: one process-scoped owner keyed
by log path, fed from the release site, read by whoever reports. Note that
`AppendGUIEventLog` resolves its own path via `DaemonStateDir()` rather than taking an
injected path, so the key derivation differs from the supervisor-event-log case.

## Not yet verified

`ASSUMPTION (UNVERIFIED)`: that `gui-events.log` has a reporting surface that would
consume the verdict (the supervisor-events.log case had `AuditHandoff` on the
daemon-recover response). Resolved by inventorying `AppendGUIEventLog` callers and
asking whether any of them reaches an operator-visible response.
