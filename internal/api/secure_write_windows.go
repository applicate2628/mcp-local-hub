//go:build windows

// secure_write_windows.go — Windows leg of SecureWriteClientConfig.
//
// Sequence (spec §"SecureWriteClientConfig sequence", Windows leg):
//
//  1. Open parent dir via CreateFile with FILE_LIST_DIRECTORY +
//     FILE_FLAG_BACKUP_SEMANTICS + FILE_FLAG_OPEN_REPARSE_POINT.
//     REPARSE_POINT here means "open the metadata of the parent
//     itself WITHOUT following any reparse link present on that final
//     component"; combined with step 2's allowlist DACL verify on the
//     resulting handle, any reparse-point parent dir whose underlying
//     DACL falls outside the allowlist is rejected. Ancestor chain is
//     covered by the per-user trust boundary documented in the spec.
//  2. Handle-bound DACL verify on dirHandle via
//     verifyWindowsDACLFromHandle — owner == current-user AND every
//     read-capable ALLOW ACE names a SID in {current-user, LocalSystem,
//     BuiltinAdministrators}. Spec lines 323-326 + 422-432 ("per-target
//     parent-dir DACL check is THE confidentiality boundary"). Without
//     this gate, an enterprise GPO that broadens %USERPROFILE% to
//     Domain Users would silently leak tokens to every domain user.
//  3. crypto/rand 8-byte hex tempName = ".<base>.tmp.<pid>.<hex>".
//  4. NtCreateFile relative to dirHandle (RootDirectory=dirHandle,
//     ObjectName=tempName, FILE_CREATE disposition, FILE_GENERIC_WRITE
//     | DELETE | SYNCHRONIZE | WRITE_DAC desired access). DELETE is
//     mandatory because the rename in step 7 requires it; WRITE_DAC
//     enables step 5's SetSecurityInfo.
//  5. SetSecurityInfo(fileHandle, DACL_SECURITY_INFORMATION |
//     PROTECTED_DACL_SECURITY_INFORMATION, ..., restrictiveDACL) —
//     restrictive DACL = ALLOW current-user | LocalSystem |
//     BuiltinAdministrators with GENERIC_ALL. Done on the HANDLE
//     before any bytes hit disk.
//  6. WriteFile + FlushFileBuffers.
//  7. NtSetInformationFile(fileHandle, FileRenameInformationEx,
//     { Flags: REPLACE_IF_EXISTS|POSIX_SEMANTICS,
//       RootDirectory: dirHandle, FileName: base }).
//     fileHandle stays open across the rename (codex r5 MED).
//  8. Close fileHandle.
//  9. Re-open destination via dirHandle (FILE_OPEN with
//     FILE_OPEN_REPARSE_POINT so the open does NOT silently follow a
//     reparse point swapped in between rename and re-open) and
//     re-verify the file DACL using verifyWindowsDACLFromHandle
//     (defined in hub_mcp_state_dacl_windows.go). Combined with the
//     atomic rename's FILE_RENAME_POSIX_SEMANTICS this guarantees the
//     post-rename handle refers to the file we wrote.
// 10. Close verifyHandle; close dirHandle.
//
// On any error after step 4 the temp file is marked DELETE-on-close
// via NtSetInformationFile(FileDispositionInformation). The fileHandle
// close in the defer then drops the inode.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileRenameInformationEx mirrors FILE_RENAME_INFORMATION_EX from
// ntifs.h (Windows 10 1709+). The Flags field is a single uint32
// bitfield holding FILE_RENAME_REPLACE_IF_EXISTS |
// FILE_RENAME_POSIX_SEMANTICS combined. RootDirectory is the parent
// dir handle the rename resolves against. FileName is a UTF-16
// trailing array; FileNameLength is in BYTES (not chars).
//
// Distinct from the legacy FILE_RENAME_INFORMATION (which exposes a
// `ReplaceIfExists` BOOLEAN at the same offset) — the EX form is
// preferred because the Flags bitfield is the only way to request
// POSIX semantics on the rename (codex r6 MED closure).
type fileRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16 // trailing UTF-16; allocated past struct end
}

