//go:build windows

// hub_mcp_state_dacl_windows.go — Windows leg of VerifyHubMcpStateDACL.
//
// Sequence:
//
//  1. CreateFile(path, READ_CONTROL, FILE_FLAG_OPEN_REPARSE_POINT |
//     FILE_FLAG_BACKUP_SEMANTICS). REPARSE_POINT here means "open the
//     reparse point's metadata WITHOUT following it"; combined with
//     the FILE_ATTRIBUTE_REPARSE_POINT check below, any symlink /
//     junction at `path` is refused outright. BACKUP_SEMANTICS is
//     required so the open works on directories too.
//  2. GetSecurityInfo(handle, SE_FILE_OBJECT,
//     OWNER | DACL_SECURITY_INFORMATION) — fetches owner SID + DACL.
//  3. Owner SID == current user (else ErrWrongOwner).
//  4. Iterate DACL ACEs via GetAce. For each ALLOW ACE whose mask,
//     after MapGenericMask, contains FILE_GENERIC_READ or
//     GENERIC_READ, resolve the SID and check it against the allowlist
//     {current-user, S-1-5-18 (LocalSystem), S-1-5-32-544 (BuiltinAdministrators)}.
//     Any read-capable ALLOW outside the allowlist → ErrDaclOutsideAllowlist.
//  5. DENY ACEs are skipped — they cannot widen access.
//  6. Open the immediate parent dir via FILE_LIST_DIRECTORY +
//     FILE_FLAG_BACKUP_SEMANTICS + FILE_FLAG_OPEN_REPARSE_POINT and
//     apply the SAME allowlist check via verifyWindowsParentDACL.
//     Spec lines 277-281: a state file whose parent dir is broadened
//     (Group Policy, MDM) leaks the directory listing to every
//     domain user, even if the file's own DACL is tight. Mirror of
//     the POSIX leg's verifyPosixParentDirFromFd (secure_write_posix.go).
//
// The per-package helper verifyWindowsDACLFromHandle is reused by
// secure_write_windows.go's post-rename re-verify step.
//
// Spec: §"Windows DACL verification" (allowlist form, codex r3 F-S3
// closure) and §"Enterprise stance — Group Policy / MDM-managed ACLs"
// for operator-recovery guidance.

package api

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func verifyHubMcpStateDACLImpl(path string) error {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("utf16 %q: %w", path, err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)

	// Reject reparse points / symlinks via the file attributes.
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("file info %s: %w", path, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrIrregularFile
	}
	// Directory rejection (codex bot r2 P2 closure): FILE_FLAG_BACKUP_SEMANTICS
	// is required to open the parent dir's handle later, but it ALSO
	// permits the open of the path itself to succeed when path is a
	// directory. A directory substitution at a state-file path would
	// otherwise pass the DACL gate (an attacker who swapped
	// hub-mcp-tokens.json for a same-named directory would silently
	// satisfy the verifier). Reject FILE_ATTRIBUTE_DIRECTORY explicitly
	// — the contract is "verify a state FILE", not "verify a state
	// object of any type."
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return ErrIrregularFile
	}
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		return err
	}

	// Spec lines 277-281 require ALSO verifying the parent dir's DACL.
	// A state file whose parent is broadened externally (Group Policy,
	// MDM) leaks the directory listing to every domain user even when
	// the file's own DACL is tight. The POSIX leg performs the
	// equivalent check via verifyPosixParentDirFromFd in
	// secure_write_posix.go's open-time gate; this mirrors that on
	// the load-time read path.
	return verifyWindowsParentDACL(filepath.Dir(path))
}

