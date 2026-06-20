//go:build windows

// hub_mcp_state_read_inode_windows.go — inode-anchored secure read
// for hub-mcp state files, mirroring the verify pipeline in
// hub_mcp_state_dacl_windows.go but eliminating the TOCTOU swap
// window the old verify→os.ReadFile chain left open on hosts whose
// parent directory granted write/delete/DAC-edit access to a
// non-allowlisted SID.
//
// Why this exists (work-items/bugs/2026-05-19-state-file-verify-rejects-write-broadened-parent-dacl.md):
//
//   The old readHubMcpStateFile called VerifyHubMcpStateDACL (which
//   opens a parent-handle, opens the file relative to it, verifies
//   the file's DACL, then CLOSES the handle), and then performed a
//   path-based os.ReadFile. Between the verify-close and the
//   os.ReadFile path resolution, a co-resident principal with
//   FILE_DELETE_CHILD on the parent could replace the file with
//   attacker-controlled content; the hub would then read attacker
//   bytes that the verify never saw. To mitigate that window
//   without a re-open, hub_mcp_state_dacl_windows.go:143-145
//   refused parent DACLs that granted WRITE/DAC-edit to
//   non-allowlisted SIDs even in default-relax mode. The side
//   effect: on solo-developer Windows hosts whose %LOCALAPPDATA%
//   parent had a CodexSandboxUsers or orphan AD SID ACE (common
//   on third-party-installer-managed boxes), every state-file
//   read failed, blocking demigrate / strict-mode --recover /
//   any other operator action that consults managed-entries.json.
//
// Fix design:
//
//   This file's readStateFileInodeAnchoredWindows function opens
//   the file via ntOpenRelative against the verified parent handle
//   with FILE_READ_DATA in the desiredAccess mask, then performs
//   the read via windows.ReadFile on the SAME handle. The handle is
//   bound to the verified file inode regardless of subsequent
//   directory-entry changes; an attacker who replaces the entry
//   after our open does not change the inode our handle points to,
//   so the swap window is closed at the kernel level. The relaxed
//   parent-DACL gate becomes safe under default-relax — strict
//   mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1) still refuses any
//   parent broadening because the underlying ACL diverges from the
//   single-user invariant strict mode promises.

package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const (
	statusNoSuchFile        windows.NTStatus = 0xC000000F
	statusObjectNameMissing windows.NTStatus = 0xC0000034
	statusObjectPathMissing windows.NTStatus = 0xC000003A
)

// readStateFileInodeAnchoredWindows opens parent + file under the
// verified parent handle, asserts the file is single-user-safe via
// the same allowlist gate as VerifyHubMcpStateDACL, and reads the
// content via the same handle. No path-based read between verify
// and read, so the TOCTOU swap window the old chain left open is
// closed at the kernel level.
//
// Returns up to maxStateFileBytes of content. Files larger than the
// cap cause the read to surface an error rather than truncate
// silently (the cap is OOM protection; legitimate state files are
// well under it).
//
// Parent-DACL gate semantics:
//
//   - Strict mode (MCPHUB_REQUIRE_SINGLE_USER_HOME=1): any
//     non-allowlisted ACE on the parent rejects. Matches strict-mode
//     behavior in verifyHubMcpStateDACLImpl.
//   - Default-relax mode: read-only broadening logs a warn event
//     and proceeds. Write/DAC-edit broadening ALSO proceeds (the
//     swap window the old verifyHubMcpStateDACLImpl rejected on is
//     closed by the inode-anchored read in this function — the
//     attacker can replace the directory entry but not the inode
//     our handle points to). File-DACL broadening also warns and
//     proceeds in default-relax mode so inherited solo-host principals
//     do not make read-side probes fail closed. Strict mode keeps both
//     DACL gates hard. Separate warn events distinguish parent vs file
//     broadening and write-broadening vs read-only-broadening so
//     operators can audit the more-permissive cases.
func readStateFileInodeAnchored(path string) ([]byte, error) {
	return readStateFileInodeAnchoredWithStrictPolicy(path, operatorRequiresSingleUserHome)
}

