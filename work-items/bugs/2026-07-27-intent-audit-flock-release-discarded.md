---
title: intent-audit.log cross-process flock release failure is discarded — a stranded lock blocks the intent audit trail with no signal
severity: medium
found-by: $architect, class sweep while closing the REVISE on PR feat/liveness-headless-gui-recovery
affected-surface: internal/api/intent_audit.go:494 (the intent-audit append path)
context: adjacent-finding
status: open
---

## What is wrong

The intent-audit append acquires a cross-process flock on `intent-audit.log` and
releases it with:

```go
defer func() { _ = lock.Unlock() }()
```

A failed release strands the flock for the process's lifetime and blocks every other
intent-audit writer, with the verdict discarded so nothing reports it. Same defect
class as the `supervisor-events.log` one fixed on 2026-07-27, different lock.

This one carries extra weight because the surface is an AUDIT TRAIL: a stranded lock
means subsequent audit rows from other processes stall or are lost, which is exactly
the property an audit trail exists to guarantee.

## Why it was NOT fixed in that change

Same reason as the `gui-events.log` sibling: the unit of the invariant is the LOCK, so
each of the three log families needs its own owner and carries its own blast radius.
Bundling them into a GUI-recovery PR would have been scope expansion.

Decision that established the pattern to follow:
`work-items/decisions/2026-07-27-supervisor-event-flock-release-single-owner.md`.

## Suggested shape

Mirror `internal/api/supervisor_event_lock_health.go`: one process-scoped owner keyed
by log path, fed from the release site, read by whoever reports. Check whether
`writeRotatedSelfEvent` (called under the same held lock) introduces a second release
path that also needs the hook.

## Not yet verified

`ASSUMPTION (UNVERIFIED)`: that `intent-audit.log` has exactly one flock release site.
Resolved by grepping `intent_audit.go` for every `Unlock` and every early return
between acquire and release.
