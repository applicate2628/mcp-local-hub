//go:build af1_counters

package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// AF-1 entry-count tests — the handle-pinned-path proof (T5) and the per-lane
// no-string-re-walk assertions, gated behind -tags=af1_counters so the counter
// variables (secure_write_counters_aftag.go) exist. The matching FUNCTIONAL
// tests (write succeeds, audit event fires, target content) run in the default
// build in client_write_init_a3_toctou_test.go / client_write_consent_surface_test.go;
// these add ONLY the counter assertions for each follow-symlink lane.
//
// Run with: go test -tags=test_state_path_env,af1_counters -run 'Counter|T5' ./internal/api/
//
// Reuses a3SymlinkFixture / hardenedTempDir / hubMcpStateTestHelper from the
// ungated A3 test files (same package, compiled together under this tag).

// TestA3_T5_EnvVarLaneUsesHandlePinnedPath_NoStringReWalk — env-var lane goes
// through the handle-pinned path: NO string re-walk. The explicit AF-1 closure
// guard. After a symlink-lane write the path-based string entry counter MUST be
// 0 and the resolved-parent counter MUST be > 0.
func TestA3_T5_EnvVarLaneUsesHandlePinnedPath_NoStringReWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1") // F2 env-var lane
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	resetA3EntryCounters()
	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("T5: env-var lane write must SUCCEED: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("T5: resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("T5 (AF-1 closure): env-var lane re-walked a resolved string "+
			"(path-based string entry count=%d, want 0) — the TOCTOU re-walk is back",
			secureWritePathBasedStringEntryCount)
	}
	if secureWriteResolvedParentEntryCount == 0 {
		t.Errorf("T5: env-var lane did not go through the handle-pinned resolved-parent owner (count=0)")
	}
}

// TestA3_Counter_ScopedConsentLane_NoStringReWalk — the scoped-consent lane
// (no env opt-in) stays on the handle-pinned path: string counter 0,
// resolved-parent counter > 0.
func TestA3_Counter_ScopedConsentLane_NoStringReWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // consent is the only follow input
	_ = hubMcpStateTestHelper(t)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	consent := &ResolvedSymlinkConsent{
		Client:             "claude-code",
		OriginalPath:       link,
		PinnedResolvedPath: filepath.Clean(real), // FULL resolved target path
	}
	resetA3EntryCounters()
	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), consent); err != nil {
		t.Fatalf("scoped-consent write must SUCCEED: %v", err)
	}
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("scoped-consent lane re-walked a string (count=%d, want 0)", secureWritePathBasedStringEntryCount)
	}
	if secureWriteResolvedParentEntryCount == 0 {
		t.Errorf("scoped-consent lane did not go through the resolved-parent owner (count=0)")
	}
}

// TestA3_Counter_WithConsentFacade_NoStringReWalk — the exported WRITE facade
// (SecureWriteClientConfigWithConsent) stays on the handle-pinned path.
func TestA3_Counter_WithConsentFacade_NoStringReWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	_ = hubMcpStateTestHelper(t)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	_, pinnedPath, isSymlink := ResolveClientConfigSymlink(link)
	if !isSymlink {
		t.Fatalf("expected %q to resolve as a symlink", link)
	}
	consent := ResolvedSymlinkConsent{Client: "claude-code", OriginalPath: link, PinnedResolvedPath: pinnedPath}
	resetA3EntryCounters()
	if err := SecureWriteClientConfigWithConsent(consent, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("WithConsent write must SUCCEED: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("WithConsent re-walked a string (count=%d, want 0)", secureWritePathBasedStringEntryCount)
	}
}

// TestA3_Counter_InteractivePortLane_NoStringReWalk — the injected interactive
// consent port lane stays on the handle-pinned path.
func TestA3_Counter_InteractivePortLane_NoStringReWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // the port is the only follow input
	_ = hubMcpStateTestHelper(t)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	prev := InteractiveSymlinkConsent
	InteractiveSymlinkConsent = func(client, originalPath, pinnedPath string) bool { return true }
	t.Cleanup(func() { InteractiveSymlinkConsent = prev })

	resetA3EntryCounters()
	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("approving interactive port must follow + SUCCEED: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("interactive-port lane re-walked a string (count=%d, want 0)", secureWritePathBasedStringEntryCount)
	}
}

// TestA3_Counter_BroadenedRelaxRetry_NoStringReWalk — the relax retry (broadened
// resolved-target parent, non-strict) stays on the handle-pinned path.
func TestA3_Counter_BroadenedRelaxRetry_NoStringReWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; POSIX exercises the broadened-parent relax lane")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	_ = hubMcpStateTestHelper(t)

	broadenedTree := filepath.Join(t.TempDir(), "broadened")
	if err := os.Mkdir(broadenedTree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(broadenedTree, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(broadenedTree, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTree := hardenedTempDir(t)
	link := filepath.Join(linkTree, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	resetA3EntryCounters()
	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("broadened-parent symlink write must SUCCEED via relax lane: %v", err)
	}
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("relax retry re-walked a resolved string (count=%d, want 0)", secureWritePathBasedStringEntryCount)
	}
}
