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

	// --- DEAD worktree: .git file → admin dir absent (ENOENT) → DEAD.
	// REALISTIC `git worktree remove` shape: the parent `.git/worktrees/` dir
	// SURVIVES; only the `<name>` subdir is removed. The Finding-2 parent-exists
	// guard requires the parent to be present before treating ENOENT as dead. ---
	deadWT := t.TempDir()
	deadWorktreesParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(deadWorktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir dead worktrees parent: %v", err)
	}
	deadAdmin := filepath.Join(deadWorktreesParent, "gone") // parent exists; subdir never created
	writeGitFile(t, deadWT, deadAdmin)

	// --- DEAD worktree via RELATIVE pointer (resolved against the dir) → DEAD.
	// Same realistic shape: create the parent worktrees/ dir; leave the leaf gone. ---
	deadRelWT := t.TempDir()
	// "../main/.git/worktrees/gone-rel" relative to deadRelWT.
	if err := os.MkdirAll(filepath.Join(deadRelWT, "..", "main", ".git", "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir dead rel worktrees parent: %v", err)
	}
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

// TestIsDeadGitWorktreePath_SubdirWorkspace covers Finding 1: a workspace
// registered as a SUBDIR inside a linked worktree keeps its `.git` pointer at
// the worktree ROOT (an ancestor). The predicate must walk UP to find it.
func TestIsDeadGitWorktreePath_SubdirWorkspace(t *testing.T) {
	// --- DEAD: subdir workspace inside a worktree whose admin dir is gone.
	// Layout:
	//   <root>/.git           (regular FILE → worktree pointer)
	//   <root>/internal/gui/frontend  (the registered subdir workspace)
	//   admin dir's parent worktrees/ exists; the <name> leaf is gone → DEAD.
	deadRoot := t.TempDir()
	deadSub := filepath.Join(deadRoot, "internal", "gui", "frontend")
	if err := os.MkdirAll(deadSub, 0o755); err != nil {
		t.Fatalf("mkdir dead subdir: %v", err)
	}
	deadWorktreesParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(deadWorktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir dead worktrees parent: %v", err)
	}
	writeGitFile(t, deadRoot, filepath.Join(deadWorktreesParent, "gone")) // leaf never created

	// --- LIVE: subdir workspace inside a worktree whose admin dir IS present. ---
	liveRoot := t.TempDir()
	liveSub := filepath.Join(liveRoot, "packages", "app")
	if err := os.MkdirAll(liveSub, 0o755); err != nil {
		t.Fatalf("mkdir live subdir: %v", err)
	}
	liveAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "live")
	if err := os.MkdirAll(liveAdmin, 0o755); err != nil {
		t.Fatalf("mkdir live admin: %v", err)
	}
	writeGitFile(t, liveRoot, liveAdmin)

	// --- PLAIN REPO ancestor: subdir under a normal repo (`.git` is a DIRECTORY
	// at the ancestor) → the walk must STOP at the repo root and return false
	// (a live repo, NOT a dead worktree), never climbing further. ---
	plainRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plainRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir plain .git dir: %v", err)
	}
	plainSub := filepath.Join(plainRoot, "src", "pkg")
	if err := os.MkdirAll(plainSub, 0o755); err != nil {
		t.Fatalf("mkdir plain subdir: %v", err)
	}

	// --- NO `.git` anywhere up the chain → not a worktree → false. ---
	bareSub := filepath.Join(t.TempDir(), "a", "b", "c")
	if err := os.MkdirAll(bareSub, 0o755); err != nil {
		t.Fatalf("mkdir bare subdir: %v", err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"subdir inside DEAD worktree (ancestor .git, admin gone)", deadSub, true},
		{"subdir inside LIVE worktree (ancestor .git, admin present)", liveSub, false},
		{"subdir inside PLAIN repo (ancestor .git is a dir) → stop walk", plainSub, false},
		{"subdir with no .git anywhere up the chain", bareSub, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_UnavailableMount covers Finding 2: an admin dir that
// is ENOENT because its whole admin ROOT is gone/unmounted must NOT be pruned;
// only when the parent `worktrees/` survives but the `<name>` leaf is gone is it
// genuinely a removed worktree.
func TestIsDeadGitWorktreePath_UnavailableMount(t *testing.T) {
	// --- UNAVAILABLE ROOT: the admin dir's PARENT is also ENOENT (simulate an
	// unmounted root — the whole `<mount>/main/.git/worktrees` chain is absent).
	// → ambiguous → NOT pruned. ---
	unmountedWT := t.TempDir()
	// Point at a path whose parent chain is entirely absent.
	unmountedAdmin := filepath.Join(t.TempDir(), "no-such-mount", "main", ".git", "worktrees", "name")
	writeGitFile(t, unmountedWT, unmountedAdmin)

	// --- GENUINE REMOVAL: the parent `.git/worktrees/` EXISTS, only the `<name>`
	// subdir is gone → genuinely a removed worktree → pruned. ---
	removedWT := t.TempDir()
	removedParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(removedParent, 0o755); err != nil {
		t.Fatalf("mkdir removed parent: %v", err)
	}
	writeGitFile(t, removedWT, filepath.Join(removedParent, "gone")) // leaf never created

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"unavailable admin ROOT (parent also ENOENT) → NOT pruned", unmountedWT, false},
		{"genuine worktree removal (parent worktrees/ exists, leaf gone) → pruned", removedWT, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestParseGitWorktreePointer_ForeignGitdir covers Finding 3: a foreign-absolute
// gitdir (drive-letter/UNC on POSIX, POSIX-absolute on Windows) must be rejected
// as AMBIGUOUS (ok=false) so it is never relative-joined under the workspace and
// the live workspace is never pruned. A native absolute and a native relative
// still resolve correctly.
func TestParseGitWorktreePointer_ForeignGitdir(t *testing.T) {
	ws := t.TempDir()

	type ptrCase struct {
		name      string
		target    string
		wantOK    bool
		onlyOn    string // "" = all, "windows" / "posix" = GOOS-gated
		wantAdmin string // expected adminDir when wantOK (empty = don't assert exact)
	}
	cases := []ptrCase{
		// Foreign-absolute on POSIX/WSL: rejected (ok=false).
		{name: "windows drive-letter (forward slash) on POSIX", target: "C:/foo/bar", wantOK: false, onlyOn: "posix"},
		{name: "windows drive-letter (backslash) on POSIX", target: `C:\foo\bar`, wantOK: false, onlyOn: "posix"},
		{name: "UNC path on POSIX", target: `\\srv\share\x`, wantOK: false, onlyOn: "posix"},
		// Foreign-absolute on Windows: a POSIX-absolute `/...` is rejected.
		{name: "posix-absolute on Windows", target: "/foo/bar", wantOK: false, onlyOn: "windows"},
		// Native relative resolves against the workspace dir on every OS.
		{name: "native relative (joined under workspace)", target: filepath.Join("..", "x", "y"), wantOK: true,
			wantAdmin: filepath.Clean(filepath.Join(ws, "..", "x", "y"))},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.onlyOn == "windows" && runtime.GOOS != "windows" {
				t.Skip("windows-only case")
			}
			if c.onlyOn == "posix" && runtime.GOOS == "windows" {
				t.Skip("posix-only case")
			}
			gitPath := filepath.Join(t.TempDir(), ".git")
			if err := os.WriteFile(gitPath, []byte("gitdir: "+c.target+"\n"), 0o600); err != nil {
				t.Fatalf("write .git: %v", err)
			}
			admin, ok := parseGitWorktreePointer(gitPath, ws)
			if ok != c.wantOK {
				t.Fatalf("parseGitWorktreePointer(target=%q) ok = %v, want %v (admin=%q)", c.target, ok, c.wantOK, admin)
			}
			if c.wantOK && c.wantAdmin != "" && admin != c.wantAdmin {
				t.Fatalf("parseGitWorktreePointer(target=%q) admin = %q, want %q", c.target, admin, c.wantAdmin)
			}
		})
	}

	// Native absolute resolves correctly on the current OS (built from t.TempDir
	// so it is a real native-absolute path either way).
	t.Run("native absolute resolves correctly", func(t *testing.T) {
		nativeAbs := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "n")
		gitPath := filepath.Join(t.TempDir(), ".git")
		if err := os.WriteFile(gitPath, []byte("gitdir: "+nativeAbs+"\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		admin, ok := parseGitWorktreePointer(gitPath, ws)
		if !ok {
			t.Fatalf("parseGitWorktreePointer(native abs %q) ok=false, want true", nativeAbs)
		}
		if admin != filepath.Clean(nativeAbs) {
			t.Fatalf("parseGitWorktreePointer(native abs) admin = %q, want %q", admin, filepath.Clean(nativeAbs))
		}
	})
}

// TestIsDeadGitWorktreePath_ForeignGitdirEndToEnd covers Finding 3 at the
// predicate level: a worktree whose `.git` points at a foreign-absolute gitdir
// (the LIVE workspace still exists) must NOT be pruned — the foreign pointer is
// ambiguous, not dead.
func TestIsDeadGitWorktreePath_ForeignGitdirEndToEnd(t *testing.T) {
	var foreign string
	if runtime.GOOS == "windows" {
		foreign = "/opt/repo/.git/worktrees/x" // POSIX-absolute on Windows
	} else {
		foreign = `C:\repo\.git\worktrees\x` // drive-letter on POSIX
	}
	wt := t.TempDir()
	writeGitFile(t, wt, foreign)
	if IsDeadGitWorktreePath(wt) {
		t.Fatalf("IsDeadGitWorktreePath(%q) = true for a foreign-absolute gitdir; must be false (cross-OS ambiguous, never prune)", wt)
	}
}
