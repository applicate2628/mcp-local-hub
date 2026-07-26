---
title: Increment 2 — clients point at a dedicated supervisor-managed MCP front port; the GUI keeps its own web-UI port
status: proposed
date: 2026-07-25
owner: architect (design) / lead (accepted Mechanism B)
supersedes: none
relates-to:
  - work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-supervised-front-daemon.md
  - work-items/decisions/2026-07-25-supervisor-builtin-singleton-daemon.md
  - work-items/active/2026-07-25-mcp-front-daemon/
---

## Decision
Meet the survival requirement (serena+LSP survive GUI death) by rewriting client
serena/LSP URLs to a single-owned `mcp_front.port` (default = DefaultRouteDaemonPort
9137) that the supervisor-managed `mcphub route` daemon binds — NOT by seizing the
GUI's 9125. The GUI keeps 9125 for its web UI; single-instance + RestartV3 stay
untouched. A fail-closed `mcphub install --reconcile-mcp-front` performs the one-time,
reversible client rewrite; both surfaces serve concurrently for an unbounded backward-
compat window. Route stays READ-ONLY (F1 stands); auto-register-on-miss on client
traffic becomes an explicit 503 (gated regression), restoration deferred to a follow-up (2b).

Lead accepted Mechanism B over Mechanism A (2026-07-25): B avoids the fragile
single-instance/RestartV3/port-resolution rework + a lock-less cross-process collision
handoff, for no requirement-level benefit over the client-rewrite. Verified: P=9125 is
fixed (settings-registry default, not ephemeral); the hub aggregate is a separate
listener (not on P); the single-instance flock is file-based (not port-bound). The
operator's actual clients use the DIRECT /serena/mcp + /lsp/<lang>/mcp routes (gate-OFF,
confirmed via `claude mcp list`), so the survival guarantee covers their real usage.

## Why not seize 9125 (Mechanism A)
It forces the fragile single-instance/RestartV3/port-resolution rework plus a lock-less
cross-process collision handoff, for no requirement-level benefit over the client-rewrite.
Retained as an option gated on a proven hard 9125 dependency (open probe P4).

## Scope boundary
Gate-ON hub-aggregate clients are OUT of the survival guarantee (the aggregate is a
separate in-process GUI listener); tracked separately, pending open probe P3.

## Phasing (expand-contract)
- 2a (first PR): add `mcp_front.port` setting; route seeder + route.go read it; retarget
  the 3 client-URL port consumers; new `mcphub install --reconcile-mcp-front[/--rollback]`
  (fail-closed on route OWNERSHIP + liveness, reuses existing reconcile+backup+rollback);
  extend the probe (kill GUI → client still 200 on mcp_front.port). Minimal, reversible,
  ZERO GUI-lifecycle code.

  **Ownership, not just liveness (pre-submission review finding 3).** The cutover
  requires the port to be served by a LIVE SUPERVISOR's own built-in route child
  (`api.AssertMCPFrontPortSupervisorOwned`: supervisor lock held, canonical
  `\mcp-local-hub-route-front` descriptor at exactly this port, supervisor-state row
  `running`, and the kernel's owner of the loopback socket equal to that recorded PID).
  A hand-started `mcphub route --port N` is the real route server and satisfies the
  readiness probe perfectly, but nothing restarts it — so accepting it would rewrite
  every in-scope client onto a port that goes dark when that shell closes, which is the
  exact failure mode this increment exists to eliminate. Operator-visible consequence:
  the standalone `mcphub route` path is fine for developing/testing the route daemon,
  but the CUTOVER requires `mcphub supervise` (or autostart). Windows runs the full
  proof; Linux runs it minus the image check (no image resolver on that target); macOS
  and other POSIX refuse, because the OS socket-owner primitive the proof needs is a
  fail-closed stub there.
- 2b: restore auto-register-on-miss via GUI delegation (route signals the single-writer
  GUI on a trusted miss) — closes the I6 503-on-new-workspace regression.
- 2c (deferred, iff open probe P4 finds a hard 9125 dependency): Mechanism-A literal-9125.
- 2d (deferred): retire the redundant GUI-side serena/LSP mounts, after 2b.

## The I6 regression (explicit, gated — the key tradeoff)
After 2a, clients hit the READ-ONLY route, so a brand-new (unregistered) workspace's
first serena tool-call returns HTTP 503 "register workspace first" instead of
auto-registering. This is intended + gated (503, not silent), and restored by 2b.
Shipping 2a WITHOUT 2b regresses new-workspace auto-registration for the operator's
"open a new project" workflow — so 2a and 2b should ship together (or 2b immediately
follows 2a) before the operator relies on it. Lead decision pending on 2a-alone vs 2a+2b.
