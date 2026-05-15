//go:build windows

// client_write_init_windows_test.go — Windows-only DACL assertion
// tests for secureWriteWithOperatorOpt's relax lane (PR #185 r3,
// codex deep-sec P1 closure on r2).
//
// PR #185 r2 codex deep-sec found that the previous relax lane
// (os.CreateTemp + path-based SetNamedSecurityInfo) left a race
// window: temp file created with parent-inherited DACL → bytes
// written before DACL hardened → co-resident principal could
// race-open the temp file before the path-based DACL setter ran,
// retain handle past the DACL tighten, and read tokens.
//
// PR #185 r3 routes the relax lane through the SAME handle-relative
// hardened pipeline as the strict path (parent-dir gate disabled
// only). The handle-based SetSecurityInfo at step 5 of the pipeline
// installs the restrictive DACL BEFORE any bytes hit disk, closing
// the race window.
//
// These tests pin the security boundary by synthesizing a parent
// dir with a non-allowlisted ACE (Authenticated Users:GenericRead),
// running the relax lane, then opening the resulting file and
// asserting its DACL has NO non-allowlisted principals.
//
// Codex deep-sec PR #185 r2 P1 test-gap closure:
//   "TestSecureWriteWithOperatorOpt_DefaultRelaxOnGateFailure on
//    Windows asserts no error + content only ... would still pass if
//    hardenTempFileForUnhardenedFallback became no-op and the file
//    inherited Authenticated Users or Everyone."

package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSecureWriteWithOperatorOpt_RelaxOnGateFailure_WindowsDACLHardened
// is the Windows-only security-boundary test for the relax lane.
//
// Setup: a parent dir with a synthesized Authenticated Users
// (S-1-5-11) GENERIC_READ ACE — this is the exact failure mode the
// parent-dir gate is designed to reject, and the relax lane is
// designed to write through anyway.
//
// Assertion: after secureWriteWithOperatorOpt runs (default-relax,
// no env vars set), the resulting file at the destination MUST have
// a restrictive DACL — only {current-user, LocalSystem,
// BuiltinAdministrators} get access. Authenticated Users and Everyone
// must NOT appear in the file's DACL.
//
// Why this matters: the previous r2 implementation passed this same
// test by relying on a path-based SetNamedSecurityInfo call AFTER
// the temp file was already created with parent-inherited DACL —
// the test ran in a single-threaded context so the race window
// didn't actually manifest, but the security boundary was nominally
// the same. The r3 fix moves the DACL apply to the file HANDLE at
// create time (via the hardened pipeline's step 5), which is what
// closes the race against a co-resident principal that watches the
// directory for new files. This test verifies the FINAL state of
// the file matches expectations; race-window observation tests
// would need real co-resident threads and are out of scope for the
// unit-test layer.
func TestSecureWriteWithOperatorOpt_RelaxOnGateFailure_WindowsDACLHardened(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "") // no legacy opt-in
	t.Setenv(RequireSingleUserHomeEnv, "")      // no strict opt-in

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	dst := filepath.Join(parent, "client-config.json")
	want := []byte(`{"token":"secret"}`)
	if err := secureWriteWithOperatorOpt(dst, want); err != nil {
		t.Fatalf("relax write under permissive parent: %v", err)
	}

	// Assert content first — write succeeded.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}

	// Open the file with READ_CONTROL to read its DACL.
	pathW, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile on written file: %v", err)
	}
	defer windows.CloseHandle(h)

	// Reuse the hardened pipeline's own verifier on the written
	// file. verifyWindowsDACLFromHandle enforces the same allowlist
	// the hardened path's post-rename re-verify uses: current user,
	// LocalSystem, BuiltinAdministrators. Anything else (Authenticated
	// Users, Everyone, Domain Users, CodexSandboxUsers, AppContainer
	// SIDs) makes it return an error naming the disallowed principal.
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Errorf("relax lane left file with non-allowlisted DACL: %v", err)
	}
}

