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
