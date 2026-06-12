//go:build !windows

package api

import "mcp-local-hub/internal/process"

func stopForceKillSupervisorPIDTree(pid int) error {
	return process.TreeKillByPID(pid)
}