// TestSecureWriteWithOperatorOpt_RelaxOnGateFailure_NoTempLeak
// verifies the relax lane does not leave a temp file behind under
// the parent dir after success. The handle-relative pipeline uses
// an unpredictable temp name + atomic rename across the held
// handle, so the only file in the parent should be the destination
// itself.
//
// Codex deep-sec PR #185 r2 P3 (orthogonal): regression coverage
// against a "rename moved part of the file, temp stays" scenario.
func TestSecureWriteWithOperatorOpt_RelaxOnGateFailure_NoTempLeak(t *testing.T) {
	t.Setenv(AllowUnhardenedClientWriteEnv, "")
	t.Setenv(RequireSingleUserHomeEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	dst := filepath.Join(parent, "client-config.json")
	if err := secureWriteWithOperatorOpt(dst, []byte(`{"v":1}`)); err != nil {
		t.Fatalf("relax write: %v", err)
	}

	// Enumerate parent. Only entry should be the destination basename.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir parent: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly 1 file in parent (the destination); got %d: %v", len(entries), names)
	}
	if len(entries) == 1 && entries[0].Name() != "client-config.json" {
		t.Errorf("unexpected file in parent: %s (want client-config.json)", entries[0].Name())
	}
}

// TestSecureWriteWithOperatorOpt_RelaxLaneEmitsWarnAuditLog pins
// codex deep-sec PR #185 r2 P3 closure: when the relax lane fires
// (gate failure + neither strict opt-in nor unset suppression), a
// structured event MUST be emitted to hub-mcp.log with:
//
//   - event = "client-write-unhardened-fallback"
//   - level = "warn" (not "info" — security-boundary downgrade is
//     dashboard-visible)
//   - reason = "default-relax-on-solo-host" or the legacy-opt-in
//     wording (this test exercises the default path)
//   - path = destination
//
// Without this assertion, a future refactor could silently change
// the level back to "info" (the original r2 implementation), and
// log-monitoring dashboards filtering "warn+" would miss the
// downgrade.
func TestSecureWriteWithOperatorOpt_RelaxLaneEmitsWarnAuditLog(t *testing.T) {
	// Redirect the state dir so RecentHubMcpEvents reads from a
	// clean per-test path (no leftover events from production
	// hub-mcp.log).
	statePathsHelper(t)
	stateDir := t.TempDir()
	daemonStateRootOverride = stateDir

	t.Setenv(AllowUnhardenedClientWriteEnv, "") // no legacy opt-in
	t.Setenv(RequireSingleUserHomeEnv, "")      // no strict opt-in

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	dst := filepath.Join(parent, "client-config.json")
	if err := secureWriteWithOperatorOpt(dst, []byte(`{"token":"x"}`)); err != nil {
		t.Fatalf("relax write: %v", err)
	}

	events, err := RecentHubMcpEvents(20)
	if err != nil {
		t.Fatalf("read recent events: %v", err)
	}
	var found map[string]any
	for _, ev := range events {
		if ev["event"] == "client-write-unhardened-fallback" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("no client-write-unhardened-fallback event in last %d entries (got %d events)", 20, len(events))
	}
	if found["level"] != "warn" {
		t.Errorf("event level = %v, want \"warn\" (security-boundary downgrade must be dashboard-visible, not info)", found["level"])
	}
	if reason, _ := found["reason"].(string); !strings.Contains(reason, "default-relax-on-solo-host") {
		t.Errorf("event reason = %v, want substring \"default-relax-on-solo-host\" (no legacy opt-in env var was set)", found["reason"])
	}
	if path, _ := found["path"].(string); path != dst {
		t.Errorf("event path = %v, want %q", found["path"], dst)
	}
}

// TestSecureWriteWithOperatorOpt_StrictBeatsLegacyAllow_DeterministicGateFailure
// pins codex deep-sec r3 P2: the existing StrictBeatsLegacyAllow
// test in client_write_init_test.go relies on the ambient
// `t.TempDir()` failing the parent-dir gate. If a hardened CI
// runner uses a 0700 / non-Authenticated-Users temp dir, the
// existing test silently t.Skip()s before the no-write assertion.
//
// This Windows-only variant synthesizes a known-rejected parent
// via synthesizeDirWithAuthUsersReadACE, so the strict-mode no-
// write assertion ALWAYS runs.
func TestSecureWriteWithOperatorOpt_StrictBeatsLegacyAllow_DeterministicGateFailure(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1")      // strict
	t.Setenv(AllowUnhardenedClientWriteEnv, "1") // legacy opt-in

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	dst := filepath.Join(parent, "client-config.json")
	err := secureWriteWithOperatorOpt(dst, []byte(`{"servers":{}}`))
	if err == nil {
		t.Fatalf("strict-mode must reject permissive synthesized parent; got nil")
	}
	if !strings.Contains(err.Error(), RequireSingleUserHomeEnv) {
		t.Errorf("error must mention %q (strict wins); got %v", RequireSingleUserHomeEnv, err)
	}
	// Codex deep-sec r3 P2 contract: strict path MUST NOT leak a
	// fallback write on its way out.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("strict-mode rejection leaked a write at %s (stat err = %v)", dst, statErr)
	}
}

