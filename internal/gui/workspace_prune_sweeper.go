package gui

import (
	"context"
	"time"

	"mcp-local-hub/internal/api"
)

// workspace_prune_sweeper.go — Phase 1 of the workspace-daemon auto-prune
// feature: an in-GUI sweep that auto-removes daemons whose workspace is
// STRUCTURALLY dead — an ephemeral `.claude/worktrees/agent-*` worktree, or a
// deleted directory — so the per-workspace serena+LSP daemon set stops growing
// without bound. Mirrors the serena idle sweeper (serena_idle_sweeper.go): a
// per-tick, error-tolerant, cancellable pass driven by runWorkspacePruneTicker
// in the gui-command wiring.
//
// SAFETY (operator-flagged "don't remove a live one"): a workspace is pruned
// only when it is NOT mid-call — we skip any workspace whose serena daemon has
// an in-flight /serena/mcp forward (s.hasSerenaForwardInFlight, the same guard
// the idle sweeper uses). Prune is non-destructive: a pruned workspace
// re-registers on its next open (EnsureLSPRegistered / AutoRegisterSerenaWorkspace),
// so the only residual (an in-flight LSP-only call against a workspace with no
// serena row) at worst drops one short LSP request the client retries.

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

// pruneInFlightFn reports whether the workspace's serena daemon has an open
// /serena/mcp forward (the mid-call guard). Test seam over hasSerenaForwardInFlight.
var pruneInFlightFn = func(s *Server, serenaKey string) bool {
	return s.hasSerenaForwardInFlight(serenaKey)
}

// SweepPruneWorkspaces auto-prunes structurally-dead workspace daemons:
// agent-worktree rows immediately, deleted-directory rows after 2 consecutive
// ENOENT ticks (the in-memory pruneEnoentTicks grace absorbs a transient
// unmount). It skips any workspace with an in-flight serena forward. Returns the
// number of workspaces pruned this tick; a no-op when the gate is off, deps are
// unwired, or there are no workspace rows. ctx/now are accepted for symmetry
// with the idle sweeper and the Phase-3 idle predicate (unused in Phase 1).
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

	// One candidate per workspace PATH (a workspace carries a serena row + N LSP
	// rows). Track the serena key for the in-flight guard.
	type cand struct{ serenaKey string }
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
	}
	if len(byPath) == 0 {
		return 0
	}

	pruned := 0
	for path, c := range byPath {
		// In-flight guard: never prune a workspace mid serena call.
		if c.serenaKey != "" && pruneInFlightFn(s, c.serenaKey) {
			continue
		}

		var reason string
		switch {
		case api.IsAgentWorktreePath(path):
			reason = "agent-worktree" // ephemeral by design → immediate.
		case api.WorkspaceDirDeleted(path):
			s.pruneEnoentMu.Lock()
			s.pruneEnoentTicks[path]++
			n := s.pruneEnoentTicks[path]
			s.pruneEnoentMu.Unlock()
			if n < 2 {
				continue // wait for a confirming second ENOENT tick.
			}
			reason = "deleted-dir"
		default:
			// Healthy, present, non-worktree → reset any stale ENOENT count.
			s.pruneEnoentMu.Lock()
			delete(s.pruneEnoentTicks, path)
			s.pruneEnoentMu.Unlock()
			continue
		}

		report, err := pruneActionFn(s, path)
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
