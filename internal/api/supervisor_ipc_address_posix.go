//go:build !windows

package api

import "path/filepath"

// SupervisorIPCAddress returns the POSIX unix-domain-socket path for the
// supervisor IPC channel. The stateDir argument must be the per-user
// mcp-local-hub state directory.
func SupervisorIPCAddress(stateDir string) string {
	return filepath.Join(stateDir, "supervisor.sock")
}

// EnableSupervisorIPCTestPipeIsolation is a no-op on POSIX: the unix socket
// path already derives from the per-test stateDir (each test gets its own temp
// dir via MCPHUB_STATE_DIR_OVERRIDE), so concurrent in-process supervisors
// never collide on a shared address the way Windows' per-SID kernel pipe does.
// The symbol exists so cross-platform test setup (internal/cli TestMain) can
// call it unconditionally; see the Windows counterpart for the real isolation.
func EnableSupervisorIPCTestPipeIsolation() {}
