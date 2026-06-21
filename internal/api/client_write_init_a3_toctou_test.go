package api

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A3 PR-1 — symlink-client-config scoped consent + AF-1 TOCTOU closure.
//
// These tests pin the threat model T1-T6 for the handle-pinned symlink-
// resolve write lane (SEAM-A shared post-parent-open owner + SEAM-B scoped
// consent). The symlink-bearing cases run on POSIX only (Windows symlink
// creation needs SeCreateSymbolicLinkPrivilege / elevation; the
// cross-platform code path is identical and exercised on the POSIX leg —
// same convention as the pre-existing TestSecureWriteWithOperatorOpt_Symlink*
// tests in secure_write_client_config_test.go).
//
// State-safety: each test that emits audit events routes the hub-mcp log
// into a hardened temp state dir via hubMcpStateTestHelper(t), and every
// env var is set through t.Setenv so the real host posture is never touched.

// The AF-1 entry-count helpers (resetA3EntryCounters + the counters they
// touch) live behind the af1_counters build tag in
// secure_write_counters_aftag.go; the counter-asserting tests are in
// client_write_init_a3_counters_test.go (same tag). The functional tests below
// stay in the default build so the canonical
// `go test -tags=test_state_path_env ./internal/api/` gate still exercises the
// symlink threat model T1-T6 without the af1_counters tag.

// skipIfNoSymlink skips on Windows (elevation) and creates real -> link,
// returning the link path. The real file is seeded with `{}` 0600.
func a3SymlinkFixture(t *testing.T, dir string) (real, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform path is exercised on POSIX")
	}
	real = filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	link = filepath.Join(dir, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}
	return real, link
}

// T1 — symlink at config, NO env var, NO consent → write REFUSED with the
// default refusal error. This is the PR #209 default-posture guard: without
// any operator consent the secure-write pipeline refuses the pre-existing
// symlink and never follows it.
func TestA3_T1_SymlinkNoOptInNoConsent_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // no env opt-in
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	// nil consent: the production hook shape.
	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil)
	if err == nil {
		t.Fatalf("T1: symlink with no opt-in and no consent must be REFUSED")
	}
	// The default refusal is the pre-existing-symlink refusal from the
	// hardened pipeline (path-based lane, since follow was not consented).
	if got := err.Error(); !bytesContainsStr(got, "symlink") && !bytesContainsStr(got, "reparse") {
		t.Errorf("T1: expected a symlink/reparse refusal error, got %q", got)
	}
	// The real target must be untouched.
	if b, _ := os.ReadFile(real); string(b) != "{}" {
		t.Errorf("T1: real target mutated despite refusal: %q", b)
	}
}

// T2 — strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) + a scoped consent
// present → REFUSED. Strict overrides consent: corp-managed hosts get
// symlink-attack hardening regardless of any per-write consent. This pins
// the PROTECTED strict-override invariant for the NEW consent input.
func TestA3_T2_StrictModeOverridesScopedConsent_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "1") // strict
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	t.Cleanup(resetStrictModeIntentCacheForTest)
	dir := hardenedTempDir(t)
	real, link := a3SymlinkFixture(t, dir)

	// Consent pinned to the (correct) full resolved target — but strict must
	// still refuse before any follow.
	consent := &ResolvedSymlinkConsent{
		Client:             "claude-code",
		OriginalPath:       link,
		PinnedResolvedPath: filepath.Clean(real),
	}
	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), consent)
	if err == nil {
		t.Fatalf("T2: strict mode must REFUSE the symlink even with a matching scoped consent")
	}
	if b, _ := os.ReadFile(real); string(b) != "{}" {
		t.Errorf("T2: real target mutated despite strict refusal: %q", b)
	}
}

// T3 — scoped consent pinned to parent P; engineer a swap to P' BETWEEN
// confirm and write via the after-resolve injection hook → write REFUSED on
// pin mismatch (the TOCTOU guard). The window is engineered deterministically
// via afterResolveBeforePinHook (race-window-assertion discipline: no
// reliance on a natural timing window).
func TestA3_T3_ScopedConsentPinMismatchAfterSwap_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	dir := hardenedTempDir(t)

	// Two distinct target parents P and P'.
	parentP := filepath.Join(dir, "P")
	parentPrime := filepath.Join(dir, "Pprime")
	if err := os.Mkdir(parentP, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parentPrime, 0o700); err != nil {
		t.Fatal(err)
	}
	realP := filepath.Join(parentP, "real.json")
	realPrime := filepath.Join(parentPrime, "real.json")
	if err := os.WriteFile(realP, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPrime, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realP, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Operator consented to the full target realP (under parent P).
	consent := &ResolvedSymlinkConsent{
		Client:             "claude-code",
		OriginalPath:       link,
		PinnedResolvedPath: filepath.Clean(realP),
	}

	// Inject the swap: after the confirm-time resolve confirmed a symlink
	// but before the write-time re-resolve + pin-match, repoint link -> P'.
	swapped := false
	afterResolveBeforePinHook = func() {
		if swapped {
			return
		}
		swapped = true
		_ = os.Remove(link)
		if err := os.Symlink(realPrime, link); err != nil {
			t.Fatalf("T3: swap symlink to P': %v", err)
		}
	}
	t.Cleanup(func() { afterResolveBeforePinHook = nil })

	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), consent)
	if err == nil {
		t.Fatalf("T3: pin mismatch after swap must REFUSE the write")
	}
	if !bytesContainsStr(err.Error(), "does not match the operator-consented target") {
		t.Errorf("T3: expected the pin-mismatch refusal, got %q", err.Error())
	}
	// Neither target should have received the new bytes.
	if b, _ := os.ReadFile(realP); string(b) != "{}" {
		t.Errorf("T3: P target mutated: %q", b)
	}
	if b, _ := os.ReadFile(realPrime); string(b) != "{}" {
		t.Errorf("T3: P' target mutated (the swap target was written — TOCTOU NOT closed): %q", b)
	}
}