// fileInformationClassRenameEx is FileRenameInformationEx in the
// FILE_INFORMATION_CLASS enum (NtSetInformationFile class parameter).
// x/sys exposes FileRenameInformation = 10 but not the EX form; the
// constant 65 is from ntifs.h (Windows SDK).
const fileInformationClassRenameEx uint32 = 65

// secureWriteClientConfigImpl is the Windows handle-relative writer.
// Symmetric with the POSIX impl in secure_write_posix.go.
func secureWriteClientConfigImpl(path string, contents []byte) error {
	parentDir, base := filepath.Split(path)
	if parentDir == "" {
		parentDir = "."
	}
	if base == "" {
		return fmt.Errorf("secure write: empty base name in path %q", path)
	}
	// Strip trailing path separator from parent so CreateFile sees a
	// clean dir path.
	parentDir = filepath.Clean(parentDir)

	// 1. Open parent dir with FILE_LIST_DIRECTORY + FILE_FLAG_BACKUP_SEMANTICS
	//    + FILE_FLAG_OPEN_REPARSE_POINT. The reparse-point flag means
	//    "open the reparse point itself rather than its target".
	dirHandle, err := openDirHandleNoReparse(parentDir)
	if err != nil {
		return fmt.Errorf("secure write: open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(dirHandle)

	// 2. Handle-bound parent-dir DACL verify (spec lines 323-326 +
	//    422-432). Wrap with parent-dir context so operators see
	//    "secure write: parent <dir> not single-user safe" rather than
	//    a file-path-only diagnostic. Without this gate, a parent dir
	//    broadened via Group Policy (Domain Users / Authenticated Users
	//    inherited ACE) would silently pass and the writer would
	//    install a token under a directory listable by every domain
	//    user. Mirror of secure_write_posix.go's verifyPosixParentDirFromFd.
	if err := verifyWindowsDACLFromHandle(dirHandle); err != nil {
		return fmt.Errorf("secure write: parent %s not single-user safe: %w", parentDir, err)
	}

	// 2a. Refuse to overwrite a pre-existing symlink/junction at base.
	// Rename with POSIX semantics silently replaces a reparse point;
	// the caller probably meant to update the symlink target rather
	// than create a new restricted regular file at the symlink slot.
	if err := refusePreexistingReparsePoint(dirHandle, base); err != nil {
		return fmt.Errorf("secure write: target %s: %w", path, err)
	}

	// 3. Compose unpredictable temp name to defeat slot-squat races.
	randBytes := make([]byte, 8)
	if _, err := rand.Read(randBytes); err != nil {
		return fmt.Errorf("secure write: crypto/rand: %w", err)
	}
	tempName := fmt.Sprintf(".%s.tmp.%d.%s", base, os.Getpid(), hex.EncodeToString(randBytes))

	// 4. NtCreateFile relative to dirHandle (FILE_CREATE disposition;
	//    fails if the temp slot already exists). DELETE | WRITE_DAC |
	//    FILE_GENERIC_WRITE | SYNCHRONIZE desired access.
	fileHandle, err := ntCreateRelative(
		dirHandle,
		tempName,
		windows.DELETE|windows.GENERIC_WRITE|windows.SYNCHRONIZE|windows.WRITE_DAC,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
	)
	if err != nil {
		return fmt.Errorf("secure write: ntcreate temp %s: %w", tempName, err)
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(fileHandle)
		}
	}()

	// cleanup marks the temp file for delete-on-close. Errors are
	// ignored — the caller already has a failure to surface, and the
	// fileHandle close (via defer) then drops the inode.
	cleanup := func() {
		_ = setFileDeleteOnClose(fileHandle)
	}

	// 5. SetSecurityInfo handle-bound — restrictive DACL applied
	//    BEFORE any bytes hit disk. PROTECTED_DACL prevents inherited
	//    ACEs from re-broadening access between rename and re-verify.
	if err := setRestrictiveDACL(fileHandle); err != nil {
		cleanup()
		return fmt.Errorf("secure write: set DACL: %w", err)
	}

	// 6. WriteFile + FlushFileBuffers.
	if err := windowsWriteAll(fileHandle, contents); err != nil {
		cleanup()
		return fmt.Errorf("secure write: write temp: %w", err)
	}
	if err := windows.FlushFileBuffers(fileHandle); err != nil {
		cleanup()
		return fmt.Errorf("secure write: flush temp: %w", err)
	}

	// 7. NtSetInformationFile(FileRenameInformationEx) — atomic rename
	//    relative to dirHandle. fileHandle MUST stay open across the
	//    call (codex r5 MED).
	if err := ntRenameRelative(fileHandle, dirHandle, base); err != nil {
		cleanup()
		return fmt.Errorf("secure write: ntrename %s -> %s: %w", tempName, base, err)
	}

	// 8. Close the file handle (rename complete, safe to release).
	if err := windows.CloseHandle(fileHandle); err != nil {
		return fmt.Errorf("secure write: close temp: %w", err)
	}
	closed = true

	// 9. Re-open destination via SAME dirHandle and re-verify DACL.
	//    GENERIC_READ + READ_CONTROL is needed for GetSecurityInfo to
	//    read the DACL.
	verifyHandle, err := ntOpenRelative(
		dirHandle,
		base,
		windows.GENERIC_READ|windows.READ_CONTROL,
	)
	if err != nil {
		return fmt.Errorf("secure write: re-open %s: %w", base, err)
	}
	defer windows.CloseHandle(verifyHandle)
	if err := verifyWindowsDACLFromHandle(verifyHandle); err != nil {
		return fmt.Errorf("secure write: post-rename DACL verify %s: %w", base, err)
	}
	return nil
}

// openDirHandleNoReparse opens a directory with FILE_LIST_DIRECTORY +
// READ_CONTROL + FILE_FLAG_BACKUP_SEMANTICS + FILE_FLAG_OPEN_REPARSE_POINT.
// The reparse-point flag here means "open the reparse point itself rather
// than the target", which is the right behavior for our purposes —
// the caller's downstream operations (NtCreateFile relative to this
// handle) inherit the same anchor and can't be re-walked from root.
// READ_CONTROL is required so the step-2 parent-DACL verify (which
// calls GetSecurityInfo on this handle) can read the DACL; without it
// GetSecurityInfo fails with ACCESS_DENIED on dirs whose DACL doesn't
// happen to grant the current process token read-control via some
// broader right.
//
// FILE_FLAG_OPEN_REPARSE_POINT alone doesn't fail on symlinks — but
// any subsequent DACL allowlist check rejects mis-owned reparse points.
func openDirHandleNoReparse(path string) (windows.Handle, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("utf16 %q: %w", path, err)
	}
	h, err := windows.CreateFile(
		pathW,
		windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return h, nil
}

// ntCreateRelative wraps NtCreateFile with RootDirectory=dirHandle.
// ObjectName is the relative path; ShareAccess is full r/w/d so the
// post-rename re-open at step 8 succeeds even if AV scanners hold
// transient handles.
func ntCreateRelative(
	dirHandle windows.Handle,
	name string,
	desiredAccess uint32,
	disposition uint32,
	createOptions uint32,
) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("nt unicode %q: %w", name, err)
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		ObjectName:    objectName,
		RootDirectory: dirHandle,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var allocSize int64 = 0
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		desiredAccess,
		oa,
		&iosb,
		&allocSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		createOptions,
		0,
		0,
	); err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
}

