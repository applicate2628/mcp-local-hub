package gui

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	"mcp-local-hub/internal/api"
)

// The sweeper classifies candidates and routes them through the API's
// automatic preservation purpose. Its stop gate still protects concurrent forwards.

// pruneEnoentEntry is the per-path value of Server.pruneEnoentTicks: the orphan
// REASON last observed on the path plus the count of CONSECUTIVE ticks that saw
// the SAME reason. Keying the grace window by (path, reason) — resetting the
// count to 1 whenever the reason flips between ticks — keeps a deleted-dir tick
// followed by a dead-worktree tick from prematurely reaching the 2-tick
// threshold: only the SAME signal observed on two consecutive ticks prunes
// (Finding 2). agent-worktree and idle never populate this map (they prune with
// no grace window).
type pruneEnoentEntry struct {
	reason string
	count  int
}

// pruneEnabledFn reports whether auto-prune is enabled (the GUI-settable
// daemons.auto_prune_workspaces gate, default "true"). Read each tick so a
// toggle takes effect within ~60s. Fail-safe: a settings-read failure returns
// false (never prune when the gate is unreadable). Test seam.
var pruneEnabledFn = defaultPruneEnabled

// pruneActionFn evaluates a candidate through the automatic preservation purpose. Test seam.
var pruneActionFn = func(s *Server, path string) (*api.PruneReport, error) {
	return s.api.PruneWorkspace(path, "all", api.PrunePurposeAutomatic)
}

// pruneClearDefaultFn clears the default-workspace marker when a just-pruned
// workspace (canonical path) was the persisted default. The default marker is
// cleared whenever the returned report records a committed Serena-row removal,
// including a late error after that removal. Such an operation remains an error
// outcome: committed work is counted and the marker is cleared,
// workspace-prune-failed is emitted with removal counts, and workspace-pruned
// is not emitted. Test seam (the production form resolves the state dir from
// the registry path so the marker stays co-located with workspaces.yaml).
var pruneClearDefaultFn = func(canonical string) error {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	return api.ClearDefaultWorkspaceIfMatches(filepath.Dir(regPath), canonical)
}

func defaultPruneEnabled() bool {
	vals, err := api.NewAPI().SettingsList()
	if err != nil {
		return false // fail-safe: never prune on a settings-read failure
	}
	v, ok := vals[api.AutoPruneWorkspacesSettingKey]
	if !ok {
		return true // not persisted → the registry Default ("true") applies
	}
	return v == "true"
}

// pruneWorkspaceRowsFn returns the registered workspace rows the sweep classifies
// (production: the serena-router resolver's ListWorkspaces). Test seam.
var pruneWorkspaceRowsFn = defaultPruneWorkspaceRows

func defaultPruneWorkspaceRows(s *Server) []*api.WorkspaceEntry {
	if s == nil || s.api == nil {
		return nil
	}
	// Read the FULL registry (serena + every per-language LSP row) — NOT the
	// serena router resolver's ListWorkspaces, which is serena-ONLY and would
	// leave LSP-only agent worktrees (language: go, etc.) growing unpruned.
	rows, err := s.api.ListAllWorkspaceRows()
	if err != nil {
		return nil // skip this tick on a registry-read error
	}
	return rows
}

// pruneBeginFn claims the serena stop gate for prune teardown. It must use the
// same gate as idle-stop/forward so "no in-flight request" is atomic with prune
// mutation. Test seam over beginSerenaPrune.
var pruneBeginFn = func(s *Server, serenaKey string) bool {
	return s.beginSerenaPrune(serenaKey)
}

// pruneEndFn releases the prune teardown claim. Test seam over endSerenaPrune.
var pruneEndFn = func(s *Server, serenaKey string) {
	s.endSerenaPrune(serenaKey)
}

// pruneIdleThresholdFn returns the Phase-3 idle-prune threshold; 0 disables
// idle-prune (only the structural triggers run). Read each tick from the
// daemons.prune_idle_hours setting. Test seam.
var pruneIdleThresholdFn = defaultPruneIdleThreshold

