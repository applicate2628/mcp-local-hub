//go:build linux

package process

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// ProcessOwnerMatchesCurrent reports whether pid is owned by the same kernel
// user ID as the current mcphub process.
func ProcessOwnerMatchesCurrent(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("process: invalid PID %d", pid)
	}
	info, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return false, fmt.Errorf("process: stat owner for PID %d: %w", pid, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return false, fmt.Errorf("process: owner metadata unavailable for PID %d", pid)
	}
	return processOwnerUIDMatchesCurrent(int(stat.Uid), os.Getuid()), nil
}

func processOwnerUIDMatchesCurrent(targetUID, currentUID int) bool {
	return targetUID >= 0 && currentUID >= 0 && targetUID == currentUID
}
