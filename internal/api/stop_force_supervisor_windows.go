//go:build windows

package api

func stopForceKillSupervisorPIDTree(pid int) error {
	return taskkillProcessTreeByPIDFn(pid)
}