// T7 — F2 same-parent repoint: consent pinned to the FULL path of realA.json;
// the symlink is repointed to realB.json in the SAME parent P (different
// basename) before the write-time re-resolve. The write MUST be REFUSED on
// full-path mismatch and BOTH targets must be unmutated. This is the consent
// BYPASS the parent-only pin allowed: a parent-only comparison would have
// passed (same parent P), landing the privileged write on the unapproved
// realB.json. It must FAIL pre-fix (proving the bypass) and PASS post-fix.
func TestA3_T7_ScopedConsentSameParentRepoint_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")
	dir := hardenedTempDir(t)

	// realA and realB live in the SAME parent P.
	parentP := filepath.Join(dir, "P")
	if err := os.Mkdir(parentP, 0o700); err != nil {
		t.Fatal(err)
	}
	realA := filepath.Join(parentP, "realA.json")
	realB := filepath.Join(parentP, "realB.json")
	if err := os.WriteFile(realA, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realB, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(realA, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Operator consented to the FULL path of realA.json (parent P + realA.json).
	consent := &ResolvedSymlinkConsent{
		Client:             "claude-code",
		OriginalPath:       link,
		PinnedResolvedPath: filepath.Clean(realA),
	}

	// Inject the swap: repoint link -> realB.json in the SAME parent P after the
	// confirm-time resolve but before the write-time re-resolve + pin-match.
	swapped := false
	afterResolveBeforePinHook = func() {
		if swapped {
			return
		}
		swapped = true
		_ = os.Remove(link)
		if err := os.Symlink(realB, link); err != nil {
			t.Fatalf("T7: swap symlink to realB (same parent): %v", err)
		}
	}
	t.Cleanup(func() { afterResolveBeforePinHook = nil })

	err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), consent)
	if err == nil {
		t.Fatalf("T7: same-parent repoint to an UNAPPROVED file must REFUSE the write (full-path pin)")
	}
	if !bytesContainsStr(err.Error(), "does not match the operator-consented target") {
		t.Errorf("T7: expected the pin-mismatch refusal, got %q", err.Error())
	}
	// NEITHER realA (the approved file) NOR realB (the swap target) may be
	// written. Before the fix, realB.json received the bytes (the bypass).
	if b, _ := os.ReadFile(realA); string(b) != "{}" {
		t.Errorf("T7: approved target realA mutated: %q", b)
	}
	if b, _ := os.ReadFile(realB); string(b) != "{}" {
		t.Errorf("T7: same-parent swap target realB was written — consent BYPASS not closed: %q", b)
	}
}

// T4 — legit symlink to a regular file on ANOTHER directory tree (EXDEV
// proxy) → write SUCCEEDS on the resolved location, original symlink intact,
// audit event emitted. The temp file is created as a SIBLING of the resolved
// destination (inside the resolved target's parent), so even a real
// cross-volume target never hits EXDEV on the rename — this proves the
// "temp is sibling of resolved dest" property.
func TestA3_T4_SymlinkToRegularFile_SucceedsOnResolvedTree_AuditEmitted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1") // env opt-in (F2 lane)
	logDir := hubMcpStateTestHelper(t)

	// Resolved target lives in a SEPARATE hardened tree from the symlink.
	targetTree := hardenedTempDir(t)
	real := filepath.Join(targetTree, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTree := hardenedTempDir(t)
	link := filepath.Join(linkTree, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("T4: legit symlink-to-regular-file write must SUCCEED: %v", err)
	}

	// Target received the bytes.
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("T4: resolved target content = %q, want %q", b, `{"v":1}`)
	}
	// Original symlink still a symlink (not rewritten to a regular file).
	if lst, err := os.Lstat(link); err != nil {
		t.Fatalf("T4: lstat link: %v", err)
	} else if lst.Mode()&os.ModeSymlink == 0 {
		t.Errorf("T4: symlink was rewritten to a regular file; want symlink preserved")
	}
	// Audit event emitted (env opt-in lane).
	logBytes, rerr := os.ReadFile(filepath.Join(logDir, "hub-mcp.log"))
	if rerr != nil {
		t.Fatalf("T4: read hub-mcp.log: %v", rerr)
	}
	if !bytes.Contains(logBytes, []byte("client-write-symlink-resolved-via-optin")) {
		t.Errorf("T4: env-opt-in audit event missing from hub-mcp.log: %s", logBytes)
	}
	// The AF-1 handle-pinned-path (no string re-walk) assertion for this lane
	// lives in TestA3_T5_EnvVarLaneUsesHandlePinnedPath_NoStringReWalk under
	// -tags=af1_counters (client_write_init_a3_counters_test.go).
}

