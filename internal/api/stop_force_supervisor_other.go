//go:build !windows

package api

func stopForceKillSupervisorPIDTree(pid int) error {
	return &OriginalProcessOwnerRequiredError{PID: pid}
}
