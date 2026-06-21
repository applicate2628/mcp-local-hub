package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsAgentWorktreePath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"forward-slash agent worktree", "d:/dev/x/.claude/worktrees/agent-a9a6e239b47035d5e", true},
		{"forward-slash agent worktree nested", "d:/dev/x/.claude/worktrees/agent-abc/renderer", true},
		{"windows-backslash agent worktree", `d:\dev\x\.claude\worktrees\agent-abc\sub`, true},
		{"no agent- segment", "d:/dev/x/.claude/worktrees/foo", false},
		{"agentfoo lacks the hyphen", "d:/dev/x/.claude/worktrees/agentfoo", false},
		{"worktrees but not under .claude", "d:/dev/x/worktrees/agent-abc", false},
		{"real project, no worktree", "d:/dev/mcp-local-hub", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAgentWorktreePath(c.path); got != c.want {
				t.Fatalf("IsAgentWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

func TestWorkspaceDirDeleted(t *testing.T) {
	// An existing directory is NOT deleted.
	existing := t.TempDir()
	if WorkspaceDirDeleted(existing) {
		t.Fatalf("WorkspaceDirDeleted(%q) = true for an existing dir; want false", existing)
	}

	// A path under a definitely-nonexistent parent chain → ENOENT → deleted.
	gone := filepath.Join(t.TempDir(), "no-such-parent", "deeper", "workspace")
	if !WorkspaceDirDeleted(gone) {
		t.Fatalf("WorkspaceDirDeleted(%q) = false for a nonexistent path; want true (ENOENT)", gone)
	}

	// Empty path is never "deleted" (guarded).
	if WorkspaceDirDeleted("") {
		t.Fatalf("WorkspaceDirDeleted(\"\") = true; want false")
	}
}

// writeGitFile writes a `.git` regular FILE under dir containing a single
// `gitdir: <target>` pointer (target is written verbatim — caller passes either
// an absolute path or a relative one to exercise both resolution paths).
func writeGitFile(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+target+"\n"), 0o600); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
}

func TestIsDeadGitWorktreePath(t *testing.T) {
	// --- live worktree: .git file → present admin dir → NOT dead ---
	liveWT := t.TempDir()
	liveAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "live")
	if err := os.MkdirAll(liveAdmin, 0o755); err != nil {
		t.Fatalf("mkdir live admin: %v", err)
	}
	writeGitFile(t, liveWT, liveAdmin)

	// --- plain clone: .git is a DIRECTORY → NOT dead (condition 2 fails) ---
	plainClone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plainClone, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git dir: %v", err)
	}

	// --- non-git dir: no .git at all → NOT dead ---
	nonGit := t.TempDir()

	// --- DEAD worktree: .git file → admin dir absent (ENOENT) → DEAD ---
	deadWT := t.TempDir()
	deadAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "gone") // never created
	writeGitFile(t, deadWT, deadAdmin)

	// --- DEAD worktree via RELATIVE pointer (resolved against the dir) → DEAD ---
	deadRelWT := t.TempDir()
	// "../main/.git/worktrees/gone-rel" relative to deadRelWT, never created.
	writeGitFile(t, deadRelWT, filepath.Join("..", "main", ".git", "worktrees", "gone-rel"))

	// --- empty .git file (no gitdir line) → unparsable → NOT dead (ambiguous) ---
	emptyGit := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyGit, ".git"), []byte("\n"), 0o600); err != nil {
		t.Fatalf("write empty .git: %v", err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"empty path", "", false},
		{"live worktree (admin present)", liveWT, false},
		{"plain clone (.git is a dir)", plainClone, false},
		{"non-git dir (no .git)", nonGit, false},
		{"dead worktree (admin ENOENT, abs pointer)", deadWT, true},
		{"dead worktree (admin ENOENT, relative pointer)", deadRelWT, true},
		{"unparsable .git file (no gitdir line)", emptyGit, false},
		{"deleted workspace dir is NOT a dead-worktree (deleted-dir owns it)",
			filepath.Join(t.TempDir(), "no-such", "ws"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}

	// --- submodule on an OFFLINE/unreadable superproject mount: a NON-ENOENT
	// stat error on the admin dir MUST return false (not pruned). Simulate the
	// non-ENOENT stat error with a chmod-000 parent directory so the admin-dir
	// Stat fails with EACCES, not ENOENT. POSIX-only: Windows chmod cannot
	// remove directory traversal, so the EACCES path is unreachable there.
	t.Run("submodule on offline mount (non-ENOENT stat) is NOT pruned", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod-based unreadable-parent simulation is POSIX-only")
		}
		if os.Geteuid() == 0 {
			t.Skip("running as root bypasses the 0000 permission gate")
		}
		subWT := t.TempDir()
		// The submodule's admin dir lives under a parent we will make
		// traverse-denied so Stat(adminDir) fails EACCES (non-ENOENT).
		offlineRoot := t.TempDir()
		denied := filepath.Join(offlineRoot, "denied")
		if err := os.MkdirAll(denied, 0o755); err != nil {
			t.Fatalf("mkdir denied: %v", err)
		}
		adminUnderDenied := filepath.Join(denied, ".git", "modules", "sub")
		if err := os.MkdirAll(adminUnderDenied, 0o755); err != nil {
			t.Fatalf("mkdir admin under denied: %v", err)
		}
		writeGitFile(t, subWT, adminUnderDenied)
		// Remove traversal on `denied` so Stat(adminUnderDenied) → EACCES.
		if err := os.Chmod(denied, 0o000); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(denied, 0o755) }) // restore so TempDir cleanup can remove it

		// Sanity: confirm the admin-dir stat is a NON-ENOENT error (else the
		// test would be vacuous — it must exercise the os.IsNotExist-only guard,
		// not the ENOENT branch).
		if _, err := os.Stat(adminUnderDenied); err == nil || os.IsNotExist(err) {
			t.Skipf("kernel did not produce a non-ENOENT stat error (err=%v); environment cannot exercise this guard", err)
		}

		if IsDeadGitWorktreePath(subWT) {
			t.Fatalf("IsDeadGitWorktreePath(%q) = true for a non-ENOENT admin-dir stat error; must be false (offline-mount/submodule safety)", subWT)
		}
	})
}
