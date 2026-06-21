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
	"runtime"
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

// IsDeadGitWorktreePath reports whether canonicalPath is (or is a SUBDIR inside)
// a leftover git LINKED WORKTREE whose admin directory has been deleted — the
// workspace directory still exists on disk (so WorkspaceDirDeleted is false) but
// the worktree it represents is structurally dead because git's per-worktree
// admin dir (`<main-repo>/.git/worktrees/<name>`) is gone. This is the REAL
// incident signal: such a directory slips through BOTH the agent-worktree path
// check (its path is not under `.claude/worktrees/agent-`) AND the deleted-dir
// check (the directory still exists).
//
// It returns true ONLY when ALL of the following hold — conservative by
// construction so a live repo, a live worktree, a live submodule, an unmounted
// admin root, a cross-OS pointer, or any ambiguous-error stat can NEVER be
// misclassified:
//
//  1. the workspace directory STILL EXISTS (a definitive ENOENT on the dir is
//     the deleted-dir case, owned by WorkspaceDirDeleted — NOT here);
//  2. walking UP from the workspace dir through its ancestors, the NEAREST
//     `.git` found is a REGULAR FILE (the worktree pointer). The walk handles a
//     SUBDIR workspace inside a linked worktree (a monorepo package, or an
//     LSP/.serena marker root) whose `.git` pointer lives at the worktree ROOT
//     (an ancestor), not the subdir itself. If the nearest `.git` is a DIRECTORY
//     it is a normal repo root → LIVE → false (stop the walk). A non-git tree
//     with no `.git` anywhere up to the volume root → false;
//  3. the pointer's `gitdir: <path>` (relative paths resolved against the
//     worktree-root dir that holds the pointer) names an admin directory that is
//     ABSENT via os.IsNotExist-ONLY, AND the admin dir's PARENT chain is still
//     present (distinguishing "worktree removed" from "whole admin root
//     unmounted" — see isAdminDirGenuinelyDeleted). Any OTHER stat error on the
//     admin dir (permission, transient I/O, an offline/removable mount) returns
//     FALSE — it inherits the EXACT false-positive-safe discipline of
//     WorkspaceDirDeleted: a transient or ambiguous error must NEVER read as
//     "dead".
//
// SUBMODULE SAFETY: a git submodule ALSO stores a `.git` FILE, pointing at
// `<superproject>/.git/modules/<name>`. A LIVE submodule's admin dir is present,
// so condition 3 fails and it is correctly never matched. A submodule whose
// superproject sits on an OFFLINE mount yields a NON-ENOENT stat error on the
// admin dir (or an ENOENT whose PARENT is also gone), so condition 3 returns
// false (not pruned). No submodule special-casing is needed beyond that guard.
//
// Exported so the GUI prune sweeper (internal/gui) can classify rows.
func IsDeadGitWorktreePath(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	dir := resolveSymlinksBestEffort(canonicalPath)

	// Condition 1: the workspace directory must STILL EXIST. A definitive ENOENT
	// on the directory is the deleted-dir case (WorkspaceDirDeleted owns it); any
	// other stat error is ambiguous → not a dead worktree. Either way, bail.
	if _, err := os.Stat(dir); err != nil {
		return false
	}

	// Condition 2: walk UP from the workspace dir to the NEAREST `.git`. A subdir
	// workspace inside a linked worktree keeps its `.git` pointer at the worktree
	// ROOT (an ancestor), so probing only `<dir>/.git` would miss it (Finding 1).
	gitPath, pointerDir, ok := findNearestGitPointer(dir)
	if !ok {
		// No regular-file `.git` pointer found before hitting a normal-repo `.git`
		// directory or the volume root → not a (dead) linked worktree.
		return false
	}

	// Condition 3: parse the `gitdir:` pointer (relative paths resolved against
	// the dir that HOLDS the pointer — the worktree root — not the subdir
	// workspace) and confirm the admin dir is GENUINELY deleted.
	adminDir, ok := parseGitWorktreePointer(gitPath, pointerDir)
	if !ok {
		// Unreadable/unparsable/cross-OS `.git` pointer → ambiguous, never prune.
		return false
	}
	return isAdminDirGenuinelyDeleted(adminDir)
}

