//go:build !windows

package gui

import (
	"os/exec"

	processowner "mcp-local-hub/internal/process"
)

// startDetachedSupervisorTolerant: POSIX has no Windows Job Object cascade, so
// there is no breakaway flag and no ERROR_ACCESS_DENIED retry — configureDetachedSupervisor
// (Setpgid) already does the detach. Just start the built cmd.
func startDetachedSupervisorTolerant(build func() *exec.Cmd, _ processowner.BreakawayPolicy) (*exec.Cmd, error) {
	cmd := build()
	if err := cmd.Start(); err != nil {
		return cmd, err
	}
	return cmd, nil
}
