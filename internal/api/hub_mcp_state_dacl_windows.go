//go:build windows

// hub_mcp_state_dacl_windows.go — shared Windows DACL primitives for
// secure writes and inode-anchored state reads.
//
// The per-package helper verifyWindowsDACLFromHandle is reused by
// secure_write_windows.go's post-rename re-verify step and by
// hub_mcp_state_read_inode_windows.go's handle-bound read gate.
//
// Spec: §"Windows DACL verification" (allowlist form, codex r3 F-S3
// closure) and §"Enterprise stance — Group Policy / MDM-managed ACLs"
// for operator-recovery guidance.

package api

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ACE type constants not exported by golang.org/x/sys/windows but
// defined in ntsecapi.h. Used by the canonical-ACE evaluation loop
// in verifyWindowsDACLFromHandle (codex bot r7 P1 closure).
const (
	aceTypeAuditBasic            uint8 = 0x02
	aceTypeAlarmBasic            uint8 = 0x03
	aceTypeAllowedObject         uint8 = 0x05
	aceTypeDeniedObject          uint8 = 0x06
	aceTypeAuditObject           uint8 = 0x07
	aceTypeAlarmObject           uint8 = 0x08
	aceTypeAllowedCallback       uint8 = 0x09
	aceTypeDeniedCallback        uint8 = 0x0A
	aceTypeAllowedCallbackObject uint8 = 0x0B
	aceTypeDeniedCallbackObj     uint8 = 0x0C
	aceTypeMandatoryLabel        uint8 = 0x11
)

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

// windowsDACLSignificantBits is the full set the strict allowlist
// check refuses on a non-allowlisted SID. Includes BOTH read
// (confidentiality) and write/admin (integrity) bits, plus the
// directory-only FILE_DELETE_CHILD tamper right (codex bot r5 P1
// follow-through: include it in strict too so a future caller
// using verifyWindowsDACLFromHandle on a directory still refuses
// child-delete grants).
var windowsDACLSignificantBits = uint32(
	windows.FILE_READ_DATA|windows.FILE_READ_EA|
		windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|
		windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES|
		windows.FILE_EXECUTE|
		windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER,
) | windowsFileDeleteChild

// windowsFileDeleteChild is the directory-specific access right
// FILE_DELETE_CHILD (0x40 per Microsoft AccessMask documentation,
// "ACE Mask Values" table in ntifs.h). Not exported by
// golang.org/x/sys/windows so we declare it locally. A principal
// holding this right on a DIRECTORY can delete child entries
// regardless of the children's own DACLs — making it a TOCTOU
// swap vector when present on the state-dir's PARENT.
const windowsFileDeleteChild uint32 = 0x40

// windowsDACLWriteOrAdminBits names the access bits whose presence
// in an ALLOW ACE for a non-allowlisted SID lets that principal
// TAMPER with the file (write data, delete, replace DACL, take
// ownership) — or, on a directory, delete child entries via
// FILE_DELETE_CHILD. The v0.4.2 read-side relax lane refuses any
// parent granting these bits to a non-allowlisted SID — those
// rights on the PARENT mean the principal can replace the state
// file's directory entry between verify (fd-bound) and read
// (path-based os.ReadFile), breaking the TOCTOU guarantee that the
// verified bytes are the bytes consumed.
//
// codex bot r5 P1 on PR #192: FILE_DELETE_CHILD (directory-only,
// 0x40) was missing from this mask. A parent ACE granting
// read+FILE_DELETE_CHILD passed the strict gate (because read is
// significant) then passed the relax recheck (because
// FILE_DELETE_CHILD wasn't in the mask) → swap window opened.
// Including FILE_DELETE_CHILD closes the hole. Other "write"
// bits in the same family (FILE_WRITE_DATA etc.) remain present.
//
// Read-only ALLOW ACEs (FILE_READ_DATA / FILE_READ_EA /
// FILE_EXECUTE only) do NOT enable swap and are tolerable under
// default-relax mode.
var windowsDACLWriteOrAdminBits = uint32(
	windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|
		windows.FILE_WRITE_EA|windows.FILE_WRITE_ATTRIBUTES|
		windows.DELETE|windows.WRITE_DAC|windows.WRITE_OWNER,
) | windowsFileDeleteChild

// verifyWindowsDACLFromHandle reads the owner SID + DACL from `h` via
// GetSecurityInfo, then enforces the allowlist. Exported within the
// api package only — secure_write_windows.go uses it for the
// post-rename re-verify step.
func verifyWindowsDACLFromHandle(h windows.Handle) error {
	return verifyWindowsDACLFromHandleMasked(h, windowsDACLSignificantBits)
}

