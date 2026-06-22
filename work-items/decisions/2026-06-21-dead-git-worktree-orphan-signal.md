---
status: proposed
date: 2026-06-21
slug: dead-git-worktree-orphan-signal
---

# Add a `dead-worktree` structural orphan signal to the workspace auto-prune classifier

## Context

The workspace-daemon auto-prune feature (PR-1, merged in #417) introduced
`api.ClassifyWorkspaceOrphan(path, ClassifyOpts) (WorkspaceOrphanReason, bool)`
(`internal/api/workspace_prune.go`) as the SINGLE owner of the orphan-classification
predicate. It evaluates three shipped signals in priority order:

1. `agent-worktree` — the path is under `.claude/worktrees/agent-` (ephemeral by
   design; pruned immediately).
2. `deleted-dir` — `WorkspaceDirDeleted` (`os.IsNotExist`-ONLY on the workspace
   directory; pruned after a 2-consecutive-ENOENT-tick grace in the GUI sweeper).
3. `idle` — present, non-worktree, but stale past a configurable threshold.

The same owner is called by both the in-GUI sweeper
(`internal/gui/workspace_prune_sweeper.go`) and the manual `mcphub workspace prune`
command (`internal/cli/workspace_prune_cmd.go`).

**The incident.** An operator accumulated 17 leftover workspace-daemon
registrations that slipped through ALL three signals. Their workspace
directories were **git linked worktrees** (created by `git worktree add`) whose
*directory still exists on disk* but whose per-worktree git ADMIN directory
(`<main-repo>/.git/worktrees/<name>`) had been deleted (the main repo was
re-cloned, the `.git/worktrees/<name>` entry pruned with `git worktree prune`, or
the main checkout was removed). Such a directory:

- is NOT under `.claude/worktrees/agent-` → `IsAgentWorktreePath` = false;
- still EXISTS → `WorkspaceDirDeleted` = false (`os.IsNotExist`-only, correctly);
- may have recent recorded activity or none → idle does not reliably fire and is
  off by default.

So the registration is structurally dead but invisible to every existing signal.
This is the **real** fix for the incident, distinct from the two structural
signals PR-1 shipped.

## Decision

Add a fourth structural orphan class, `dead-worktree`, owned by a new pure
predicate `IsDeadGitWorktreePath(canonicalPath string) bool` in
`internal/api/workspace_prune.go`, and wire it into `ClassifyWorkspaceOrphan` in
priority position **3** (after `deleted-dir`, before `idle`):

```
agent-worktree → deleted-dir → dead-worktree → idle
```

`IsDeadGitWorktreePath` returns true ONLY when ALL of:

1. the workspace dir STILL EXISTS (a definitive ENOENT on the dir is the
   deleted-dir case, owned by `WorkspaceDirDeleted` — not here);
2. `<dir>/.git` exists AND is a REGULAR FILE. A normal repo has a `.git`
   DIRECTORY (→ false); a non-git dir has no `.git` (→ false). Only a linked
   worktree (or a submodule — see below) stores a `.git` file;
3. the `.git` file's `gitdir: <path>` pointer (relative paths resolved against
   the workspace dir) names an admin directory that is ABSENT via
   `os.IsNotExist`-ONLY. **Any OTHER stat error** on the admin dir (permission,
   transient I/O, an offline/removable mount) returns FALSE — it inherits the
   EXACT false-positive-safe discipline of `WorkspaceDirDeleted`.

The signal is gated by a new GUI-settable bool `daemons.prune_dead_worktrees`
(default **TRUE**, consistent with `deleted-dir`'s default-on posture — the
signal is structural and false-positive-safe by construction). The gate is
threaded via `ClassifyOpts.PruneDeadWorktrees`, read once per tick by the caller
(mirroring how the sweeper resolves `IdleThreshold` from
`daemons.prune_idle_hours` and `auto_prune_workspaces`). The sweeper and the
manual command both pick it up automatically because the classifier is the single
owner.

