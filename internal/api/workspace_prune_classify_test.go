package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClassifyWorkspaceOrphan is the table test for the single orphan-
// classification owner. It exercises the three shipped signals plus the
// healthy-and-disabled cases against real on-disk paths.
func TestClassifyWorkspaceOrphan(t *testing.T) {
	live := t.TempDir() // an existing directory (not an agent worktree)
	deleted := filepath.Join(t.TempDir(), "no-such", "ws")
	agent := "d:/dev/x/.claude/worktrees/agent-abc/sub"

	// deadWT: an existing dir that is a leftover git linked worktree whose admin
	// dir is gone → IsDeadGitWorktreePath true (gated by opts.PruneDeadWorktrees).
	deadWT := t.TempDir()
	deadAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "gone") // never created
	if err := os.WriteFile(filepath.Join(deadWT, ".git"), []byte("gitdir: "+deadAdmin+"\n"), 0o600); err != nil {
		t.Fatalf("write .git file: %v", err)
	}

	now := time.Now()

	tests := []struct {
		name       string
		path       string
		opts       ClassifyOpts
		wantReason WorkspaceOrphanReason
		wantOrphan bool
	}{
		{
			name:       "agent worktree → agent-worktree",
			path:       agent,
			opts:       ClassifyOpts{Now: now},
			wantReason: OrphanReasonAgentWorktree,
			wantOrphan: true,
		},
		{
			name:       "deleted dir → deleted-dir",
			path:       deleted,
			opts:       ClassifyOpts{Now: now},
			wantReason: OrphanReasonDeletedDir,
			wantOrphan: true,
		},
		{
			name:       "dead worktree, gate ON → dead-worktree",
			path:       deadWT,
			opts:       ClassifyOpts{PruneDeadWorktrees: true, Now: now},
			wantReason: OrphanReasonDeadWorktree,
			wantOrphan: true,
		},
		{
			name:       "dead worktree, gate OFF → healthy (signal disabled)",
			path:       deadWT,
			opts:       ClassifyOpts{PruneDeadWorktrees: false, Now: now},
			wantReason: "",
			wantOrphan: false,
		},
		{
			name:       "live dir, no idle threshold → healthy",
			path:       live,
			opts:       ClassifyOpts{Now: now},
			wantReason: "",
			wantOrphan: false,
		},
		{
			name: "idle past threshold → idle",
			path: live,
			opts: ClassifyOpts{
				IdleThreshold:   48 * time.Hour,
				LastToolsCallAt: now.Add(-72 * time.Hour),
				Now:             now,
			},
			wantReason: OrphanReasonIdle,
			wantOrphan: true,
		},
		{
			name: "idle threshold 0 disables idle → healthy",
			path: live,
			opts: ClassifyOpts{
				IdleThreshold:   0,
				LastToolsCallAt: now.Add(-72 * time.Hour),
				Now:             now,
			},
			wantReason: "",
			wantOrphan: false,
		},
		{
			name: "recently active within threshold → healthy",
			path: live,
			opts: ClassifyOpts{
				IdleThreshold:   48 * time.Hour,
				LastToolsCallAt: now.Add(-1 * time.Hour),
				Now:             now,
			},
			wantReason: "",
			wantOrphan: false,
		},
		{
			name: "zero activity timestamp never idle-pruned",
			path: live,
			opts: ClassifyOpts{
				IdleThreshold:   48 * time.Hour,
				LastToolsCallAt: time.Time{},
				Now:             now,
			},
			wantReason: "",
			wantOrphan: false,
		},
		{
			name: "agent worktree wins over idle (priority)",
			path: agent,
			opts: ClassifyOpts{
				IdleThreshold:   48 * time.Hour,
				LastToolsCallAt: now.Add(-72 * time.Hour),
				Now:             now,
			},
			wantReason: OrphanReasonAgentWorktree,
			wantOrphan: true,
		},
		{
			name: "deleted dir wins over idle (priority)",
			path: deleted,
			opts: ClassifyOpts{
				IdleThreshold:   48 * time.Hour,
				LastToolsCallAt: now.Add(-72 * time.Hour),
				Now:             now,
			},
			wantReason: OrphanReasonDeletedDir,
			wantOrphan: true,
		},
		{
			// A deleted dir with the dead-worktree gate ON still classifies as
			// deleted-dir: WorkspaceDirDeleted (ENOENT) fires before the
			// dead-worktree case, and IsDeadGitWorktreePath requires the dir to
			// still exist, so the two are mutually exclusive — deleted-dir wins.
			name: "deleted dir wins over dead-worktree (priority + mutual exclusion)",
			path: deleted,
			opts: ClassifyOpts{
				PruneDeadWorktrees: true,
				Now:                now,
			},
			wantReason: OrphanReasonDeletedDir,
			wantOrphan: true,
		},
		{
			name: "dead worktree wins over idle (priority)",
			path: deadWT,
			opts: ClassifyOpts{
				PruneDeadWorktrees: true,
				IdleThreshold:      48 * time.Hour,
				LastToolsCallAt:    now.Add(-72 * time.Hour),
				Now:                now,
			},
			wantReason: OrphanReasonDeadWorktree,
			wantOrphan: true,
		},
		{
			name: "idle with zero Now disables idle → healthy",
			path: live,
			opts: ClassifyOpts{
				IdleThreshold:   48 * time.Hour,
				LastToolsCallAt: now.Add(-72 * time.Hour),
				Now:             time.Time{},
			},
			wantReason: "",
			wantOrphan: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotReason, gotOrphan := ClassifyWorkspaceOrphan(tc.path, tc.opts)
			if gotOrphan != tc.wantOrphan {
				t.Fatalf("isOrphan = %v, want %v (reason %q)", gotOrphan, tc.wantOrphan, gotReason)
			}
			if gotReason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", gotReason, tc.wantReason)
			}
		})
	}
}
