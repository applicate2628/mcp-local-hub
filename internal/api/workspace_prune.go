// internal/api/workspace_prune.go
//
// Pure structural-prune classifiers for the workspace-daemon auto-prune
// feature (Phase 1, 1b). These are the two predicates the in-GUI prune sweeper
// evaluates per workspace daemon row to decide whether a registration is
// structurally dead and should be auto-pruned. They are deliberately pure (no
// fleet, no IPC, no registry) so they are unit-testable with table cases.
//
// Both operate on the CANONICAL workspace path. Registry rows store the
// EvalSymlinks-resolved + drive-lowercased canonical form (set at register
// time: serena_auto_register.go and lsp_auto_register.go canonicalize before
// writing the row), so the predicates can rely on that normalization.

package api

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AutoPruneWorkspacesSettingKey is the GUI-settable bool that gates the in-GUI
// workspace-daemon auto-prune sweeper (SettingsRegistry:
// daemons.auto_prune_workspaces, Default "true"). The sweeper reads it each
// tick so a toggle takes effect within ~60s with no restart.
const AutoPruneWorkspacesSettingKey = "daemons.auto_prune_workspaces"

// PruneIdleHoursSettingKey is the GUI-settable int (HOURS) that enables the
// Phase-3 idle auto-prune. "0" = off (only the structural triggers run); >0 =
// also prune a workspace whose most-recent activity is older than that many
// hours. The sweeper reads it each tick.
const PruneIdleHoursSettingKey = "daemons.prune_idle_hours"

// PruneDeadWorktreesSettingKey is the GUI-settable bool that gates the
// dead-git-worktree structural orphan signal (SettingsRegistry:
// daemons.prune_dead_worktrees, Default "true"). The sweeper reads it each tick
// so a toggle takes effect within ~60s with no restart. Default-on because the
// signal is structural and false-positive-safe by construction (see
// IsDeadGitWorktreePath), consistent with the deleted-dir signal's default-on
// posture.
const PruneDeadWorktreesSettingKey = "daemons.prune_dead_worktrees"

// gitWorktreePointerPrefix is the leading token of the single-line pointer a git
// worktree stores in its `.git` FILE: `gitdir: <path-to-admin-dir>`. A normal
// repo has a `.git` DIRECTORY (no pointer file); a linked worktree has this
// `.git` regular file pointing at its admin directory under the main repo's
// `.git/worktrees/<name>`.
const gitWorktreePointerPrefix = "gitdir:"

// agentWorktreeMarker is the path segment that identifies an ephemeral
// agent worktree (e.g. ".claude/worktrees/agent-<id>"). Such worktrees are
// created per agent session and are ephemeral by design, so a daemon
// registered against one is pruned IMMEDIATELY (no consecutive-tick grace
// window) once observed.
const agentWorktreeMarker = "/.claude/worktrees/agent-"

// IsAgentWorktreePath reports whether canonicalPath points inside an ephemeral
// `.claude/worktrees/agent-*` worktree. The check is a forward-slash substring
// match (filepath.ToSlash normalizes the Windows backslash separators in the
// stored canonical path) so it works identically on every GOOS. canonicalPath
// is already canonical (the registry stores EvalSymlinks-resolved,
// drive-lowercased paths), so the substring is reliable after ToSlash.
// Exported so the GUI prune sweeper (internal/gui) can classify rows.
func IsAgentWorktreePath(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	return strings.Contains(filepath.ToSlash(canonicalPath), agentWorktreeMarker)
}