// verifyWindowsDACLFromHandleWriteOrAdmin is the narrower variant
// used by the v0.4.2 read-relax lane: it refuses ONLY when an ALLOW
// ACE grants write / delete / DAC-edit / ownership-takeover bits to
// a non-allowlisted SID. Used to gate the read-relax: tolerate
// broadened-read parents (codex bot r1 P1 closure on PR #190 r1 +
// v0.4.2 fixes), refuse broadened-write parents (codex bot r4 P1
// on PR #192 — TOCTOU swap risk between fd-bound verify and path-
// based os.ReadFile).
func verifyWindowsDACLFromHandleWriteOrAdmin(h windows.Handle) error {
	return verifyWindowsDACLFromHandleMasked(h, windowsDACLWriteOrAdminBits)
}

// verifyWindowsDACLFromHandleMasked is the shared implementation;
// the public verifiers above pin which access-bit mask flags an
// ALLOW ACE as a violation.
func verifyWindowsDACLFromHandleMasked(h windows.Handle, significantBits uint32) error {
	currentSID, systemSID, adminSID, err := allowlistSIDs()
	if err != nil {
		return err
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
	// Owner allowlist: current user, SYSTEM, BuiltinAdministrators.
	//
	// Default Windows config: C:\Users\<user> is created by
	// CreateUserProfile API with owner=NT AUTHORITY\SYSTEM and an
	// inherited ACE granting the user Full Access — the user is a
	// DACL grantee, not the owner. Requiring strict owner==currentUser
	// rejected the dominant Windows install scenario. The DACL
	// iterator below still enforces that no SID outside this same
	// allowlist holds a confidentiality- or integrity-significant ACE,
	// so granting "owner may be SYSTEM/Admin" doesn't let a third
	// party gain access — only allowlist SIDs may hold an ALLOW ACE
	// with significant bits.
	if !ownerSIDAllowed(ownerSID, allowlist) {
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
	// significantBits is the caller-supplied set of access bits whose
	// presence in an ALLOW ACE for a non-allowlisted SID counts as a
	// violation. The strict caller (verifyWindowsDACLFromHandle) uses
	// windowsDACLSignificantBits (full read+write+admin); the relax
	// caller (verifyWindowsDACLFromHandleWriteOrAdmin) uses
	// windowsDACLWriteOrAdminBits so only TOCTOU-relevant
	// write/delete/admin broadening fails.
	//
	// Background on the strict mask (preserved here for context;
	// codex bot r8 P1 closure on the original PR): the mask spans
	// both the confidentiality boundary (reads) AND the integrity
	// boundary (writes/delete/dacl-edit/ownership-takeover). State
	// files holding tokens must allow ONLY allowlist SIDs ANY
	// access at all. Earlier (pre-r8) mask was read-only and
	// fail-OPEN'd against write-only ACEs.
	for i := uint32(0); i < count; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("get ace %d: %w", i, err)
		}
		// ACE types (Microsoft, ntsecapi.h):
		//   0x00 ACCESS_ALLOWED_ACE_TYPE          (basic ALLOW)
		//   0x01 ACCESS_DENIED_ACE_TYPE           (DENY — cannot widen access)
		//   0x02 SYSTEM_AUDIT_ACE_TYPE            (audit — not access-granting)
		//   0x03 SYSTEM_ALARM_ACE_TYPE            (alarm — not access-granting)
		//   0x05 ACCESS_ALLOWED_OBJECT_ACE_TYPE   (object ALLOW)
		//   0x06 ACCESS_DENIED_OBJECT_ACE_TYPE    (object DENY)
		//   0x07 SYSTEM_AUDIT_OBJECT_ACE_TYPE     (object audit)
		//   0x09 ACCESS_ALLOWED_CALLBACK_ACE_TYPE (callback ALLOW)
		//   0x0A ACCESS_DENIED_CALLBACK_ACE_TYPE  (callback DENY)
		//   0x0B ACCESS_ALLOWED_CALLBACK_OBJECT_ACE_TYPE
		//   0x0C ACCESS_DENIED_CALLBACK_OBJECT_ACE_TYPE
		//   0x11 SYSTEM_MANDATORY_LABEL_ACE_TYPE  (integrity label)
		//
		// codex bot r7 P1 closure: earlier loop treated ONLY basic ALLOW
		// (0x00) as effective and silently skipped all others. That
		// fail-OPEN'd against object-variant and callback-variant ALLOW
		// ACEs that managed environments push via Group Policy. Now:
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			// Basic ALLOW — falls through to inspection below.
		case windows.ACCESS_DENIED_ACE_TYPE,
			aceTypeDeniedObject,      // 0x06
			aceTypeAuditBasic,        // 0x02
			aceTypeAlarmBasic,        // 0x03
			aceTypeAuditObject,       // 0x07
			aceTypeAlarmObject,       // 0x08
			aceTypeDeniedCallback,    // 0x0A
			aceTypeDeniedCallbackObj, // 0x0C
			aceTypeMandatoryLabel:    // 0x11
			// DENY / audit / alarm / mandatory-label ACEs cannot widen
			// access; safe to skip.
			continue
		case aceTypeAllowedObject, // 0x05
			aceTypeAllowedCallback,       // 0x09
			aceTypeAllowedCallbackObject: // 0x0B
			// Object/callback ALLOW variants have a different on-the-
			// wire layout (extra ObjectType + InheritedObjectType GUIDs
			// before the SID). The basic ACCESS_ALLOWED_ACE struct's
			// SidStart field points at the wrong location, so we cannot
			// reliably extract the SID via sidFromAce. **Fail closed**:
			// we'd rather refuse a managed-environment ACL than risk a
			// silent fail-open against a Group-Policy-pushed object/
			// callback ALLOW for a non-allowlisted SID.
			return fmt.Errorf("%w: ACE %d has type 0x%02x (object/callback ALLOW); allowlist verifier does not parse this variant — fail closed",
				ErrDaclOutsideAllowlist, i, ace.Header.AceType)
		default:
			// Unknown ACE type — also fail closed. New Windows versions
			// may add types we haven't seen; the conservative read is
			// "if we don't understand it, we cannot prove it's safe."
			return fmt.Errorf("%w: ACE %d has unknown type 0x%02x — fail closed",
				ErrDaclOutsideAllowlist, i, ace.Header.AceType)
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
		mapped := mapGenericRights(uint32(ace.Mask))
		if mapped&significantBits == 0 {
			// ALLOW ACE that doesn't grant any meaningful access —
			// either it's a vestigial zero-mask ACE or it only grants
			// SYNCHRONIZE/READ_CONTROL which we don't gate on.
			continue
		}
		sid := sidFromAce(ace)
		if !sidInAllowlist(sid, allowlist) {
			name := sidString(sid)
			return &DACLAllowlistViolation{SID: name, Mask: mapped & significantBits}
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

// mapGenericRights expands the four GENERIC_* bits in `mask` to their
// file-specific equivalents per Microsoft's MapGenericMask semantics for
// file objects. Used by the DACL evaluation loop so that an ACE granting
// GENERIC_WRITE / GENERIC_EXECUTE / GENERIC_ALL surfaces the underlying
// FILE_WRITE_* / FILE_EXECUTE / FILE_ALL_ACCESS bits we gate on
// (codex bot r8 P1 closure — earlier mapGenericReadRights expanded only
// the read direction, so a write-only GENERIC_WRITE ACE evaded the
// allowlist gate).
//
// FILE_GENERIC_READ    = STANDARD_RIGHTS_READ | FILE_READ_DATA |
//
//	FILE_READ_ATTRIBUTES | FILE_READ_EA |
//	SYNCHRONIZE
//
// FILE_GENERIC_WRITE   = STANDARD_RIGHTS_WRITE | FILE_WRITE_DATA |
//
//	FILE_WRITE_ATTRIBUTES | FILE_WRITE_EA |
//	FILE_APPEND_DATA | SYNCHRONIZE
//
// FILE_GENERIC_EXECUTE = STANDARD_RIGHTS_EXECUTE | FILE_READ_ATTRIBUTES |
//
//	FILE_EXECUTE | SYNCHRONIZE
//
// FILE_ALL_ACCESS      = STANDARD_RIGHTS_REQUIRED | SYNCHRONIZE | 0x1FF
func mapGenericRights(mask uint32) uint32 {
	if mask&windows.GENERIC_READ != 0 {
		mask |= uint32(windows.FILE_GENERIC_READ)
	}
	if mask&windows.GENERIC_WRITE != 0 {
		mask |= uint32(windows.FILE_GENERIC_WRITE)
	}
	if mask&windows.GENERIC_EXECUTE != 0 {
		mask |= uint32(windows.FILE_GENERIC_EXECUTE)
	}
	if mask&windows.GENERIC_ALL != 0 {
		// GENERIC_ALL → FILE_ALL_ACCESS. Just OR all the file-specific
		// rights bits in so the significantBits check sees write +
		// delete + WRITE_DAC + WRITE_OWNER too.
		mask |= uint32(windows.FILE_GENERIC_READ |
			windows.FILE_GENERIC_WRITE |
			windows.FILE_GENERIC_EXECUTE |
			windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER)
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

// ownerSIDAllowed reports whether ownerSID is in the allowlist. Pure
// function so the seam is unit-testable without Windows file-handle
// fixtures (which require Administrator to set owner=SYSTEM on a temp
// directory).
func ownerSIDAllowed(ownerSID *windows.SID, allowlist []*windows.SID) bool {
	if ownerSID == nil {
		return false
	}
	for _, allowed := range allowlist {
		if allowed != nil && ownerSID.Equals(allowed) {
			return true
		}
	}
	return false
}
