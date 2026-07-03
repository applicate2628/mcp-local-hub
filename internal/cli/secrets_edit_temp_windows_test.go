//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSecureCreateSecretsEditTemp_OwnerOnlyUnderBroadenedParent proves
// the `mcphub secrets edit` decrypted-vault temp is created owner-only
// even when its parent dir grants Authenticated Users — the exact
// sandbox-broadened %LOCALAPPDATA% scenario the vault read-hardening
// exists for. Negative control on the SAME host: a plain os.CreateTemp
// file in the SAME broadened dir INHERITS the Authenticated Users ACE
// (the cleartext exposure the old implementation had). The test asserts
// the hardened path's file does NOT grant Authenticated Users while the
// os.CreateTemp control does — so a regression back to os.CreateTemp
// would fail here.
func TestSecureCreateSecretsEditTemp_OwnerOnlyUnderBroadenedParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "broadened")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	broadenDirAuthenticatedUsers(t, dir)

	// Negative control / precondition: os.CreateTemp (the OLD, vulnerable
	// path) yields a file that inherits the Authenticated Users ACE. If
	// this does not hold, the environment is not reproducing the vuln and
	// the positive assertion below would be vacuous — fail loud.
	negTmp, err := os.CreateTemp(dir, "mcp-secrets-*.yaml")
	if err != nil {
		t.Fatalf("os.CreateTemp negative control: %v", err)
	}
	negPath := negTmp.Name()
	_ = negTmp.Close()
	negSDDL := fileDaclSDDL(t, negPath)
	if !sddlGrantsAuthenticatedUsers(negSDDL) {
		t.Fatalf("negative-control precondition failed: os.CreateTemp file under a broadened dir did not inherit Authenticated Users; SDDL=%s", negSDDL)
	}

	// Production path: the hardened owner-only create.
	payload := []byte("API_KEY: sk-live-SENSITIVE\nTOKEN: ghp_verysecret\n")
	got, err := secureCreateSecretsEditTemp(dir, payload)
	if err != nil {
		t.Fatalf("secureCreateSecretsEditTemp: %v", err)
	}
	gotSDDL := fileDaclSDDL(t, got)
	if sddlGrantsAuthenticatedUsers(gotSDDL) {
		t.Fatalf("edit temp %s is NOT owner-only: its DACL grants Authenticated Users; SDDL=%s", got, gotSDDL)
	}

	onDisk, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read edit temp: %v", err)
	}
	if string(onDisk) != string(payload) {
		t.Fatalf("edit temp content = %q, want the decrypted vault %q", onDisk, payload)
	}
}

// fileDaclSDDL returns the SDDL string of a file's DACL.
func fileDaclSDDL(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo %s: %v", path, err)
	}
	return sd.String()
}

// sddlGrantsAuthenticatedUsers reports whether an SDDL DACL string
// contains an ACE whose trustee is Authenticated Users (alias "AU" or
// SID S-1-5-11). The account SID is the final field of an SDDL ACE, so
// it sits immediately before the closing ')'.
func sddlGrantsAuthenticatedUsers(sddl string) bool {
	return strings.Contains(sddl, ";AU)") || strings.Contains(sddl, ";S-1-5-11)")
}

// broadenDirAuthenticatedUsers applies a PROTECTED DACL to dir granting
// the current user GENERIC_ALL plus Authenticated Users GENERIC_ALL with
// object+container inheritance, so newly created (non-PROTECTED) child
// files inherit the Authenticated Users ACE — the sandbox-broadened
// parent the fix must survive.
func broadenDirAuthenticatedUsers(t *testing.T, dir string) {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	currentSID, err := u.User.Sid.Copy()
	if err != nil {
		t.Fatalf("copy current sid: %v", err)
	}
	auSID, err := windows.StringToSid("S-1-5-11")
	if err != nil {
		t.Fatalf("AU sid: %v", err)
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
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(auSID),
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
		nil, nil, dacl, nil,
	); err != nil {
		t.Fatalf("SetNamedSecurityInfo broaden %s: %v", dir, err)
	}
}
