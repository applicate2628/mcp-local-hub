package api

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mcp-local-hub/internal/clients"
)

// A3 PR-2 — the cross-package EXPOSURE surface: the exported facade
// (ResolveClientConfigSymlink + SecureWriteClientConfigWithConsent) and the
// injected InteractiveSymlinkConsent port. These pin that the facade derives
// the pin EXACTLY as the write-time guard does, that the interactive port
// follows / refuses / and can never bypass strict mode, and that the WRITE
// facade routes through the same handle-pinned, pin-verified choke point as an
// explicit scoped consent.
//
// Symlink-bearing cases run POSIX-only (Windows symlink creation needs
// elevation); the cross-platform code path is identical and exercised here.
// Reuses a3SymlinkFixture / hardenedTempDir / hubMcpStateTestHelper /
// bytesContainsStr / resetA3EntryCounters from client_write_init_a3_toctou_test.go.

// TestResolveClientConfigSymlink_PinMatchesWriteTimeGuard pins the
// single-owner property: the pinnedParent the RESOLVE facade returns is
// byte-identical to filepath.Clean(filepath.Dir(resolvedTarget)) — the exact
// value the write-time guard (secureWriteFollowingSymlink) recomputes and
// compares. A drift between the two would silently break the
// "operator-saw-pin == verified-pin" invariant.
func TestResolveClientConfigSymlink_PinMatchesWriteTimeGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	resolvedTarget, pinnedParent, isSymlink := ResolveClientConfigSymlink(link)
	if !isSymlink {
		t.Fatalf("ResolveClientConfigSymlink(%q) reported isSymlink=false; want true", link)
	}
	// The write-time guard compares filepath.Clean(filepath.Dir(resolved));
	// the facade must produce that exact value.
	wantPin := filepath.Clean(filepath.Dir(resolvedTarget))
	if pinnedParent != wantPin {
		t.Errorf("pinnedParent=%q, want %q (must match write-time guard derivation)", pinnedParent, wantPin)
	}
	// And the resolved target's parent IS the seeded real file's parent.
	if pinnedParent != filepath.Clean(filepath.Dir(real)) {
		t.Errorf("pinnedParent=%q, want parent of real target %q", pinnedParent, filepath.Clean(filepath.Dir(real)))
	}
}

// TestResolveClientConfigSymlink_RegularFile reports isSymlink=false with an
// empty pin for a regular (non-symlink) file — the caller then takes its
// ordinary write path, nothing to consent to.
func TestResolveClientConfigSymlink_RegularFile(t *testing.T) {
	dir := hardenedTempDir(t)
	regular := filepath.Join(dir, "regular.json")
	if err := os.WriteFile(regular, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedTarget, pinnedParent, isSymlink := ResolveClientConfigSymlink(regular)
	if isSymlink {
		t.Errorf("regular file reported isSymlink=true")
	}
	if pinnedParent != "" {
		t.Errorf("regular file pinnedParent=%q, want empty", pinnedParent)
	}
	if resolvedTarget != regular {
		t.Errorf("regular file resolvedTarget=%q, want %q (echoed unchanged)", resolvedTarget, regular)
	}
}

// TestSecureWriteClientConfigWithConsent_PinMatch_Succeeds drives the WRITE
// facade end-to-end with a pin built from the RESOLVE facade: no swap, the
// pin matches, the symlink is followed via the handle-pinned path, and the
// distinct scoped-consent audit event fires.
func TestSecureWriteClientConfigWithConsent_PinMatch_Succeeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // consent is the only follow input
	_ = hubMcpStateTestHelper(t)

	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	_, pinnedParent, isSymlink := ResolveClientConfigSymlink(link)
	if !isSymlink {
		t.Fatalf("expected %q to resolve as a symlink", link)
	}
	consent := ResolvedSymlinkConsent{
		Client:             "claude-code",
		OriginalPath:       link,
		PinnedResolvedPath: pinnedParent,
	}
	resetA3EntryCounters()
	if err := SecureWriteClientConfigWithConsent(consent, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("WithConsent pin-match write must SUCCEED: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("resolved target content = %q, want %q", b, `{"v":1}`)
	}
	// Handle-pinned path, no string re-walk (AF-1).
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("WithConsent re-walked a string (count=%d, want 0)", secureWritePathBasedStringEntryCount)
	}
}

