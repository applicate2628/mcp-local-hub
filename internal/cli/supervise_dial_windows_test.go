//go:build windows

// Package cli — Task 6.2 Windows IPC test-client dial helper.
//
// Test-only surface: production callers (mcphub stop, mcphub status,
// mcphub migrate) own their own dialer with the supervisor.lock pre-
// read and handshake validation per spec §"Handshake". This helper
// exists so the supervise package's own end-to-end tests can talk to
// the IPC channel without pulling in the migration-side client code.
//
// On Windows the dial uses go-winio's DialPipe so the named-pipe path
// (`\\.\pipe\mcphub-supervisor-<USERNAME>`) is resolved through the
// same kernel namespace winio.ListenPipe created the pipe on. The
// SDDL allowlist applied at listener-create time gates non-owner
// CreateFileW attempts; this dial path is the owner-issued client.
package cli

import (
	"net"
	"time"
)

// dialSuperviseIPC dials the supervisor IPC channel at the given
// pipe/socket path. The 2-second timeout matches the test fixture's
// outer per-test budget — a hung dial would otherwise wedge the
// whole test process for the full -timeout window.
//
// Implementation delegates to winioDialPipe (defined in
// supervise_ipc_windows.go) so the test code does not import
// go-winio directly.
func dialSuperviseIPC(pipePath string) (net.Conn, error) {
	return winioDialPipe(pipePath, 2*time.Second)
}
