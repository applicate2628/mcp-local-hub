package gui

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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
	origEnabled, origRows, origBegin, origEnd, origAction := pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn = origEnabled, origRows, origBegin, origEnd, origAction
	})

	var pruned []string
	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(*Server, string) bool { return true }
	pruneEndFn = func(*Server, string) {}
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
		pruneBeginFn = func(_ *Server, key string) bool { return key != "kbusy" }
		t.Cleanup(func() { pruneBeginFn = func(*Server, string) bool { return true } })
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

// TestSweepPruneWorkspaces_DeadWorktree asserts the sweeper prunes a leftover
// git linked worktree whose directory still exists but whose git admin dir is
// gone — but only after the 2-consecutive-ENOENT-tick grace (shared with
// deleted-dir). A single tick must NOT prune. It also asserts the gate (off)
// suppresses the signal.
func TestSweepPruneWorkspaces_DeadWorktree(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	origEnabled, origRows, origBegin, origEnd, origAction, origDead :=
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneDeadWorktreesFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneDeadWorktreesFn =
			origEnabled, origRows, origBegin, origEnd, origAction, origDead
	})

	var pruned []string
	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(*Server, string) bool { return true }
	pruneEndFn = func(*Server, string) {}
	pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
		pruned = append(pruned, path)
		return &api.PruneReport{Workspace: path}, nil
	}

	// Build a real dead-worktree fixture: a dir that exists, with a `.git` FILE
	// pointing at an admin dir that does NOT exist.
	deadWT := t.TempDir()
	// Realistic `git worktree remove` shape: the admin PARENT (.git/worktrees)
	// exists, only the <name> leaf is gone — so isAdminDirGenuinelyDeleted (r3)
	// reads it as a genuinely removed worktree, not an unavailable/offline admin
	// root. Without the parent dir, the r3 grandparent guard treats the whole
	// chain as offline and refuses to prune (the P1 bot fixture finding).
	deadAdminParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(deadAdminParent, 0o700); err != nil {
		t.Fatalf("mkdir admin parent: %v", err)
	}
	deadAdmin := filepath.Join(deadAdminParent, "gone") // leaf never created
	if err := os.WriteFile(filepath.Join(deadWT, ".git"), []byte("gitdir: "+deadAdmin+"\n"), 0o600); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})

	t.Run("gate on: pruned only after 2 ticks", func(t *testing.T) {
		pruned = nil
		pruneDeadWorktreesFn = func() bool { return true }
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(deadWT, "kdead", "go")}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("tick 1 must NOT prune (2-tick grace shared with deleted-dir), got %d", n)
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("tick 2 must prune the dead worktree, got %d", n)
		}
		if len(pruned) != 1 || pruned[0] != deadWT {
			t.Fatalf("want dead worktree pruned, got %v", pruned)
		}
	})

	t.Run("gate off: never pruned", func(t *testing.T) {
		pruned = nil
		pruneDeadWorktreesFn = func() bool { return false }
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(deadWT, "kdead2", "go")}
		}
		// Two ticks — neither must prune while the gate is off.
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("gate off tick 1 must not prune, got %d", n)
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("gate off tick 2 must not prune, got %d (%v)", n, pruned)
		}
	})
}

// makeDeadWorktreeFixture creates a LIVE directory at wsDir holding a `.git`
// FILE that points at a removed-worktree admin dir (realistic `git worktree
// remove` shape: the `.git/worktrees/` parent exists, the `<name>` leaf is gone)
// so IsDeadGitWorktreePath classifies wsDir as a dead-worktree orphan.
func makeDeadWorktreeFixture(t *testing.T, wsDir string) {
	t.Helper()
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatalf("mkdir dead-worktree ws: %v", err)
	}
	adminParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(adminParent, 0o700); err != nil {
		t.Fatalf("mkdir admin parent: %v", err)
	}
	admin := filepath.Join(adminParent, "gone") // leaf never created → genuinely removed
	if err := os.WriteFile(filepath.Join(wsDir, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatalf("write .git: %v", err)
	}
}