func defaultPruneIdleThreshold() time.Duration {
	vals, err := api.NewAPI().SettingsList()
	if err != nil {
		return 0
	}
	v, ok := vals[api.PruneIdleHoursSettingKey]
	if !ok {
		return 0 // absent from resolved settings; fail closed
	}
	h, err := strconv.Atoi(v)
	if err != nil || h <= 0 {
		return 0
	}
	return time.Duration(h) * time.Hour
}

// pruneDeadWorktreesFn reports whether the dead-git-worktree structural signal
// is enabled (the GUI-settable daemons.prune_dead_worktrees gate, default
// "true"). Read each tick so a toggle takes effect within ~60s. Fail-SAFE: a
// settings-read failure returns false (never run the dead-worktree signal when
// the gate is unreadable) — symmetric with pruneEnabledFn's posture. Test seam.
var pruneDeadWorktreesFn = defaultPruneDeadWorktrees

func defaultPruneDeadWorktrees() bool {
	vals, err := api.NewAPI().SettingsList()
	if err != nil {
		return false // fail-safe: do not run the signal on a settings-read failure
	}
	v, ok := vals[api.PruneDeadWorktreesSettingKey]
	if !ok {
		return true // not persisted → the registry Default ("true") applies
	}
	return v == "true"
}

// SweepPruneWorkspaces classifies automatic candidates and reports retained
// configuration. It returns the number of actually pruned workspaces.
func (s *Server) SweepPruneWorkspaces(ctx context.Context, now time.Time) int {
	if s == nil {
		return 0
	}
	if pruneEnabledFn == nil || !pruneEnabledFn() {
		return 0 // gate off (or unreadable) — no prune.
	}
	rows := pruneWorkspaceRowsFn(s)
	if len(rows) == 0 {
		return 0
	}

	idleThreshold := pruneIdleThresholdFn()
	// Read the dead-worktree gate ONCE per tick (mirrors idleThreshold) and thread
	// the resolved bool through ClassifyOpts so the SINGLE owner decides; the
	// sweeper never re-implements the predicate.
	pruneDeadWorktrees := pruneDeadWorktreesFn != nil && pruneDeadWorktreesFn()

	// One candidate per workspace PATH (a workspace carries a serena row + N LSP
	// rows). Track the serena key (for the in-flight guard) and the MOST-RECENT
	// activity across the workspace's rows (for the idle predicate).
	type cand struct {
		serenaKey    string
		lastActivity time.Time
	}
	byPath := map[string]*cand{}
	for _, ws := range rows {
		if ws == nil || ws.WorkspacePath == "" {
			continue
		}
		c := byPath[ws.WorkspacePath]
		if c == nil {
			c = &cand{}
			byPath[ws.WorkspacePath] = c
		}
		if ws.Language == api.SerenaLanguageSentinel {
			c.serenaKey = ws.WorkspaceKey
		}
		if ws.LastToolsCallAt.After(c.lastActivity) {
			c.lastActivity = ws.LastToolsCallAt
		}
	}
	if len(byPath) == 0 {
		return 0
	}

	pruned := 0
	for path, c := range byPath {
		// Classify through the SINGLE owner (api.ClassifyWorkspaceOrphan) so the
		// agent-worktree / deleted-dir / idle predicate has ONE home shared with
		// the `mcphub workspace prune` command. The 2-consecutive-ENOENT-tick
		// grace below is the sweeper's own stateful concern (per-sweeper-instance
		// pruneEnoentTicks), NOT the pure predicate's — the owner returns the
		// deleted-dir verdict on a single observation and the sweeper layers the
		// grace on top before acting.
		orphanReason, isOrphan := api.ClassifyWorkspaceOrphan(path, api.ClassifyOpts{
			PruneDeadWorktrees: pruneDeadWorktrees,
			IdleThreshold:      idleThreshold,
			LastToolsCallAt:    c.lastActivity,
			Now:                now,
		})
		if !isOrphan {
			// Healthy, present, non-worktree → reset any stale ENOENT count.
			s.pruneEnoentMu.Lock()
			delete(s.pruneEnoentTicks, path)
			s.pruneEnoentMu.Unlock()
			continue
		}
		reason := string(orphanReason)
		if orphanReason == api.OrphanReasonDeletedDir || orphanReason == api.OrphanReasonDeadWorktree {
			// Layer the 2-consecutive-ENOENT-tick grace over the owner's
			// single-observation verdict (absorbs a transient unmount). Applies to
			// BOTH deleted-dir (the workspace dir vanished) and dead-worktree (the
			// worktree admin dir vanished) — a momentarily unreadable admin dir on a
			// slow mount must not prune on the first tick. The grace is keyed by
			// (path, reason): if the reason FLIPS between ticks (deleted-dir then
			// dead-worktree, or vice versa) the count RESETS to 1 so only the SAME
			// signal observed on two CONSECUTIVE ticks prunes (Finding 2) — a path
			// that is transient-deleted-dir-ENOENT once and transient-dead-worktree-
			// ENOENT once never reaches the threshold. agent-worktree and idle prune
			// immediately (they never enter this block).
			s.pruneEnoentMu.Lock()
			e := s.pruneEnoentTicks[path]
			if e.reason == reason {
				e.count++
			} else {
				e.reason = reason
				e.count = 1 // reason flipped (or first observation) → restart the window.
			}
			s.pruneEnoentTicks[path] = e
			n := e.count
			s.pruneEnoentMu.Unlock()
			if n < 2 {
				continue // wait for a confirming second SAME-reason tick.
			}
		}

		if c.serenaKey != "" && !pruneBeginFn(s, c.serenaKey) {
			continue
		}
		report, err := func() (*api.PruneReport, error) {
			if c.serenaKey != "" {
				defer pruneEndFn(s, c.serenaKey)
			}
			return pruneActionFn(s, path)
		}()
		s.pruneEnoentMu.Lock()
		delete(s.pruneEnoentTicks, path) // clear; re-accrues next tick if still dead.
		s.pruneEnoentMu.Unlock()
		// A fence release can fail after the row delete has committed. Consume the
		// report first so committed work is counted and a removed Serena row clears
		// its default marker, while the operation remains an error outcome.
		committed := report != nil && (len(report.LSPRemoved) > 0 || report.SerenaRemoved > 0)
		if report != nil && report.PreservedReason == "" && report.SerenaRemoved > 0 && pruneClearDefaultFn != nil {
			markerPath := report.Workspace
			if markerPath == "" {
				markerPath = path
			}
			if cerr := pruneClearDefaultFn(markerPath); cerr != nil {
				_ = api.LogHubMcpEvent("warn", "workspace-prune-default-clear-failed", map[string]any{
					"workspace": markerPath, "reason": reason, "err": cerr.Error(),
				})
			}
		}
		if err != nil {
			body := map[string]any{
				"workspace": path, "reason": reason, "err": err.Error(),
			}
			if report != nil {
				body["lsp_removed"] = len(report.LSPRemoved)
				body["serena_removed"] = report.SerenaRemoved
			}
			_ = api.LogHubMcpEvent("warn", "workspace-prune-failed", body)
			if committed {
				pruned++
			}
			continue
		}
		if report != nil && report.PreservedReason != "" {
			_ = api.LogHubMcpEvent("info", "workspace-prune-retained", map[string]any{
				"workspace": path, "reason": reason, "preserved_reason": report.PreservedReason,
			})
			continue
		}
		pruned++
		body := map[string]any{"workspace": path, "reason": reason}
		level := "info"
		if report != nil {
			body["lsp_removed"] = len(report.LSPRemoved)
			body["serena_removed"] = report.SerenaRemoved
			if len(report.Warnings) > 0 {
				body["warnings"] = report.Warnings
				level = "warn"
			}
		}
		_ = api.LogHubMcpEvent(level, "workspace-pruned", body)
	}

	// Drop ENOENT counters for workspaces that vanished from the registry this
	// tick (pruned above, or unregistered by another actor) so the map cannot
	// leak entries for paths it will never see again.
	s.pruneEnoentMu.Lock()
	for p := range s.pruneEnoentTicks {
		if _, present := byPath[p]; !present {
			delete(s.pruneEnoentTicks, p)
		}
	}
	s.pruneEnoentMu.Unlock()

	return pruned
}
