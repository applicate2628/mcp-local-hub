---
status: resolved
severity: medium
found-by: pre-catch review (PR #377 exhaustive r9 Workflow)
---

# serena workspace-proxy ignores the per-daemon env overlay

## Summary

The GLOBAL daemon launch path (`internal/cli/daemon.go` `daemonEnvWithOverlay`)
merges the manifest env with the per-daemon env OVERLAY the operator sets via
the GUI (Settings → Server env overrides → `daemonOverlayEnv`). The
SERENA-PROXY launch path (`internal/cli/daemon_serena.go`) resolves
`spec.EnvRefs` via `ResolveMapBestEffort` directly and builds the
`HTTPHostConfig.Env` WITHOUT calling `daemonOverlayEnv` — so an operator env
override targeted at a serena workspace daemon is silently CLOBBERED/STRIPPED
(never reaches the child). A per-workspace serena needing e.g. a custom
`SERENA_*` or proxy env var cannot be configured via the GUI overlay.

## Scope

Pre-existing (NOT introduced by PR #377 — daemon_serena never applied the
overlay). Surfaced by the #377 exhaustive pre-catch review. Deferred OUT of
#377 (whose scope is readiness + optional-secret), tracked here.

## Fix direction

Apply the same overlay in the serena-proxy path: after
`ResolveMapBestEffort(spec.EnvRefs)`, merge `daemonOverlayEnv(server,
daemonName)` (the global path's `mergeDaemonEnvMaps` is the single owner of the
merge order). The serena daemon key/name for the overlay lookup is the
workspace daemon's task/spec identity. Verify the overlay UNSET semantics
compose with the optional-secret UnsetEnv plumbing.

_2026-06-21: FIXED — the serena-proxy wrapper now applies the per-daemon operator env overlay via the task-name-keyed mergeResolvedDaemonEnvWithOverlay owner (same overlay-wins merge as the global daemon path), keyed by the descriptor taskNameFlag. Architect-designed (single-owner), verified._
