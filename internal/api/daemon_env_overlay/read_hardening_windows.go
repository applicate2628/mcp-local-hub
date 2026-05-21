//go:build windows

// read_hardening_windows.go — Windows hardenedOpen using
// FILE_FLAG_OPEN_REPARSE_POINT + post-open FILE_ATTRIBUTE_REPARSE_POINT
// refusal so the open itself does NOT traverse a symlink / mount
// point / NTFS reparse point AND the helper refuses any file marked
// with the reparse-point attribute.
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Read-side hardening" (B-V4-4).
//
// Pattern mirrored from internal/api/hub_mcp_state_dacl_windows.go
// (verifyHubMcpStateDACLImpl):
//
//  1. CreateFile with FILE_FLAG_OPEN_REPARSE_POINT so the kernel
//     opens the reparse-point entry itself instead of following it.
//     FILE_FLAG_BACKUP_SEMANTICS is included so the call also works
//     uniformly when the path turns out to be a directory — the
//     IsRegular() check inside Load is what surfaces "not a regular
//     file" in that case. (We deliberately do NOT use the broader
//     pattern with READ_CONTROL; this is a read of the file's body,
//     not a DACL probe.)
//
//  2. GetFileInformationByHandle to fetch FileAttributes from the
//     open handle. If FILE_ATTRIBUTE_REPARSE_POINT is set, the
//     helper refuses the open — even though the kernel did not
//     traverse the reparse point, the reparse-point entry itself
//     is not a regular file's bytes and we must not read it.
//
//  3. The Win32 HANDLE is wrapped via os.NewFile(uintptr(h), path)
//     so the returned *os.File owns the handle. *os.File.Close()
//     closes the underlying handle; callers (Load in overlay.go)
//     already defer Close on the open.
//
// Note: we do NOT verify the file's own DACL here. The state-write
// pipeline installed the restrictive owner-only DACL on the file
// handle at create time (state_file_helper.go), so the file is
// owner-only. Verifying it again on read would duplicate the
// write-side guarantee. The parent-DACL gate
// (checkStateDirParentReadSafe in parent_check.go) is what guards
// against parent-directory broadening that would let a co-resident
// principal replace the overlay file's directory entry between
// writes.

package daemon_env_overlay

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

func hardenedOpen(path string) (*os.File, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("utf16 path %q: %w", path, err)
	}

	h, err := windows.CreateFile(
		pathW,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		// Surface ERROR_FILE_NOT_FOUND / ERROR_PATH_NOT_FOUND as the
		// canonical Go fs.ErrNotExist signal so Load's
		// errors.Is(err, fs.ErrNotExist) branch — which maps "no
		// file" to an empty overlay — still fires. windows.Errno
		// satisfies the syscall.Errno contract that os.IsNotExist
		// and errors.Is(err, fs.ErrNotExist) both honor, so a plain
		// wrap with %w is enough — but we wrap explicitly via
		// os.PathError so the surfaced error reads "open <path>:
		// The system cannot find the file specified." (matches the
		// shape Load already gets on POSIX from os.OpenFile).
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}

	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("%s: file info: %w", path, err)
	}
	if fi.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}

	// os.NewFile takes ownership: *os.File.Close() closes the
	// underlying handle. Callers in overlay.go already defer
	// f.Close() on the Load() open.
	return os.NewFile(uintptr(h), path), nil
}