// WorkspaceDirDeleted reports whether the workspace directory at canonicalPath
// has been DEFINITIVELY deleted. It stats the best-effort symlink-resolved form
// of the path (resolveSymlinksBestEffort, the same resolver the cleanup
// canonicalizer uses) and returns true ONLY on a definitive ENOENT
// (os.IsNotExist). Any OTHER stat error (permission denied, transient I/O, an
// unavailable network/removable drive) returns FALSE — a transient or
// ambiguous error must NEVER be read as "deleted", because pruning a workspace
// whose directory merely momentarily failed to stat would tear down a live
// registration. The sweeper layers a 2-consecutive-ENOENT-tick grace on top of
// this to absorb a transient unmount; this predicate is the single-observation
// primitive. Exported so the GUI prune sweeper (internal/gui) can classify rows.
func WorkspaceDirDeleted(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	resolved := resolveSymlinksBestEffort(canonicalPath)
	if _, err := os.Stat(resolved); err != nil {
		return os.IsNotExist(err)
	}
	return false
}

// IsDeadGitWorktreePath reports whether canonicalPath is a leftover git LINKED
// WORKTREE whose admin directory has been deleted — the directory still exists
// on disk (so WorkspaceDirDeleted is false) but the worktree it represents is
// structurally dead because git's per-worktree admin dir
// (`<main-repo>/.git/worktrees/<name>`) is gone. This is the REAL incident
// signal: such a directory slips through BOTH the agent-worktree path check
// (its path is not under `.claude/worktrees/agent-`) AND the deleted-dir check
// (the directory still exists).
//
// It returns true ONLY when ALL of the following hold — conservative by
// construction so a live repo, a live worktree, a live submodule, or any
// ambiguous-error stat can NEVER be misclassified:
//
//  1. the workspace directory STILL EXISTS (a definitive ENOENT on the dir is
//     the deleted-dir case, owned by WorkspaceDirDeleted — NOT here);
//  2. `<dir>/.git` exists AND is a REGULAR FILE. A normal repo has a `.git`
//     DIRECTORY (→ false); a non-git dir has no `.git` (→ false). Only a linked
//     worktree (and a submodule — handled by condition 3) uses a `.git` file;
//  3. the `.git` file's `gitdir: <path>` pointer (relative paths resolved
//     against the workspace dir) names an admin directory that is ABSENT via
//     os.IsNotExist-ONLY. Any OTHER stat error on the admin dir (permission,
//     transient I/O, an offline/removable mount) returns FALSE — it inherits
//     the EXACT false-positive-safe discipline of WorkspaceDirDeleted: a
//     transient or ambiguous error must NEVER read as "dead".
//
// SUBMODULE SAFETY: a git submodule ALSO stores a `.git` FILE, pointing at
// `<superproject>/.git/modules/<name>`. A LIVE submodule's admin dir is present,
// so condition 3 fails and it is correctly never matched. A submodule whose
// superproject sits on an OFFLINE mount yields a NON-ENOENT stat error on the
// admin dir, so condition 3's os.IsNotExist-only guard returns false (not
// pruned). No submodule special-casing is needed beyond that guard.
//
// Exported so the GUI prune sweeper (internal/gui) can classify rows.
func IsDeadGitWorktreePath(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	dir := resolveSymlinksBestEffort(canonicalPath)

	// Condition 1: the directory must STILL EXIST. A definitive ENOENT on the
	// directory is the deleted-dir case (WorkspaceDirDeleted owns it); any other
	// stat error is ambiguous → not a dead worktree. Either way, bail.
	if _, err := os.Stat(dir); err != nil {
		return false
	}

	// Condition 2: `<dir>/.git` must exist AND be a REGULAR FILE.
	gitPath := filepath.Join(dir, ".git")
	gitInfo, err := os.Lstat(gitPath)
	if err != nil {
		// No `.git` (ENOENT) → not a git dir at all; any other error → ambiguous.
		// Both are NOT a dead worktree.
		return false
	}
	// A normal repo's `.git` is a DIRECTORY; a worktree/submodule's is a regular
	// file. Reject anything that is not a regular file (directory, symlink, etc.)
	// — Mode().IsRegular() is false for all of those.
	if !gitInfo.Mode().IsRegular() {
		return false
	}

	// Condition 3: parse the `gitdir:` pointer and confirm the admin dir is
	// ABSENT via os.IsNotExist ONLY.
	adminDir, ok := parseGitWorktreePointer(gitPath, dir)
	if !ok {
		// Unreadable/unparsable `.git` file → ambiguous, never prune.
		return false
	}
	if _, err := os.Stat(adminDir); err != nil {
		// ENOENT → admin dir gone → DEAD worktree. Any other error (permission,
		// transient I/O, offline mount, submodule-on-offline-superproject) →
		// FALSE (inherit WorkspaceDirDeleted's false-positive-safe discipline).
		return os.IsNotExist(err)
	}
	// Admin dir present → live worktree (or live submodule) → not dead.
	return false
}

