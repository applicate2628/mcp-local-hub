//go:build windows

// hub_mcp_state_parent_write_check_windows.go — narrow write-only
// parent-DACL check used by writeHubMcpStateFile to refuse
// write/delete-capable parents in the v0.4.2 relax lane.
//
// Codex bot r6 P1 on PR #192: state-write relax must match the
// read-side write-bits gate (r3 POSIX + r4 Windows). Otherwise a
// write-capable parent accepts the WRITE but the READ rejects,
// stranding state files in unreadable directories.

package api

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// checkStateDirParentWriteSafe opens parent and runs the narrow
// verifyWindowsDACLFromHandleWriteOrAdmin check. Returns nil if no
// ALLOW ACE for a non-allowlisted SID grants write/delete/DAC-edit
// (or directory-child-delete). Non-nil error means the parent is
// write-capable by a non-allowlisted principal — TOCTOU swap risk.
func checkStateDirParentWriteSafe(parentDir string) error {
	pathW, err := windows.UTF16PtrFromString(parentDir)
	if err != nil {
		return fmt.Errorf("utf16 parent %q: %w", parentDir, err)
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
		return fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer windows.CloseHandle(h)
	return verifyWindowsDACLFromHandleWriteOrAdmin(h)
}