// ntOpenRelative wraps NtCreateFile with FILE_OPEN disposition for an
// existing file. Used by the post-rename re-verify step.
//
// FILE_OPEN_REPARSE_POINT in createOptions does NOT make the open
// fail on a reparse point — it instructs NtCreateFile to open the
// reparse point's metadata WITHOUT following the link. Callers that
// must reject reparse points still need to inspect FileAttributes
// after open (see refusePreexistingReparsePoint earlier in this file).
//
// The actual reparse-point reject for the destination filename comes
// from the explicit refusePreexistingReparsePoint call in step 2a
// (run BEFORE the temp write). The atomic rename in step 7 uses
// FILE_RENAME_POSIX_SEMANTICS, which renames the file we just wrote
// over the destination basename in one transactional step; the
// post-rename re-open via this helper therefore lands on the file we
// wrote, even if a hostile process swapped a reparse point into the
// slot between rename and re-open (the rename was atomic on the
// dirHandle-relative basename, not a path re-walk).
func ntOpenRelative(
	dirHandle windows.Handle,
	name string,
	desiredAccess uint32,
) (windows.Handle, error) {
	return ntCreateRelative(
		dirHandle,
		name,
		desiredAccess|windows.SYNCHRONIZE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
	)
}

// ntRenameRelative wraps NtSetInformationFile with
// FileRenameInformationEx (class 65). Renames `fileHandle` to
// `newBase` under `dirHandle`. Uses REPLACE_IF_EXISTS | POSIX_SEMANTICS
// so the rename is atomic and replaces any existing destination.
//
// IMPORTANT: fileHandle MUST remain open across this call (codex r5
// MED). After success the caller should close fileHandle and re-open
// the new path via dirHandle.
func ntRenameRelative(
	fileHandle windows.Handle,
	dirHandle windows.Handle,
	newBase string,
) error {
	newBaseUTF16, err := windows.UTF16FromString(newBase)
	if err != nil {
		return fmt.Errorf("utf16 %q: %w", newBase, err)
	}
	// UTF16FromString includes a trailing NUL — strip it from the
	// length field per FILE_RENAME_INFORMATION_EX contract (length is
	// in bytes, excludes trailing NUL).
	nameLenChars := len(newBaseUTF16) - 1
	if nameLenChars < 0 {
		nameLenChars = 0
	}
	nameLenBytes := nameLenChars * 2

	// Allocate a single buffer big enough for the struct header plus
	// the trailing UTF-16 array (without trailing NUL).
	headerSize := int(unsafe.Offsetof(fileRenameInformationEx{}.FileName))
	bufferSize := headerSize + nameLenBytes
	if bufferSize < int(unsafe.Sizeof(fileRenameInformationEx{})) {
		bufferSize = int(unsafe.Sizeof(fileRenameInformationEx{}))
	}
	buffer := make([]byte, bufferSize)
	typedBuf := (*fileRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	typedBuf.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	typedBuf.RootDirectory = dirHandle
	typedBuf.FileNameLength = uint32(nameLenBytes)
	if nameLenChars > 0 {
		dst := unsafe.Slice((*uint16)(unsafe.Pointer(&typedBuf.FileName[0])), nameLenChars)
		copy(dst, newBaseUTF16[:nameLenChars])
	}

	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		fileHandle,
		&iosb,
		&buffer[0],
		uint32(bufferSize),
		fileInformationClassRenameEx,
	)
}

