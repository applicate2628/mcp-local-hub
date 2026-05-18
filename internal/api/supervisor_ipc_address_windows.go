//go:build windows

package api

import (
	"os"
	"strings"
)

// SupervisorIPCAddress returns the Windows named-pipe path for the
// supervisor IPC channel. The stateDir argument is accepted for API parity
// with POSIX; Windows pipes live in the kernel namespace.
func SupervisorIPCAddress(_ string) string {
	user := os.Getenv("USERNAME")
	user = strings.ReplaceAll(user, " ", "-")
	if user == "" {
		user = "default"
	}
	return `\\.\pipe\mcphub-supervisor-` + user
}