// TestSecureWriteClientConfigSkipParentGate_WritesThroughRejectedParent
// pins codex deep-sec r3 P3: the skip-parent-gate impl is covered
// indirectly through secureWriteWithOperatorOpt only. A direct test
// against secureWriteClientConfigSkipParentGate verifies its own
// contract — writes through a parent that the strict path rejects,
// produces a file with the restrictive DACL.
//
// This locks the contract: someone refactoring
// secureWriteWithOperatorOpt cannot accidentally bypass the
// skip-gate writer without breaking THIS test.
func TestSecureWriteClientConfigSkipParentGate_WritesThroughRejectedParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	// Confirm the strict path rejects this parent (sanity check on
	// the fixture — if synthesize ever drifts to a gate-passing DACL
	// this assertion catches it).
	strictDst := filepath.Join(parent, "strict-target.json")
	if err := SecureWriteClientConfig(strictDst, []byte(`{}`)); err == nil {
		t.Fatalf("test fixture invalid: SecureWriteClientConfig accepted parent %s", parent)
	}

	// Skip-gate path writes through.
	dst := filepath.Join(parent, "client-config.json")
	want := []byte(`{"token":"secret"}`)
	if err := secureWriteClientConfigSkipParentGate(dst, want); err != nil {
		t.Fatalf("skip-gate write through synthesized parent: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("written contents = %q, want %q", got, want)
	}

	// Open the file and assert its DACL — same security boundary as
	// the strict pipeline produces.
	pathW, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatalf("UTF16PtrFromString: %v", err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile on written file: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Errorf("skip-gate write left file with non-allowlisted DACL: %v", err)
	}

	// Codex deep-sec r4 P3 closure: assert no temp-file leak in
	// parent directory. The skip-gate writer uses crypto-random
	// temp name + atomic rename across the held handle, so post-
	// success the only entry in the parent should be the
	// destination basename. A regression that drops the rename
	// or leaves the temp open would show up here.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir parent: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly 1 file in parent (destination); got %d: %v", len(entries), names)
	}
	if len(entries) == 1 && entries[0].Name() != "client-config.json" {
		t.Errorf("unexpected file in parent: %s (want client-config.json)", entries[0].Name())
	}
}

// synthesizeDirWithInheritableAuthUsersReadACE applies a PROTECTED
// DACL to dir that grants the current user GENERIC_ALL (no
// inheritance) AND Authenticated Users (S-1-5-11) GENERIC_READ WITH
// OBJECT_INHERIT_ACE — meaning the Auth Users ACE is propagated to
// FILES created inside dir under normal new-object DACL inheritance
// rules. Codex deep-sec r4 P2 closure fixture: this is the parent
// shape that probes the SE_DACL_PROTECTED contract on the new
// file's SECURITY_DESCRIPTOR. Without the protected flag, the new
// file would inherit Authenticated Users read; with it, the
// inheritance is blocked.
//
// Distinct from synthesizeDirWithAuthUsersReadACE (NO_INHERITANCE
// variant) which is used to probe the parent-dir gate failure path
// — the gate check looks at the parent's own DACL, not at what its
// ACEs say about children. The inheritable variant tests the
// SE_DACL_PROTECTED contract on the child file.
func synthesizeDirWithInheritableAuthUsersReadACE(t *testing.T, dir string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	authUsersSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("Authenticated Users sid: %v", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(currentSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_READ,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.OBJECT_INHERIT_ACE, // files created in dir inherit this
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(authUsersSID),
			},
		},
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("SetNamedSecurityInfo on parent dir: %v", err)
	}
}

