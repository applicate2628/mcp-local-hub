//go:build windows

package api

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// applyProtectedDACLFromEntries builds a DACL from the given
// EXPLICIT_ACCESS entries and applies it to `target` as a PROTECTED
// DACL (DACL_SECURITY_INFORMATION | PROTECTED_DACL_SECURITY_INFORMATION)
// via SetNamedSecurityInfo. It is the shared test-side boilerplate for
// the ~6 Windows DACL fixtures that each used to open-code the
// ACLFromEntries + SetNamedSecurityInfo pair.
//
// It is deliberately entries-agnostic: the caller supplies whichever
// principal/mask/inheritance set the test needs (the allowlist triple
// via allowlistExplicitAccess, or a divergent Authenticated-Users
// fixture). PROTECTED is always applied because every call site needs
// to strip %TEMP%-inherited Authenticated Users ACEs so the only DACL
// under test is the one synthesized here.
func applyProtectedDACLFromEntries(t *testing.T, target string, entries []windows.EXPLICIT_ACCESS) {
	t.Helper()
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
		t.Fatalf("SetNamedSecurityInfo on %s: %v", target, err)
	}
}

func TestBuildAllowlistSDDL_File(t *testing.T) {
	sddl, err := BuildAllowlistSDDL(AllowlistMaskFile)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(sddl, "D:P") {
		t.Fatalf("expected protected DACL prefix D:P, got %q", sddl)
	}
	if !strings.Contains(sddl, ";GA;") {
		t.Fatalf("expected GENERIC_ALL (GA) mask for file form, got %q", sddl)
	}
	if !strings.Contains(sddl, "BA") {
		t.Fatalf("file SDDL should retain BuiltinAdministrators (BA), got %q", sddl)
	}
	if !strings.Contains(sddl, ";;SY") {
		t.Fatalf("file SDDL must retain LocalSystem (SY), got %q", sddl)
	}
}

func TestBuildAllowlistSDDL_Pipe(t *testing.T) {
	sddl, err := BuildAllowlistSDDL(AllowlistMaskPipe)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(sddl, ";GRGW;") {
		t.Fatalf("expected GENERIC_READ|GENERIC_WRITE (GRGW) mask for pipe form, got %q", sddl)
	}
	if strings.Contains(sddl, "BA") {
		t.Fatalf("pipe SDDL must DROP BuiltinAdministrators (defense-in-depth), got %q", sddl)
	}
	if !strings.Contains(sddl, ";;SY") {
		t.Fatalf("pipe SDDL must retain LocalSystem (SY), got %q", sddl)
	}
}

func TestBuildAllowlistSDDL_UnknownMode(t *testing.T) {
	_, err := BuildAllowlistSDDL(AllowlistMaskMode(99))
	if err == nil {
		t.Fatalf("expected error for unknown mode, got nil")
	}
}

// TestBuildAllowlistSD_Parses verifies BuildAllowlistSD returns a
// non-nil SD that round-trips back to an SDDL string. This is a
// structural smoke test; the on-disk ACE behavior is exercised by
// SecureWriteClientConfig tests after the secure_write_windows.go
// refactor.
func TestBuildAllowlistSD_Parses(t *testing.T) {
	sd, err := BuildAllowlistSD()
	if err != nil {
		t.Fatalf("build SD: %v", err)
	}
	if sd == nil {
		t.Fatalf("expected non-nil SD")
	}
	// Round-trip back to SDDL with the same flags BuildAllowlistSDDL
	// emits so we can compare textual form. DACL + control bits.
	// SECURITY_DESCRIPTOR.String() returns the SDDL form (x/sys
	// windows pkg; returns "" on conversion failure).
	roundTrip := sd.String()
	if roundTrip == "" {
		t.Fatalf("SD String() returned empty")
	}
	// SecurityDescriptorFromString preserves all DACL ACEs; assert
	// the load-bearing tokens survive the round trip.
	if !strings.HasPrefix(roundTrip, "D:P") {
		t.Fatalf("round-tripped SD missing D:P prefix: %q", roundTrip)
	}
	if !strings.Contains(roundTrip, ";GA;") {
		t.Fatalf("round-tripped SD missing GA mask: %q", roundTrip)
	}
	if !strings.Contains(roundTrip, "BA") {
		t.Fatalf("round-tripped SD missing BA: %q", roundTrip)
	}
}

// TestBuildAllowlistSDDL_File_SubstitutesUserSID asserts the current
// process token's SID appears in the textual SDDL. This guards against
// a stray SDDL-alias regression (e.g. accidentally emitting "OW" or a
// hardcoded literal in place of the current user).
func TestBuildAllowlistSDDL_File_SubstitutesUserSID(t *testing.T) {
	sid, err := currentUserSID()
	if err != nil {
		t.Fatalf("currentUserSID: %v", err)
	}
	want := sid.String()
	if want == "" {
		t.Skip("current user SID has empty textual form on this host")
	}
	sddl, err := BuildAllowlistSDDL(AllowlistMaskFile)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(sddl, want) {
		t.Fatalf("file SDDL missing current user SID %q: %q", want, sddl)
	}
}

// Smoke: ensure SecurityDescriptorFromString accepts the pipe form too
// even though the file form is the only one wrapped via BuildAllowlistSD
// today. go-winio expects the string form for its SecurityDescriptor
// field; this verifies the SDDL is syntactically valid.
func TestBuildAllowlistSDDL_Pipe_ParsesAsSD(t *testing.T) {
	sddl, err := BuildAllowlistSDDL(AllowlistMaskPipe)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("pipe SDDL did not parse: %v (sddl=%q)", err, sddl)
	}
	if sd == nil {
		t.Fatalf("expected non-nil SD from pipe SDDL")
	}
}
