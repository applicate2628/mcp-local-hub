//go:build !windows

package reversedepgraph

import "syscall"

func processExistsForTest(pid int) bool { return syscall.Kill(pid, 0) == nil }
