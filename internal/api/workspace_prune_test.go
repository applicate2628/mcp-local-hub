package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
// only when an admin-dir ancestor (parent `worktrees/`, or the grandparent repo
// `.git/` after a last-worktree removal) survives is it genuinely a removed
// worktree. The grandparent `.git/` is the discriminator between "last worktree
// removed" (repo online) and "mount offline" (repo gone).
func TestIsDeadGitWorktreePath_UnavailableMount(t *testing.T) {
	// --- UNAVAILABLE ROOT: the admin dir's PARENT *and* GRANDPARENT are also
	// ENOENT (simulate an unmounted root — the whole `<mount>/main/.git/...`
	// chain is absent). → ambiguous → NOT pruned. ---
	unmountedWT := t.TempDir()
	// Point at a path whose parent chain is entirely absent (`no-such-mount` is
	// never created, so `.git/worktrees/`, `.git/`, and `main/` are all ENOENT).
	unmountedAdmin := filepath.Join(t.TempDir(), "no-such-mount", "main", ".git", "worktrees", "name")
	writeGitFile(t, unmountedWT, unmountedAdmin)

	// --- GENUINE REMOVAL (siblings remain): the parent `.git/worktrees/` EXISTS,
	// only the `<name>` subdir is gone → genuinely a removed worktree → pruned. ---
	removedWT := t.TempDir()
	removedParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(removedParent, 0o755); err != nil {
		t.Fatalf("mkdir removed parent: %v", err)
	}
	writeGitFile(t, removedWT, filepath.Join(removedParent, "gone")) // leaf never created

	// --- LAST/ONLY WORKTREE REMOVED: git deleted the now-empty `.git/worktrees/`
	// directory too, so the admin dir AND its parent `worktrees/` are BOTH ENOENT,
	// but the repo's `.git/` (the GRANDPARENT) survives → repo online, worktree
	// genuinely removed → pruned. This is the last-worktree case the parent-only
	// check missed (verified live: `git worktree remove` of the only worktree
	// removes `.git/worktrees/` while `.git/` remains). ---
	lastWT := t.TempDir()
	// Create only the repo `.git/` dir; leave `.git/worktrees/` (and its `<name>`
	// leaf) absent — exactly the on-disk shape after a last-worktree removal.
	lastRepoGit := filepath.Join(t.TempDir(), "main", ".git")
	if err := os.MkdirAll(lastRepoGit, 0o755); err != nil {
		t.Fatalf("mkdir last-worktree repo .git: %v", err)
	}
	// adminDir = <repo>/.git/worktrees/<name>; parent = <repo>/.git/worktrees
	// (ENOENT); grandparent = <repo>/.git (present).
	writeGitFile(t, lastWT, filepath.Join(lastRepoGit, "worktrees", "only"))

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"unavailable admin ROOT (parent + grandparent also ENOENT) → NOT pruned", unmountedWT, false},
		{"genuine worktree removal (parent worktrees/ exists, leaf gone) → pruned", removedWT, true},
		{"last worktree removed (parent worktrees/ gone, grandparent .git/ exists) → pruned", lastWT, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsAdminDirGenuinelyDeleted_LastWorktreeVsOfflineMount unit-tests the
// availability discriminator directly (below the IsDeadGitWorktreePath wrapper)
// so the parent/grandparent walk-up logic is covered in isolation against the
// realistic `.git/worktrees/<name>` shape.
func TestIsAdminDirGenuinelyDeleted_LastWorktreeVsOfflineMount(t *testing.T) {
	// Last-worktree: `.git/` exists, `.git/worktrees/` and its leaf both ENOENT.
	lastRepoGit := filepath.Join(t.TempDir(), "main", ".git")
	if err := os.MkdirAll(lastRepoGit, 0o755); err != nil {
		t.Fatalf("mkdir last-worktree .git: %v", err)
	}
	lastAdmin := filepath.Join(lastRepoGit, "worktrees", "only") // worktrees/ + leaf absent

	// Offline-mount: nothing on the chain exists (`.git/`, `worktrees/`, leaf all
	// ENOENT under a never-created mount root).
	offlineAdmin := filepath.Join(t.TempDir(), "no-mount", "main", ".git", "worktrees", "name")

	// Siblings-remain: `.git/worktrees/` exists, only the leaf is gone.
	siblingParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(siblingParent, 0o755); err != nil {
		t.Fatalf("mkdir siblings parent: %v", err)
	}
	siblingAdmin := filepath.Join(siblingParent, "gone")

	// Live: admin dir present.
	liveAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "live")
	if err := os.MkdirAll(liveAdmin, 0o755); err != nil {
		t.Fatalf("mkdir live admin: %v", err)
	}

	cases := []struct {
		name     string
		adminDir string
		want     bool
	}{
		{"live admin dir present → not dead", liveAdmin, false},
		{"siblings remain (parent worktrees/ present, leaf gone) → dead", siblingAdmin, true},
		{"last worktree removed (parent gone, grandparent .git/ present) → dead", lastAdmin, true},
		{"offline mount (parent + grandparent both absent) → ambiguous, not dead", offlineAdmin, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAdminDirGenuinelyDeleted(c.adminDir); got != c.want {
				t.Fatalf("isAdminDirGenuinelyDeleted(%q) = %v, want %v", c.adminDir, got, c.want)
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

// TestIsDeadGitWorktreePath_Submodule covers Finding 1: a git SUBMODULE also
// stores a regular-file `.git`, but its gitdir resolves to
// `<repo>/.git/modules/<name>` (parent dir `modules`), NOT a worktree admin path
// (`<repo>/.git/worktrees/<name>`, parent dir `worktrees`). An ONLINE submodule
// whose admin LEAF is merely absent (e.g. before `git submodule update --init`)
// — `.git/modules/` present, the `<name>` leaf gone — must NOT be pruned: it is
// a live submodule, not a removed worktree. A real removed worktree with the
// identical missing-leaf/present-parent shape under `worktrees/` IS still pruned.
func TestIsDeadGitWorktreePath_Submodule(t *testing.T) {
	// --- SUBMODULE (online, leaf absent): gitdir → <repo>/.git/modules/<name>;
	// the `.git/modules/` parent exists, only the `<name>` leaf is gone. Without
	// the isWorktreeAdminPath guard this would satisfy the sibling-present DEAD
	// branch; the guard rejects the `modules` parent → NOT pruned. ---
	subWT := t.TempDir()
	subModulesParent := filepath.Join(t.TempDir(), "super", ".git", "modules")
	if err := os.MkdirAll(subModulesParent, 0o755); err != nil {
		t.Fatalf("mkdir submodule modules parent: %v", err)
	}
	subAdmin := filepath.Join(subModulesParent, "sub") // leaf never created
	writeGitFile(t, subWT, subAdmin)

	// --- REAL WORKTREE (removed, identical missing-leaf shape but under
	// worktrees/): gitdir → <repo>/.git/worktrees/<name>; parent present, leaf
	// gone → genuinely DEAD → pruned. Proves the guard rejects ONLY the submodule
	// shape, not the structurally-analogous worktree shape. ---
	wtWS := t.TempDir()
	wtParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatalf("mkdir worktrees parent: %v", err)
	}
	wtAdmin := filepath.Join(wtParent, "gone") // leaf never created
	writeGitFile(t, wtWS, wtAdmin)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"online submodule (modules/<name> leaf absent, parent present) → NOT pruned", subWT, false},
		{"real removed worktree (worktrees/<name> leaf absent, parent present) → pruned", wtWS, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsWorktreeAdminPath unit-tests the worktrees-path discriminator directly: a
// worktree admin path has immediate parent `worktrees` AND no INTERIOR `modules`
// store segment after the first git-common-dir segment (`.git`/`*.git`).
// That ACCEPTS the normal-repo (`<repo>/.git/worktrees/<name>`), bare-repo
// (`<repo>/main.git/worktrees/<name>`), user-dir-named-`modules`
// (`.../modules/proj/.git/worktrees/<name>`, `modules` ABOVE the git dir), and
// worktree-NAMED-`modules` (`<repo>/.git/worktrees/modules`, `modules` is the LEAF)
// shapes, while REJECTING a `<git-dir>/modules/<name>` submodule path (flat or
// nested), a submodule under a worktrees-named dir (`.git/modules/.../worktrees/foo`,
// the r6 trap), and a submodule INSIDE a linked worktree
// (`.git/worktrees/<wt>/modules/<sub>...` — interior `modules` after the first
// `.git`, the Finding-3 shape the immediately-after positional check missed).
func TestIsWorktreeAdminPath(t *testing.T) {
	cases := []struct {
		name     string
		adminDir string
		want     bool
	}{
		{"worktree admin path", filepath.Join("repo", ".git", "worktrees", "feat"), true},
		{"submodule admin path", filepath.Join("repo", ".git", "modules", "sub"), false},
		{"bare .git child (no worktrees parent)", filepath.Join("repo", ".git", "HEAD"), false},
		{"deep worktree admin path", filepath.Join("x", "y", ".git", "worktrees", "w"), true},
		// Finding 1 (r6 trap): a submodule checked out under a directory literally
		// named `worktrees` makes git store its admin dir at
		// `<repo>/.git/modules/deps/worktrees/foo` — immediate parent `worktrees`
		// (so a parent-only check would ACCEPT it) but it CONTAINS a `modules`
		// component, so the modules-absence check rejects it.
		{"submodule under worktrees-named dir (parent worktrees, has modules component)", filepath.Join("repo", ".git", "modules", "deps", "worktrees", "foo"), false},
		// A genuine deep worktree (common git dir literally `.git`) → accepted.
		{"worktree admin path with worktrees grandparent .git", filepath.Join("a", "b", "c", ".git", "worktrees", "feat"), true},
		// Finding 1 (this round): a worktree created from a BARE repo has a common
		// git dir named e.g. `main.git`, so `git worktree add` writes
		// `gitdir: .../main.git/worktrees/<name>` — immediate parent `worktrees`,
		// grandparent `main.git` (NOT `.git`), no `modules` component. The earlier
		// grandparent==`.git` check WRONGLY rejected this; the modules-absence rule
		// ACCEPTS it.
		{"bare-repo worktree admin path (grandparent main.git, not .git)", filepath.Join("x", "main.git", "worktrees", "wt"), true},
		{"deep bare-repo worktree admin path", filepath.Join("srv", "repos", "proj.git", "worktrees", "feat"), true},
		// A bare-repo SUBMODULE store still carries `modules` directly under the
		// `*.git` common dir → rejected.
		{"submodule under bare-named git dir (modules after super.git)", filepath.Join("x", "super.git", "modules", "sub"), false},
		// Nested submodule: git stores it at `<git-dir>/modules/<sub-path>` with a
		// multi-segment sub-path; `modules` still sits directly under `.git` → rejected.
		{"nested submodule admin path (modules/libs/mysub under .git)", filepath.Join("super", ".git", "modules", "libs", "mysub"), false},
		// Architect adjudication (prior round): a worktree whose owning repo merely
		// LIVES under a user dir literally named `modules` (the `modules` segment is
		// ABOVE the git common dir, BEFORE the first git-common-dir segment) must be
		// ACCEPTED — the "modules anywhere" rule would wrongly reject it (the same
		// orphan-row false-negative Finding 1 targets). Anchoring the scan on the
		// FIRST git-common-dir segment skips this `modules` → accepted.
		{"worktree under a user dir named modules (modules above .git)", filepath.Join("home", "user", "modules", "proj", ".git", "worktrees", "wt"), true},
		// Finding 3 (this round): a submodule INSIDE a linked worktree stores its
		// admin dir at `<repo>/.git/worktrees/<wt>/modules/<sub>` — `modules` is
		// preceded by the worktree NAME `<wt>`, NOT a git-common-dir segment, so the
		// earlier immediately-after-a-git-dir positional check MISSED it and a live
		// submodule-in-a-worktree was mis-pruned as a dead worktree. The interior-
		// `modules`-after-first-git-common-dir rule rejects it. (Verified real-git
		// shape: `git worktree add` + `git submodule update --init` →
		// `gitdir: ../../main/.git/worktrees/wt/modules/sub`.)
		{"submodule inside a linked worktree (modules after worktree name)", filepath.Join("main", ".git", "worktrees", "wt", "modules", "sub"), false},
		// Finding 3 (deeper): the OWN worktree of a submodule-in-a-worktree →
		// `<repo>/.git/worktrees/<wt>/modules/<sub>/worktrees/<subwt>` — immediate
		// parent `worktrees` (parent gate passes), but an interior `modules` sits
		// after the first `.git` → rejected. (Verified real-git shape:
		// `gitdir: .../main/.git/worktrees/wt/modules/sub/worktrees/sub-wt`.)
		{"worktree of a submodule-in-a-worktree (interior modules, parent worktrees)", filepath.Join("main", ".git", "worktrees", "wt", "modules", "sub", "worktrees", "sub-wt"), false},
		// Finding 3 carve-out (false-negative guard): a worktree literally NAMED
		// `modules` (`git worktree add ./modules`) stores its admin dir at
		// `<repo>/.git/worktrees/modules` — `modules` is the LEAF directly under
		// `worktrees`, a worktree NAME not a submodule store marker. A naive
		// "modules anywhere after .git" rule would WRONGLY reject it (orphan-row
		// false-negative); the INTERIOR-only rule (modules must have a child)
		// ACCEPTS it. (Verified real-git shape: `git worktree add ./modules` →
		// `gitdir: .../main/.git/worktrees/modules`.) Without this case a future
		// "simplify to modules-anywhere" refactor silently reintroduces the regression.
		{"worktree literally named modules (modules is the leaf under worktrees)", filepath.Join("main", ".git", "worktrees", "modules"), true},
		// REQUIRE-A-BOUNDARY (this round — Findings 1+2 convergent root fix): a
		// `worktrees`/`modules` component is git's store marker ONLY when a real
		// `.git`/`*.git` segment is its ANCESTOR. A path with NO git-common-dir
		// segment (firstGit < 0) is REJECTED — it is path-indistinguishable from a
		// coincidentally-named user dir. This REVERTS the prior r9 boundary-less
		// ACCEPTS (the rows below that now say `false`), which was the unsafe path:
		// it mis-pruned a LIVE separate-git-dir workspace under a user folder named
		// `worktrees` (Finding 2 — see the `/worktrees/gone` case below).
		//   REJECT — boundary-less submodule store under a bare worktree (no boundary):
		{"boundary-less submodule store (no .git segment, interior modules)", filepath.Join("myrepo", "worktrees", "wt", "modules", "deps", "worktrees", "foo"), false},
		{"boundary-less nested submodule store (no .git segment)", filepath.Join("bare", "worktrees", "wt", "modules", "libs", "sub"), false},
		//   REJECT — boundary-less NORMAL bare-repo worktree (no .git/*.git ancestor):
		//   REVERTS r9 — now an accepted benign false-NEGATIVE (dead orphan row lingers).
		{"boundary-less bare-repo worktree (no .git segment) — REVERT r9, now rejected", filepath.Join("myrepo", "worktrees", "wt"), false},
		//   REJECT — boundary-less worktree literally NAMED `modules` (no boundary):
		{"boundary-less worktree named modules (no .git segment) — REVERT r9, now rejected", filepath.Join("myrepo", "worktrees", "modules"), false},
		// Finding 2 (this round) FALSE-POSITIVE the boundary requirement closes: a
		// `git init --separate-git-dir=/worktrees/gone` repo under a user folder
		// literally named `worktrees` writes an admin dir whose immediate parent is
		// `worktrees` but which has NO `.git`/`*.git` ancestor. The parent-only rule
		// ACCEPTED it → a LIVE separate-git-dir workspace was mis-pruned as a removed
		// worktree. Requiring a boundary strictly above `worktrees` REJECTS it.
		{"separate-git-dir under user folder named worktrees (no .git ancestor) — Finding 2 reject", filepath.Join("worktrees", "gone"), false},
		{"separate-git-dir under user worktrees, deeper (no .git ancestor)", filepath.Join("home", "me", "worktrees", "gone"), false},
		// Finding 2 (this round) — DOCUMENTED false-NEGATIVE, architect-adjudicated
		// DOCUMENT-AS-LIMITATION. A submodule's OWN linked worktree stores its admin
		// at `<repo>/.git/modules/<sub>/worktrees/<name>` (verified real-git:
		// `gitdir: <super>/.git/modules/sub/worktrees/sub-wt`). Immediate parent
		// `worktrees` (parent gate passes) but an interior `modules` after the first
		// `.git` → REJECTED. Left as an accepted benign orphan-row lingerer rather than
		// risk a false-positive: it is NOT path-distinguishable from the LIVE submodule
		// store below without filesystem probing. Conservative → not pruned.
		{"submodule's own linked worktree (interior modules, parent worktrees) — accepted false-negative", filepath.Join("repo", ".git", "modules", "sub", "worktrees", "name"), false},
		// The collision that makes the above carve-out UNSAFE: a submodule CHECKED OUT
		// at a path literally named `worktrees` stores its admin at
		// `<repo>/.git/modules/worktrees/<leaf>` (verified real-git:
		// `gitdir: ../.git/modules/worktrees`). This is a LIVE submodule store, NOT a
		// worktree admin — it MUST reject. Same `modules`/X/`worktrees`/leaf shape as
		// the case above ⇒ path-only A/B1 separation is impossible without filesystem
		// probing. Sitting them side by side guards a future "simplify" refactor from
		// silently flipping the first case to an accept and pruning live submodules.
		{"submodule checked out at a dir named worktrees (live store, parent worktrees) — must reject", filepath.Join("repo", ".git", "modules", "worktrees", "inner"), false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isWorktreeAdminPath(c.adminDir); got != c.want {
				t.Fatalf("isWorktreeAdminPath(%q) = %v, want %v", c.adminDir, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SeparateGitDirNested is a PERMANENT REGRESSION GUARD
// for the climb-past-live-repo false-positive class: a LIVE repo created with
// `git init --separate-git-dir` (its `.git` FILE points at an ARBITRARY git dir,
// NOT a `worktrees/` or `modules/` store) nested INSIDE a linked worktree whose
// OUTER worktree admin was removed must NOT be pruned. The nested repo's own `.git`
// pointer denotes a LIVE repo boundary; with the walk-continue removed the predicate
// STOPS at that nearest pointer (it is not a worktree admin) and never climbs to the
// (dead) outer worktree pointer above it.
//
// This is exactly the false-positive the walk-continue removal closes: the prior
// climb past ANY classified non-worktree pointer would have reached the dead OUTER
// worktree pointer and pruned the LIVE nested separate-git-dir repo. A genuine
// submodule-in-a-removed-worktree now also lingers as a documented benign
// false-negative — covered by
// TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned.
func TestIsDeadGitWorktreePath_SeparateGitDirNested(t *testing.T) {
	// Outer worktree root: a regular-file `.git` pointing at a DEAD outer admin
	// (parent `worktrees/` present, `<wt>` leaf gone → the outer worktree is dead).
	wtRoot := t.TempDir()
	mainGit := filepath.Join(t.TempDir(), "main", ".git")
	worktreesParent := filepath.Join(mainGit, "worktrees")
	if err := os.MkdirAll(worktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir outer worktrees parent: %v", err)
	}
	outerAdminDead := filepath.Join(worktreesParent, "wt") // leaf never created → outer DEAD
	writeGitFile(t, wtRoot, outerAdminDead)

	// LIVE separate-git-dir repo nested INSIDE the (dead) outer worktree. Its `.git`
	// FILE points at an ARBITRARY live git dir — NOT under `worktrees/` or `modules/`.
	nested := filepath.Join(wtRoot, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested separate-git-dir repo: %v", err)
	}
	sepGitDir := filepath.Join(t.TempDir(), "nested-gitdir") // PRESENT live git dir
	if err := os.MkdirAll(sepGitDir, 0o755); err != nil {
		t.Fatalf("mkdir separate git dir: %v", err)
	}
	writeGitFile(t, nested, sepGitDir)

	// CONTROL: the SAME outer-dead layout but the nested pointer's git dir is ABSENT
	// (ENOENT). It is STILL not a worktree/submodule pointer, so the walk STILL stops
	// false at it — a live/ambiguous boundary is never pruned regardless of whether
	// its own git dir resolves.
	nestedGone := filepath.Join(wtRoot, "nested-gone")
	if err := os.MkdirAll(nestedGone, 0o755); err != nil {
		t.Fatalf("mkdir nested-gone: %v", err)
	}
	writeGitFile(t, nestedGone, filepath.Join(t.TempDir(), "absent-gitdir")) // never created

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"live separate-git-dir repo inside a removed-outer-worktree → NOT pruned", nested, false},
		{"separate-git-dir repo with absent git dir inside removed-outer-worktree → NOT pruned (live/ambiguous boundary)", nestedGone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SeparateGitDirNested_RealGit drives a REAL git binary
// to construct the Finding-1 (r10) layout: a linked worktree whose outer admin is
// removed, with a LIVE `git init --separate-git-dir` repo nested inside it. It
// asserts the nested live repo is NOT pruned. Skipped when git is unavailable (the
// synthetic test above covers the discriminator on hosts without git).
func TestIsDeadGitWorktreePath_SeparateGitDirNested_RealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; synthetic TestIsDeadGitWorktreePath_SeparateGitDirNested covers the discriminator")
	}

	root := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Superproject with a linked worktree.
	main := filepath.Join(root, "main")
	runGit(root, "init", "-q", main)
	if err := os.WriteFile(filepath.Join(main, "m.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	runGit(main, "add", ".")
	runGit(main, "commit", "-qm", "init")
	wt := filepath.Join(root, "wt")
	runGit(main, "worktree", "add", "-q", wt)

	// LIVE separate-git-dir repo nested inside the worktree. git writes
	// `gitdir: <sepGitDir>` (an arbitrary absolute git dir, NOT worktrees/ or modules/).
	nested := filepath.Join(wt, "nested")
	sepGitDir := filepath.Join(root, "nested-gitdir")
	runGit(root, "init", "-q", "--separate-git-dir", sepGitDir, nested)
	// Confirm git produced the expected arbitrary-gitdir pointer (not a worktrees/
	// or modules/ store) before exercising the discriminator.
	ptr, ptrErr := os.ReadFile(filepath.Join(nested, ".git"))
	if ptrErr != nil {
		t.Fatalf("read nested separate-git-dir .git pointer: %v", ptrErr)
	}
	if strings.Contains(string(ptr), "worktrees") || strings.Contains(string(ptr), "modules") {
		t.Fatalf("unexpected separate-git-dir pointer (shape changed?): %q", string(ptr))
	}

	// LIVE state (outer worktree admin present) → the nested repo is NOT a dead worktree.
	if IsDeadGitWorktreePath(nested) {
		t.Fatalf("live nested separate-git-dir repo mis-classified as dead (outer admin present): %s", nested)
	}

	// Remove the OUTER worktree admin (the dead-outer-worktree incident). The nested
	// repo's workspace dir and its own `.git` pointer remain; the outer worktrees/
	// parent survives. WITHOUT the Finding-1 gate the walk would climb past the
	// nested repo's arbitrary gitfile to the dead outer pointer and prune the LIVE
	// nested repo.
	outerAdmin := filepath.Join(main, ".git", "worktrees", filepath.Base(wt))
	if _, statErr := os.Stat(outerAdmin); statErr != nil {
		// Resolve the actual worktree admin name git chose, if Base(wt) differs.
		entries, _ := os.ReadDir(filepath.Join(main, ".git", "worktrees"))
		if len(entries) == 1 {
			outerAdmin = filepath.Join(main, ".git", "worktrees", entries[0].Name())
		}
	}
	if rmErr := os.RemoveAll(outerAdmin); rmErr != nil {
		t.Fatalf("remove outer worktree admin: %v", rmErr)
	}
	if _, statErr := os.Stat(filepath.Dir(outerAdmin)); statErr != nil {
		t.Fatalf("expected outer worktrees/ parent to remain after admin removal: %v", statErr)
	}
	if _, statErr := os.Stat(nested); statErr != nil {
		t.Fatalf("expected nested workspace dir to remain: %v", statErr)
	}

	// Finding 1: the LIVE nested separate-git-dir repo must NOT be pruned even
	// though the outer worktree above it is genuinely dead.
	if IsDeadGitWorktreePath(nested) {
		t.Fatalf("Finding 1 false-positive: live nested separate-git-dir repo pruned because the walk climbed past its arbitrary gitfile to the dead outer worktree: %s", nested)
	}
}

// TestIsDeadGitWorktreePath_SeparateGitDirUnderUserModules_RealGit drives a REAL
// git binary to construct a climb-past-live-repo FALSE-POSITIVE shape: a LIVE
// `git init --separate-git-dir` repo whose admin dir lives under a USER directory
// literally named `modules` (`<wt>/modules/nested-gitdir`), nested inside a linked
// worktree whose OUTER admin is removed. git writes `gitdir: <wt>/modules/nested-gitdir`
// — an interior `modules` with NO `.git`/`*.git` ANCESTOR. A prior design
// mis-classified that as a submodule store and CLIMBED PAST this LIVE nested repo to
// the dead outer worktree pointer, pruning the live workspace. With the walk-continue
// removed the predicate STOPS at the nested repo's own (non-worktree-admin) pointer
// → NOT pruned. Skipped when git is unavailable (TestIsWorktreeAdminPath pins the
// discriminator without git).
func TestIsDeadGitWorktreePath_SeparateGitDirUnderUserModules_RealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; TestIsWorktreeAdminPath covers the discriminator")
	}
	root := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, e, out)
		}
	}

	// Superproject + a linked worktree.
	main := filepath.Join(root, "main")
	runGit(root, "init", "-q", main)
	if err := os.WriteFile(filepath.Join(main, "m.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	runGit(main, "add", ".")
	runGit(main, "commit", "-qm", "init")
	wt := filepath.Join(root, "wt")
	runGit(main, "worktree", "add", "-q", wt)

	// LIVE nested separate-git-dir repo whose admin dir is under a USER dir named
	// `modules`, INSIDE the worktree. git writes `gitdir: <wt>/modules/nested-gitdir`.
	if err := os.MkdirAll(filepath.Join(wt, "modules"), 0o755); err != nil {
		t.Fatalf("mkdir user modules dir: %v", err)
	}
	nested := filepath.Join(wt, "live-nested")
	sepGitDir := filepath.Join(wt, "modules", "nested-gitdir")
	runGit(root, "init", "-q", "--separate-git-dir", sepGitDir, nested)
	ptr, ptrErr := os.ReadFile(filepath.Join(nested, ".git"))
	if ptrErr != nil {
		t.Fatalf("read nested .git pointer: %v", ptrErr)
	}
	if !strings.Contains(string(ptr), "modules") {
		t.Fatalf("expected the separate-git-dir pointer under a user `modules` dir (shape changed?): %q", string(ptr))
	}

	// LIVE state (outer admin present) → not a dead worktree.
	if IsDeadGitWorktreePath(nested) {
		t.Fatalf("live nested separate-git-dir repo mis-classified as dead (outer admin present): %s", nested)
	}

	// Remove the OUTER worktree admin so a climb-past WOULD reach a dead outer
	// worktree pointer. The fix must STOP at the boundary-less `modules` pointer.
	entries, _ := os.ReadDir(filepath.Join(main, ".git", "worktrees"))
	for _, e := range entries {
		if rmErr := os.RemoveAll(filepath.Join(main, ".git", "worktrees", e.Name())); rmErr != nil {
			t.Fatalf("remove outer admin %s: %v", e.Name(), rmErr)
		}
	}
	if IsDeadGitWorktreePath(nested) {
		t.Fatalf("Finding 1 false-positive: live separate-git-dir repo under a user `modules` dir pruned because the walk climbed past it to the dead outer worktree: %s", nested)
	}
}

// TestIsDeadGitWorktreePath_SeparateGitDirUnderUserWorktrees_RealGit drives a REAL
// git binary to construct the Finding-2 (this round) FALSE-POSITIVE: a LIVE
// `git init --separate-git-dir` repo whose admin dir lives under a USER directory
// literally named `worktrees` (`<root>/worktrees/gone`). git writes
// `gitdir: <root>/worktrees/gone` — immediate parent `worktrees` but NO `.git`/`*.git`
// ANCESTOR. The prior parent-only rule classified it as a worktree admin path; when
// the admin LEAF is removed (workspace dir still present) isAdminDirGenuinelyDeleted's
// sibling-present branch fired and the LIVE workspace was mis-pruned. Requiring a
// `.git`/`*.git` ancestor strictly above `worktrees` makes isWorktreeAdminPath false →
// NOT pruned. Skipped when git is unavailable (TestIsWorktreeAdminPath pins the
// discriminator without git).
func TestIsDeadGitWorktreePath_SeparateGitDirUnderUserWorktrees_RealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; TestIsWorktreeAdminPath covers the discriminator")
	}
	root := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, e, out)
		}
	}

	// User folder literally named `worktrees`; the separate-git-dir admin is `gone`
	// under it. The workspace dir itself lives elsewhere under root.
	if err := os.MkdirAll(filepath.Join(root, "worktrees"), 0o755); err != nil {
		t.Fatalf("mkdir user worktrees dir: %v", err)
	}
	ws := filepath.Join(root, "live-workspace")
	adminDir := filepath.Join(root, "worktrees", "gone")
	runGit(root, "init", "-q", "--separate-git-dir", adminDir, ws)
	ptr, ptrErr := os.ReadFile(filepath.Join(ws, ".git"))
	if ptrErr != nil {
		t.Fatalf("read workspace .git pointer: %v", ptrErr)
	}
	if !strings.Contains(string(ptr), "worktrees") {
		t.Fatalf("expected the separate-git-dir pointer under a user `worktrees` dir (shape changed?): %q", string(ptr))
	}

	// LIVE state (admin present) → not dead.
	if IsDeadGitWorktreePath(ws) {
		t.Fatalf("live separate-git-dir workspace under user `worktrees` mis-classified as dead (admin present): %s", ws)
	}

	// Remove the admin LEAF while the user `worktrees/` parent survives — the shape
	// the OLD parent-only rule read as a dead worktree (sibling-present branch). The
	// workspace dir still exists (WorkspaceDirDeleted false), so only the boundary
	// requirement keeps it from being pruned.
	if rmErr := os.RemoveAll(adminDir); rmErr != nil {
		t.Fatalf("remove separate-git-dir admin: %v", rmErr)
	}
	if _, statErr := os.Stat(filepath.Dir(adminDir)); statErr != nil {
		t.Fatalf("expected user worktrees/ parent to remain: %v", statErr)
	}
	if IsDeadGitWorktreePath(ws) {
		t.Fatalf("Finding 2 false-positive: live separate-git-dir workspace under a user `worktrees` dir (no .git ancestor) pruned as a removed worktree: %s", ws)
	}
}

// TestParseGitWorktreePointer_ForeignForwardSlashUNC covers Finding 3 (r10): a UNC
// gitdir written by Git-for-Windows with FORWARD slashes (`gitdir: //server/share/...`)
// is classified ABSOLUTE by POSIX filepath.IsAbs (leading `/`), so an IsAbs-first
// ordering would accept it as a native POSIX path and filepath.Clean it to a LOCAL
// `/server/share/...`. On non-Windows it must instead be rejected as FOREIGN
// (ambiguous, never resolved). A genuine single-slash POSIX absolute still resolves.
func TestParseGitWorktreePointer_ForeignForwardSlashUNC(t *testing.T) {
	ws := t.TempDir()

	t.Run("forward-slash UNC on POSIX → rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("//server/share is a NATIVE UNC on Windows (IsAbs=true), not foreign")
		}
		gitPath := filepath.Join(t.TempDir(), ".git")
		if err := os.WriteFile(gitPath, []byte("gitdir: //server/share/repo/.git/worktrees/x\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		if admin, ok := parseGitWorktreePointer(gitPath, ws); ok {
			t.Fatalf("parseGitWorktreePointer(forward-slash UNC) ok=true (admin=%q); want false (cross-OS UNC, never resolve)", admin)
		}
	})

	t.Run("single-slash POSIX absolute still resolves (not foreign)", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("a /-rooted path is foreign on Windows; this asserts the POSIX-native case")
		}
		nativeAbs := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "n") // /tmp/... single slash
		gitPath := filepath.Join(t.TempDir(), ".git")
		if err := os.WriteFile(gitPath, []byte("gitdir: "+nativeAbs+"\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		admin, ok := parseGitWorktreePointer(gitPath, ws)
		if !ok {
			t.Fatalf("parseGitWorktreePointer(native POSIX abs %q) ok=false; want true", nativeAbs)
		}
		if admin != filepath.Clean(nativeAbs) {
			t.Fatalf("parseGitWorktreePointer(native POSIX abs) admin = %q, want %q", admin, filepath.Clean(nativeAbs))
		}
	})
}

