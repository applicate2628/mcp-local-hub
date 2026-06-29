//go:build windows

// workspace_registry_write_hardening_windows_test.go — regression guard
// that LOCKS the serena workspace-registry write-hardening invariant
// (verified already present on HEAD: Save() publishes owner-only, symmetric
// with the hardened read). It fails if a future change reintroduces the
// non-hardened os.WriteFile+rename that would make a broadened parent
// produce an unreadable workspaces.yaml. Real-world cold-read symptom +
// remediations: work-items/bugs/2026-06-29-stale-pre-hardening-workspaces-yaml-unreadable.md.
//
// THE BUG CLASS:
//
//   The serena workspace registry (workspaces.yaml / DefaultRegistryPath)
//   READ goes through the hardened inode-anchored reader, whose file-DACL
//   gate REFUSES a file whose own DACL grants WRITE/DAC/DELETE/Modify to a
//   non-allowlisted SID — in EVERY mode, including default-relax
//   (readStateFileInodeAnchoredWithOptions, file-DACL gate). On a host whose
//   %LOCALAPPDATA%\mcp-local-hub parent is broadened (e.g. codex's sandbox
//   grants Wave\CodexSandboxUsers Modify with OBJECT_INHERIT_ACE), a registry
//   WRITE that merely inherited the parent's DACL would produce a
//   workspaces.yaml whose own DACL carries that Modify ACE. The hardened READ
//   then refuses it → `read registry ...\workspaces.yaml: {Access Denied}` →
//   the serena router reports `-32001: no serena workspace registered` and
//   tools/list fails (flagship serena partially down).
//
//   The fix invariant: the registry WRITE must produce an owner-only
//   workspaces.yaml (PROTECTED DACL installed on the handle at create time,
//   stripping inherited parent ACEs) so the file is readable by the hardened
//   read regardless of how broad the parent's DACL is — symmetric with the
//   read.
//
// This test engineers exactly the observed parent shape: an inheritable
// (OBJECT_INHERIT_ACE) Authenticated-Users (S-1-5-11) Modify-class ACE on the
// registry parent dir. Under default new-object DACL inheritance, a
// non-hardened child write (plain os.WriteFile / os.CreateTemp + rename, no
// PROTECTED DACL) would inherit that ACE. The test drives the PRODUCTION
// Registry.Save() path and asserts:
//
//   1. the published workspaces.yaml's OWN DACL passes
//      verifyWindowsDACLFromHandle (owner-only: {current user, LocalSystem,
//      BuiltinAdministrators} — no Authenticated Users / CodexSandboxUsers /
//      orphan SID), AND
//   2. the hardened READ (readStateFileInodeAnchored) succeeds on the written
//      file (the symptom surface — this is what `mcphub workspace list` /
//      register / the serena router call).
//
// A regression that reverts Registry.Save() to a non-hardened write (plain
// os.WriteFile(0600)+os.Rename, which inherits the parent DACL) FAILS both
// assertions: the file would carry the inherited Modify ACE and the hardened
// read would refuse it with the WRITE/DAC/DELETE branch.

package api

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// synthesizeDirWithInheritableAuthUsersModifyACE applies a PROTECTED DACL to
// dir granting the current user GENERIC_ALL (no inheritance) AND
// Authenticated Users (S-1-5-11) a Modify-class mask
// (GENERIC_READ|GENERIC_WRITE) WITH OBJECT_INHERIT_ACE — files created inside
// dir under normal new-object inheritance would inherit the Auth Users Modify
// ACE. This is the WRITE/Modify analogue of the existing inheritable-READ
// fixture (synthesizeDirWithInheritableAuthUsersReadACE); the observed
// codex-sandbox breakage granted CodexSandboxUsers *Modify*, not read-only, so
// the regression fixture must carry a WRITE-capable inherited ACE to exercise
// the read-side WRITE/DAC/DELETE refusal branch.
func synthesizeDirWithInheritableAuthUsersModifyACE(t *testing.T, dir string) {
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
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		{
			// Intentionally open-coded (not explicitAccessAllow, which
			// hardcodes NO_INHERITANCE): OBJECT_INHERIT_ACE is the
			// contract under test — a non-hardened child write would
			// inherit this Modify ACE. GENERIC_READ|GENERIC_WRITE is the
			// Modify-class mask that trips the read-side
			// verifyWindowsDACLFromHandleWriteOrAdmin refusal.
			AccessPermissions: windows.GENERIC_READ | windows.GENERIC_WRITE,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.OBJECT_INHERIT_ACE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(authUsersSID),
			},
		},
	}
	applyProtectedDACLFromEntries(t, dir, entries)
}

// TestRegistrySave_BroadenedParent_PublishesOwnerOnlyAndReadable is the
// regression guard for the write/read asymmetry. See file header.
func TestRegistrySave_BroadenedParent_PublishesOwnerOnlyAndReadable(t *testing.T) {
	// The relax lane emits a state-file-write-unhardened-fallback audit row
	// through DaemonStateDir(); isolate the state dir so the row never touches
	// the operator's real supervisor-events.log / hub-mcp.log. (The broadened
	// registry parent below is a SEPARATE temp dir, unaffected by this.)
	isolateStateDir(t)
	t.Setenv(RequireSingleUserHomeEnv, "") // default-relax, not strict

	// Registry parent dir with an INHERITABLE Modify ACE for a non-allowlisted
	// SID — exactly the codex-sandbox %LOCALAPPDATA% broadening shape.
	parent := filepath.Join(t.TempDir(), "broadened-state-dir")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	synthesizeDirWithInheritableAuthUsersModifyACE(t, parent)

	regPath := filepath.Join(parent, "workspaces.yaml")
	reg := NewRegistry(regPath)
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "k1",
		WorkspacePath: `C:\proj`,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9300,
		TaskName:      `\mcp-local-hub-serena-k1`,
	}); err != nil {
		t.Fatalf("PutSerena: %v", err)
	}

	// PRODUCTION write path.
	if err := reg.Save(); err != nil {
		t.Fatalf("Registry.Save: %v", err)
	}

	// Assertion 1 — the published workspaces.yaml's OWN DACL is owner-only.
	// A non-hardened write would inherit the parent's Modify ACE and this
	// fails naming the disallowed principal.
	pathW, err := windows.UTF16PtrFromString(regPath)
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
		t.Fatalf("CreateFile on written registry: %v", err)
	}
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		t.Errorf("written workspaces.yaml has a non-allowlisted DACL (write did not produce an owner-only file symmetric with the hardened read): %v", err)
	}

	// Assertion 2 — the hardened READ succeeds. This is the actual symptom
	// surface: `mcphub workspace list`, register's ListByWorkspace, and the
	// serena router all read through this reader, and a Modify-broadened file
	// makes it refuse with the WRITE/DAC/DELETE branch → -32001.
	data, err := readStateFileInodeAnchored(regPath)
	if err != nil {
		t.Fatalf("hardened read of written registry failed (this is the -32001 serena breakage): %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("hardened read returned empty content for a non-empty registry")
	}

	// Sanity: the round-trip parses and contains the serena row.
	reg2 := NewRegistry(regPath)
	if err := reg2.Load(); err != nil {
		t.Fatalf("Load of written registry: %v", err)
	}
	if _, ok := reg2.GetSerena("k1"); !ok {
		t.Fatalf("written registry missing the serena row after round-trip")
	}
}
