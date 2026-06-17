package api

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// statePathsHelper resets all package-level seams introduced by
// state_paths.go so each test starts from production defaults. Cleanup
// runs even if the test panics.
//
// Tests that override knownFolderResolverFn or daemonStateRootOverride
// MUST call this first; otherwise leftover state from one test leaks
// into another via the shared package state.
//
// It also CLEARS daemonStateRootOverride at entry (saving + restoring it via the
// cleanup below). The api TestMain (main_test.go) installs a global non-empty
// override as a state-leak fence for the whole test binary; the resolver-chain
// tests (state_paths_test.go, state_paths_envfallback_test.go) call
// daemonStateDir() expecting the REAL LOCALAPPDATA/KnownFolder resolver to run
// (which the non-empty override would short-circuit), and the
// override-driving tests below (managedEntriesTestHelper, hubMcpStateTestHelper,
// TestWriteStateFileAtomic_*, TestInstallPlanCore_*, ...) set their own value
// AFTER this call so the clear is harmless to them. Clearing here lets every
// statePathsHelper caller start from the empty/real-resolver baseline while the
// cleanup restores the TestMain global default for subsequent tests.
func statePathsHelper(t *testing.T) {
	t.Helper()
	prevResolver := knownFolderResolverFn
	prevOverride := daemonStateRootOverride
	t.Cleanup(func() {
		knownFolderResolverFn = prevResolver
		daemonStateRootOverride = prevOverride
	})
	daemonStateRootOverride = ""
}

// installKnownFolderStub replaces the Windows KnownFolder resolver with
// fn for the duration of t. Always invoke statePathsHelper(t) first so
// the cleanup chain restores the production resolver.
func installKnownFolderStub(t *testing.T, fn func() (string, error)) {
	t.Helper()
	knownFolderResolverFn = fn
}

// isolateStateDir redirects every state-dir-resolving write for the
// duration of t into a fresh per-test temp dir and returns its path.
// Use it in any test that triggers an audit/state write whose target
// resolves through DaemonStateDir() — chiefly the secure-write
// relax-lane fallbacks (client-write-unhardened-fallback,
// state-file-write-unhardened-fallback). Without this, those audit
// events land in the operator's REAL %LOCALAPPDATA%\mcp-local-hub\
// hub-mcp.log / supervisor-events.log (test-hygiene bug
// 2026-05-20-tests-leak-state-into-production-logs.md): the SUBJECT of
// the write goes to t.TempDir(), but the AUDIT ROW reporting it leaks
// to the production log because the log path is resolved separately
// via DaemonStateDir().
//
// It composes statePathsHelper(t) so the prior daemonStateRootOverride
// is saved and restored on cleanup (panic-safe), then points the
// override at t.TempDir(). Tests that need to inspect the redirected
// log (e.g. assert a fallback event landed) can use the returned path.
func isolateStateDir(t *testing.T) string {
	t.Helper()
	statePathsHelper(t)
	// pr301 r5 Finding 1: harden the redirected STATE ROOT (not any broadened
	// write target a test may build separately) so the new absent-intent strict
	// verdict resolves relax=FALSE on this root, mirroring production's owner-only
	// %LOCALAPPDATA%. Relax-lane tests that point a SEPARATE broadened dir at the
	// write gate are unaffected — only the intent-read root is hardened here.
	dir := hardenedTempDir(t)
	daemonStateRootOverride = dir
	return dir
}

// TestDaemonStateDir_Windows_KnownFolderFails_NoFallbackInProduction
// exercises the production fail-closed path defined by plan §16: when
// SHGetKnownFolderPath fails, daemonStateDir MUST NOT consult the
// LOCALAPPDATA / USERPROFILE env fallbacks (those exist only in test
// builds compiled with -tags=test_state_path_env). The wrapper must
// return errKnownFolderUnavailable so the watchdog CLI can translate
// the failure into exit code 8.
//
// Build-tag note: this test is in the default test file (no build tag),
// so it is compiled into BOTH production-default and test-env-fallback
// builds. Inside the test we still expect the production behavior
// regardless of which build tag is active — the env-fallback test file
// covers the opposite assertion under its own tag.
func TestDaemonStateDir_Windows_KnownFolderFails_NoFallbackInProduction(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("KnownFolder resolution is Windows-only")
	}
	// Skip when running under -tags=test_state_path_env. The fallback
	// build of daemonStateDir intentionally consults env vars; that is
	// covered by TestDaemonStateDir_Windows_KnownFolderFails_FallsBackToEnv
	// in state_paths_envfallback_test.go.
	if testEnvFallbackBuild {
		t.Skip("env fallback is gated to test_state_path_env build only")
	}
	statePathsHelper(t)

	stubErr := errors.New("synthetic SHGetKnownFolderPath failure")
	installKnownFolderStub(t, func() (string, error) {
		return "", stubErr
	})

	// Set both fallback env vars so the test would catch any code path
	// that silently consults them. Production must ignore both.
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "localappdata"))
	t.Setenv("USERPROFILE", filepath.Join(t.TempDir(), "userprofile"))

	got, err := daemonStateDir()
	if err == nil {
		t.Fatalf("expected error in production path; got %q with nil err", got)
	}
	if !errors.Is(err, errKnownFolderUnavailable) {
		t.Fatalf("expected errKnownFolderUnavailable, got %v", err)
	}
}