// findNearestGitPointer walks UP from dir through its ancestors and returns the
// path of the NEAREST `.git` that is a REGULAR FILE (a linked-worktree /
// submodule pointer), along with the directory that holds it (pointerDir — used
// to resolve a relative `gitdir:` target). The walk stops — with ok=false — the
// moment it finds a `.git` that is a DIRECTORY (a normal repo root: that tree is
// a LIVE repo, NOT a dead worktree), or when it climbs past the volume/filesystem
// root (filepath.Dir stops changing) without finding any `.git`.
//
// Bounding the walk on filepath.Dir's fixpoint (root) is the depth guard the
// design requires: on every OS filepath.Dir("/") == "/" and filepath.Dir(`c:\`)
// == `c:\`, so the loop terminates at the volume root. Each ancestor's `.git` is
// probed with Lstat (so a symlinked `.git` is rejected as not-a-regular-file,
// matching the prior single-dir behavior). A non-ENOENT Lstat error on an
// ancestor's `.git` (permission, transient I/O) is treated as ambiguous and
// stops the walk with ok=false — never climb past an unreadable ancestor and
// risk matching a deeper unrelated pointer.
func findNearestGitPointer(dir string) (gitPath, pointerDir string, ok bool) {
	cur := dir
	for {
		candidate := filepath.Join(cur, ".git")
		info, err := os.Lstat(candidate)
		switch {
		case err == nil && info.Mode().IsRegular():
			// Regular-file `.git` → worktree/submodule pointer. Nearest wins.
			return candidate, cur, true
		case err == nil && info.IsDir():
			// Directory `.git` → normal repo root → LIVE repo, not a dead
			// worktree. Stop the walk (and any deeper ancestor pointer is
			// irrelevant: this repo root is the owning boundary).
			return "", "", false
		case err == nil:
			// `.git` exists but is neither a regular file nor a directory
			// (symlink, device, etc.) → ambiguous → stop, never prune.
			return "", "", false
		case !os.IsNotExist(err):
			// Non-ENOENT Lstat error (permission, transient I/O) → ambiguous →
			// stop the walk; do not climb past an unreadable ancestor.
			return "", "", false
		}
		// ENOENT here → no `.git` at this ancestor → climb one level up.
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the volume/filesystem root without a `.git` → not a worktree.
			return "", "", false
		}
		cur = parent
	}
}

// isAdminDirGenuinelyDeleted reports whether the worktree admin dir at adminDir
// is GENUINELY deleted (a `git worktree remove` outcome) rather than merely
// UNAVAILABLE because its whole admin ROOT is offline/unmounted (Finding 2).
//
// A `git worktree remove` deletes ONLY the `worktrees/<name>` subdir, leaving
// the parent `.git/worktrees/` (or, for the main-repo layout, the main repo's
// `.git`) present. So:
//
//   - adminDir Stat succeeds → admin dir present → LIVE worktree → false.
//   - adminDir Stat is a NON-ENOENT error (permission, transient I/O, offline
//     mount) → ambiguous → false (inherit WorkspaceDirDeleted's discipline).
//   - adminDir Stat is ENOENT → the worktree subdir is gone, BUT only treat it
//     as DEAD when adminDir's immediate PARENT still EXISTS. If the parent is
//     ALSO absent/inaccessible, the whole admin root (mount/repo) is gone — that
//     is an unmounted-root ambiguity, NOT a removed worktree → false.
//
// The parent-exists check cleanly separates "worktree removed" (parent
// `.git/worktrees/` survives) from "mount offline" (parent also vanished).
func isAdminDirGenuinelyDeleted(adminDir string) bool {
	if _, err := os.Stat(adminDir); err == nil {
		// Admin dir present → live worktree (or live submodule) → not dead.
		return false
	} else if !os.IsNotExist(err) {
		// Non-ENOENT (permission, transient I/O, offline mount) → ambiguous.
		return false
	}
	// adminDir is ENOENT. Confirm its PARENT is present before declaring DEAD —
	// otherwise the whole admin root may merely be unmounted (Finding 2).
	parent := filepath.Dir(adminDir)
	if parent == adminDir {
		// Degenerate: adminDir is itself a root. No parent to corroborate against
		// → treat as ambiguous (never prune).
		return false
	}
	if _, err := os.Stat(parent); err != nil {
		// Parent absent OR inaccessible (ENOENT, permission, offline mount) → the
		// admin ROOT is gone/unavailable, not just the worktree subdir → ambiguous.
		return false
	}
	// Parent present (e.g. `.git/worktrees/` survives) but the `<name>` subdir
	// is ENOENT → genuinely a removed worktree → DEAD.
	return true
}

