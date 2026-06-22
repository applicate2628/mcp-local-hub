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
