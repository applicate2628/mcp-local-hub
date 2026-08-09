---
title: Registry/trusted-roots reads on a broadened-parent host emit a hub-mcp.log write, reachable from the read-only route daemon
severity: low
found-by: backend-engineer, P1-1/P2-3 adversarial-review fix round (mcphub-front-daemon Increment 1)
affected-surface: internal/api/hub_mcp_state_read_inode_windows.go, internal/api/hub_mcp_state_read_inode_posix.go (readStateFileInodeAnchored's "unhardened parent" relax-fallback diagnostic)
context: adjacent-finding
status: fixed
fixed-by: backend-engineer, round-3 adversarial cross-family review (finding 1), 2026-07-26
---

## Resolution (2026-07-26)

Fixed as finding 1 of the round-3 review pass. The reviewer established this
was in scope (not merely adjacent) because it reopened the branch's central
READ-ONLY claim.

An injectable diagnostic-sink seam was threaded through the shared
inode-anchored state reader:

- `readStateFileInodeAnchoredWithOptions` (both the Windows and POSIX legs)
  gained a trailing `auditSink` parameter; every existing call site now
  passes `LogHubMcpEvent` explicitly, preserving prior behavior exactly.
- `api.ReadStateFileInodeAnchoredWithAuditSink(path, sink)` is the new
  per-call (not process-global) entry point — a nil sink is exactly
  equivalent to the old default.
- `api.Registry.SetAuditSink(sink)` lets a caller redirect the sink `Load()`
  uses; `api.LSPWorkspaceRootTrustedWithAuditSink(root, sink)` does the same
  for the trusted-root read.
- `api.RouteReadOnlyStderrSink` (moved from `internal/gui.routeReadOnlySink`,
  which now delegates to it) is the concrete stderr-only sink. It lives in
  `internal/api` — the lowest common package both `internal/gui` and
  `internal/api/serena_routing`/`internal/api/lsp_routing` already import —
  so no package gained a new layering-violating dependency.
- `internal/cli/route.go`'s `buildRouteServer` calls `reg.SetAuditSink(...)`
  on both `*Registry` instances (serena + LSP) right after construction, and
  `internal/gui/lsp_router.go`'s `SetLSPRouterReadOnly` wires
  `TrustedRootCheckFn` through the audit-sink variant.

Regression coverage: `internal/cli/route_broadened_parent_windows_test.go`
(two new tests, real production `buildRouteServer` construction path, a
genuinely broadened — not hardened — state-dir DACL) proves the relax
fallback still fires but lands on the route daemon's own stderr sink, never
`hub-mcp.log`/`hub-mcp.log.lock`. Both tests were mutation-proven: reverting
either `SetAuditSink` call or the `TrustedRootCheckFn` closure makes the
corresponding test fail with the file present, confirming the guard is
load-bearing.

## What happened

While writing the P1-1 regression test
(`TestSetSerenaRouterReadOnly_RegisteredWorkspaceUnreachableBackend_NoSharedStateFileWrite`,
`internal/gui/route_readonly_test.go`), a whole-state-directory before/after
snapshot diff caught an EXTRA `hub-mcp.log` write that was NOT coming from
either router's own `AuditFn` path (the thing P1-1 fixed). Root-caused via a
scratch debug test: the write's content was
`"event":"hub-mcp-state-read-unhardened-parent-fallback"`, emitted directly
from `readStateFileInodeAnchored` (via `api.LogHubMcpEvent`, hardcoded, no
seam) whenever the read helper's parent-directory DACL gate hits the
documented "default-relax-on-solo-host" fallback while reading
`workspaces.yaml` (i.e. on every `Registry.Load()` a resolver's `refresh()`
performs, INCLUDING the new read-only resolver's unlocked reload P2-3 added).

This is a DIFFERENT emit site than the ones P1-1's review named
(`serena_router.go`, `lsp_router.go`) — it lives in the shared, low-level
hardened state-file READ helper used by virtually every state/registry read
across the whole codebase (GUI, CLI, and now the route daemon alike), not in
either router's own code.

## Why this matters for the route daemon specifically

On a host where the state-dir's parent directory ACL is broadened to
non-owner principals (a documented, by-design, NOT rare condition on
corp-managed or certain sandboxed hosts — see CLAUDE.md's "Hardened
client-config writes + corp-policy posture" section), EVERY registry read the
read-only route daemon's resolver performs (via `Registry.Load()` inside
`refresh()`) will ALSO emit this warn event to the SHARED, GUI-owned
hub-mcp.log — the exact same class of violation P1-1 fixed, just via a
different, lower-layer code path outside the two files that review named.

## Why not fixed here

This function has NO existing seam for a caller to redirect its diagnostic
sink (unlike the router's `AuditFn`, which P1-1 could inject
`routeReadOnlySink` into). Fixing it properly would mean threading a
caller-injectable audit sink through `readStateFileInodeAnchored` and EVERY
one of its many callers across `internal/api` (registry, supervisor-intent,
trusted-roots, ...) — a cross-cutting change to a stable, widely-shared
low-level primitive, far outside the two-file change surface P1-1 named and
outside this backend-engineer's approved scope for this fix round. Per the
architecture-layering-hygiene "Edit the adapter, not the backend" /
"REVISE-to-architect when the seam is missing" guidance, this needs an
architect-level decision on where the injected diagnostic port for this
shared read helper should live, not an implementer-forced scenario-specific
edit.

The mitigation the P1-1 regression test itself applied (hardened,
owner-only-DACL parent directories via `apitest.HardenedTempDir`) sidesteps
the relax-fallback entirely and is what the codebase already recommends via
`MCPHUB_REQUIRE_SINGLE_USER_HOME=1` for solo-host operators who want the
strict gate. On a host that has NOT set that env var, this residual write
path is real but bounded: it is a single `warn` diagnostic line, not a
functional state mutation, and it already exists for every OTHER
process/reader in the codebase today (GUI included) — the route daemon
merely inherits the pre-existing shared-helper behavior rather than
introducing a new one.

## Suggested follow-up

Add a caller-injectable diagnostic-sink parameter (or a package-level,
overridable default) to `readStateFileInodeAnchored`'s relax-fallback emit,
and thread `routeReadOnlySink` into it from the route daemon's registry/
trusted-roots reads — architect input needed on whether this seam belongs on
`readStateFileInodeAnchored` directly or on a higher-level per-caller wrapper
(e.g. a `Registry` option), given how many call sites already depend on the
current signature.
