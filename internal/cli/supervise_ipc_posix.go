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
// Handshake (Spec §"Wire format" / §"Handshake"): on every Accept()
// the supervisor writes one JSON line:
//
//	{"hello":{"version":1,"pid":<pid>,"started_at":"<RFC3339Nano>"}}\n
//
// Clients compare the hello payload against supervisor.lock owner
// sidecar to reject stale sockets left by a crashed previous holder.
package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

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
func NewSupervisorIPCListener(socketPath string) (*SupervisorIPCListener, error) {
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
	return &SupervisorIPCListener{
		listener:   listener,
		socketPath: socketPath,
		pid:        os.Getpid(),
		startedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// Accept blocks until a client connects, then sends the hello frame.
// Caller is responsible for reading subsequent IPC frames and
// closing the returned net.Conn.
//
// Note on owner-uid checks: full SO_PEERCRED verification (Linux) or
// LOCAL_PEERCRED (BSD/Darwin) is documented as a follow-up — for the
// v0.5.0 POSIX preview the trust boundary is the 0600 mode + the
// owner-only parent directory. Documented at the package doc above.
//
// On any error writing the hello, the connection is closed and the
// error is returned so the supervisor's accept loop can log + retry.
func (l *SupervisorIPCListener) Accept() (net.Conn, error) {
	conn, err := l.listener.Accept()
	if err != nil {
		return nil, err
	}
	hello := api.IPCHello{
		Version:   1,
		PID:       l.pid,
		StartedAt: l.startedAt,
	}
	frame := map[string]any{"hello": hello}
	body, err := json.Marshal(frame)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("marshal hello: %w", err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write hello: %w", err)
	}
	return conn, nil
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