// TestNtCreateRelative_CreateTimeSDOverridesParentInheritance pins
// the r4 codex deep-sec P2 regression guard: verify that
// OBJECT_ATTRIBUTES.SecurityDescriptor passed to ntCreateRelative
// ACTUALLY applies at create time, so the file is born with the
// restrictive DACL even when the parent has an inheritable
// non-allowlisted ACE.
//
// Without the r4 fix (SD parameter dropped, post-create
// SetSecurityInfo only), the file would briefly inherit
// Authenticated Users:GENERIC_READ from the parent's
// OBJECT_INHERIT_ACE before setRestrictiveDACL ran. The test
// verifies the DACL BEFORE any setRestrictiveDACL call — so a
// regression dropping the SD parameter would show up here as a
// non-allowlisted ACE on the temp handle.
//
// This is the strongest unit-test-level regression coverage for the
// race window closure short of an actual concurrent observer
// (out-of-scope per spec).
func TestNtCreateRelative_CreateTimeSDOverridesParentInheritance(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "inheriting-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithInheritableAuthUsersReadACE(t, parent)

	dirHandle, err := openDirHandleNoReparse(parent)
	if err != nil {
		t.Fatalf("openDirHandleNoReparse: %v", err)
	}
	defer windows.CloseHandle(dirHandle)

	sd, err := buildRestrictiveSecurityDescriptor()
	if err != nil {
		t.Fatalf("buildRestrictiveSecurityDescriptor: %v", err)
	}

	// Request READ_CONTROL so we can call GetSecurityInfo on the
	// returned handle for the assertion. The hardened pipeline
	// uses WRITE_DAC + GENERIC_WRITE; for this probe we add
	// READ_CONTROL because we read the SD back without writing.
	fileHandle, err := ntCreateRelative(
		dirHandle,
		".create-time-sd-probe.tmp",
		windows.DELETE|windows.GENERIC_WRITE|windows.SYNCHRONIZE|windows.WRITE_DAC|windows.READ_CONTROL,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		sd,
	)
	if err != nil {
		t.Fatalf("ntCreateRelative with create-time SD: %v", err)
	}
	defer windows.CloseHandle(fileHandle)
	// Mark the probe file for delete-on-close so it disappears
	// when fileHandle closes — no leftover artifact in the test
	// tmpdir.
	defer func() { _ = setFileDeleteOnClose(fileHandle) }()

	// CRITICAL ASSERTION: verify the file's DACL BEFORE any
	// setRestrictiveDACL call runs. If the r4 fix is ever
	// regressed (SD parameter dropped, falling back to post-create
	// hardening only), this assertion fails because the file
	// inherited Authenticated Users:GENERIC_READ from the parent.
	if err := verifyWindowsDACLFromHandle(fileHandle); err != nil {
		t.Errorf("ntCreateRelative with SD did NOT apply restrictive DACL at create time — file inherits parent ACEs before any post-create hardening: %v", err)
	}
}

// TestSecureWriteWithOperatorOpt_LegacyOptInEmitsWarnAuditLogWithDistinctReason
// pins that the legacy MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE=1
// branch emits the SAME warn event but with a "legacy opt-in"
// reason so operators can grep their shell profile post-upgrade
// and remove the now-redundant env var.
func TestSecureWriteWithOperatorOpt_LegacyOptInEmitsWarnAuditLogWithDistinctReason(t *testing.T) {
	statePathsHelper(t)
	stateDir := t.TempDir()
	daemonStateRootOverride = stateDir

	t.Setenv(AllowUnhardenedClientWriteEnv, "1") // legacy opt-in
	t.Setenv(RequireSingleUserHomeEnv, "")

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	dst := filepath.Join(parent, "client-config.json")
	if err := secureWriteWithOperatorOpt(dst, []byte(`{"token":"x"}`)); err != nil {
		t.Fatalf("relax write: %v", err)
	}

	events, err := RecentHubMcpEvents(20)
	if err != nil {
		t.Fatalf("read recent events: %v", err)
	}
	var found map[string]any
	for _, ev := range events {
		if ev["event"] == "client-write-unhardened-fallback" {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatalf("no client-write-unhardened-fallback event")
	}
	if found["level"] != "warn" {
		t.Errorf("event level = %v, want \"warn\"", found["level"])
	}
	reason, _ := found["reason"].(string)
	if !strings.Contains(reason, "legacy opt-in") {
		t.Errorf("event reason = %q, want substring \"legacy opt-in\" so operators can grep their shell profile", reason)
	}
	if !strings.Contains(reason, AllowUnhardenedClientWriteEnv) {
		t.Errorf("event reason = %q, want substring %q so operators see which env var to remove", reason, AllowUnhardenedClientWriteEnv)
	}
}