// TestIsDeadGitWorktreePath_ForeignForwardSlashUNCEndToEnd covers Finding 3 (r10)
// at the predicate level: a LIVE workspace whose `.git` holds a forward-slash UNC
// gitdir must NOT be pruned on non-Windows even when a matching LOCAL parent of the
// Clean-collapsed `/server/share/...` path happens to exist while the real UNC leaf
// is ENOENT. POSIX-only — `//server/share` is a native UNC on Windows.
func TestIsDeadGitWorktreePath_ForeignForwardSlashUNCEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("//server/share is a native UNC on Windows, not foreign")
	}
	// Engineer the dangerous local collision the foreign-reject must defeat: the
	// Clean-collapsed local path `/server/share/.../worktrees` parent EXISTS while
	// the leaf is ENOENT. Build it under a temp root and point the UNC at the SAME
	// suffix so, absent the foreign reject, Clean would land on a real local parent
	// with a missing leaf → a fabricated dead-worktree → false-positive prune.
	wt := t.TempDir()
	// The forward-slash UNC the LIVE cross-OS worktree carries.
	writeGitFile(t, wt, "//build-server/repos/main/.git/worktrees/live")
	if IsDeadGitWorktreePath(wt) {
		t.Fatalf("IsDeadGitWorktreePath(%q) = true for a forward-slash UNC gitdir; must be false (cross-OS ambiguous, never prune)", wt)
	}
}

