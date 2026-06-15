# Closure — Workspace-daemon auto-prune + manual GUI Remove

Closed: 2026-06-16

## Outcome — DELIVERED (Phase 1 + Phase 3; Phase 2 → ROADMAP)

Stale per-workspace daemons (serena + per-language LSP) auto-registered on every
opened workspace and never pruned → unbounded growth (live: 27 daemons, 8 for
dead `.claude/worktrees/agent-*` worktrees). Now auto-pruned.

- **Phase 1 — auto-prune stale workspace daemons (2d947ab):** agent-worktree
  (immediate) + deleted-dir (confirm-after-2-ticks) prune via the shared
  `(*API).PruneWorkspace` two-phase teardown lifted out of the CLI-only path.
- **Phase 3 — idle-N-hours auto-prune (cefecae):** GUI-settable idle threshold;
  LSP rows reuse persisted `WorkspaceEntry.LastToolsCallAt`, serena rows use the
  new debounced activity stamp; never prunes on a zero timestamp.
- **GUI follow-ups (5649351, cbcdf28):** auto-prune master toggle surfaced +
  Backups Clean honors live keep_n (WYSIWYG); failed `os.Remove` during clean is
  logged not silently dropped.

`daemons.auto_prune_workspaces` default ENABLED (structural triggers low-risk +
re-register on reopen).

## Residual risk / deferred
- **Phase 2 — manual GUI "Remove" button** (JSON DTO {workspace_path, backend})
  for on-demand removal of a specific workspace daemon was scoped but NOT built.
  → ROADMAP. Auto-prune covers the unbounded-growth problem; manual remove is an
  operator-convenience add-on.

## Archive location
work-items/archive/2026-06/2026-06-15-workspace-daemon-prune/
