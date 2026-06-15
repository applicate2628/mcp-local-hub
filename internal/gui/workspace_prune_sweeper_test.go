package gui

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func mkPruneEntry(path, key, lang string) *api.WorkspaceEntry {
	return &api.WorkspaceEntry{WorkspacePath: path, WorkspaceKey: key, Language: lang}
}

func TestSweepPruneWorkspaces(t *testing.T) {
	// Defensive: the sweep path is fully stubbed via the seams below (no real
	// registry/settings/PruneWorkspace), but redirect any incidental state read
	// away from the live fleet per the wiped-intent lesson.
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	// Save + restore every seam.
	origEnabled, origRows, origInFlight, origAction := pruneEnabledFn, pruneWorkspaceRowsFn, pruneInFlightFn, pruneActionFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneInFlightFn, pruneActionFn = origEnabled, origRows, origInFlight, origAction
	})

	var pruned []string
	pruneEnabledFn = func() bool { return true }
	pruneInFlightFn = func(*Server, string) bool { return false }
	pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
		pruned = append(pruned, path)
		return &api.PruneReport{Workspace: path}, nil
	}

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})

	t.Run("agent-worktree pruned immediately", func(t *testing.T) {
		pruned = nil
		agentPath := "d:/dev/x/.claude/worktrees/agent-abc/sub"
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(agentPath, "k1", api.SerenaLanguageSentinel)}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("want 1 pruned, got %d", n)
		}
		if len(pruned) != 1 || pruned[0] != agentPath {
			t.Fatalf("want agent worktree pruned, got %v", pruned)
		}
	})

	t.Run("present non-worktree dir never pruned", func(t *testing.T) {
		pruned = nil
		present := t.TempDir()
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(present, "k2", "go")}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("present dir must not prune, got %d (%v)", n, pruned)
		}
	})

	t.Run("deleted dir pruned only after 2 ticks", func(t *testing.T) {
		pruned = nil
		gone := filepath.Join(t.TempDir(), "no-such", "ws")
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(gone, "k3", "go")}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("tick 1 must NOT prune (2-tick grace), got %d", n)
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("tick 2 must prune, got %d", n)
		}
		if len(pruned) != 1 || pruned[0] != gone {
			t.Fatalf("want deleted dir pruned, got %v", pruned)
		}
	})

	t.Run("in-flight serena skips prune", func(t *testing.T) {
		pruned = nil
		agentPath := "d:/dev/x/.claude/worktrees/agent-busy/sub"
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(agentPath, "kbusy", api.SerenaLanguageSentinel)}
		}
		pruneInFlightFn = func(_ *Server, key string) bool { return key == "kbusy" }
		t.Cleanup(func() { pruneInFlightFn = func(*Server, string) bool { return false } })
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("in-flight workspace must be skipped, got %d (%v)", n, pruned)
		}
	})

	t.Run("gate off is a no-op", func(t *testing.T) {
		pruned = nil
		pruneEnabledFn = func() bool { return false }
		t.Cleanup(func() { pruneEnabledFn = func() bool { return true } })
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry("d:/dev/x/.claude/worktrees/agent-z/s", "kz", api.SerenaLanguageSentinel)}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("gate off must be a no-op, got %d", n)
		}
	})
}
