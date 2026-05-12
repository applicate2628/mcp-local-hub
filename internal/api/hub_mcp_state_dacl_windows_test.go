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
	"testing"

	"golang.org/x/sys/windows"
)

// TestVerifyHubMcpStateDACLRejectsAuthenticatedUsersAllow builds a
// DACL granting GENERIC_READ to S-1-5-11 (Authenticated Users) on a
// freshly-created file, then asserts the verifier rejects it with
// ErrDaclOutsideAllowlist. This is the canonical enterprise-stance
// test: Group-Policy ACLs that grant read to Domain Users / Auth
// Users / corporate management SIDs must fail closed.
func TestVerifyHubMcpStateDACLRejectsAuthenticatedUsersAllow(t *testing.T) {
	dir := t.TempDir()
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
func TestVerifyHubMcpStateDACLAcceptsAllowlistOnly(t *testing.T) {
	dir := t.TempDir()
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
