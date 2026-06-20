//go:build windows

// hub_mcp_state_windows.go — Phase 2 Task 2.2 Windows leg.
//
// On Windows, the NT relative-open inside readStateFileInodeAnchored
// surfaces STATUS_OBJECT_NAME_NOT_FOUND / STATUS_OBJECT_PATH_NOT_FOUND
// as `windows.NTStatus` values when the target file is missing. These
// do NOT match `errors.Is(err, os.ErrNotExist)` directly, so the
// portable layer needs a platform-specific fall-through. Routine
// startup paths treat any of the four well-known not-found values as
// "generate fresh state"; every other error surfaces.

package api

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isHubMcpStateMissingErrPlatform recognizes Windows-specific
// not-found sentinels that flow up from the inode-anchored read open
// path. The portable layer already covered os.ErrNotExist; this
// adds NTStatus + Win32 errno matches.
//
// Match list:
//   - windows.NTStatus(STATUS_OBJECT_NAME_NOT_FOUND) — NT open via
//     RootDirectory found the parent dir but no child of `name`.
//   - windows.NTStatus(STATUS_OBJECT_PATH_NOT_FOUND) — NT open could
//     not resolve an intermediate path component.
//   - windows.ERROR_FILE_NOT_FOUND / windows.ERROR_PATH_NOT_FOUND —
//     reserved for any future call that may bypass NT and surface a
//     Win32 errno.
func isHubMcpStateMissingErrPlatform(err error) bool {
	if err == nil {
		return false
	}
	// Unwrap to the NTStatus if one is present anywhere in the chain.
	var ns windows.NTStatus
	if errors.As(err, &ns) {
		if ns == windows.STATUS_OBJECT_NAME_NOT_FOUND ||
			ns == windows.STATUS_OBJECT_PATH_NOT_FOUND {
			return true
		}
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return true
	}
	return false
}
