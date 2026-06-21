package gui

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	"mcp-local-hub/internal/api"
)

// workspace_prune_sweeper.go — Phase 1 of the workspace-daemon auto-prune
// feature: an in-GUI sweep that auto-removes daemons whose workspace is
// STRUCTURALLY dead — an ephemeral `.claude/worktrees/agent-*` worktree, a
// deleted directory, or a leftover git linked worktree whose admin dir is gone
// (dead-worktree) — so the per-workspace serena+LSP daemon set stops growing
// without bound. Mirrors the serena idle sweeper (serena_idle_sweeper.go): a
// per-tick, error-tolerant, cancellable pass driven by runWorkspacePruneTicker
// in the gui-command wiring.
//
// SAFETY (operator-flagged "don't remove a live one"): a workspace is pruned
// only after claiming the same per-workspace serena stop gate the idle sweeper
// uses. The claim atomically observes no in-flight /serena/mcp forward and blocks
// a new forward from entering until prune teardown has finished mutating the
// registry/intent state. Prune is non-destructive: a pruned workspace
// re-registers on its next open (EnsureLSPRegistered / AutoRegisterSerenaWorkspace),
// so the only residual (an in-flight LSP-only call against a workspace with no
// serena row) at worst drops one short LSP request the client retries.

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

// pruneActionFn tears down one workspace's daemon rows (production:
// (*api.API).PruneWorkspace(path, "all")). Test seam.
var pruneActionFn = func(s *Server, path string) (*api.PruneReport, error) {
	return s.api.PruneWorkspace(path, "all")
}

// pruneClearDefaultFn clears the default-workspace marker when a just-pruned
// workspace (canonical path) was the persisted default. It runs after a
// successful prune that removed a serena row so a stale default cannot outlive
// the workspace it pointed at — the SAME api.ClearDefaultWorkspaceIfMatches
// owner the CLI `workspace unregister --backend serena|all` and `workspace
// prune` call. Best-effort: a marker-clear failure is logged, never fatal to
// the sweep. Test seam (the production form resolves the state dir from the
// registry path so the marker stays co-located with workspaces.yaml).
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
		return 0 // not set → the registry Default ("0") = off
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

// SweepPruneWorkspaces auto-prunes structurally-dead workspace daemons:
// agent-worktree rows immediately; deleted-directory AND dead-git-worktree rows
// after 2 consecutive SAME-reason ENOENT ticks (the in-memory pruneEnoentTicks
// grace, keyed by (path, reason) so a reason flip between ticks resets the
// window — absorbs a transient unmount without letting a deleted-dir tick plus a
// dead-worktree tick prune across reasons; Finding 2). It skips any workspace
// with an in-flight serena forward. Returns the number of workspaces pruned this
// tick; a no-op when the gate is off, deps are unwired, or there are no workspace
// rows. ctx/now are accepted for symmetry with the idle sweeper and the Phase-3
// idle predicate.
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
		if err != nil {
			_ = api.LogHubMcpEvent("warn", "workspace-prune-failed", map[string]any{
				"workspace": path, "reason": reason, "err": err.Error(),
			})
			continue
		}
		pruned++
		// If this prune removed the serena row the default marker pointed at,
		// clear the marker so a stale default cannot route to a workspace that no
		// longer has a live registration (the gap the manual `workspace
		// unregister` already closes; the sweeper previously did not). Best-effort
		// — a clear failure is logged, never fatal to the sweep. Use report.Workspace
		// (the prune owner's canonical form) so it compares equal to the marker's
		// stored canonical path even if `path` arrived in a non-canonical shape.
		if report != nil && report.SerenaRemoved > 0 && pruneClearDefaultFn != nil {
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
		body := map[string]any{"workspace": path, "reason": reason}
		if report != nil {
			body["lsp_removed"] = len(report.LSPRemoved)
			body["serena_removed"] = report.SerenaRemoved
		}
		_ = api.LogHubMcpEvent("info", "workspace-pruned", body)
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
