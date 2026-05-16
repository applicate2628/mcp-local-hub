//go:build !windows

// Package cli — Task 6.1 POSIX socket-path helper for the supervisor
// IPC channel.
//
// Spec §"POSIX: unix socket at mode 0600" + plan Task 6.1.
//
// Socket path: `<state-dir>/supervisor.sock`. The parent state dir is
// already 0700 (single-user boundary set by the state-file helper),
// and the socket inode itself is chmodded to 0600 inside
// NewSupervisorIPCListener (see supervise_ipc_posix.go). Both layers
// together form the Q11 POSIX equivalent of the Windows SDDL DACL.
package cli

import "path/filepath"

// defaultPipePathOS returns the POSIX unix-domain-socket path for the
// supervisor IPC channel. stateDir MUST be a writable per-user state
// directory; the helper does NOT create it (api.DaemonStateDir() is
// the owning creator).
func defaultPipePathOS(stateDir string) string {
	return filepath.Join(stateDir, "supervisor.sock")
}
