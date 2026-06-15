# Workspace-daemon auto-prune + manual GUI Remove

Template: full-delivery (self-managed); Orchestrator: main conversation
Opened: 2026-06-15

## Goal
Stale per-workspace daemons (serena + per-language LSP) auto-register on every workspace an MCP
client opens and NEVER prune → grow without bound. Live: 27 daemons, 16 workspace, 8 for ephemeral
`.claude/worktrees/agent-*` worktrees from old sessions. No GUI way to remove → "can't even remove
by hand". Owner wants: auto-prune (idle + agent-worktree + deleted) + a manual GUI Remove button.

## Design (DONE — workflow wf_45c11174-6f7, 4 agents, gate PASS)
Full design: `tasks/wm0w1v7z0.output` (result.design). Reuses proven primitives: (*API).Unregister
teardown (register.go:825), serena teardown RemoveSerenaSupervisorIntentForWorkspace
(register_supervisor.go:479) + RemoveByBackend("serena") (workspace_registry.go:397), the in-GUI 60s
ticker pattern (serena_idle_sweeper.go:190). Single structural move: lift the two-phase teardown
(today CLI-only in runWorkspaceUnregister workspace_cmd.go:341) into a shared `(*API).PruneWorkspace`.

Idle signal decision: LSP rows reuse the already-persisted WorkspaceEntry.LastToolsCallAt
(workspace_registry.go:68, debounced lazy_proxy.go:748); serena rows need a new debounced persist at
recordSerenaActivity (serena_router.go:743,1043) — Phase 3 only. Never prune on a zero timestamp.

## Owner decisions (made)
- daemons.auto_prune_workspaces default: **ENABLED** (structural triggers low-risk + re-register on reopen).
- deleted-dir prune: **confirm-after-2-ticks** (in-memory counter, absorbs transient unmount).
- agent-worktree prune: immediate.
- manual-remove DTO: **JSON body {workspace_path, backend}** (paths aren't URL-path-safe).
- idle-prune scope: both serena+LSP, but only after the serena stamp (Phase 3).

## Phasing
- **Phase 1 (IN PROGRESS — dispatched to codex xhigh, task bne2itv7k):** shared PruneWorkspace owner +
  re-point CLI + classifiers (isAgentWorktreePath, workspaceDirDeleted) + in-GUI prune sweep ticker
  (deleted+worktree only) + daemons.auto_prune_workspaces gate (default true). NO new state. Clears the
  live 8-agent-worktree pile within ~60s. Prompt: `.scratch/codex-prune-phase1.md`.
- **Phase 2:** manual GUI Remove button — DELETE /api/workspaces/ over PruneWorkspace + frontend
  DaemonStatus interface + Card Remove + ConfirmModal (gated is_workspace_scoped) + api.ts client.
- **Phase 3 (last):** idle-N-days — debounced serena LastToolsCallAt persist + daemons.prune_idle_days
  + extend the sweep with the idle predicate + zero-timestamp never-prune fallback.

## Risks (from design)
- transient-unmount false-positive → confirm-2-ticks + non-destructive re-register. serena-migrate race
  → prune takes no supervisor.lock, fails-closed on wedged supervisor, sweep skips-tick on IPC error.
  GUI-only prune host (accepted — growth is over long sessions, GUI is always-on).

## State
- [completed] Design workflow + owner decisions.
- [completed] Phase 1 — IMPLEMENTED + REVIEWED + LIVE-PROVEN + PUSHED (master 2d947ab).
  - codex hit its usage-limit (until Jun 18) → opus implementer (wnh7l8ai6) drafted the teardown
    owner + classifiers then crashed (socket); adversarial review (2 lenses) verified the lift,
    flagged the missing sweeper + the agent-worktree false-positive; sweeper + wiring + map-init +
    in-flight safety guard + tests completed by hand.
  - BUG caught LIVE on first deploy: the sweep read the SERENA resolver's ListWorkspaces (serena-only)
    so only 1 of 8 worktrees pruned; fixed with (*API).ListAllWorkspaceRows (SerenaEntries+LSPEntries)
    and amended into the commit. Re-deploy: ALL 8 agent-worktree daemons auto-pruned within ~2 min
    (events workspace-pruned ×8), fleet 26→18, `claude mcp list` 26/0 (real servers intact).
- [pending] Phase 2 — manual GUI Remove button (DELETE /api/workspaces/ over PruneWorkspace +
  DaemonStatus TS fields + Card Remove + ConfirmModal). Closes "can't remove by hand" for ANY workspace.
- [pending] Phase 3 — idle-N-days (debounced serena LastToolsCallAt persist + daemons.prune_idle_days
  + extend the sweep with the idle predicate + zero-timestamp never-prune fallback).
- [note] pre-existing TestLanguageServerCleanup_* reads the dev's REAL Zed settings.json (JSONC) with
  strict json → fails on a comment; a separate test-isolation defect (the test should use a temp HOME).
  Check whether the production language-server-cleanup Zed read needs the same JSONC tolerance as the
  scanZed/scanVSCode fix.

## Next action
Phase 2 (GUI Remove button) — additive, shares the PruneWorkspace owner from Phase 1. Then Phase 3.
