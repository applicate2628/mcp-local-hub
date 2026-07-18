//go:build !windows

package cli

import "syscall"

func restartV3BindRefusedTestError() error {
	return syscall.EADDRINUSE
}
