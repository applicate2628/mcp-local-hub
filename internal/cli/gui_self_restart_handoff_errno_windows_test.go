//go:build windows

package cli

import "golang.org/x/sys/windows"

func restartV3BindRefusedTestError() error {
	return windows.WSAEADDRINUSE
}