// verifyWindowsParentDACL opens parentDir with FILE_LIST_DIRECTORY +
// FILE_FLAG_BACKUP_SEMANTICS + FILE_FLAG_OPEN_REPARSE_POINT and applies
// the same allowlist-based DACL check used for the file itself. Any
// error is wrapped with parent-dir context ("parent <dir> not
// single-user safe") so operators see the actionable cause (a GPO /
// MDM-pushed ACL on the parent, not on the file) rather than chase
// the file path.
//
// Mirror of POSIX `verifyPosixParentDirFromFd` (secure_write_posix.go).
// Spec lines 277-281 + 422-432.
func verifyWindowsParentDACL(parentDir string) error {
	pathW, err := windows.UTF16PtrFromString(parentDir)
	if err != nil {
		return fmt.Errorf("utf16 parent %q: %w", parentDir, err)
	}
	h, err := windows.CreateFile(
		pathW,
		// READ_CONTROL is required so GetSecurityInfo can read the
		// parent dir's owner + DACL via the resulting handle. Without
		// it, GetSecurityInfo returns ERROR_ACCESS_DENIED even when
		// the current user owns the directory.
		windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(h)
	if err := verifyWindowsDACLFromHandle(h); err != nil {
		return fmt.Errorf("parent %s not single-user safe: %w", parentDir, err)
	}
	return nil
}

// verifyWindowsDACLFromHandle reads the owner SID + DACL from `h` via
// GetSecurityInfo, then enforces the allowlist. Exported within the
// api package only — secure_write_windows.go uses it for the
// post-rename re-verify step.
func verifyWindowsDACLFromHandle(h windows.Handle) error {
	currentSID, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("current user sid: %w", err)
	}
	systemSID, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return fmt.Errorf("system sid: %w", err)
	}
	adminSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return fmt.Errorf("admin sid: %w", err)
	}
	allowlist := []*windows.SID{currentSID, systemSID, adminSID}

	sd, err := windows.GetSecurityInfo(
		h,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("get security info: %w", err)
	}

	ownerSID, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("get owner: %w", err)
	}
	if !ownerSID.Equals(currentSID) {
		return ErrWrongOwner
	}

	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("get dacl: %w", err)
	}
	// A NIL DACL means "all access" — fail closed.
	if dacl == nil {
		return fmt.Errorf("%w: nil DACL (implicit allow-all)", ErrDaclOutsideAllowlist)
	}

	// Iterate ACEs via GetAce. The ACL is laid out as a header
	// followed by AceCount ACE entries; AceCount is exposed via the
	// x/sys ACL accessor.
	count := windowsACLAceCount(dacl)
	readMask := uint32(windows.FILE_GENERIC_READ | windows.GENERIC_READ)
	for i := uint32(0); i < count; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("get ace %d: %w", i, err)
		}
		// ACCESS_ALLOWED_ACE_TYPE = 0, ACCESS_DENIED_ACE_TYPE = 1.
		// Skip DENY ACEs — they cannot widen access.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		// INHERIT_ONLY_ACE (0x08) flags an ACE that applies ONLY to
		// child objects created under this object, never to the object
		// itself (codex bot r3 P2 closure — earlier loop treated every
		// ALLOW read ACE as effective access, which would falsely
		// reject managed environments where a GPO pushes a child-only
		// inheritance rule onto the parent dir). Per the Microsoft
		// canonical-ACE evaluation algorithm, inherit-only ACEs do not
		// contribute to access decisions for the current object.
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		mapped := mapGenericReadRights(uint32(ace.Mask))
		if mapped&readMask == 0 {
			// ALLOW ACE that doesn't grant read access — irrelevant
			// to the confidentiality boundary we protect.
			continue
		}
		sid := sidFromAce(ace)
		if !sidInAllowlist(sid, allowlist) {
			name := sidString(sid)
			return fmt.Errorf("%w: SID %s grants read", ErrDaclOutsideAllowlist, name)
		}
	}
	return nil
}

// windowsACLAceCount reads the AceCount field from an ACL header.
// x/sys exposes the ACL pointer but not the layout; the field lives
// at offset 4 (after AclRevision uint8 + Sbz1 uint8 + AclSize uint16).
//
// Layout per Microsoft:
//
//	typedef struct _ACL {
//	  BYTE  AclRevision;
//	  BYTE  Sbz1;
//	  WORD  AclSize;
//	  WORD  AceCount;
//	  WORD  Sbz2;
//	} ACL;
func windowsACLAceCount(acl *windows.ACL) uint32 {
	type aclHeader struct {
		AclRevision uint8
		_           uint8
		AclSize     uint16
		AceCount    uint16
		_           uint16
	}
	hdr := (*aclHeader)(unsafe.Pointer(acl))
	return uint32(hdr.AceCount)
}

// mapGenericReadRights expands GENERIC_READ in `mask` to its
// specific-object equivalents per MapGenericMask semantics for files.
// We only care whether the resulting mask contains a read right, so
// we don't need to load advapi32 — the only generic right relevant
// to confidentiality is GENERIC_READ → FILE_GENERIC_READ.
func mapGenericReadRights(mask uint32) uint32 {
	if mask&windows.GENERIC_READ != 0 {
		mask |= uint32(windows.FILE_GENERIC_READ)
	}
	if mask&windows.GENERIC_ALL != 0 {
		mask |= uint32(windows.FILE_GENERIC_READ)
	}
	return mask
}

// sidFromAce extracts the SID from an ACCESS_ALLOWED_ACE. The SID
// starts at the SidStart offset within the ACE; we compute the SID
// pointer by adding the offset of SidStart to the ACE base.
func sidFromAce(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	// The SID immediately follows the fixed ACE header. SidStart is
	// the first byte of the SID — its address is the SID pointer.
	return (*windows.SID)(unsafe.Pointer(&ace.SidStart))
}

// sidInAllowlist returns true if sid equals any of allowlist's SIDs.
func sidInAllowlist(sid *windows.SID, allowlist []*windows.SID) bool {
	if sid == nil {
		return false
	}
	for _, a := range allowlist {
		if sid.Equals(a) {
			return true
		}
	}
	return false
}

// currentUserSID returns the SID of the current process token's user.
// Cached for free since GetCurrentProcessToken is a pseudo-handle on
// Windows. Used by verifyWindowsDACLFromHandle and by
// setRestrictiveDACL (in secure_write_windows.go).
func currentUserSID() (*windows.SID, error) {
	t := windows.GetCurrentProcessToken()
	u, err := t.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("token user: %w", err)
	}
	return u.User.Sid.Copy()
}

// sidString returns the textual form of sid (S-1-5-... form). Used
// in diagnostic error messages. Returns "<unresolved-sid>" if the
// conversion fails (x/sys SID.String returns "" on failure).
func sidString(sid *windows.SID) string {
	if sid == nil {
		return "<nil>"
	}
	s := sid.String()
	if s == "" {
		return "<unresolved-sid>"
	}
	return s
}
