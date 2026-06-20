---
status: open
severity: P2
date: 2026-06-20
slug: serena-stale-daemon-session-after-idle-stop
discovered-by: codex different-model subsystem review (2026-06-20, parallel to PR #384)
---

# Stale serena daemon-session binding reused after an idle-stop

## Symptom (potential)

After the idle sweeper stops a serena daemon, the daemon-session binding for that workspace can
be left valid; a concurrent wake clears the idle-stop marker, and a later request reuses the
STALE daemon-session id (minted against the now-stopped/replaced daemon generation) instead of
re-handshaking — leading to a session the upstream no longer recognizes.

## Root cause (codex review)

In `SweepIdleSerenaDaemons`, after a successful idle-stop write (`wrote == true`,
`internal/gui/serena_idle_sweeper.go:328`), the daemon-session invalidation depends on
RE-READING an active idle stop. A concurrent wake can CLEAR the idle stop before this check
runs, so the invalidation is skipped and the stale `serenaDaemonSessions` binding survives to be
reused by `resolveDaemonSession` (`internal/gui/serena_router_handshake.go:883`).

## Scope

PRE-EXISTING in the broader serena idle-sweeper subsystem — NOT introduced by the backend-loss
feature (PR #384). Surfaced by a codex subsystem review run alongside #384; filed separately.

## Proposed fix

On `wrote == true`, invalidate `serenaDaemonSessions` for the workspace UNCONDITIONALLY (or
under the same stop gate proposed for the sibling race) BEFORE any wake can clear the idle
marker, so the stop-then-invalidate is atomic w.r.t. a concurrent wake.

## Related

Sibling race: [[2026-06-20-serena-idle-stop-races-inflight-forward]] (the idle-stop vs
in-flight-forward half). Best fixed together (one shared per-workspace stop/forward gate
resolves both).