// parseGitWorktreePointer reads the `.git` FILE at gitPath and returns the
// absolute path of the admin directory named by its single `gitdir: <path>`
// line. A relative pointer is resolved against workspaceDir (git writes a
// relative pointer for a worktree created with `--relative-paths`, and an
// absolute one otherwise). It returns ok=false on any read error, a missing
// `gitdir:` line, or an empty target — every such case makes the caller treat
// the path as NOT a dead worktree (ambiguous → safe).
func parseGitWorktreePointer(gitPath, workspaceDir string) (adminDir string, ok bool) {
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, gitWorktreePointerPrefix) {
			continue
		}
		target := strings.TrimSpace(strings.TrimPrefix(line, gitWorktreePointerPrefix))
		if target == "" {
			return "", false
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(workspaceDir, target)
		}
		return filepath.Clean(target), true
	}
	return "", false
}

// WorkspaceOrphanReason names WHY a registered workspace classifies as an
// orphan eligible for auto-prune. The four values mirror the four SHIPPED
// detection signals; the type makes the reason a typed value the sweeper logs
// and the `mcphub workspace prune` command surfaces, instead of a bare string
// duplicated across call sites.
type WorkspaceOrphanReason string

const (
	// OrphanReasonAgentWorktree — the workspace lives inside an ephemeral
	// `.claude/worktrees/agent-*` worktree (IsAgentWorktreePath). Highest
	// priority: ephemeral by design, pruned with no grace window.
	OrphanReasonAgentWorktree WorkspaceOrphanReason = "agent-worktree"
	// OrphanReasonDeletedDir — the workspace directory is DEFINITIVELY gone
	// (WorkspaceDirDeleted, os.IsNotExist-only). ClassifyWorkspaceOrphan returns
	// this on a single ENOENT observation; the GUI sweeper still layers its own
	// 2-consecutive-tick grace on top before acting (the grace is stateful
	// per-sweeper-instance and stays in the sweeper, NOT here).
	OrphanReasonDeletedDir WorkspaceOrphanReason = "deleted-dir"
	// OrphanReasonDeadWorktree — the workspace directory STILL EXISTS but it is a
	// leftover git LINKED WORKTREE whose admin directory has been deleted
	// (IsDeadGitWorktreePath, os.IsNotExist-only on the admin dir). This is the
	// REAL incident signal: such a dir slips through BOTH agent-worktree (path
	// not under `.claude/worktrees/agent-`) and deleted-dir (dir still present).
	// Gated by opts.PruneDeadWorktrees. Like deleted-dir, the GUI sweeper layers
	// its 2-consecutive-tick ENOENT grace on top before acting.
	OrphanReasonDeadWorktree WorkspaceOrphanReason = "dead-worktree"
	// OrphanReasonIdle — the workspace exists and is not an agent worktree, but
	// its most-recent activity (LastToolsCallAt) is older than the idle
	// threshold. Only fires when opts.IdleThreshold > 0 AND LastToolsCallAt is
	// non-zero (a zero timestamp = no activity signal → never idle-pruned).
	OrphanReasonIdle WorkspaceOrphanReason = "idle"
)

