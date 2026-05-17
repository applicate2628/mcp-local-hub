//go:build !windows

// Package cli — Task 6.2 POSIX IPC test-client dial helper.
//
// Test-only surface: production callers (mcphub stop, mcphub status,
// mcphub migrate) own their own dialer with the supervisor.lock pre-
// read and handshake validation per spec §"Handshake". This helper
// exists so the supervise package's own end-to-end tests can talk to
// the IPC channel without pulling in the migration-side client code.
//
// On POSIX the dial is a plain net.Dial("unix", path) — the unix-
// socket inode at path is the only addressing the kernel needs. The
// chmod 0600 + parent-dir 0700 boundary applied at listener-create
// time gates non-owner connect(2) attempts; this dial path is the
// owner-issued client.
package cli

import (
	"net"
	"time"
)

// dialSuperviseIPC dials the supervisor IPC channel at the given
// pipe/socket path. The 2-second timeout matches the test fixture's
// outer per-test budget — a hung dial would otherwise wedge the
// whole test process for the full -timeout window.
func dialSuperviseIPC(pipePath string) (net.Conn, error) {
	return net.DialTimeout("unix", pipePath, 2*time.Second)
}
