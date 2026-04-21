package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkspaceKey_Deterministic(t *testing.T) {
	a := WorkspaceKey("C:/users/dima/projects/foo")
	b := WorkspaceKey("C:/users/dima/projects/foo")
	if a != b {
		t.Errorf("key not deterministic: %q vs %q", a, b)
	}
	if len(a) != 8 {
		t.Errorf("key len = %d, want 8 (hex)", len(a))
	}
	c := WorkspaceKey("C:/users/dima/projects/bar")
	if a == c {
		t.Error("keys for distinct paths should differ")
	}
}

func TestCanonicalWorkspacePath_RelativeResolvedToAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got, err := CanonicalWorkspacePath(".")
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("not absolute: %q", got)
	}
}

func TestCanonicalWorkspacePath_RejectsNonexistent(t *testing.T) {
	_, err := CanonicalWorkspacePath(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestCanonicalWorkspacePath_WindowsDriveLetterLowercased(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only")
	}
	// Create a real directory to canonicalize.
	dir := t.TempDir()
	// Craft an uppercased-drive variant of dir (TempDir returns lowercase on most
	// setups; manually uppercase if possible).
	if len(dir) < 2 || dir[1] != ':' {
		t.Skipf("unexpected temp dir shape: %q", dir)
	}
	upper := strings.ToUpper(string(dir[0])) + dir[1:]
	got, err := CanonicalWorkspacePath(upper)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	if got[0] != dir[0] || strings.ToLower(string(got[0])) != string(got[0]) {
		t.Errorf("drive letter not lowercased: input %q -> %q", upper, got)
	}
}

// TestCanonicalWorkspacePathForCleanup_ResolvesSymlinkViaRegularAncestor
// guards the subtle case where the user registered via a symlinked
// parent AND later deleted only the leaf. Example: register
// /alias/dir/project where /alias → /real. Canonical at register time
// is /real/dir/project. Then user deletes `project`. EvalSymlinks on
// the full path fails (project gone). Walking up, Lstat(/alias/dir)
// succeeds — but reports a REGULAR directory (the Lstat follows /alias
// transparently because /alias/dir is reached THROUGH it, and the
// mode-check sees the regular target, not the symlink). The old code
// returned /alias/dir/project at that point, producing a different
// WorkspaceKey than Register stored. Without this fix, unregister
// cannot find orphaned entries for a valid common deletion pattern.
// Skipped on Windows (symlink creation requires admin).
func TestCanonicalWorkspacePathForCleanup_ResolvesSymlinkViaRegularAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires developer mode / admin")
	}
	realParent := filepath.Join(t.TempDir(), "real")
	realDir := filepath.Join(realParent, "dir")
	leaf := filepath.Join(realDir, "project")
	if err := os.MkdirAll(leaf, 0755); err != nil {
		t.Fatalf("mkdir real/dir/project: %v", err)
	}
	aliasBase := t.TempDir()
	alias := filepath.Join(aliasBase, "alias") // alias → realParent
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	aliasLeaf := filepath.Join(alias, "dir", "project")
	// Snapshot key at register time.
	registerKey, err := CanonicalWorkspacePath(aliasLeaf)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	// Delete ONLY the leaf; /alias/dir stays alive (via symlink chain).
	if err := os.RemoveAll(leaf); err != nil {
		t.Fatalf("remove leaf: %v", err)
	}
	cleanupKey, err := CanonicalWorkspacePathForCleanup(aliasLeaf)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if cleanupKey != registerKey {
		t.Errorf("cleanup key diverged (/alias/.../project vs /real/.../project):\n  register = %q\n  cleanup  = %q", registerKey, cleanupKey)
	}
}

