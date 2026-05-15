//go:build windows

// hub_mcp_state_dacl_windows_test.go — Windows-specific DACL synthesis
// test (Task 1.5 step 7).
//
// Builds a DACL that includes a read-capable ALLOW ACE for the
// Authenticated Users SID (S-1-5-11), applies it to a fresh file via
// SetNamedSecurityInfo, and asserts that VerifyHubMcpStateDACL rejects
// the file with ErrDaclOutsideAllowlist.

package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestVerifyHubMcpStateDACLRejectsAuthenticatedUsersAllow builds a
// DACL granting GENERIC_READ to S-1-5-11 (Authenticated Users) on a
// freshly-created file, then asserts the verifier rejects it with
// ErrDaclOutsideAllowlist. This is the canonical enterprise-stance
// test: Group-Policy ACLs that grant read to Domain Users / Auth
// Users / corporate management SIDs must fail closed.
//
// Uses hardenedTempDir so the parent-dir DACL gate accepts the
// parent; the only failure signal under test is the FILE's own DACL.
func TestVerifyHubMcpStateDACLRejectsAuthenticatedUsersAllow(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	// Build a DACL: ALLOW current-user GENERIC_ALL, ALLOW Authenticated
	// Users GENERIC_READ. The Authenticated Users ACE is the
	// disallowed read-capable ACE — verifier MUST reject.
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
			Inheritance:       windows.NO_INHERITANCE,
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
		target,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("SetNamedSecurityInfo: %v", err)
	}

	err = VerifyHubMcpStateDACL(target)
	if err == nil {
		t.Fatalf("VerifyHubMcpStateDACL must reject Authenticated Users read ALLOW; got nil")
	}
	if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Errorf("expected ErrDaclOutsideAllowlist, got %v", err)
	}
}

// TestVerifyHubMcpStateDACLAcceptsAllowlistOnly synthesizes the
// happy-path DACL (current-user + LocalSystem + BuiltinAdministrators
// GENERIC_ALL) and asserts the verifier accepts. Symmetric coverage
// for the synthesis suite — without this case we can't tell the
// reject test passed for the right reason.
//
// Uses hardenedTempDir so both the parent-dir AND the file-DACL gates
// pass; if either failed for a different reason, this test would
// surface that ambiguity.
func TestVerifyHubMcpStateDACLAcceptsAllowlistOnly(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "tight.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	systemSID, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatalf("system sid: %v", err)
	}
	adminSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("admin sid: %v", err)
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
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSID),
			},
		},
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("SetNamedSecurityInfo: %v", err)
	}

	if err := VerifyHubMcpStateDACL(target); err != nil {
		t.Errorf("VerifyHubMcpStateDACL must accept allowlist-only DACL; got %v", err)
	}
}

// applyAllowlistOnlyDACL applies an allowlist-conforming PROTECTED
// DACL to target via SetNamedSecurityInfo. Used by the parent-DACL
// reject test below to ensure the FILE's own DACL is conforming, so
// the only signal under test is the parent-dir DACL gate.
func applyAllowlistOnlyDACL(t *testing.T, target string) {
	t.Helper()
	currentSID, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	systemSID, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatalf("system sid: %v", err)
	}
	adminSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatalf("admin sid: %v", err)
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
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(adminSID),
			},
		},
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("ACLFromEntries: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatalf("SetNamedSecurityInfo on file: %v", err)
	}
}

