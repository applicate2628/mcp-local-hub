//go:build !windows

// hub_mcp_state_parent_write_check_posix.go — narrow write-only
// parent mode check used by writeHubMcpStateFile to refuse
// write-capable parents in the v0.4.2 relax lane.
//
// Codex bot r6 P1 on PR #192: state-write relax must match the
// read-side write-bits gate (r3 POSIX + r4 Windows). Otherwise a
// write-capable parent accepts the WRITE but the READ rejects,
// stranding state files in unreadable directories.

package api

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// checkStateDirParentWriteSafe stat-fd's parent and refuses if the
// parent grants group/world write bits (0o022). Read+exec bits
// (0o055) are tolerated.
// Symmetric with the read-side gate in verifyHubMcpStateDACLImpl
// (POSIX leg).
func checkStateDirParentWriteSafe(parentDir string) error {
	pfd, err := unix.Open(parentDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open parent %s: %w", parentDir, err)
	}
	defer unix.Close(pfd)
	var pst unix.Stat_t
	if err := unix.Fstat(pfd, &pst); err != nil {
		return fmt.Errorf("fstat parent %s: %w", parentDir, err)
	}
	pmode := uint32(pst.Mode) & 0o777
	if pmode&0o022 != 0 {
		return fmt.Errorf("parent mode %#o grants group/world write (TOCTOU swap risk)", pmode)
	}
	return nil
}