func readStateFileInodeAnchoredWithStrictPolicy(path string, requiresStrict func() bool) ([]byte, error) {
	parentDir := filepath.Dir(path)
	basename := filepath.Base(path)

	parentDirW, err := windows.UTF16PtrFromString(parentDir)
	if err != nil {
		return nil, fmt.Errorf("utf16 parent %q: %w", parentDir, err)
	}
	parentHandle, err := windows.CreateFile(
		parentDirW,
		windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		if windowsAnchoredReadErrIsNotExist(err) {
			return nil, &os.PathError{Op: "open", Path: parentDir, Err: os.ErrNotExist}
		}
		return nil, fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(parentHandle)

	// Parent-DACL gate. Strict mode rejects any non-allowlisted ACE
	// (same as the old verify); default-relax now tolerates BOTH
	// read-only and write/DAC-edit broadening because the read
	// below is inode-anchored.
	if err := verifyWindowsDACLFromHandle(parentHandle); err != nil {
		if requiresStrict() {
			return nil, fmt.Errorf("parent %s not single-user safe: %w; %s=1 is set, so the strict parent-dir gate is enforced (unset that env var, or tighten the parent's DACL to remove the offending principal, to proceed)",
				parentDir, err, RequireSingleUserHomeEnv)
		}
		if errors.Is(err, ErrWrongOwner) {
			return nil, fmt.Errorf("parent %s not single-user safe: %w", parentDir, err)
		}
		// Default-relax. Distinguish write-broadening from
		// read-only-broadening in the audit log so operators can
		// review the more-permissive case.
		reason := "default-relax-on-solo-host (parent grants read-only access to non-allowlisted SID)"
		if wrErr := verifyWindowsDACLFromHandleWriteOrAdmin(parentHandle); wrErr != nil {
			reason = "default-relax-on-solo-host (parent grants WRITE/DAC-edit access; safe under inode-anchored read because subsequent ReadFile is bound to the file handle, not the path)"
		}
		_ = LogHubMcpEvent("warn", "hub-mcp-state-read-unhardened-parent-fallback", map[string]any{
			"path":   path,
			"parent": parentDir,
			"reason": reason,
			"err":    err.Error(),
			"note":   "file's own DACL verified below; ReadFile is handle-bound so TOCTOU swap window is closed at the kernel level",
		})
	}

	// Open the file RELATIVE to the parent handle, with
	// FILE_READ_DATA so we can issue ReadFile on the same handle
	// after the verify steps below. SYNCHRONIZE is added by
	// ntOpenRelative internally.
	fileHandle, err := ntOpenRelative(
		parentHandle,
		basename,
		windows.FILE_READ_DATA|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
	)
	if err != nil {
		if windowsAnchoredReadErrIsNotExist(err) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		// FILE_NON_DIRECTORY_FILE inside ntOpenRelative makes NT
		// fail the open with STATUS_FILE_IS_A_DIRECTORY (0xC00000BA)
		// when the target is a directory. Map that to our portable
		// ErrIrregularFile sentinel.
		if ns, ok := err.(windows.NTStatus); ok && ns == 0xC00000BA {
			return nil, ErrIrregularFile
		}
		if errors.Is(err, windows.ERROR_DIRECTORY) {
			return nil, ErrIrregularFile
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(fileHandle)

	// Reject reparse points / symlinks via the file attributes.
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(fileHandle, &fi); err != nil {
		return nil, fmt.Errorf("file info %s: %w", path, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, ErrIrregularFile
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, ErrIrregularFile
	}

	// File-DACL gate. Strict mode preserves the single-user DACL
	// invariant. Default-relax mirrors the write side: inherited
	// non-allowlisted principals on normal temp/state files warn but
	// do not block the handle-bound read. Wrong-owner remains hard.
	if err := verifyWindowsDACLFromHandle(fileHandle); err != nil {
		if requiresStrict() {
			return nil, fmt.Errorf("file %s not single-user safe: %w; %s=1 is set, so the strict file-DACL gate is enforced (unset that env var, or tighten the file's DACL to remove the offending principal, to proceed)",
				path, err, RequireSingleUserHomeEnv)
		}
		if errors.Is(err, ErrWrongOwner) {
			return nil, err
		}
		reason := "default-relax-on-solo-host (file grants read-only access to non-allowlisted SID)"
		if wrErr := verifyWindowsDACLFromHandleWriteOrAdmin(fileHandle); wrErr != nil {
			reason = "default-relax-on-solo-host (file grants WRITE/DAC-edit access to non-allowlisted SID; continuing because default read-side posture is advisory unless strict mode is enabled)"
		}
		_ = LogHubMcpEvent("warn", "hub-mcp-state-read-unhardened-file-fallback", map[string]any{
			"path":   path,
			"parent": parentDir,
			"reason": reason,
			"err":    err.Error(),
			"note":   "ReadFile is handle-bound; symlink/reparse refusal and inode anchoring remain enforced",
		})
	}

	// Read the content via the verified handle. windows.ReadFile
	// fills a single buffer; loop until EOF or the size cap fires.
	// Pre-allocate based on the file size we already have from
	// GetFileInformationByHandle to avoid reslicing.
	fileSize := int64(fi.FileSizeHigh)<<32 | int64(fi.FileSizeLow)
	if fileSize > maxStateFileBytes {
		return nil, fmt.Errorf("hub-mcp state read %s: file size %d exceeds cap %d (OOM-protection)", path, fileSize, maxStateFileBytes)
	}
	if fileSize < 0 {
		return nil, fmt.Errorf("hub-mcp state read %s: invalid file size %d", path, fileSize)
	}
	buf := make([]byte, 0, fileSize)
	chunk := make([]byte, 4096)
	for {
		var read uint32
		err := windows.ReadFile(fileHandle, chunk, &read, nil)
		if err != nil {
			// ERROR_HANDLE_EOF (38) or zero-read on a regular file
			// means EOF — both signal "we got everything".
			if errors.Is(err, windows.ERROR_HANDLE_EOF) {
				break
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if read == 0 {
			break
		}
		buf = append(buf, chunk[:read]...)
		if int64(len(buf)) > maxStateFileBytes {
			return nil, fmt.Errorf("hub-mcp state read %s: content exceeds cap %d (OOM-protection)", path, maxStateFileBytes)
		}
	}
	// io.ReadAll-style invariant: a successful read of an empty
	// file returns an empty non-nil slice, not nil.
	if buf == nil {
		buf = []byte{}
	}
	return buf, nil
}

func windowsAnchoredReadErrIsNotExist(err error) bool {
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return true
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		switch status {
		case statusNoSuchFile, statusObjectNameMissing, statusObjectPathMissing:
			return true
		}
	}
	return false
}