The GUI prune sweeper applies the SAME 2-consecutive-ENOENT-tick grace to the
`dead-worktree` verdict that it already applies to `deleted-dir` (shared grace
counter `pruneEnoentTicks`). A momentarily-unreadable admin dir on a slow mount
must not prune on the first tick.

## Safety analysis

- **`os.IsNotExist`-only on the admin dir.** Same discipline as `WorkspaceDirDeleted`:
  only a definitive ENOENT counts as "dead". A permission error, a transient I/O
  error, or an offline mount returns false (not pruned). This is the load-bearing
  conservatism that prevents false positives.
- **Regular-`.git`-file requirement.** A normal repo (`.git` directory) and a
  non-git dir (no `.git`) are both rejected at condition 2, so the signal can
  ONLY ever fire on a path that is genuinely a worktree/submodule pointer.
- **2-tick grace.** Shared with `deleted-dir` — a single transient ENOENT on the
  admin dir (e.g. a removable drive mid-unmount) does not prune; two consecutive
  ticks are required.
- **Submodule safety.** A git SUBMODULE also stores a `.git` FILE, pointing at
  `<superproject>/.git/modules/<name>`. A LIVE submodule's admin dir is present →
  condition 3 fails → never matched (correct). A submodule whose superproject
  sits on an OFFLINE mount yields a NON-ENOENT stat error on the admin dir →
  condition 3's `os.IsNotExist`-only guard returns false → not pruned (correct).
  **No submodule special-casing is needed** beyond the `os.IsNotExist`-only
  guard; that guard is sufficient and is the chosen design. A table test
  (`TestIsDeadGitWorktreePath` "submodule on offline mount") exercises the
  non-ENOENT path explicitly.
- **Non-destructive.** Like the other signals, prune only removes the registry
  rows; a pruned workspace re-registers on its next open. The `.serena/` on-disk
  directory is never touched.
- **Priority / mutual exclusion.** `dead-worktree` ranks AFTER `deleted-dir`. A
  directory that is BOTH a former worktree AND now deleted classifies as
  `deleted-dir` (the `WorkspaceDirDeleted` ENOENT fires first; and
  `IsDeadGitWorktreePath` requires the dir to still exist, so the two are mutually
  exclusive by construction — the ordering is belt-and-suspenders).

## Protected (unchanged)

`PruneWorkspace` teardown; `WorkspaceDirDeleted` (verbatim — the
`os.IsNotExist`-only discipline is reused, not loosened); the `agent-worktree`
and `idle` signals; the sweep seam / cadence (60s, GUI process); the supervisor;
`.serena/` on-disk directories.

## Consequences

- The single classifier owner now has four signals; both callers (GUI sweeper,
  manual CLI) gain the new signal for free.
- One new GUI-settable bool (`daemons.prune_dead_worktrees`, default on) plus a
  Settings → Daemons → Auto-prune toggle.
- `.git`-file parsing is a NEW capability with no prior owner in the repo
  (verified: no existing `gitdir:` parser), so a minimal single-line pointer
  parser (`parseGitWorktreePointer`) is added next to the predicate it serves.

## Files

- `internal/api/workspace_prune.go` — `IsDeadGitWorktreePath`,
  `parseGitWorktreePointer`, `OrphanReasonDeadWorktree`,
  `ClassifyOpts.PruneDeadWorktrees`, `PruneDeadWorktreesSettingKey`,
  classifier wiring.
- `internal/api/settings_registry.go` — `daemons.prune_dead_worktrees` (bool,
  default "true").
- `internal/gui/workspace_prune_sweeper.go` — `pruneDeadWorktreesFn` seam,
  once-per-tick read, shared 2-tick grace.
- `internal/cli/workspace_prune_cmd.go` — `pruneDeadWorktreesEnabled` resolver,
  threaded gate, help text.
- `internal/gui/frontend/src/components/settings/SectionDaemons.tsx` — toggle.