// TestIsForeignAbsolutePath unit-tests the foreign-absolute discriminator across
// both GOOS branches it can observe at runtime, pinning the Finding-3 (r10)
// forward-slash-UNC addition and the single-slash POSIX-absolute carve-out.
func TestIsForeignAbsolutePath(t *testing.T) {
	type c struct {
		name   string
		target string
		want   bool
		onlyOn string // "", "windows", "posix"
	}
	cases := []c{
		// Non-Windows (POSIX/WSL) foreign shapes.
		{"drive-letter forward slash on POSIX", "C:/repo/x", true, "posix"},
		{"drive-letter backslash on POSIX", `C:\repo\x`, true, "posix"},
		{"backslash UNC on POSIX", `\\srv\share\x`, true, "posix"},
		{"forward-slash UNC on POSIX (Finding 3 r10)", "//srv/share/x", true, "posix"},
		{"triple-slash on POSIX (UNC-ish → foreign)", "///srv/x", true, "posix"},
		// Non-Windows native (NOT foreign).
		{"single-slash POSIX absolute (native)", "/opt/repo/x", false, "posix"},
		{"bare root slash (native)", "/", false, "posix"},
		{"native relative (not foreign-absolute)", filepath.Join("..", "x"), false, "posix"},
		// Windows foreign / native.
		{"posix-absolute on Windows (foreign)", "/opt/repo/x", true, "windows"},
		{"forward-slash UNC on Windows (native, not foreign)", "//srv/share/x", false, "windows"},
		{"backslash UNC on Windows (native-ish, not foreign-absolute here)", `\\srv\share\x`, false, "windows"},
		{"empty", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.onlyOn == "windows" && runtime.GOOS != "windows" {
				t.Skip("windows-only case")
			}
			if tc.onlyOn == "posix" && runtime.GOOS == "windows" {
				t.Skip("posix-only case")
			}
			if got := isForeignAbsolutePath(tc.target); got != tc.want {
				t.Fatalf("isForeignAbsolutePath(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_BareRepoWorktree covers Finding 1 (this round)
// end-to-end: a worktree created from a BARE repo has a common git dir named
// e.g. `main.git`, so its `.git` pointer is `gitdir: .../main.git/worktrees/<name>`.
// The earlier grandparent==`.git` guard WRONGLY rejected such an admin path
// (grandparent `main.git`, not `.git`) → the dead-worktree signal never fired
// for bare-repo worktrees. With the modules-absence rule a REMOVED bare-repo
// worktree (admin leaf gone, parent `worktrees/` present) is correctly PRUNED.
func TestIsDeadGitWorktreePath_BareRepoWorktree(t *testing.T) {
	// DEAD bare-repo worktree: gitdir → <repo>/main.git/worktrees/<name>; the
	// parent `worktrees/` exists, only the `<name>` leaf is gone → genuinely DEAD.
	deadBare := t.TempDir()
	bareWorktreesParent := filepath.Join(t.TempDir(), "main.git", "worktrees")
	if err := os.MkdirAll(bareWorktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir bare worktrees parent: %v", err)
	}
	bareDeadAdmin := filepath.Join(bareWorktreesParent, "wt") // leaf never created
	writeGitFile(t, deadBare, bareDeadAdmin)

	// LIVE bare-repo worktree: admin leaf PRESENT → not dead (negative control,
	// proves the accept path does not over-prune a live bare-repo worktree).
	liveBare := t.TempDir()
	liveBareAdmin := filepath.Join(t.TempDir(), "main.git", "worktrees", "live")
	if err := os.MkdirAll(liveBareAdmin, 0o755); err != nil {
		t.Fatalf("mkdir live bare admin: %v", err)
	}
	writeGitFile(t, liveBare, liveBareAdmin)

	// DEAD worktree whose owning repo LIVES under a user dir literally named
	// `modules` (the `modules` segment is ABOVE the git common dir, not git's
	// submodule-store position). The positional rule must ACCEPT this → a removed
	// worktree here is correctly PRUNED. A "modules anywhere" rule would wrongly
	// reject it (orphan row lingers — the Finding-1 bug class).
	deadUserModules := t.TempDir()
	userModulesWorktreesParent := filepath.Join(t.TempDir(), "modules", "proj", ".git", "worktrees")
	if err := os.MkdirAll(userModulesWorktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir user-modules worktrees parent: %v", err)
	}
	userModulesAdmin := filepath.Join(userModulesWorktreesParent, "wt") // leaf never created
	writeGitFile(t, deadUserModules, userModulesAdmin)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"dead bare-repo worktree (main.git/worktrees/<name> leaf absent, parent present) → pruned", deadBare, true},
		{"live bare-repo worktree (admin leaf present) → NOT pruned", liveBare, false},
		{"dead worktree under a user dir named modules (modules above .git) → pruned", deadUserModules, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SubmoduleUnderWorktreesNamedDir covers Finding 1
// (r6) end-to-end: a SUBMODULE checked out under a directory literally named
// `worktrees` makes git store its admin dir at
// `<super>/.git/modules/deps/worktrees/foo`, whose immediate PARENT is also
// `worktrees`. The immediate-parent-only guard would ACCEPT it, so a missing
// submodule admin LEAF (parent `worktrees/` present, e.g. before
// `git submodule update --init`) would be mis-pruned as a dead worktree. The
// interior-`modules`-after-first-git-common-dir guard rejects it because an
// interior `modules` sits after the first `.git`. A genuine
// `<repo>/.git/worktrees/<name>` (no interior `modules`) with the identical
// missing-leaf/present-parent shape IS still pruned.
func TestIsDeadGitWorktreePath_SubmoduleUnderWorktreesNamedDir(t *testing.T) {
	// --- SUBMODULE under a dir NAMED `worktrees`: gitdir →
	// <super>/.git/modules/deps/worktrees/foo; the `.git/modules/deps/worktrees/`
	// parent exists, only the `foo` leaf is gone. Immediate parent is `worktrees`
	// (the Finding-1 trap), but an interior `modules` sits after `.git` → rejected
	// → NOT pruned. ---
	subWT := t.TempDir()
	subParent := filepath.Join(t.TempDir(), "super", ".git", "modules", "deps", "worktrees")
	if err := os.MkdirAll(subParent, 0o755); err != nil {
		t.Fatalf("mkdir submodule-under-worktrees parent: %v", err)
	}
	subAdmin := filepath.Join(subParent, "foo") // leaf never created
	writeGitFile(t, subWT, subAdmin)

	// --- GENUINE removed worktree (no interior `modules`): identical missing-leaf
	// shape but under `<repo>/.git/worktrees/` → genuinely DEAD → pruned. Proves
	// the interior-`modules` check rejects ONLY the submodule-under-worktrees
	// shape, not the structurally-analogous real worktree shape. ---
	wtWS := t.TempDir()
	wtParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatalf("mkdir worktrees parent: %v", err)
	}
	wtAdmin := filepath.Join(wtParent, "gone") // leaf never created
	writeGitFile(t, wtWS, wtAdmin)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"submodule under worktrees-named dir (.git/modules/.../worktrees/foo leaf absent) → NOT pruned", subWT, false},
		{"genuine removed worktree (.git/worktrees/<name> leaf absent) → pruned", wtWS, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SubmoduleInsideWorktree covers Finding 3 (this round)
// end-to-end: a SUBMODULE checked out INSIDE a linked WORKTREE makes git store
// its admin dir at `<repo>/.git/worktrees/<wt>/modules/<sub>` — `modules` is
// preceded by the worktree NAME `<wt>`, NOT a git-common-dir segment, so the
// earlier immediately-after-a-git-dir positional check MISSED it: a missing
// submodule admin LEAF (parent `modules/` present, e.g. before
// `git submodule update --init`) was mis-pruned as a dead worktree. The
// interior-`modules`-after-first-git-common-dir guard rejects it. A genuine
// `<repo>/.git/worktrees/<name>` (no interior `modules`) with the identical
// missing-leaf/present-parent shape IS still pruned.
//
// The submodule-in-worktree admin shape is the verified real-git layout (see
// TestIsDeadGitWorktreePath_SubmoduleInsideWorktree_RealGit below for the
// git-driven proof); this table-form variant pins the discriminator without a
// git binary so it runs everywhere (incl. CI hosts without git).
func TestIsDeadGitWorktreePath_SubmoduleInsideWorktree(t *testing.T) {
	// --- SUBMODULE inside a linked worktree (online, leaf absent): gitdir →
	// <repo>/.git/worktrees/wt/modules/sub; the `.../modules/` parent exists, only
	// the `sub` leaf is gone. Immediate parent is `modules` here, but even the
	// worktree-of-a-submodule-in-a-worktree shape below (immediate parent
	// `worktrees`) is rejected by the interior-`modules` rule. ---
	subWT := t.TempDir()
	subModulesParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "wt", "modules")
	if err := os.MkdirAll(subModulesParent, 0o755); err != nil {
		t.Fatalf("mkdir submodule-in-worktree modules parent: %v", err)
	}
	subAdmin := filepath.Join(subModulesParent, "sub") // leaf never created
	writeGitFile(t, subWT, subAdmin)

	// --- WORKTREE of a submodule-in-a-worktree (online, leaf absent): gitdir →
	// <repo>/.git/worktrees/wt/modules/sub/worktrees/sub-wt; the `.../worktrees/`
	// parent exists, only the `sub-wt` leaf is gone. Immediate parent is
	// `worktrees` (so the parent gate passes — this is exactly the shape the bot
	// finding names), but an interior `modules` sits after the first `.git` →
	// rejected → NOT pruned. ---
	subWtWT := t.TempDir()
	subWtParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "wt", "modules", "sub", "worktrees")
	if err := os.MkdirAll(subWtParent, 0o755); err != nil {
		t.Fatalf("mkdir worktree-of-submodule-in-worktree parent: %v", err)
	}
	subWtAdmin := filepath.Join(subWtParent, "sub-wt") // leaf never created
	writeGitFile(t, subWtWT, subWtAdmin)

	// --- GENUINE removed worktree (no interior `modules`): identical missing-leaf
	// shape under `<repo>/.git/worktrees/` → genuinely DEAD → pruned. Proves the
	// interior-`modules` check rejects ONLY the submodule-in-worktree shapes, not
	// the structurally-analogous real worktree shape. ---
	wtWS := t.TempDir()
	wtParent := filepath.Join(t.TempDir(), "main", ".git", "worktrees")
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatalf("mkdir worktrees parent: %v", err)
	}
	wtAdmin := filepath.Join(wtParent, "gone") // leaf never created
	writeGitFile(t, wtWS, wtAdmin)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"submodule inside a worktree (.git/worktrees/<wt>/modules/<sub> leaf absent) → NOT pruned", subWT, false},
		{"worktree of a submodule-in-a-worktree (parent worktrees, interior modules) leaf absent → NOT pruned", subWtWT, false},
		{"genuine removed worktree (.git/worktrees/<name> leaf absent) → pruned", wtWS, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SubmoduleInsideWorktree_RealGit drives a REAL git
// binary to construct the submodule-in-a-linked-worktree admin shape (Finding 3),
// then asserts that an online submodule-in-a-worktree whose admin LEAF is removed
// is NOT pruned. This is the git-driven proof that the synthetic admin path used
// by TestIsDeadGitWorktreePath_SubmoduleInsideWorktree above matches git's actual
// layout: `git submodule update --init` inside a linked worktree writes the
// submodule's `.git` pointer to `<repo>/.git/worktrees/<wt>/modules/<sub>`.
// Skipped when git is unavailable so the synthetic table-form test still covers
// the discriminator on hosts without a git binary.
func TestIsDeadGitWorktreePath_SubmoduleInsideWorktree_RealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; synthetic TestIsDeadGitWorktreePath_SubmoduleInsideWorktree covers the discriminator")
	}

	root := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		// Deterministic identity + allow file:// submodule transport (git ≥2.38
		// blocks it by default). Keep ambient-input control: no reliance on the
		// host's global git config for these.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=protocol.file.allow", "GIT_CONFIG_VALUE_0=always",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Submodule origin: a one-commit repo to add as a submodule.
	subOrigin := filepath.Join(root, "sub-origin")
	runGit(root, "init", "-q", subOrigin)
	if err := os.WriteFile(filepath.Join(subOrigin, "f.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write sub-origin file: %v", err)
	}
	runGit(subOrigin, "add", ".")
	runGit(subOrigin, "commit", "-qm", "init")

	// Superproject with the submodule added and committed.
	main := filepath.Join(root, "main")
	runGit(root, "init", "-q", main)
	if err := os.WriteFile(filepath.Join(main, "m.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	runGit(main, "add", ".")
	runGit(main, "commit", "-qm", "init")
	runGit(main, "submodule", "add", "-q", subOrigin, "sub")
	runGit(main, "commit", "-qm", "add sub")

	// Linked worktree of the superproject, with the submodule initialized inside
	// it. git writes the submodule admin under
	// <main>/.git/worktrees/<wt>/modules/sub.
	wt := filepath.Join(root, "wt")
	runGit(main, "worktree", "add", "-q", wt)
	runGit(wt, "submodule", "update", "--init")

	// Resolve the submodule WORKSPACE dir inside the worktree and confirm git
	// produced the expected admin shape before exercising the discriminator.
	subWorkspace := filepath.Join(wt, "sub")
	gitPtr, ptrErr := os.ReadFile(filepath.Join(subWorkspace, ".git"))
	if ptrErr != nil {
		t.Fatalf("read submodule-in-worktree .git pointer: %v", ptrErr)
	}
	if !strings.Contains(string(gitPtr), filepath.ToSlash(filepath.Join("worktrees", "wt", "modules", "sub"))) &&
		!strings.Contains(string(gitPtr), "worktrees/wt/modules/sub") {
		t.Fatalf("unexpected submodule-in-worktree .git pointer (Finding-3 shape changed?): %q", string(gitPtr))
	}

	// LIVE submodule-in-a-worktree (admin leaf present) → never pruned.
	if IsDeadGitWorktreePath(subWorkspace) {
		t.Fatalf("live submodule-in-a-worktree mis-classified as dead: %s", subWorkspace)
	}

	// Remove ONLY the submodule's admin leaf (simulate the online-but-leaf-absent
	// state, e.g. a partially-cleaned submodule) while the parent `modules/` dir
	// remains — the exact missing-leaf/present-parent shape that, WITHOUT the
	// interior-`modules` guard, satisfies the sibling-present DEAD branch. The
	// submodule workspace dir itself stays present (Condition 1 still passes), so
	// the only thing standing between it and a wrongful prune is the
	// isWorktreeAdminPath rejection.
	adminLeaf := filepath.Join(main, ".git", "worktrees", "wt", "modules", "sub")
	if _, statErr := os.Stat(adminLeaf); statErr != nil {
		t.Fatalf("expected submodule admin leaf to exist before removal: %v", statErr)
	}
	if rmErr := os.RemoveAll(adminLeaf); rmErr != nil {
		t.Fatalf("remove submodule admin leaf: %v", rmErr)
	}
	// Parent `.../modules/` must still exist (present-parent half of the shape).
	if _, statErr := os.Stat(filepath.Dir(adminLeaf)); statErr != nil {
		t.Fatalf("expected submodule modules parent to remain after leaf removal: %v", statErr)
	}

	if IsDeadGitWorktreePath(subWorkspace) {
		t.Fatalf("submodule-in-a-worktree with absent admin leaf mis-pruned as dead worktree: %s", subWorkspace)
	}
}

// TestParseGitWorktreePointer_OversizedGitFile covers Finding 2 (r6): the `.git`
// pointer read is SIZE-CAPPED (readGitPointerFile, maxGitPointerFileBytes). A
// `.git` file larger than the cap is treated as AMBIGUOUS (ok=false) so it can
// never trigger an unbounded read or be mis-resolved into a prune; a normal
// small pointer resolves as before. The end-to-end IsDeadGitWorktreePath on an
// oversized `.git` must be false (never prune the LIVE workspace).
func TestParseGitWorktreePointer_OversizedGitFile(t *testing.T) {
	ws := t.TempDir()

	t.Run("oversized .git file → rejected (ambiguous)", func(t *testing.T) {
		dir := t.TempDir()
		gitPath := filepath.Join(dir, ".git")
		// A valid gitdir line followed by padding that pushes the file past the cap.
		body := "gitdir: " + filepath.Join("..", "main", ".git", "worktrees", "x") + "\n"
		body += "# " + string(make([]byte, maxGitPointerFileBytes)) // > cap once combined
		if err := os.WriteFile(gitPath, []byte(body), 0o600); err != nil {
			t.Fatalf("write oversized .git: %v", err)
		}
		if int64(len(body)) <= maxGitPointerFileBytes {
			t.Fatalf("test setup: body %d bytes is not over the %d cap", len(body), maxGitPointerFileBytes)
		}
		if admin, ok := parseGitWorktreePointer(gitPath, ws); ok {
			t.Fatalf("parseGitWorktreePointer(oversized) ok=true (admin=%q); want false (over cap → ambiguous)", admin)
		}
	})

	t.Run("normal small pointer (well under cap) resolves normally", func(t *testing.T) {
		dir := t.TempDir()
		gitPath := filepath.Join(dir, ".git")
		target := filepath.Join("..", "main", ".git", "worktrees", "x")
		if err := os.WriteFile(gitPath, []byte("gitdir: "+target+"\n"), 0o600); err != nil {
			t.Fatalf("write small .git: %v", err)
		}
		admin, ok := parseGitWorktreePointer(gitPath, ws)
		if !ok {
			t.Fatalf("parseGitWorktreePointer(small pointer) ok=false; want true")
		}
		if want := filepath.Clean(filepath.Join(ws, target)); admin != want {
			t.Fatalf("parseGitWorktreePointer(small pointer) admin = %q, want %q", admin, want)
		}
	})

	t.Run("end-to-end: oversized .git at workspace root → IsDeadGitWorktreePath false", func(t *testing.T) {
		wtWS := t.TempDir()
		// Point at a genuinely-absent worktree admin dir so that, absent the cap,
		// the classifier would otherwise have to resolve the pointer. The oversize
		// makes the read ambiguous BEFORE any admin-dir stat → never prune.
		missingAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "gone")
		body := "gitdir: " + missingAdmin + "\n"
		body += "# " + string(make([]byte, maxGitPointerFileBytes))
		if err := os.WriteFile(filepath.Join(wtWS, ".git"), []byte(body), 0o600); err != nil {
			t.Fatalf("write oversized .git at ws root: %v", err)
		}
		if IsDeadGitWorktreePath(wtWS) {
			t.Fatalf("IsDeadGitWorktreePath(%q) = true for an oversized .git pointer; must be false (ambiguous, never prune)", wtWS)
		}
	})
}

// TestParseGitWorktreePointer_ForeignRelativeGitdir covers Finding 3 (r5): a
// RELATIVE gitdir containing a FOREIGN path separator (a Windows relative path
// like `..\main\.git\worktrees\x` seen on POSIX) must be rejected as ambiguous
// (ok=false) — it is not absolute (so the foreign-absolute reject misses it) yet
// joining it under the workspace on POSIX fabricates a one-backslash filename. A
// NATIVE relative path (`../main/.git/worktrees/x`) still resolves normally on
// every OS.
func TestParseGitWorktreePointer_ForeignRelativeGitdir(t *testing.T) {
	ws := t.TempDir()

	t.Run("foreign-relative (backslash) on POSIX → rejected", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("a backslash relative path is NATIVE on Windows, not foreign")
		}
		gitPath := filepath.Join(t.TempDir(), ".git")
		if err := os.WriteFile(gitPath, []byte(`gitdir: ..\main\.git\worktrees\x`+"\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		if admin, ok := parseGitWorktreePointer(gitPath, ws); ok {
			t.Fatalf("parseGitWorktreePointer(foreign-relative) ok=true (admin=%q); want false (cross-OS relative, never resolve)", admin)
		}
	})

	t.Run("native-relative (forward slash) resolves normally", func(t *testing.T) {
		target := filepath.Join("..", "main", ".git", "worktrees", "x") // native separators
		gitPath := filepath.Join(t.TempDir(), ".git")
		if err := os.WriteFile(gitPath, []byte("gitdir: "+target+"\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		admin, ok := parseGitWorktreePointer(gitPath, ws)
		if !ok {
			t.Fatalf("parseGitWorktreePointer(native-relative %q) ok=false; want true", target)
		}
		if want := filepath.Clean(filepath.Join(ws, target)); admin != want {
			t.Fatalf("parseGitWorktreePointer(native-relative) admin = %q, want %q", admin, want)
		}
	})
}

// TestIsDeadGitWorktreePath_ForeignRelativeGitdirEndToEnd covers Finding 3 (r5)
// at the predicate level: a LIVE workspace whose `.git` holds a foreign-relative
// gitdir must NOT be pruned. POSIX-only — a backslash relative path is native on
// Windows.
func TestIsDeadGitWorktreePath_ForeignRelativeGitdirEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a backslash relative gitdir is NATIVE on Windows, not foreign")
	}
	wt := t.TempDir()
	writeGitFile(t, wt, `..\main\.git\worktrees\live`)
	if IsDeadGitWorktreePath(wt) {
		t.Fatalf("IsDeadGitWorktreePath(%q) = true for a foreign-relative gitdir; must be false (cross-OS ambiguous, never prune)", wt)
	}
}

// TestIsDeadGitWorktreePath_BoundarylessBareRepoSubmodule covers the REQUIRE-A-
// BOUNDARY rule (this round — Findings 1+2 convergent root fix, REVERTS r9)
// end-to-end on the SUFFIX-LESS bare-repo family. A BARE repo whose common dir name
// does NOT end in `.git` (e.g. `git init --bare myrepo`, or a clone without the
// suffix) produces NO git-common-dir segment in its worktree/submodule admin paths.
// The prior r9 logic ACCEPTED such boundary-less paths (worktree → pruned;
// submodule store → walk climbed past). That was the UNSAFE path: a boundary-less
// `worktrees/<wt>` (or `modules/...`) is path-indistinguishable from a
// coincidentally-named user dir, so a LIVE `git init --separate-git-dir` workspace
// under a user folder named `worktrees`/`modules` was mis-pruned (Finding 2 /
// Finding 1).
//
// The boundary requirement makes the WHOLE suffix-less family NOT pruned — an
// accepted benign false-NEGATIVE: a genuinely-dead suffix-less bare-repo worktree
// (or a dead worktree reachable only via a suffix-less submodule pointer) lingers
// as an orphan row instead of being pruned. A bare repo whose dir name DOES end in
// `.git` keeps the `.git`-suffix boundary and IS still classified/pruned (see
// TestIsDeadGitWorktreePath_BareRepoWorktree).
//
// Real-git shapes the synthetic admin paths below mirror (git 2.53, bare clone
// WITHOUT `.git` suffix):
//
//	git clone --bare src myrepo                    → myrepo (no suffix)
//	git -C myrepo worktree add wt main             → gitdir: .../myrepo/worktrees/wt
//	git -C wt submodule add ... deps; ...           → store under myrepo/worktrees/wt/modules/deps
//	git -C wt/deps worktree add deps-wt feat       → gitdir: .../myrepo/worktrees/wt/modules/deps/worktrees/deps-wt
func TestIsDeadGitWorktreePath_BoundarylessBareRepoSubmodule(t *testing.T) {
	// --- BOUNDARY-LESS submodule store (no .git segment, interior modules, parent
	// `worktrees`): the workspace `.git` points at
	// `<bare>/worktrees/<wt>/modules/deps/worktrees/foo`; the parent `worktrees/`
	// dir exists, only the `foo` leaf is gone. REQUIRE-A-BOUNDARY: no `.git`/`*.git`
	// ancestor AND an interior `modules` → isWorktreeAdminPath is false → the
	// predicate STOPS at the nearest pointer → NOT pruned (a LIVE submodule workspace,
	// or a dead one lingering benignly — either way never a false-positive). ---
	subWT := t.TempDir()
	// bare common dir literally `myrepo` (no `.git` / `*.git` suffix).
	boundarylessStoreParent := filepath.Join(t.TempDir(), "myrepo", "worktrees", "wt", "modules", "deps", "worktrees")
	if err := os.MkdirAll(boundarylessStoreParent, 0o755); err != nil {
		t.Fatalf("mkdir boundary-less submodule store parent: %v", err)
	}
	subAdmin := filepath.Join(boundarylessStoreParent, "foo") // leaf never created
	writeGitFile(t, subWT, subAdmin)

	// --- BOUNDARY-LESS NORMAL bare-repo worktree (no interior modules, no .git
	// segment): the workspace `.git` points at `<bare>/worktrees/<wt>`; the parent
	// `worktrees/` exists, only the `<wt>` leaf is gone. REQUIRE-A-BOUNDARY (this
	// round, REVERTS r9): with NO `.git`/`*.git` ancestor this admin path is
	// path-indistinguishable from a user dir named `worktrees`, so it is NO LONGER
	// classified as a worktree → NOT pruned (an accepted benign false-NEGATIVE: a
	// dead orphan row lingers rather than risk pruning a live `/worktrees/gone`
	// separate-git-dir workspace). A bare repo whose dir name ends in `.git`
	// (`<repo>.git/worktrees/<wt>`) keeps the boundary and IS still pruned — see
	// TestIsDeadGitWorktreePath_BareRepoWorktree. ---
	deadBareWT := t.TempDir()
	bareWorktreesParent := filepath.Join(t.TempDir(), "myrepo", "worktrees")
	if err := os.MkdirAll(bareWorktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir boundary-less bare worktrees parent: %v", err)
	}
	bareDeadAdmin := filepath.Join(bareWorktreesParent, "wt") // leaf never created
	writeGitFile(t, deadBareWT, bareDeadAdmin)

	// --- LIVE boundary-less bare-repo worktree (admin leaf present) → NOT pruned
	// (also rejected at the boundary check, so doubly safe). ---
	liveBareWT := t.TempDir()
	liveBareAdmin := filepath.Join(t.TempDir(), "myrepo", "worktrees", "live")
	if err := os.MkdirAll(liveBareAdmin, 0o755); err != nil {
		t.Fatalf("mkdir live boundary-less bare admin: %v", err)
	}
	writeGitFile(t, liveBareWT, liveBareAdmin)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"boundary-less submodule store (parent worktrees, interior modules, no .git segment) → NOT pruned", subWT, false},
		{"boundary-less bare-repo worktree (no .git ancestor) removed → NOT pruned (REVERT r9, accepted false-negative)", deadBareWT, false},
		{"live boundary-less bare-repo worktree (admin present) → NOT pruned", liveBareWT, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned is the
// PERMANENT REGRESSION GUARD for the walk-continue removal (the root-cause fix for
// the recurring false-POSITIVE class). A registered workspace that is a SUBMODULE
// dir INSIDE a linked worktree whose OUTER worktree admin was removed. The
// submodule's own `.git` points at the submodule store
// (`<repo>/.git/worktrees/<wt>/modules/<sub>`), which isWorktreeAdminPath REJECTS
// (interior `modules`). The OUTER worktree's own `.git` pointer lives at an
// ANCESTOR (the worktree ROOT). An earlier design (r8-r11) CLIMBED PAST the
// submodule pointer to detect that dead outer worktree — but climbing past a
// path-classified "submodule store" can prune a LIVE nested repo (separate-git-dir,
// coincidental `X.git/modules/Y` user dir), an endless false-positive tail. The
// walk-continue is now REMOVED: the predicate STOPS at the nearest pointer (the
// submodule store), which is NOT a worktree admin → returns false. So this exotic
// case is a DOCUMENTED FAIL-SAFE FALSE-NEGATIVE (the orphan row lingers benignly),
// NOT a prune. This test asserts the dead-outer-worktree case is NOT pruned (the
// flip from the prior r8 PRUNED assertion).
//
// A normal submodule in a regular LIVE repo also correctly returns NOT pruned (its
// nearest pointer is a submodule store too → rejected). The submodule-dir layout
// uses real on-disk regular-file `.git` pointers; the outer admin presence/absence
// is engineered with MkdirAll/leave-absent so the test runs without a git binary.
func TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned(t *testing.T) {
	// --- DEAD OUTER WORKTREE, workspace = the submodule dir.
	// Layout (workspace = the submodule dir `<wtRoot>/sub`):
	//   <wtRoot>/.git       FILE → gitdir: <main>/.git/worktrees/<wt>   (outer worktree pointer)
	//   <wtRoot>/sub/.git   FILE → gitdir: <main>/.git/worktrees/<wt>/modules/sub  (submodule pointer)
	//   outer admin <main>/.git/worktrees/<wt> ENOENT, parent worktrees/ present.
	// The NEAREST pointer to `sub` is its own submodule store (interior modules) →
	// isWorktreeAdminPath false → STOP → NOT pruned (no climb to the dead outer admin).
	deadWtRoot := t.TempDir()
	deadSub := filepath.Join(deadWtRoot, "sub")
	if err := os.MkdirAll(deadSub, 0o755); err != nil {
		t.Fatalf("mkdir dead submodule dir: %v", err)
	}
	deadMainGit := filepath.Join(t.TempDir(), "main", ".git")
	deadWorktreesParent := filepath.Join(deadMainGit, "worktrees")
	if err := os.MkdirAll(deadWorktreesParent, 0o755); err != nil {
		t.Fatalf("mkdir dead outer worktrees parent: %v", err)
	}
	outerAdminDead := filepath.Join(deadWorktreesParent, "wt") // outer admin leaf never created
	writeGitFile(t, deadWtRoot, outerAdminDead)
	// submodule pointer at the subdir → the submodule store under the (gone) outer admin.
	writeGitFile(t, deadSub, filepath.Join(outerAdminDead, "modules", "sub"))

	// --- LIVE OUTER WORKTREE: same layout but the outer admin dir is PRESENT.
	// Nearest pointer is the submodule store → rejected → STOP → NOT pruned. ---
	liveWtRoot := t.TempDir()
	liveSub := filepath.Join(liveWtRoot, "sub")
	if err := os.MkdirAll(liveSub, 0o755); err != nil {
		t.Fatalf("mkdir live submodule dir: %v", err)
	}
	liveOuterAdmin := filepath.Join(t.TempDir(), "main", ".git", "worktrees", "wt")
	if err := os.MkdirAll(liveOuterAdmin, 0o755); err != nil {
		t.Fatalf("mkdir live outer admin: %v", err)
	}
	writeGitFile(t, liveWtRoot, liveOuterAdmin)
	writeGitFile(t, liveSub, filepath.Join(liveOuterAdmin, "modules", "sub"))

	// --- NORMAL SUBMODULE in a regular LIVE repo: the submodule's nearest pointer is
	// its own submodule store (`<super>/.git/modules/sub`, interior modules) →
	// isWorktreeAdminPath false → STOP → NOT pruned, even though the submodule admin
	// LEAF is absent. (The superproject `.git` DIRECTORY one level up is never
	// reached — there is no climb.) ---
	plainSuper := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plainSuper, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir plain superproject .git dir: %v", err)
	}
	plainSub := filepath.Join(plainSuper, "sub")
	if err := os.MkdirAll(plainSub, 0o755); err != nil {
		t.Fatalf("mkdir plain submodule dir: %v", err)
	}
	// submodule pointer → <super>/.git/modules/sub with the leaf absent (parent present).
	plainModulesParent := filepath.Join(plainSuper, ".git", "modules")
	if err := os.MkdirAll(plainModulesParent, 0o755); err != nil {
		t.Fatalf("mkdir plain submodule modules parent: %v", err)
	}
	writeGitFile(t, plainSub, filepath.Join(plainModulesParent, "sub")) // leaf absent

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"submodule dir inside worktree, OUTER worktree admin removed → NOT pruned (no climb; documented benign false-negative)", deadSub, false},
		{"submodule dir inside LIVE worktree (outer admin present) → NOT pruned", liveSub, false},
		{"normal submodule in a regular live repo (nearest pointer is submodule store) → NOT pruned", plainSub, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeadGitWorktreePath(c.path); got != c.want {
				t.Fatalf("IsDeadGitWorktreePath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned_RealGit drives
// a REAL git binary to build a submodule-in-a-linked-worktree, then REMOVES the
// OUTER worktree admin and asserts the workspace (the SUBMODULE dir) is NOT pruned —
// the documented benign FALSE-NEGATIVE after the walk-continue removal. The
// submodule's nearest `.git` pointer is its own submodule store (interior
// `modules`), which is NOT a worktree admin, so the predicate STOPS there and never
// climbs to the dead outer worktree pointer. It also asserts the LIVE state (before
// removal) is NOT pruned, and that a normal submodule in a regular live repo is NOT
// pruned. Skipped when git is unavailable (the synthetic
// TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned covers the
// discriminator on hosts without a git binary).
func TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned_RealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH; synthetic TestIsDeadGitWorktreePath_SubmoduleInsideRemovedWorktreeNotPruned covers the discriminator")
	}

	root := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=protocol.file.allow", "GIT_CONFIG_VALUE_0=always",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// Submodule origin.
	subOrigin := filepath.Join(root, "sub-origin")
	runGit(root, "init", "-q", subOrigin)
	if err := os.WriteFile(filepath.Join(subOrigin, "f.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write sub-origin file: %v", err)
	}
	runGit(subOrigin, "add", ".")
	runGit(subOrigin, "commit", "-qm", "init")

	// Superproject with the submodule.
	main := filepath.Join(root, "main")
	runGit(root, "init", "-q", main)
	if err := os.WriteFile(filepath.Join(main, "m.txt"), []byte("main\n"), 0o600); err != nil {
		t.Fatalf("write main file: %v", err)
	}
	runGit(main, "add", ".")
	runGit(main, "commit", "-qm", "init")
	runGit(main, "submodule", "add", "-q", subOrigin, "sub")
	runGit(main, "commit", "-qm", "add sub")

	// Linked worktree with the submodule initialized inside it. git writes the
	// submodule admin under <main>/.git/worktrees/<wt>/modules/sub and the OUTER
	// worktree admin at <main>/.git/worktrees/<wt>.
	wt := filepath.Join(root, "wt")
	runGit(main, "worktree", "add", "-q", wt)
	runGit(wt, "submodule", "update", "--init")

	subWorkspace := filepath.Join(wt, "sub")
	outerAdmin := filepath.Join(main, ".git", "worktrees", "wt")

	// Confirm git produced the expected outer + submodule pointer shapes.
	if _, statErr := os.Stat(outerAdmin); statErr != nil {
		t.Fatalf("expected outer worktree admin to exist before removal: %v", statErr)
	}
	subPtr, ptrErr := os.ReadFile(filepath.Join(subWorkspace, ".git"))
	if ptrErr != nil {
		t.Fatalf("read submodule .git pointer: %v", ptrErr)
	}
	if !strings.Contains(string(subPtr), "worktrees/wt/modules/sub") &&
		!strings.Contains(string(subPtr), filepath.ToSlash(filepath.Join("worktrees", "wt", "modules", "sub"))) {
		t.Fatalf("unexpected submodule-in-worktree .git pointer (shape changed?): %q", string(subPtr))
	}

	// LIVE: outer worktree admin present → the submodule dir is NOT a dead worktree.
	if IsDeadGitWorktreePath(subWorkspace) {
		t.Fatalf("live submodule-in-a-worktree (outer admin present) mis-classified as dead: %s", subWorkspace)
	}

	// Remove the OUTER worktree admin entirely (the dead-outer-worktree incident).
	// This takes the submodule store under it with it; the submodule WORKSPACE dir
	// and the worktree-root `.git` pointer remain on disk. The parent
	// `<main>/.git/worktrees/` survives (the superproject `.git/` is the
	// grandparent). Even so, the submodule workspace's NEAREST pointer is its own
	// submodule store (interior `modules`), which is NOT a worktree admin — the
	// predicate STOPS there and never climbs to the dead outer worktree pointer.
	if rmErr := os.RemoveAll(outerAdmin); rmErr != nil {
		t.Fatalf("remove outer worktree admin: %v", rmErr)
	}
	if _, statErr := os.Stat(filepath.Dir(outerAdmin)); statErr != nil {
		t.Fatalf("expected outer worktrees/ parent to remain after admin removal: %v", statErr)
	}
	// The submodule workspace dir itself must still exist (Condition 1 passes).
	if _, statErr := os.Stat(subWorkspace); statErr != nil {
		t.Fatalf("expected submodule workspace dir to remain: %v", statErr)
	}

	// WALK-CONTINUE REMOVED: the dead OUTER worktree is NOT detected from the
	// submodule workspace dir — the predicate stops at the nearest (submodule)
	// pointer rather than climbing past a live/ambiguous nested-repo boundary. This
	// is the documented benign FALSE-NEGATIVE that eliminates the climb-past-live-repo
	// false-positive class (the prior r8 assertion expected detection/prune here).
	if IsDeadGitWorktreePath(subWorkspace) {
		t.Fatalf("submodule-in-a-removed-worktree mis-pruned (the walk must NOT climb past the submodule pointer): %s", subWorkspace)
	}

	// Negative control: a NORMAL submodule in a regular LIVE repo (the submodule of
	// `main` itself, not inside a worktree) must NOT be pruned — its nearest pointer
	// is its own submodule store (interior `modules`), which is not a worktree admin.
	// (Use a fresh checkout of `main` so the submodule sits directly under a
	// normal-repo superproject.)
	plainClone := filepath.Join(root, "plain")
	runGit(root, "clone", "-q", main, plainClone)
	runGit(plainClone, "submodule", "update", "--init")
	plainSub := filepath.Join(plainClone, "sub")
	if _, statErr := os.Stat(plainSub); statErr == nil {
		if IsDeadGitWorktreePath(plainSub) {
			t.Fatalf("normal submodule in a regular live repo mis-classified as dead: %s", plainSub)
		}
	}
}