// TestSecureWriteClientConfigWithConsent_SwapMismatch_Refused proves the WRITE
// facade inherits PR-1's swap-between-confirm-and-write guard: the operator
// consented to parent P, the symlink is repointed to P' via the test seam
// before the write-time re-resolve, and the write is refused on pin mismatch.
func TestSecureWriteClientConfigWithConsent_SwapMismatch_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	dir := hardenedTempDir(t)

	parentP := filepath.Join(dir, "P")
	parentPrime := filepath.Join(dir, "Pprime")
	for _, p := range []string{parentP, parentPrime} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realP := filepath.Join(parentP, "real.json")
	realPrime := filepath.Join(parentPrime, "real.json")
	for _, p := range []string{realP, realPrime} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realP, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Operator resolved + consented to P.
	_, pinnedParent, _ := ResolveClientConfigSymlink(link)
	consent := ResolvedSymlinkConsent{OriginalPath: link, PinnedResolvedPath: pinnedParent}

	swapped := false
	afterResolveBeforePinHook = func() {
		if swapped {
			return
		}
		swapped = true
		_ = os.Remove(link)
		if err := os.Symlink(realPrime, link); err != nil {
			t.Fatalf("swap symlink to P': %v", err)
		}
	}
	t.Cleanup(func() { afterResolveBeforePinHook = nil })

	err := SecureWriteClientConfigWithConsent(consent, []byte(`{"v":1}`))
	if err == nil {
		t.Fatalf("swap-after-consent must REFUSE the write")
	}
	if !bytesContainsStr(err.Error(), "does not match the operator-consented target") {
		t.Errorf("expected pin-mismatch refusal, got %q", err.Error())
	}
	if b, _ := os.ReadFile(realPrime); string(b) != "{}" {
		t.Errorf("swap target P' was written — TOCTOU NOT closed: %q", b)
	}
}

// TestInteractiveSymlinkConsent_Approve_Follows pins design B: with NO env
// opt-in and NO scoped consent, an approving injected port follows the symlink
// via a freshly-built pinned consent, writes through the handle-pinned path,
// and the port receives the resolved pinnedParent (so the CLI prompt can show
// it).
func TestInteractiveSymlinkConsent_Approve_Follows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // no env opt-in: the port is the only follow input
	_ = hubMcpStateTestHelper(t)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	wantPin := filepath.Clean(filepath.Dir(real))
	var gotOriginal, gotPin string
	prev := InteractiveSymlinkConsent
	InteractiveSymlinkConsent = func(client, originalPath, pinnedParent string) bool {
		gotOriginal = originalPath
		gotPin = pinnedParent
		return true
	}
	t.Cleanup(func() { InteractiveSymlinkConsent = prev })

	resetA3EntryCounters()
	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("approving interactive port must follow + SUCCEED: %v", err)
	}
	if gotOriginal != link {
		t.Errorf("port saw originalPath=%q, want %q", gotOriginal, link)
	}
	if gotPin != wantPin {
		t.Errorf("port saw pinnedParent=%q, want %q (the value the CLI shows the operator)", gotPin, wantPin)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if secureWritePathBasedStringEntryCount != 0 {
		t.Errorf("interactive-port lane re-walked a string (count=%d, want 0)", secureWritePathBasedStringEntryCount)
	}
}

// TestInteractiveSymlinkConsent_Decline_Refused: a declining port does NOT
// follow — the write falls through to the path-based pipeline whose
// pre-existing-symlink refusal is the documented default. The target is
// untouched.
func TestInteractiveSymlinkConsent_Decline_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	called := false
	prev := InteractiveSymlinkConsent
	InteractiveSymlinkConsent = func(client, originalPath, pinnedParent string) bool {
		called = true
		return false // operator declined
	}
	t.Cleanup(func() { InteractiveSymlinkConsent = prev })

	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil)
	if err == nil {
		t.Fatalf("declined interactive port must leave the symlink refused")
	}
	if !called {
		t.Errorf("interactive port was never consulted for a symlinked destination")
	}
	if got := err.Error(); !bytesContainsStr(got, "symlink") && !bytesContainsStr(got, "reparse") {
		t.Errorf("expected a symlink/reparse refusal after decline, got %q", got)
	}
	if b, _ := os.ReadFile(real); string(b) != "{}" {
		t.Errorf("target mutated despite decline: %q", b)
	}
}