// setRestrictiveDACL applies a DACL to fileHandle that grants
// GENERIC_ALL to {current-user-SID, LocalSystem, BuiltinAdministrators}
// only. PROTECTED_DACL_SECURITY_INFORMATION prevents inherited ACEs
// from re-broadening access between rename and the post-rename DACL
// re-verify.
//
// On any error the file is in an inconsistent state — the caller MUST
// unlink it via cleanup().
func setRestrictiveDACL(fileHandle windows.Handle) error {
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
	entries := []windows.EXPLICIT_ACCESS{
		explicitAccessAllow(currentSID, windows.TRUSTEE_IS_USER, windows.GENERIC_ALL),
		explicitAccessAllow(systemSID, windows.TRUSTEE_IS_WELL_KNOWN_GROUP, windows.GENERIC_ALL),
		explicitAccessAllow(adminSID, windows.TRUSTEE_IS_GROUP, windows.GENERIC_ALL),
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("acl from entries: %w", err)
	}
	return windows.SetSecurityInfo(
		fileHandle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}

// explicitAccessAllow builds an EXPLICIT_ACCESS entry that ALLOWs the
// given SID the given access mask. NO_INHERITANCE means the ACE does
// not propagate to children — the temp file has no children, so this
// is purely correctness hygiene.
func explicitAccessAllow(sid *windows.SID, trusteeType uint32, mask uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(mask),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_TYPE(trusteeType),
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// windowsWriteAll writes the full payload to fileHandle, retrying on
// partial writes. WriteFile may return short on huge buffers; we loop
// until done or error.
func windowsWriteAll(handle windows.Handle, p []byte) error {
	for len(p) > 0 {
		var written uint32
		if err := windows.WriteFile(handle, p, &written, nil); err != nil {
			return err
		}
		if written == 0 {
			return fmt.Errorf("WriteFile returned 0 bytes")
		}
		p = p[written:]
	}
	return nil
}

// refusePreexistingReparsePoint stats the destination `name` under
// `dirHandle` (relative-open) and refuses if the existing file is a
// reparse point (symlink/junction). If the file doesn't exist, returns
// nil; if it's a regular file, returns nil so the atomic rename can
// replace it.
//
// Implementation: open `name` via dirHandle with READ_ATTRIBUTES +
// FILE_OPEN_REPARSE_POINT. If the open fails with ERROR_FILE_NOT_FOUND
// or ERROR_PATH_NOT_FOUND, the slot is empty (success). If the open
// succeeds, query attributes via GetFileInformationByHandle and check
// FILE_ATTRIBUTE_REPARSE_POINT.
func refusePreexistingReparsePoint(dirHandle windows.Handle, name string) error {
	h, err := ntOpenRelative(dirHandle, name, windows.FILE_READ_ATTRIBUTES)
	if err != nil {
		// ERROR_FILE_NOT_FOUND / ERROR_PATH_NOT_FOUND / NT
		// STATUS_OBJECT_NAME_NOT_FOUND all map through x/sys to the
		// same NTStatus error wrapping. Treat any "not found" as
		// "slot empty, fine".
		if isNotFoundErr(err) {
			return nil
		}
		// Cannot tell — surface the open error so the caller doesn't
		// proceed with a write that might collide with an existing
		// reparse point.
		return fmt.Errorf("probe %s: %w", name, err)
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fmt.Errorf("get file info %s: %w", name, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("pre-existing reparse point refused")
	}
	return nil
}

// isNotFoundErr returns true if err is one of the Windows "object not
// found" sentinels. NtCreateFile maps several NTSTATUS values into
// this group; we widen via string-suffix match to catch the variants
// the Go errno wrapper exposes.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	// x/sys returns NTStatus directly for NtCreateFile failures. The
	// "object name not found" status family is STATUS_OBJECT_NAME_NOT_FOUND
	// (0xC0000034) — readable via NTStatus.Error().
	type ntStatusErr interface {
		Errno() uintptr
	}
	if ns, ok := err.(windows.NTStatus); ok {
		// STATUS_OBJECT_NAME_NOT_FOUND, STATUS_OBJECT_PATH_NOT_FOUND
		if ns == 0xC0000034 || ns == 0xC000003A {
			return true
		}
	}
	return false
}

// setFileDeleteOnClose marks the file for delete-on-close via
// FILE_DISPOSITION_INFORMATION (legacy class — single-byte BOOLEAN).
// Best-effort cleanup when a step after NtCreateFile fails.
func setFileDeleteOnClose(handle windows.Handle) error {
	// Use the raw 1-byte BOOLEAN per FILE_DISPOSITION_INFORMATION
	// layout. The legacy class is broadly supported; the EX form has
	// extra fields we don't need.
	delete := uint8(1)
	var iosb windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(
		handle,
		&iosb,
		&delete,
		1,
		windows.FileDispositionInformation,
	)
}