// ClassifyOpts carries the per-workspace inputs ClassifyWorkspaceOrphan needs
// beyond the path itself: the idle threshold for THIS run, the workspace's
// most-recent activity timestamp, and the wall-clock "now" the idle comparison
// is anchored against.
type ClassifyOpts struct {
	// PruneDeadWorktrees gates the dead-git-worktree structural signal
	// (IsDeadGitWorktreePath). The caller reads daemons.prune_dead_worktrees once
	// per tick and threads the resolved bool here (mirroring how IdleThreshold is
	// resolved from daemons.prune_idle_hours). When false the dead-worktree check
	// is skipped entirely.
	PruneDeadWorktrees bool
	// IdleThreshold enables the idle signal when > 0. Zero (or negative)
	// disables idle classification — only the two structural signals run.
	IdleThreshold time.Duration
	// LastToolsCallAt is the most-recent activity timestamp across the
	// workspace's rows. A zero value means "no activity signal observed" and
	// NEVER idle-prunes (require structural evidence, not wall-clock-since-
	// register).
	LastToolsCallAt time.Time
	// Now anchors the idle comparison. The caller supplies it (the sweeper's
	// tick time, or time.Now() for the CLI) so classification stays
	// deterministic and testable. A zero Now disables the idle signal (no
	// meaningful comparison is possible).
	Now time.Time
}

// ClassifyWorkspaceOrphan is the SINGLE owner of the orphan-classification
// predicate for the workspace-daemon auto-prune feature. It evaluates the four
// SHIPPED detection signals against ONE workspace path in a fixed priority
// order and returns the matching reason plus true, or ("", false) when the
// workspace is healthy.
//
// Priority (highest first — identical to the order the GUI sweeper's inlined
// switch used before this extraction):
//  1. agent-worktree (IsAgentWorktreePath) — ephemeral by design.
//  2. deleted-dir (WorkspaceDirDeleted) — directory definitively gone. This is
//     the SINGLE-OBSERVATION verdict; the GUI sweeper still applies its
//     2-consecutive-ENOENT-tick grace before acting (the grace is stateful
//     per-sweeper-instance and is NOT this pure predicate's concern).
//  3. dead-worktree (opts.PruneDeadWorktrees && IsDeadGitWorktreePath) — the
//     directory STILL EXISTS but it is a leftover git linked worktree whose
//     admin dir is gone. Ranked AFTER deleted-dir so a directory that is BOTH a
//     (former) worktree AND now deleted classifies as deleted-dir (the
//     WorkspaceDirDeleted ENOENT fires first; IsDeadGitWorktreePath requires the
//     dir to still exist, so the two are mutually exclusive by construction —
//     the ordering is belt-and-suspenders). The GUI sweeper shares its
//     deleted-dir 2-tick ENOENT grace with this verdict.
//  4. idle (opts.IdleThreshold > 0 && LastToolsCallAt non-zero &&
//     Now.Sub(LastToolsCallAt) > IdleThreshold) — present, non-worktree, but
//     stale.
//
// path is the CANONICAL workspace path (the registry stores the
// EvalSymlinks-resolved, drive-lowercased form), the same form every structural
// predicate already relies on. Pure: no fleet, no IPC, no registry — it only
// stats the path (via the existing predicates) and compares timestamps, so it
// is unit-testable with table cases.
func ClassifyWorkspaceOrphan(path string, opts ClassifyOpts) (WorkspaceOrphanReason, bool) {
	switch {
	case IsAgentWorktreePath(path):
		return OrphanReasonAgentWorktree, true
	case WorkspaceDirDeleted(path):
		return OrphanReasonDeletedDir, true
	case opts.PruneDeadWorktrees && IsDeadGitWorktreePath(path):
		return OrphanReasonDeadWorktree, true
	case opts.IdleThreshold > 0 &&
		!opts.LastToolsCallAt.IsZero() &&
		!opts.Now.IsZero() &&
		opts.Now.Sub(opts.LastToolsCallAt) > opts.IdleThreshold:
		return OrphanReasonIdle, true
	default:
		return "", false
	}
}