// TestInteractiveSymlinkConsent_StrictMode_NeverConsultedAndRefused is the
// PROTECTED strict-override invariant for the NEW interactive port: under
// strict mode the port is NEVER invoked (the !operatorRequiresSingleUserHome
// gate excludes it) and the write is refused. A port that panics proves it is
// never reached.
func TestInteractiveSymlinkConsent_StrictMode_NeverConsultedAndRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "1") // strict
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	t.Cleanup(resetStrictModeIntentCacheForTest)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	prev := InteractiveSymlinkConsent
	InteractiveSymlinkConsent = func(client, originalPath, pinnedParent string) bool {
		t.Fatalf("interactive port MUST NOT be consulted under strict mode (port reached for %q)", originalPath)
		return true
	}
	t.Cleanup(func() { InteractiveSymlinkConsent = prev })

	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil)
	if err == nil {
		t.Fatalf("strict mode must REFUSE the symlink regardless of the interactive port")
	}
	if b, _ := os.ReadFile(real); string(b) != "{}" {
		t.Errorf("target mutated despite strict refusal: %q", b)
	}
}

// TestInteractiveSymlinkConsent_NilDefault_RefusesSymlink: production default
// (nil port, no env opt-in, no scoped consent) leaves a symlinked destination
// refused — automation is never silently redirected.
func TestInteractiveSymlinkConsent_NilDefault_RefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	prev := InteractiveSymlinkConsent
	InteractiveSymlinkConsent = nil // production default
	t.Cleanup(func() { InteractiveSymlinkConsent = prev })

	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil)
	if err == nil {
		t.Fatalf("nil interactive port + no opt-in must REFUSE the symlink (automation safety)")
	}
	if b, _ := os.ReadFile(real); string(b) != "{}" {
		t.Errorf("target mutated despite default refusal: %q", b)
	}
}

// TestInteractiveSymlinkConsent_ProductionLane_SuppliesClient is the F1
// attribution guard: the PRODUCTION interactive lane (the only input is
// (path, contents) — no client param) must DERIVE the client name from the
// destination path against the adapter catalog and supply it BOTH to the
// prompt port AND into the scoped consent, so the
// client-write-symlink-resolved-via-scoped-consent audit event logs a
// non-empty "client". Before the fix the lane passed "" everywhere and the
// audit could not attribute which client's config was symlink-followed.
//
// The catalog is injected via the clientCatalogForDerivation seam so the test
// drives the real derivation lane without depending on the host's real client
// config paths: the seeded symlink IS the fake adapter's ConfigPath().
func TestInteractiveSymlinkConsent_ProductionLane_SuppliesClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // the interactive port is the only follow input
	logDir := hubMcpStateTestHelper(t)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	// Inject a catalog whose single adapter claims the seeded symlink as its
	// config path, so deriveClientNameForConfigPath(link) resolves to its name.
	prevCatalog := clientCatalogForDerivation
	clientCatalogForDerivation = func() map[string]clients.Client {
		return map[string]clients.Client{
			"codex-cli": &lspRouterFakeClient{
				name:      "codex-cli",
				path:      link,
				entries:   map[string]clients.MCPEntry{},
				snapshots: map[string]map[string]clients.MCPEntry{},
			},
		}
	}
	t.Cleanup(func() { clientCatalogForDerivation = prevCatalog })

	var gotClient string
	prev := InteractiveSymlinkConsent
	InteractiveSymlinkConsent = func(client, originalPath, pinnedParent string) bool {
		gotClient = client
		return true
	}
	t.Cleanup(func() { InteractiveSymlinkConsent = prev })

	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("approving interactive port must follow + SUCCEED: %v", err)
	}
	// The PROMPT received the derived client (so the CLI line names it, not "").
	if gotClient != "codex-cli" {
		t.Errorf("interactive port got client=%q, want %q (production lane must DERIVE + supply it)", gotClient, "codex-cli")
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("resolved target content = %q, want %q", b, `{"v":1}`)
	}
	// The AUDIT consent carried the derived client: the scoped-consent event
	// logs "client":"codex-cli" rather than the old empty "client":"".
	logBytes, rerr := os.ReadFile(filepath.Join(logDir, "hub-mcp.log"))
	if rerr != nil {
		t.Fatalf("read hub-mcp.log: %v", rerr)
	}
	if !bytes.Contains(logBytes, []byte("client-write-symlink-resolved-via-scoped-consent")) {
		t.Fatalf("scoped-consent audit event missing: %s", logBytes)
	}
	if !bytes.Contains(logBytes, []byte(`"client":"codex-cli"`)) {
		t.Errorf("audit event did not attribute the client (want \"client\":\"codex-cli\"): %s", logBytes)
	}
	if bytes.Contains(logBytes, []byte(`"client":""`)) {
		t.Errorf("audit event logged an EMPTY client — F1 attribution not fixed: %s", logBytes)
	}
}