// TestSweepPruneWorkspaces_GraceReasonFlip covers Finding 2: the 2-consecutive-
// tick ENOENT grace is keyed by (path, reason), not path alone. A path that is
// deleted-dir-ENOENT on tick 1 and dead-worktree-ENOENT on tick 2 must NOT prune
// (the reason flipped → the window restarts at 1); only the SAME reason observed
// on two consecutive ticks prunes. Driven with real on-disk state transitions so
// it exercises the real api.ClassifyWorkspaceOrphan path end-to-end.
func TestSweepPruneWorkspaces_GraceReasonFlip(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	origEnabled, origRows, origBegin, origEnd, origAction, origDead :=
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneDeadWorktreesFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneDeadWorktreesFn =
			origEnabled, origRows, origBegin, origEnd, origAction, origDead
	})

	var pruned []string
	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(*Server, string) bool { return true }
	pruneEndFn = func(*Server, string) {}
	pruneDeadWorktreesFn = func() bool { return true }
	pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
		pruned = append(pruned, path)
		return &api.PruneReport{Workspace: path}, nil
	}

	t.Run("reason flip (deleted-dir then dead-worktree) does NOT prune", func(t *testing.T) {
		pruned = nil
		// wsPath starts ABSENT (deleted-dir). It lives under a tmp parent that DOES
		// exist so we can materialize it between ticks; the path itself is gone.
		parent := t.TempDir()
		wsPath := filepath.Join(parent, "ws")
		s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(wsPath, "kflip", "go")}
		}

		// Tick 1: wsPath absent → deleted-dir, count=1 → no prune.
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("tick 1 (deleted-dir, count 1) must not prune, got %d", n)
		}
		// Flip the reason: materialize wsPath as a LIVE dead-worktree dir.
		makeDeadWorktreeFixture(t, wsPath)
		// Tick 2: now dead-worktree → reason flipped from deleted-dir → count RESETS
		// to 1 → must NOT prune. (Pre-Finding-2 this would be count 2 and prune.)
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("tick 2 (reason flip → window reset to 1) must NOT prune, got %d (%v)", n, pruned)
		}
		// Tick 3: still dead-worktree (SAME reason as tick 2) → count=2 → prunes.
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("tick 3 (same reason twice) must prune, got %d", n)
		}
		if len(pruned) != 1 || pruned[0] != wsPath {
			t.Fatalf("want %q pruned once on tick 3, got %v", wsPath, pruned)
		}
	})

	t.Run("same reason twice (dead-worktree) prunes on tick 2", func(t *testing.T) {
		pruned = nil
		wsPath := filepath.Join(t.TempDir(), "ws-same")
		makeDeadWorktreeFixture(t, wsPath)
		s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(wsPath, "ksame", "go")}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
			t.Fatalf("tick 1 (dead-worktree, count 1) must not prune, got %d", n)
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("tick 2 (same reason twice) must prune, got %d", n)
		}
		if len(pruned) != 1 || pruned[0] != wsPath {
			t.Fatalf("want %q pruned on tick 2, got %v", wsPath, pruned)
		}
	})
}