// TestDaemonStateDir_LinuxXDG asserts the Linux path honors
// XDG_DATA_HOME when set (per plan §15).
func TestDaemonStateDir_LinuxXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG_DATA_HOME is Linux-only")
	}
	statePathsHelper(t)
	root := t.TempDir()
	xdg := filepath.Join(root, "xdg-data-home")
	if err := os.MkdirAll(xdg, 0o700); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdg)
	t.Setenv("HOME", filepath.Join(root, "home"))

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	want := filepath.Join(xdg, "mcp-local-hub")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	// Sanity: the directory must exist after the call.
	if _, statErr := os.Stat(got); statErr != nil {
		t.Fatalf("expected dir created: %v", statErr)
	}
}

// TestDaemonStateDir_LinuxFallback asserts $HOME/.local/share is used
// when XDG_DATA_HOME is empty (per plan §15).
func TestDaemonStateDir_LinuxFallback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("$HOME/.local/share fallback is Linux-only")
	}
	statePathsHelper(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", home)

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "mcp-local-hub")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// TestDaemonStateDir_macOS asserts ~/Library/Application Support is used.
func TestDaemonStateDir_macOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Library/Application Support is macOS-only")
	}
	statePathsHelper(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	want := filepath.Join(home, "Library", "Application Support", "mcp-local-hub")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// TestDaemonStateDir_DirPermsPOSIX asserts the state dir lands at
// 0700 after MkdirAll + explicit Chmod (defense vs umask drift).
func TestDaemonStateDir_DirPermsPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	statePathsHelper(t)
	root := setupPOSIXHomes(t)
	t.Logf("posix root = %s", root)

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	st, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := st.Mode().Perm()
	if mode != 0o700 {
		t.Fatalf("dir perms = %o, want 0700", mode)
	}
}

// TestDaemonStateDir_FilePermsPOSIX asserts OpenStateFile pre-creates
// at 0600 + post-open Chmod (defense vs umask drift) per §15.
func TestDaemonStateDir_FilePermsPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	statePathsHelper(t)
	setupPOSIXHomes(t)

	f, err := OpenStateFile("permcheck.txt")
	if err != nil {
		t.Fatalf("OpenStateFile: %v", err)
	}
	if _, werr := f.Write([]byte("hello")); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	st, err := os.Stat(f.Name())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	mode := st.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("file perms = %o, want 0600", mode)
	}
}

// TestDaemonStateDir_RejectWorldWritablePOSIX asserts the sanity check
// rejects a parent dir with world-writable bit set.
func TestDaemonStateDir_RejectWorldWritablePOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	statePathsHelper(t)
	root := setupPOSIXHomes(t)

	// Pre-create the parent dir with deliberately-loose 0777 perms.
	parent := computePOSIXParent(t, root)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		t.Fatalf("mkdir loose parent: %v", err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatalf("chmod loose: %v", err)
	}

	got, err := daemonStateDir()
	if err == nil {
		t.Fatalf("expected sanity-check error; got path %q with nil err", got)
	}
	if !errors.Is(err, errStateParentInsecure) {
		t.Fatalf("expected errStateParentInsecure, got %v", err)
	}
}

// TestDaemonStateDir_RejectGroupWritablePOSIX asserts the sanity check
// rejects a parent dir with group-writable bit set (0770).
func TestDaemonStateDir_RejectGroupWritablePOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	statePathsHelper(t)
	root := setupPOSIXHomes(t)

	parent := computePOSIXParent(t, root)
	if err := os.MkdirAll(parent, 0o770); err != nil {
		t.Fatalf("mkdir group-writable parent: %v", err)
	}
	if err := os.Chmod(parent, 0o770); err != nil {
		t.Fatalf("chmod 0770: %v", err)
	}

	got, err := daemonStateDir()
	if err == nil {
		t.Fatalf("expected sanity-check error; got path %q with nil err", got)
	}
	if !errors.Is(err, errStateParentInsecure) {
		t.Fatalf("expected errStateParentInsecure, got %v", err)
	}
}