// T6 — broadened resolved-target parent, NON-strict → write succeeds via the
// skip-gate relax lane, the file is owner-only (0600 on POSIX), and the
// unhardened-fallback warn audit event fires. This proves the symlink lane's
// relax fallback installs the per-file boundary even when the resolved
// parent is broadened.
func TestA3_T6_BroadenedResolvedParentNonStrict_SucceedsOwnerOnly_WarnEmitted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; POSIX exercises the broadened-parent relax lane")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	logDir := hubMcpStateTestHelper(t)

	// Resolved target's parent is BROADENED (0755 — group/other bits set),
	// which trips the POSIX parent-dir gate → ErrSecureWriteParentInsecure →
	// relax retry. The symlink itself lives in a hardened tree.
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

	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("T6: non-strict broadened-parent symlink write must SUCCEED via relax lane: %v", err)
	}
	// File received the bytes and is owner-only (0600).
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("T6: resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if info, err := os.Stat(real); err != nil {
		t.Fatalf("T6: stat resolved target: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("T6: resolved file mode = %o, want 0600 (owner-only)", mode)
	}
	// Audit: both the symlink-resolve warn and the unhardened-fallback warn
	// must be present.
	logBytes, rerr := os.ReadFile(filepath.Join(logDir, "hub-mcp.log"))
	if rerr != nil {
		t.Fatalf("T6: read hub-mcp.log: %v", rerr)
	}
	if !bytes.Contains(logBytes, []byte("client-write-symlink-resolved-via-optin")) {
		t.Errorf("T6: symlink-resolve audit event missing: %s", logBytes)
	}
	if !bytes.Contains(logBytes, []byte("client-write-unhardened-fallback")) {
		t.Errorf("T6: unhardened-fallback warn event missing (relax lane did not fire): %s", logBytes)
	}
	// The AF-1 "even the relax retry stays on the handle-pinned path" counter
	// assertion lives under -tags=af1_counters
	// (client_write_init_a3_counters_test.go).
}

// TestA3_ScopedConsentPinMatch_Succeeds is the positive control for SEAM-B:
// a scoped consent whose PinnedResolvedPath matches the actual resolved FULL
// target (no swap) follows the symlink, writes through the handle-pinned
// path, and emits the distinct scoped-consent audit event.
func TestA3_ScopedConsentPinMatch_Succeeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "") // NO env opt-in — consent is the only follow input
	logDir := hubMcpStateTestHelper(t)

	targetTree := hardenedTempDir(t)
	real := filepath.Join(targetTree, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkTree := hardenedTempDir(t)
	link := filepath.Join(linkTree, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	consent := &ResolvedSymlinkConsent{
		Client:             "claude-code",
		OriginalPath:       link,
		PinnedResolvedPath: filepath.Clean(real), // FULL resolved target path
	}
	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), consent); err != nil {
		t.Fatalf("scoped-consent pin-match write must SUCCEED: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("scoped consent: resolved target content = %q, want %q", b, `{"v":1}`)
	}
	logBytes, rerr := os.ReadFile(filepath.Join(logDir, "hub-mcp.log"))
	if rerr != nil {
		t.Fatalf("read hub-mcp.log: %v", rerr)
	}
	if !bytes.Contains(logBytes, []byte("client-write-symlink-resolved-via-scoped-consent")) {
		t.Errorf("scoped-consent distinct audit event missing: %s", logBytes)
	}
	// The env-opt-in event must NOT fire (this followed via consent, not env).
	if bytes.Contains(logBytes, []byte("client-write-symlink-resolved-via-optin")) {
		t.Errorf("scoped-consent path wrongly emitted the env-opt-in audit event: %s", logBytes)
	}
	// The AF-1 "scoped-consent lane stays on the handle-pinned path" counter
	// assertion lives under -tags=af1_counters
	// (client_write_init_a3_counters_test.go).
}

// bytesContainsStr is a tiny substring helper kept local to avoid importing
// strings for a single Contains call across these table-free assertions.
func bytesContainsStr(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