func TestSweepPruneWorkspaces_Idle(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
	oe, orw, ob, oend, oa, oidle := pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneIdleThresholdFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneIdleThresholdFn = oe, orw, ob, oend, oa, oidle
	})

	var pruned []string
	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(*Server, string) bool { return true }
	pruneEndFn = func(*Server, string) {}
	pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
		pruned = append(pruned, path)
		return &api.PruneReport{Workspace: path}, nil
	}
	pruneIdleThresholdFn = func() time.Duration { return 48 * time.Hour }

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	now := time.Now()
	withActivity := func(path, key string, last time.Time) *api.WorkspaceEntry {
		e := mkPruneEntry(path, key, "go")
		e.LastToolsCallAt = last
		return e
	}

	t.Run("idle past threshold pruned", func(t *testing.T) {
		pruned = nil
		idle := t.TempDir() // exists (not deleted) + not agent-worktree → reaches the idle case
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{withActivity(idle, "kidle", now.Add(-72*time.Hour))}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), now); n != 1 {
			t.Fatalf("idle workspace must be pruned, got %d", n)
		}
		if len(pruned) != 1 || pruned[0] != idle {
			t.Fatalf("want idle pruned, got %v", pruned)
		}
	})

	t.Run("recently active kept", func(t *testing.T) {
		pruned = nil
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{withActivity(t.TempDir(), "krecent", now.Add(-1*time.Hour))}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), now); n != 0 {
			t.Fatalf("recently-active workspace must be kept, got %d (%v)", n, pruned)
		}
	})

	t.Run("zero activity never idle-pruned", func(t *testing.T) {
		pruned = nil
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{withActivity(t.TempDir(), "kzero", time.Time{})}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), now); n != 0 {
			t.Fatalf("zero-activity workspace must NEVER be idle-pruned, got %d", n)
		}
	})

	t.Run("threshold 0 disables idle-prune", func(t *testing.T) {
		pruned = nil
		pruneIdleThresholdFn = func() time.Duration { return 0 }
		t.Cleanup(func() { pruneIdleThresholdFn = func() time.Duration { return 48 * time.Hour } })
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{withActivity(t.TempDir(), "koff", now.Add(-72*time.Hour))}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), now); n != 0 {
			t.Fatalf("threshold 0 must disable idle-prune, got %d", n)
		}
	})
}

// TestSweepPruneWorkspaces_ClearsDefaultMarker asserts the sweeper invokes the
// default-marker-clear hook for a pruned workspace whose prune removed a serena
// row (the gap PR-1 closes: the sweeper previously never cleared a stale
// default). It also asserts the hook is NOT called when the prune removed only
// LSP rows (no serena row → the default marker, which only ever points at a
// serena workspace, is untouched).
func TestSweepPruneWorkspaces_ClearsDefaultMarker(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	origEnabled, origRows, origBegin, origEnd, origAction, origClear :=
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneClearDefaultFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneClearDefaultFn =
			origEnabled, origRows, origBegin, origEnd, origAction, origClear
	})

	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(*Server, string) bool { return true }
	pruneEndFn = func(*Server, string) {}

	var cleared []string
	pruneClearDefaultFn = func(canonical string) error {
		cleared = append(cleared, canonical)
		return nil
	}

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})

	t.Run("serena removal clears default marker", func(t *testing.T) {
		cleared = nil
		agentPath := "d:/dev/x/.claude/worktrees/agent-def/sub"
		pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
			return &api.PruneReport{Workspace: path, SerenaRemoved: 1}, nil
		}
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(agentPath, "kdef", api.SerenaLanguageSentinel)}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("want 1 pruned, got %d", n)
		}
		if len(cleared) != 1 || cleared[0] != agentPath {
			t.Fatalf("want default-clear for %q, got %v", agentPath, cleared)
		}
	})

	t.Run("lsp-only removal does not clear default marker", func(t *testing.T) {
		cleared = nil
		agentPath := "d:/dev/x/.claude/worktrees/agent-lsp/sub"
		pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
			return &api.PruneReport{Workspace: path, LSPRemoved: []string{"go"}, SerenaRemoved: 0}, nil
		}
		pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
			return []*api.WorkspaceEntry{mkPruneEntry(agentPath, "klsp", "go")}
		}
		if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 1 {
			t.Fatalf("want 1 pruned, got %d", n)
		}
		if len(cleared) != 0 {
			t.Fatalf("LSP-only prune must NOT clear the default marker, got %v", cleared)
		}
	})
}