// TestVerifyHubMcpStateDACLRejectsPermissiveParentDACL pins the
// STRICT-mode behavior (MCPHUB_REQUIRE_SINGLE_USER_HOME=1): a
// parent dir with an Authenticated Users:GenericRead ACE is
// rejected even when the file's own DACL is allowlist-conforming.
//
// v0.4.2 inverts the default — see
// TestVerifyHubMcpStateDACLAcceptsPermissiveParentDACL_DefaultRelax
// below. Strict mode preserves the v0.4.0-v0.4.1 refuse-on-broadened-
// parent posture for corp-managed / multi-tenant hosts.
func TestVerifyHubMcpStateDACLRejectsPermissiveParentDACL(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "1") // STRICT mode

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	target := filepath.Join(parent, "hub-mcp-tokens.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	err := VerifyHubMcpStateDACL(target)
	if err == nil {
		t.Fatalf("VerifyHubMcpStateDACL must reject permissive parent dir under strict mode; got nil")
	}
	if !errors.Is(err, ErrDaclOutsideAllowlist) {
		t.Errorf("expected ErrDaclOutsideAllowlist (wrapped), got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, parent) {
		t.Errorf("error %q must mention parent dir %q", msg, parent)
	}
	if !strings.Contains(msg, "parent") {
		t.Errorf("error %q must use the word 'parent' to signal the dir-DACL gate", msg)
	}
}

// TestVerifyHubMcpStateDACLAcceptsPermissiveParentDACL_DefaultRelax
// covers v0.4.2's new default: when MCPHUB_REQUIRE_SINGLE_USER_HOME
// is NOT set, a permissive parent dir is tolerated. The file's own
// DACL is the load-bearing safety layer (verified post-parent
// inside verifyHubMcpStateDACLImpl); the parent handle still binds
// the file open via ntOpenRelative so TOCTOU safety is preserved.
//
// Manual-smoke motivation: workstation %LOCALAPPDATA%\mcp-local-hub
// broadened to a third-party installer SID. Without this relax, B4
// marker reads (PR #187) and other state-file operations failed
// closed, breaking every matrix Apply on the GUI.
func TestVerifyHubMcpStateDACLAcceptsPermissiveParentDACL_DefaultRelax(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "") // DEFAULT mode

	parent := filepath.Join(t.TempDir(), "leaky-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithAuthUsersReadACE(t, parent)

	target := filepath.Join(parent, "hub-mcp-tokens.json")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	applyAllowlistOnlyDACL(t, target)

	if err := VerifyHubMcpStateDACL(target); err != nil {
		t.Errorf("default-relax: expected nil (file DACL is allowlist-clean); got %v", err)
	}
}

// TestOwnerSIDAllowed pins the owner allowlist contract: the bug-bash
// A1 fix relaxed the strict owner==current-user check so default
// Windows home directories (C:\Users\<user> owned by SYSTEM with the
// user as DACL grantee) pass. The allowlist must accept exactly
// current-user, SYSTEM, and BuiltinAdministrators — anything else
// (Authenticated Users, Domain Users, a random Group Policy SID) is
// rejected. The DACL gate (covered by the existing
// TestVerifyHubMcpStateDACLRejectsAuthenticatedUsersAllow test) is the
// confidentiality boundary; owner is the integrity boundary, and a
// pure unit test is cheaper to maintain than provisioning admin in CI.
func TestOwnerSIDAllowed(t *testing.T) {
	current, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatal(err)
	}
	authUsers, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatal(err)
	}
	allowlist := []*windows.SID{current, system, admins}

	cases := []struct {
		name  string
		owner *windows.SID
		want  bool
	}{
		{"current user owns", current, true},
		{"SYSTEM owns (default Windows home dir)", system, true},
		{"BuiltinAdministrators owns", admins, true},
		{"Authenticated Users owns (rejected — corp-policy)", authUsers, false},
		{"nil owner", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ownerSIDAllowed(tc.owner, allowlist)
			if got != tc.want {
				t.Errorf("ownerSIDAllowed(%v) = %v, want %v", sidString(tc.owner), got, tc.want)
			}
		})
	}
}

// TestVerifyHubMcpStateDACLRejectsDirectoryTarget asserts that the
// verifier refuses a directory at the state-file path — a defense
// against attacker-controlled directory substitutions on a path that
// should hold a regular file. FILE_FLAG_BACKUP_SEMANTICS in the
// CreateFile call (required to also open parent dirs) would otherwise
// let the directory pass through to the DACL gate.
//
// codex bot r2 P2 closure.
func TestVerifyHubMcpStateDACLRejectsDirectoryTarget(t *testing.T) {
	dir := hardenedTempDir(t)
	// Create a directory at the state-file path. Production callers
	// expect this path to be a regular file.
	target := filepath.Join(dir, "hub-mcp-tokens.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target as dir: %v", err)
	}
	err := VerifyHubMcpStateDACL(target)
	if err == nil {
		t.Fatalf("VerifyHubMcpStateDACL must reject directory target; got nil")
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Errorf("expected ErrIrregularFile for directory; got %v", err)
	}
}
