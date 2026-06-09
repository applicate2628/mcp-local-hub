package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// trustedRootsTestDir redirects the daemon state dir to a fresh per-test
// temp tree (0700, so the parent-DACL/mode read gate passes on POSIX and
// Windows alike, matching autoRegisterTestEnv) and returns the resolved
// store path.
func trustedRootsTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir() // 0700 on POSIX; single-user on the Windows test host
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	path, err := DefaultLSPTrustedRootsPath()
	if err != nil {
		t.Fatalf("resolve trusted-roots path: %v", err)
	}
	return path
}

// mkdirTrusted makes a real directory under base so CanonicalWorkspacePath
// / EvalSymlinks can resolve it (a workspace root is a real directory in
// production). Returns the created absolute path.
func mkdirTrusted(t *testing.T, base, name string) string {
	t.Helper()
	p := filepath.Join(base, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}

func TestLSPTrustedRoots_LoadAbsentFileIsEmpty(t *testing.T) {
	path := trustedRootsTestDir(t)
	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("LoadLSPTrustedRoots on absent file: %v", err)
	}
	if f == nil {
		t.Fatal("LoadLSPTrustedRoots returned nil file for absent path")
	}
	if len(f.Roots) != 0 {
		t.Fatalf("absent file should yield zero roots, got %v", f.Roots)
	}
	// An empty store trusts nothing.
	if f.LSPWorkspaceRootTrusted(filepath.Dir(path)) {
		t.Fatal("empty store must not trust any root")
	}
}

func TestLSPTrustedRoots_BlessThenTrustExactAndSubdir(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "proj")
	child := mkdirTrusted(t, base, "proj/sub/nested")

	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("BlessTrustedRoot: %v", err)
	}

	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	// Exact match is trusted.
	if !f.LSPWorkspaceRootTrusted(root) {
		t.Fatalf("blessed root %q should be trusted (exact match)", root)
	}
	// A true subdirectory is trusted.
	if !f.LSPWorkspaceRootTrusted(child) {
		t.Fatalf("subdirectory %q of blessed root should be trusted", child)
	}
}

func TestLSPTrustedRoots_PrefixButNotSubdirRefused(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "proj")
	// "<root>2" shares a string prefix with <root> but is NOT a subdir.
	// Create it as a real sibling directory named "proj2".
	sibling := mkdirTrusted(t, base, "proj2")

	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("BlessTrustedRoot: %v", err)
	}
	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.LSPWorkspaceRootTrusted(sibling) {
		t.Fatalf("prefix-but-not-subdir %q must NOT be trusted by root %q (bare string-prefix bug)", sibling, root)
	}
}

func TestLSPTrustedRoots_UnrelatedRootRefused(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "trusted")
	other := mkdirTrusted(t, base, "elsewhere")

	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("BlessTrustedRoot: %v", err)
	}
	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.LSPWorkspaceRootTrusted(other) {
		t.Fatalf("unrelated root %q must not be trusted", other)
	}
}

func TestLSPTrustedRoots_BlessIsIdempotent(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "proj")

	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("first bless: %v", err)
	}
	// Bless the SAME root again — must not duplicate.
	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("second bless: %v", err)
	}
	// And bless a sub-form of the same path (trailing dot-slash) — still
	// canonicalizes to the same root, so still no duplicate.
	if err := BlessTrustedRoot(path, root+string(os.PathSeparator)+"."); err != nil {
		t.Fatalf("third bless (dot-suffixed): %v", err)
	}

	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(f.Roots) != 1 {
		t.Fatalf("idempotent bless should keep exactly 1 root, got %d: %v", len(f.Roots), f.Roots)
	}
}

// TestLSPTrustedRoots_OperatorHandAddedRootTrusted verifies the
// operator-configured-allowed-root branch: a root the operator writes
// into the file by hand (NOT via bless) is honored identically.
func TestLSPTrustedRoots_OperatorHandAddedRootTrusted(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "preTrusted")
	child := mkdirTrusted(t, base, "preTrusted/svc")

	// Canonicalize the way the operator's editor content would have to
	// match for production; but the loader canonicalizes on read, so even
	// a raw abs path here is honored.
	writeTrustedRootsFile(t, path, LSPTrustedRootsFile{Version: 1, Roots: []string{root}})

	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !f.LSPWorkspaceRootTrusted(child) {
		t.Fatalf("child %q of operator-hand-added root %q should be trusted", child, root)
	}
}

func TestLSPTrustedRoots_EmptyWorkspaceRootNotTrusted(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "proj")
	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("bless: %v", err)
	}
	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.LSPWorkspaceRootTrusted("") {
		t.Fatal("empty workspace root must never be trusted (fail-closed)")
	}
}

func TestLSPTrustedRoots_NilFileNotTrusted(t *testing.T) {
	var f *LSPTrustedRootsFile
	if f.LSPWorkspaceRootTrusted("/anything") {
		t.Fatal("nil trusted-roots file must fail closed (never trust)")
	}
}

// TestLSPTrustedRoots_WindowsCaseFold asserts case-insensitive
// containment on Windows (drive + path), and case-sensitivity elsewhere.
func TestLSPTrustedRoots_WindowsCaseFold(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "CaseProj")

	if err := BlessTrustedRoot(path, root); err != nil {
		t.Fatalf("bless: %v", err)
	}
	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Build an upper/lower-cased variant of the final path component.
	dir := filepath.Dir(root)
	upperVariant := filepath.Join(dir, "CASEPROJ")

	if runtime.GOOS == "windows" {
		if !f.LSPWorkspaceRootTrusted(upperVariant) {
			t.Fatalf("Windows case-fold: %q should be trusted by blessed %q", upperVariant, root)
		}
	} else {
		// On case-sensitive POSIX, a different-cased path is a DIFFERENT
		// directory; CaseProj exists but CASEPROJ does not, so
		// canonicalization (EvalSymlinks requires existence) fails and the
		// path is not trusted. That is the correct case-sensitive answer.
		if f.LSPWorkspaceRootTrusted(upperVariant) {
			t.Fatalf("POSIX is case-sensitive: %q must NOT be trusted by %q", upperVariant, root)
		}
	}
}

// TestLSPTrustedRoots_BlessRootStoredCanonical confirms the stored entry
// is the canonical (abs+clean) form, not the raw input.
func TestLSPTrustedRoots_BlessRootStoredCanonical(t *testing.T) {
	path := trustedRootsTestDir(t)
	base := t.TempDir()
	root := mkdirTrusted(t, base, "proj")

	// Bless a messy form: trailing separator + redundant dot segment.
	messy := root + string(os.PathSeparator) + "sub" + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "."
	if err := BlessTrustedRoot(path, messy); err != nil {
		t.Fatalf("bless messy form: %v", err)
	}
	f, err := LoadLSPTrustedRoots(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(f.Roots) != 1 {
		t.Fatalf("want 1 stored root, got %v", f.Roots)
	}
	stored := f.Roots[0]
	if strings.Contains(stored, "..") {
		t.Fatalf("stored root %q is not cleaned (contains '..')", stored)
	}
	wantCanonical, err := canonicalizeTrustedRoot(root)
	if err != nil {
		t.Fatalf("canonicalize expected: %v", err)
	}
	if !storedEqualsCanonical(stored, wantCanonical) {
		t.Fatalf("stored root %q != canonical %q", stored, wantCanonical)
	}
}

func writeTrustedRootsFile(t *testing.T, path string, f LSPTrustedRootsFile) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write trusted-roots file: %v", err)
	}
}