func TestSweepPruneWorkspaces_InFlightForwardSkipsPruneAction(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	origEnabled, origRows, origBegin, origEnd, origAction := pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn = origEnabled, origRows, origBegin, origEnd, origAction
	})

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	path := "d:/dev/x/.claude/worktrees/agent-prune-busy/sub"
	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(s *Server, key string) bool { return s.beginSerenaPrune(key) }
	pruneEndFn = func(s *Server, key string) { s.endSerenaPrune(key) }
	pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
		return []*api.WorkspaceEntry{mkPruneEntry(path, "kbusy-real", api.SerenaLanguageSentinel)}
	}
	var actionCalls int
	pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
		actionCalls++
		return &api.PruneReport{Workspace: path}, nil
	}

	s.enterSerenaForward("kbusy-real")
	defer s.exitSerenaForward("kbusy-real")

	if n := s.SweepPruneWorkspaces(context.Background(), time.Now()); n != 0 {
		t.Fatalf("in-flight serena forward must skip prune, got %d", n)
	}
	if actionCalls != 0 {
		t.Fatalf("prune action ran while serena forward was in flight; calls=%d", actionCalls)
	}
}

func TestSweepPruneWorkspaces_ForwardEnteringDuringPruneWaitsForGate(t *testing.T) {
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())

	origEnabled, origRows, origBegin, origEnd, origAction, origClear := pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneClearDefaultFn
	t.Cleanup(func() {
		pruneEnabledFn, pruneWorkspaceRowsFn, pruneBeginFn, pruneEndFn, pruneActionFn, pruneClearDefaultFn = origEnabled, origRows, origBegin, origEnd, origAction, origClear
	})
	// The action returns SerenaRemoved:1, which would trigger the default-marker
	// clear hook; stub it to a hermetic no-op so this concurrency test never
	// reads/writes the live default-workspace marker.
	pruneClearDefaultFn = func(string) error { return nil }

	s := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	path := "d:/dev/x/.claude/worktrees/agent-prune-claim/sub"
	pruneEnabledFn = func() bool { return true }
	pruneBeginFn = func(s *Server, key string) bool { return s.beginSerenaPrune(key) }
	pruneEndFn = func(s *Server, key string) { s.endSerenaPrune(key) }
	pruneWorkspaceRowsFn = func(*Server) []*api.WorkspaceEntry {
		return []*api.WorkspaceEntry{mkPruneEntry(path, "kclaim", api.SerenaLanguageSentinel)}
	}

	actionStarted := make(chan struct{})
	releaseAction := make(chan struct{})
	var actionOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseAction) }) })
	pruneActionFn = func(_ *Server, path string) (*api.PruneReport, error) {
		actionOnce.Do(func() { close(actionStarted) })
		<-releaseAction
		return &api.PruneReport{Workspace: path, SerenaRemoved: 1}, nil
	}

	sweepDone := make(chan int, 1)
	go func() {
		sweepDone <- s.SweepPruneWorkspaces(context.Background(), time.Now())
	}()

	select {
	case <-actionStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("prune action did not start")
	}

	enteredForward := make(chan struct{})
	forwardDone := make(chan struct{})
	go func() {
		s.enterSerenaForward("kclaim")
		close(enteredForward)
		s.exitSerenaForward("kclaim")
		close(forwardDone)
	}()

	select {
	case <-enteredForward:
		t.Fatal("serena forward entered while prune teardown was in progress")
	case <-time.After(50 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(releaseAction) })

	select {
	case n := <-sweepDone:
		if n != 1 {
			t.Fatalf("prune sweep got %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prune sweep did not finish after release")
	}
	select {
	case <-enteredForward:
	case <-time.After(5 * time.Second):
		t.Fatal("serena forward did not enter after prune teardown completed")
	}
	select {
	case <-forwardDone:
	case <-time.After(5 * time.Second):
		t.Fatal("serena forward did not finish after prune teardown completed")
	}
}