// parseGitWorktreePointer reads the `.git` FILE at gitPath and returns the
// absolute path of the admin directory named by its single `gitdir: <path>`
// line. A relative pointer is resolved against workspaceDir (git writes a
// relative pointer for a worktree created with `--relative-paths`, and an
// absolute one otherwise). It returns ok=false on any read error, a missing
// `gitdir:` line, an empty target, OR a FOREIGN-ABSOLUTE target (Finding 3) —
// every such case makes the caller treat the path as NOT a dead worktree
// (ambiguous → safe).
//
// FOREIGN-ABSOLUTE (cross-OS) gitdir handling: a worktree created by
// Git-for-Windows writes `gitdir: C:/...` (or a `\\srv\share` UNC); a worktree
// created on POSIX writes `gitdir: /...`. filepath.IsAbs is OS-SPECIFIC — on
// POSIX it returns FALSE for `C:/...` and `\\...`, on Windows it returns FALSE
// for `/...` — so a naive `if !IsAbs { join under workspace }` would JOIN a
// foreign-absolute pointer UNDER the workspace as if relative, fabricate a path
// that does not exist, stat ENOENT, and prune a LIVE workspace. Translating a
// path across OSes is fragile, so a foreign-absolute target is rejected as
// AMBIGUOUS (ok=false): the worktree was created on the other OS and we cannot
// reliably resolve its admin dir → never prune. A NATIVE absolute path (IsAbs
// true) and a NATIVE relative path (joined against workspaceDir) keep working.
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
		if filepath.IsAbs(target) {
			// Native absolute (drive-letter/UNC on Windows, `/...` on POSIX).
			return filepath.Clean(target), true
		}
		if isForeignAbsolutePath(target) {
			// A path the CURRENT OS does not consider absolute but the OTHER OS
			// would (Windows drive-letter/UNC seen on POSIX, or a POSIX `/...`
			// seen on Windows). Cross-OS worktree → cannot resolve safely →
			// ambiguous, never prune.
			return "", false
		}
		// Genuinely native-relative → resolve against the pointer's dir.
		return filepath.Clean(filepath.Join(workspaceDir, target)), true
	}
	return "", false
}

// isForeignAbsolutePath reports whether target is absolute on the OTHER OS but
// not the current one — i.e. filepath.IsAbs(target) already returned false on
// THIS GOOS, yet the path is clearly an absolute path authored on the opposite
// platform. Callers use this to REJECT (treat as ambiguous), never to resolve.
//
//   - On non-Windows (POSIX/WSL): a Windows drive-letter root (`C:\` or `C:/`)
//     or a UNC root (`\\server\share`).
//   - On Windows: a POSIX-absolute root (`/foo`). Note `\foo` is intentionally
//     NOT treated as foreign on Windows — it is a native (drive-relative) path
//     that Windows IsAbs already classifies as relative, and git never emits it
//     as a gitdir.
func isForeignAbsolutePath(target string) bool {
	if target == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		// POSIX-absolute `/...` seen on Windows (but not `\...`, which is a
		// native Windows drive-relative path, nor `\\...` UNC which Windows
		// IsAbs already accepts as absolute).
		return target[0] == '/'
	}
	// Non-Windows: Windows drive-letter root `^[A-Za-z]:[\\/]`.
	if len(target) >= 3 &&
		((target[0] >= 'A' && target[0] <= 'Z') || (target[0] >= 'a' && target[0] <= 'z')) &&
		target[1] == ':' &&
		(target[2] == '\\' || target[2] == '/') {
		return true
	}
	// Non-Windows: UNC root `^\\` (two leading backslashes).
	if len(target) >= 2 && target[0] == '\\' && target[1] == '\\' {
		return true
	}
	return false
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
