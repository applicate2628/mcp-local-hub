//go:build test_state_path_env

// state_paths_envfallback_test.go — tests gated behind the
// test_state_path_env build tag. They drive the env-fallback variant
// of daemonStateDir defined in state_paths_envfallback.go (also gated
// by the same tag). Production binaries never compile any of this:
//
//   - Default `go test ./internal/api/`             → KnownFolder-only.
//   - `go test -tags=test_state_path_env ./internal/api/` → fallback active.
//
// Plan §16 v9 makes the production-vs-test split a compile-time guarantee.

package api

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDaemonStateDir_Windows_KnownFolder asserts that when the
// SHGetKnownFolderPath stub returns a synthetic LocalAppData root, the
// daemonStateDir resolver appends `mcp-local-hub` and returns the
// composed path.
func TestDaemonStateDir_Windows_KnownFolder(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("KnownFolder resolution is Windows-only")
	}
	statePathsHelper(t)

	root := t.TempDir()
	installKnownFolderStub(t, func() (string, error) { return root, nil })

	// Both env-fallback inputs are intentionally bogus so the test
	// would fail loudly if daemonStateDir consulted them when the
	// stub succeeded.
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "should-not-be-used"))
	t.Setenv("USERPROFILE", filepath.Join(t.TempDir(), "should-not-be-used-either"))

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	want := filepath.Join(root, "mcp-local-hub")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// TestDaemonStateDir_StateDirOverride_WinsOverResolver asserts the
// PR #300 r2 P1 fix: MCPHUB_STATE_DIR_OVERRIDE is honored BEFORE the
// platform resolver, so a tagged subprocess child inheriting the env
// redirects to the override EVEN WHEN the KnownFolder resolver returns
// a real path. Pre-fix, resolveKnownFolderWithEnvFallback called the
// resolver first and only consulted LOCALAPPDATA on resolver failure —
// inert on a real Windows host where SHGetKnownFolderPath succeeds.
//
// Cross-platform: the override short-circuit sits before the GOOS
// switch, so this test runs on Windows AND POSIX (it never reaches
// posixStateDir's parent sanity check because the override returns
// first).
func TestDaemonStateDir_StateDirOverride_WinsOverResolver(t *testing.T) {
	statePathsHelper(t)
	// The in-memory override must be empty so the ENV path is exercised
	// (the in-memory override is checked first and would otherwise mask
	// the env branch). statePathsHelper saved the prior value and will
	// restore it on cleanup.
	daemonStateRootOverride = ""

	// Resolver returns a real, NON-override path. If the override did not
	// win, daemonStateDir would compose this resolver root + mcp-local-hub
	// (Windows) or fall to posixStateDir (POSIX) — either way NOT the
	// override — and the assertion below would fail.
	resolverRoot := filepath.Join(t.TempDir(), "resolver-localappdata")
	installKnownFolderStub(t, func() (string, error) { return resolverRoot, nil })

	override := filepath.Join(t.TempDir(), "state-dir-override")
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", override)
	// Bogus env-fallback inputs: if the resolver-first chain were ever
	// consulted, these would surface in the result and fail the test.
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "should-not-be-used"))
	t.Setenv("USERPROFILE", filepath.Join(t.TempDir(), "should-not-be-used-either"))

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	// The override is the FULL state root (mirrors the in-memory
	// daemonStateRootOverride contract): ensureStateRoot MkdirAll's it and
	// returns it verbatim, NO mcp-local-hub leaf appended.
	if got != override {
		t.Fatalf("path = %q, want override %q (MCPHUB_STATE_DIR_OVERRIDE must win over the resolver)", got, override)
	}
	if info, statErr := os.Stat(override); statErr != nil || !info.IsDir() {
		t.Fatalf("daemonStateDir must MkdirAll the override dir %q (stat err: %v)", override, statErr)
	}

	// Read-only variant must take the same override short-circuit (no
	// MkdirAll, returns verbatim).
	roGot, roErr := daemonStateDirReadOnly()
	if roErr != nil {
		t.Fatalf("daemonStateDirReadOnly: %v", roErr)
	}
	if roGot != override {
		t.Fatalf("read-only path = %q, want override %q", roGot, override)
	}
}

// TestDaemonStateDir_Windows_KnownFolderFails_FallsBackToEnv asserts the
// test-build-only fallback: when the stub fails, LOCALAPPDATA wins
// before USERPROFILE\AppData\Local.
func TestDaemonStateDir_Windows_KnownFolderFails_FallsBackToEnv(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("env fallback applies only on Windows")
	}
	statePathsHelper(t)

	stubErr := errors.New("synthetic SHGetKnownFolderPath failure")
	installKnownFolderStub(t, func() (string, error) { return "", stubErr })

	root := t.TempDir()
	localAppData := filepath.Join(root, "LocalAppData")
	if err := os.MkdirAll(localAppData, 0o700); err != nil {
		t.Fatalf("mkdir LocalAppData: %v", err)
	}
	userProfile := filepath.Join(root, "UserProfile")
	if err := os.MkdirAll(filepath.Join(userProfile, "AppData", "Local"), 0o700); err != nil {
		t.Fatalf("mkdir UserProfile: %v", err)
	}
	t.Setenv("LOCALAPPDATA", localAppData)
	t.Setenv("USERPROFILE", userProfile)

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	want := filepath.Join(localAppData, "mcp-local-hub")
	if got != want {
		t.Fatalf("path = %q, want %q (LOCALAPPDATA must win over USERPROFILE)", got, want)
	}
}

// TestDaemonStateDir_Windows_KnownFolderFails_UserProfileFallback
// covers the case where LOCALAPPDATA is empty so the resolver falls
// further back to USERPROFILE\AppData\Local. Belongs to the env-
// fallback build only.
func TestDaemonStateDir_Windows_KnownFolderFails_UserProfileFallback(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("env fallback applies only on Windows")
	}
	statePathsHelper(t)
	installKnownFolderStub(t, func() (string, error) {
		return "", errors.New("synthetic")
	})

	root := t.TempDir()
	userProfile := filepath.Join(root, "UserProfile")
	if err := os.MkdirAll(filepath.Join(userProfile, "AppData", "Local"), 0o700); err != nil {
		t.Fatalf("mkdir UserProfile: %v", err)
	}
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("USERPROFILE", userProfile)

	got, err := daemonStateDir()
	if err != nil {
		t.Fatalf("daemonStateDir: %v", err)
	}
	want := filepath.Join(userProfile, "AppData", "Local", "mcp-local-hub")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// TestDaemonStateDir_Windows_KnownFolderFails_NoEnvVars asserts that
// when both stub fails AND env vars are empty, daemonStateDir surfaces
// errKnownFolderUnavailable. (Mirrors the production fail-closed
// behavior even in the test-fallback build, just without the env
// short-circuits firing.)
func TestDaemonStateDir_Windows_KnownFolderFails_NoEnvVars(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("env fallback applies only on Windows")
	}
	statePathsHelper(t)
	installKnownFolderStub(t, func() (string, error) {
		return "", errors.New("synthetic")
	})
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("USERPROFILE", "")

	got, err := daemonStateDir()
	if err == nil {
		t.Fatalf("expected errKnownFolderUnavailable; got %q nil err", got)
	}
	if !errors.Is(err, errKnownFolderUnavailable) {
		t.Fatalf("got err %v, want errKnownFolderUnavailable", err)
	}
}
