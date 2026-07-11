//go:build !windows

// Package cli — Task 5.2 POSIX unix-socket listener for the v0.5.0
// supervisor IPC channel.
//
// Spec §"Control IPC trust boundary (detail)" + §"POSIX: unix socket
// at mode 0600".
//
// Socket path convention: caller supplies the fully-formed
// `<state-dir>/supervisor.sock` path so the supervisor body keeps
// full control over per-user socket naming (same shape as the
// Windows named-pipe path on Task 5.1).
//
// Trust boundary (Q11 POSIX equivalent of the Windows SDDL DACL):
//
//   - Socket file mode 0600 — readable/writable by owner only,
//     blocking any other principal from connect(2).
//   - On Linux/BSD the parent state directory is already 0700
//     (single-user boundary set by the state-file helper); the
//     0600 socket inside that directory is owner-only by both
//     parent-dir reachability AND the socket's own mode.
//
// SO_PEERCRED-based owner-uid verification at Accept() is documented
// as a follow-up for full belt-and-braces parity with the Windows
// DACL; the v0.5.0 POSIX preview ships behind the 0600 + parent-dir
// gate because Linux/macOS targets are beta-only per Q9.
//
// Handshake (Spec §"Wire format" / §"Handshake"): per accepted
// connection the supervisor writes one JSON line (via WriteHello, from
// the per-connection serveIPCConn goroutine — no longer inside Accept):
//
//	{"hello":{"version":1,"pid":<pid>,"started_at":"<RFC3339Nano>"}}\n
//
// Clients compare the hello payload against supervisor.lock owner
// sidecar to reject stale sockets left by a crashed previous holder.
package cli

import (
	"fmt"
	"net"
	"os"

	"mcp-local-hub/internal/api"
)

// SupervisorIPCListener wraps a unix-domain-socket listener with the
// Q11 trust-boundary mode 0600 + the spec-required hello-frame
// handshake. The zero value is unusable; construct via
// NewSupervisorIPCListener.
//
// API mirrors the Windows variant (Task 5.1) so call sites in
// internal/cli/supervise.go can be platform-agnostic.
type SupervisorIPCListener struct {
	listener   net.Listener
	socketPath string
	pid        int
	startedAt  string
}

// NewSupervisorIPCListener creates a unix-domain-socket listener at
// socketPath with mode 0600. A pre-existing socket file at the same
// path is removed first (typical case: previous supervisor crashed
// without unlinking; the stale-PID check at the lock layer already
// gated this call, so the unlink is safe here).
//
// On any failure after net.Listen, the listener is closed before
// the error returns so a partially-bound socket is never leaked.
func NewSupervisorIPCListener(socketPath string, ownerOpt ...api.SupervisorLockOwner) (*SupervisorIPCListener, error) {
	// Pre-validate against the platform sun_path limit BEFORE net.Listen: an
	// unusually long state directory can push socketPath past the kernel's
	// fixed sockaddr_un buffer, and net.Listen's resulting error names
	// neither the limit nor the actual path length. Failing here with an
	// actionable message beats a cryptic bind error. See
	// api.ValidateSupervisorIPCSocketPathLen's doc for the platform
	// constants (Linux 108 / Darwin 104, NUL-terminator-inclusive).
	if err := api.ValidateSupervisorIPCSocketPathLen(socketPath); err != nil {
		return nil, err
	}

	// Best-effort remove stale socket — Linux refuses bind on EADDRINUSE
	// if the inode already exists, so this is required even on the
	// happy path of a normal restart.
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen unix(%q): %w", socketPath, err)
	}
	// Mode 0600 — owner-only. Applied AFTER bind so the umask cannot
	// widen the resulting mode. On Linux unix-socket file permissions
	// are honored by connect(2) — non-owner uids get EACCES.
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod 0600(%q): %w", socketPath, err)
	}
	owner := supervisorIPCOwnerForHello(ownerOpt...)
	return &SupervisorIPCListener{
		listener:   listener,
		socketPath: socketPath,
		pid:        owner.PID,
		startedAt:  owner.StartedAt,
	}, nil
}

// Accept blocks until a client connects and returns the raw net.Conn.
// The hello handshake frame is NO LONGER written here: it moved into
// the per-connection serveIPCConn goroutine (via WriteHello) so a slow
// or abandoned client can no longer block the supervisor's single
// accept loop while the server writes the hello. The caller
// (serveIPCConn) writes the hello frame, reads subsequent IPC frames,
// and closes the returned net.Conn.
//
// Note on owner-uid checks: full SO_PEERCRED verification (Linux) or
// LOCAL_PEERCRED (BSD/Darwin) is documented as a follow-up — for the
// v0.5.0 POSIX preview the trust boundary is the 0600 mode + the
// owner-only parent directory. Documented at the package doc above.
func (l *SupervisorIPCListener) Accept() (net.Conn, error) {
	return l.listener.Accept()
}

// Close shuts down the listener. The unix-socket inode is removed
// by the stdlib when Close runs on the wrapped *net.UnixListener.
// Outstanding accepted connections are NOT closed; the caller owns
// those net.Conn lifetimes.
func (l *SupervisorIPCListener) Close() error {
	return l.listener.Close()
}

// Addr returns the underlying listener's address (the unix-socket
// path). Useful for log lines.
func (l *SupervisorIPCListener) Addr() net.Addr {
	return l.listener.Addr()
}

// SecurityDescriptorSDDL is a no-op on POSIX. Returns the empty
// string; the trust-boundary equivalent on POSIX is the 0600 file
// mode applied at NewSupervisorIPCListener time + the owner-only
// parent directory. The cross-platform call site can inspect for an
// empty string to skip Windows-specific DACL assertions.
func (l *SupervisorIPCListener) SecurityDescriptorSDDL() (string, error) {
	return "", nil
}