// TestCanonicalWorkspacePathForCleanup_ResolvesBrokenParentSymlink guards
// the round-19 Codex case: registration used a path like /alias/project
// where /alias is the symlink. Later /alias's target is deleted. Now
// EvalSymlinks on the full path fails, Lstat on the full path also
// fails (parent traversal broken), and Readlink on the full path fails
// (it's not itself a symlink). The cleanup canonicalizer must walk up
// from the full path to find the surviving /alias symlink and resolve
// manually so the key matches Register's. Otherwise orphan registry
// entries are unreachable.
// Skipped on Windows (symlink creation requires admin).
func TestCanonicalWorkspacePathForCleanup_ResolvesBrokenParentSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires developer mode / admin")
	}
	// real target dir (will be deleted later)
	realParent := filepath.Join(t.TempDir(), "real")
	projectInReal := filepath.Join(realParent, "project")
	if err := os.MkdirAll(projectInReal, 0755); err != nil {
		t.Fatalf("mkdir real/project: %v", err)
	}
	// alias symlink to realParent
	aliasBase := t.TempDir()
	alias := filepath.Join(aliasBase, "alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	aliasProject := filepath.Join(alias, "project")
	// Key at register-time (target alive).
	registerKey, err := CanonicalWorkspacePath(aliasProject)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(aliasProject): %v", err)
	}
	// Simulate target deletion AFTER registration.
	if err := os.RemoveAll(realParent); err != nil {
		t.Fatalf("remove real: %v", err)
	}
	// Cleanup must still produce the same canonical.
	cleanupKey, err := CanonicalWorkspacePathForCleanup(aliasProject)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if cleanupKey != registerKey {
		t.Errorf("cleanup key diverged when parent symlink target gone:\n  register = %q\n  cleanup  = %q", registerKey, cleanupKey)
	}
}

// TestCanonicalWorkspacePathForCleanup_ReadsSymlinkWhenTargetGone guards
// the cleanup-stability fix: after a user registers through a symlinked
// workspace and later deletes the target, unregister via the original
// symlink path must still produce the same WorkspaceKey Register used.
// EvalSymlinks fails when the target is gone, but Readlink on the
// surviving symlink recovers the original target path so the hash
// matches. Without this, orphaned scheduler/client/registry state
// becomes unreachable via the user's original invocation.
// Skipped on Windows (symlink creation requires admin).
func TestCanonicalWorkspacePathForCleanup_ReadsSymlinkWhenTargetGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires developer mode / admin")
	}
	// Set up: a real dir, a symlink to it, snapshot its canonical key.
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Key computed via the strict Register-time function.
	registerKey, err := CanonicalWorkspacePath(link)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(link): %v", err)
	}
	// Simulate target deletion AFTER registration.
	if err := os.RemoveAll(realDir); err != nil {
		t.Fatalf("remove real: %v", err)
	}
	// Cleanup path MUST still produce the same canonical form.
	cleanupKey, err := CanonicalWorkspacePathForCleanup(link)
	if err != nil {
		t.Fatalf("Cleanup(link) after target gone: %v", err)
	}
	if cleanupKey != registerKey {
		t.Errorf("cleanup key diverged from register key after target gone:\n  register = %q\n  cleanup  = %q\n(orphaned scheduler/registry entries would be unreachable)",
			registerKey, cleanupKey)
	}
}

// TestCanonicalWorkspacePath_ResolvesSymlinkToTarget ensures symlinks at
// any level — final OR intermediate — resolve to the real underlying
// directory so the same workspace is never registered twice under
// different aliases. The previous policy rejected final-path symlinks
// outright, but that left intermediate-parent symlinks silently
// un-resolved and producing different WorkspaceKey values for the same
// directory.
// Skipped on Windows where symlink creation requires developer mode.
func TestCanonicalWorkspacePath_ResolvesSymlinkToTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires developer mode / admin")
	}
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	resolvedFromLink, err := CanonicalWorkspacePath(link)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(link): %v", err)
	}
	resolvedFromReal, err := CanonicalWorkspacePath(realDir)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath(real): %v", err)
	}
	if resolvedFromLink != resolvedFromReal {
		t.Errorf("symlink + real path must produce same canonical; got link=%q real=%q",
			resolvedFromLink, resolvedFromReal)
	}
	if WorkspaceKey(resolvedFromLink) != WorkspaceKey(resolvedFromReal) {
		t.Errorf("WorkspaceKey must match for aliased paths; got link=%q real=%q",
			WorkspaceKey(resolvedFromLink), WorkspaceKey(resolvedFromReal))
	}
}
