//go:build !windows

package api

import "path/filepath"

// SupervisorIPCAddress returns the POSIX unix-domain-socket path for the
// supervisor IPC channel. The stateDir argument must be the per-user
// mcp-local-hub state directory.
func SupervisorIPCAddress(stateDir string) string {
	return filepath.Join(stateDir, "supervisor.sock")
}