// TestDaemonStateDir_RejectNonOwnerPOSIX asserts the sanity check
// rejects a parent dir owned by a different UID. Skipped when the
// test process cannot manipulate file ownership (i.e. non-root).
func TestDaemonStateDir_RejectNonOwnerPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX UID semantics do not apply on Windows")
	}
	if os.Getuid() != 0 {
		t.Skip("UID-changing capability requires root")
	}
	statePathsHelper(t)
	root := setupPOSIXHomes(t)

	parent := computePOSIXParent(t, root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	// Reassign to the nobody UID/GID heuristic; if any step fails the
	// sanity check still fires because mode/uid mismatch surfaces a
	// clear error path. Production code must reject before write.
	const nobody = 65534
	if err := os.Chown(parent, nobody, nobody); err != nil {
		t.Skipf("Chown to nobody failed (%v); test only runs as root with chown capability", err)
	}

	got, err := daemonStateDir()
	if err == nil {
		t.Fatalf("expected sanity-check error; got path %q with nil err", got)
	}
	if !errors.Is(err, errStateParentInsecure) {
		t.Fatalf("expected errStateParentInsecure, got %v", err)
	}
}

// TestDaemonStateDir_OpenStateFileRejectsTraversal asserts OpenStateFile
// rejects names that would escape the state directory via path-traversal
// segments. Defense-in-depth — callers must never pass user input as the
// state-file name, but the wrapper still refuses to open outside the
// owned root.
func TestDaemonStateDir_OpenStateFileRejectsTraversal(t *testing.T) {
	statePathsHelper(t)
	if runtime.GOOS == "windows" {
		// Even on Windows the path-traversal guard is portable; setup
		// the override-root so the wrapper has a writable target.
		daemonStateRootOverride = t.TempDir()
	} else {
		setupPOSIXHomes(t)
	}

	bad := []string{
		"..\\escape.txt",
		"../escape.txt",
		"sub/escape.txt",
		"sub\\escape.txt",
		filepath.Join("..", "escape.txt"),
	}
	for _, name := range bad {
		f, err := OpenStateFile(name)
		if err == nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			t.Errorf("OpenStateFile(%q) accepted traversal name", name)
			continue
		}
		if !errors.Is(err, errStateNameInvalid) {
			t.Errorf("OpenStateFile(%q): want errStateNameInvalid, got %v", name, err)
		}
	}
}

// TestDaemonStateDir_PublicWrapperReturnsSamePath asserts DaemonStateDir
// (public) and daemonStateDir (private) return the same value — i.e.
// the public wrapper has no surprising rewrites.
func TestDaemonStateDir_PublicWrapperReturnsSamePath(t *testing.T) {
	statePathsHelper(t)
	if runtime.GOOS == "windows" {
		// On Windows-default-build the production resolver must succeed
		// against a real LocalAppData; substitute a stub that returns a
		// deterministic temp path so the test does not depend on host state.
		root := t.TempDir()
		installKnownFolderStub(t, func() (string, error) { return root, nil })
	} else {
		setupPOSIXHomes(t)
	}

	pub, errPub := DaemonStateDir()
	priv, errPriv := daemonStateDir()
	if errPub != nil || errPriv != nil {
		t.Fatalf("errors: pub=%v priv=%v", errPub, errPriv)
	}
	if pub != priv {
		t.Fatalf("DaemonStateDir=%q daemonStateDir=%q (must match)", pub, priv)
	}
	if !strings.HasSuffix(pub, "mcp-local-hub") {
		t.Fatalf("path %q does not end with mcp-local-hub", pub)
	}
}

// ---------------------------------------------------------------------
// Test-only helpers.
// ---------------------------------------------------------------------

// setupPOSIXHomes seeds the env so daemonStateDir resolves to a fresh
// per-test directory under t.TempDir(). Returns the root of the
// per-test tree so callers can derive the parent dir for sanity-check
// pre-seeding.
func setupPOSIXHomes(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	return root
}

// computePOSIXParent returns the parent directory daemonStateDir will
// create on the current OS, given the root produced by setupPOSIXHomes.
// Callers use this to pre-seed an insecure parent before invoking
// daemonStateDir to verify the sanity check rejects it.
func computePOSIXParent(t *testing.T, root string) string {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		return filepath.Join(root, "home", ".local", "share")
	case "darwin":
		return filepath.Join(root, "home", "Library", "Application Support")
	default:
		t.Fatalf("computePOSIXParent: unsupported GOOS %q", runtime.GOOS)
		return ""
	}
}

// modeMatchesPerms returns true if fs.FileMode m exactly matches the
// requested permission bits. Uses Perm() so non-permission bits are
// ignored. Used in informational logging only.
func modeMatchesPerms(m fs.FileMode, want fs.FileMode) bool {
	return m.Perm() == want.Perm()
}
