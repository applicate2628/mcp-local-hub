---
severity: P1
date: 2026-06-20
slug: serena-idle-stop-races-inflight-forward
discovered-by: codex different-model subsystem review (2026-06-20, parallel to PR #384)
closed-by: PR #386 (serena per-workspace stop-gate refactored into the withSerenaWorkspaceGate seam; merged 2026-06-20)
---

- **status:** fixed
- **fixed-by:** PR #386 (`edee81fe`) - shared `withSerenaWorkspaceGate` stop/forward gate.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `TRIAGE-2026-07-09.md` for code/test evidence.

# Serena idle-stop races an in-flight request's pre-forward window

## Symptom (potential)

A serena pool daemon can be idle-stopped by `SweepIdleSerenaDaemons` WHILE a new client
request for that workspace has already started but has not yet reached the forward — so the
request wakes/handshakes against a daemon the sweeper is concurrently stopping, producing a
failed or mis-routed request that should not have happened.

## Root cause (codex review, conf high)

`enterSerenaForward` (the in-flight marker the idle sweeper checks) is set only AFTER
wake/daemon-session resolution in the request path (`internal/gui/serena_router.go:1035`), so
`SweepIdleSerenaDaemons`'s in-flight check can pass — seeing no in-flight request — and proceed
to idle-stop the daemon during the window between "request started" and "enterSerenaForward set".

These file:line references are the PRE-#386 locations and have since moved. PR #386 closed this window
by refactoring the five forward sites into the single `withSerenaWorkspaceGate` seam (which marks
in-flight BEFORE resolve) and adding the `beginPhase` `waiters > 0` invariant so a sweeper phase cannot
start in front of a request already waiting — see
[`work-items/decisions/2026-06-20-serena-gate-uniform-seam.md`](../decisions/2026-06-20-serena-gate-uniform-seam.md).

## Scope

PRE-EXISTING in the broader serena router/idle-sweeper subsystem — NOT introduced by the
backend-loss feature (PR #384). Surfaced by a codex subsystem review run alongside #384; filed
separately per review discipline (do not bundle a pre-existing subsystem race into the
feature PR).

## Proposed fix

Add a shared per-workspace stop/forward gate, OR mark/recheck in-flight BEFORE wake/handshake
(at request entry) and re-check it immediately before the idle-stop write, so a request that
has started cannot be stopped as idle. Regression test: engineer the window (a slow wake) and
assert the sweeper does not stop a daemon with a request in its pre-forward window.

## Related

Sibling race: [[2026-06-20-serena-stale-daemon-session-after-idle-stop]] (the idle-stop
invalidation half). The two are best fixed together (same stop-gate seam).

_Closed 2026-06-21: done-by-tonight — #386 shared withSerenaWorkspaceGate stop-gate._
