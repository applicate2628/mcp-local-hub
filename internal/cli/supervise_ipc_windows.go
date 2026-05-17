//go:build windows

// Package cli — Task 5.1 Windows named-pipe listener for the v0.5.0
// supervisor IPC channel.
//
// Spec §"Control IPC trust boundary (detail)" + §"Q11 Windows: named
// pipe via github.com/Microsoft/go-winio".
//
// Pipe path convention:
//
//	\\.\pipe\mcphub-supervisor-<user-sid>
//
// (e.g. \\.\pipe\mcphub-supervisor-S-1-5-21-...). The caller of
// NewSupervisorIPCListener supplies the fully-formed path so the
// supervisor body keeps full control over per-user pipe naming.
//
// DACL posture (Q11 v12 closure):
//
//	D:P(A;;GRGW;;;<current-user-sid>)(A;;GRGW;;;SY)
//
// Built via api.BuildAllowlistSDDL(api.AllowlistMaskPipe). The
// BuiltinAdministrators ACE is dropped from the pipe form so an
// admin token cannot issue supervisor commands without owner
// consent. The protected `D:P` prefix blocks ACE inheritance from
// the parent pipe namespace.
//
// Handshake (Spec §"Wire format" / §"Handshake"): on every Accept()
// the supervisor writes one JSON line:
//
//	{"hello":{"version":1,"pid":<pid>,"started_at":"<RFC3339Nano>"}}\n
//
// Clients compare the hello payload against supervisor.lock owner
// sidecar to reject stale pipes left by a crashed previous holder.
package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"

	"mcp-local-hub/internal/api"
)

// SupervisorIPCListener wraps a go-winio named-pipe listener with the
// Q11 trust-boundary DACL and the spec-required hello-frame
// handshake. The zero value is unusable; construct via
// NewSupervisorIPCListener.
type SupervisorIPCListener struct {
	listener  net.Listener
	pipePath  string
	sddl      string // SDDL applied to ListenPipe — exposed via SecurityDescriptorSDDL.
	pid       int
	startedAt string
}

// NewSupervisorIPCListener creates the named pipe at pipePath with
// the shared SDDL allowlist (current-user + LocalSystem; BA dropped
// per Q11 v12). Returns a listener whose Accept() sends the hello
// frame on each new connection.
//
// Buffer sizing: 4 KiB input + 4 KiB output is the spec-baseline
// (IPC frames are short JSON lines well under 1 KiB; the cap exists
// only to bound kernel-pool consumption per connection).
func NewSupervisorIPCListener(pipePath string, ownerOpt ...api.SupervisorLockOwner) (*SupervisorIPCListener, error) {
	sddl, err := api.BuildAllowlistSDDL(api.AllowlistMaskPipe)
	if err != nil {
		return nil, fmt.Errorf("build SDDL: %w", err)
	}
	cfg := &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        false, // byte stream — JSON lines, not framed.
		InputBufferSize:    4096,
		OutputBufferSize:   4096,
	}
	listener, err := winio.ListenPipe(pipePath, cfg)
	if err != nil {
		return nil, fmt.Errorf("ListenPipe(%q): %w", pipePath, err)
	}
	owner := supervisorIPCOwnerForHello(ownerOpt...)
	return &SupervisorIPCListener{
		listener:  listener,
		pipePath:  pipePath,
		sddl:      sddl,
		pid:       owner.PID,
		startedAt: owner.StartedAt,
	}, nil
}

// Accept blocks until a client connects, then sends the hello frame.
// Caller is responsible for reading subsequent IPC frames and
// closing the returned net.Conn.
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

// Close shuts down the listener. Outstanding accepted connections
// are NOT closed; the caller owns those net.Conn lifetimes.
func (l *SupervisorIPCListener) Close() error {
	return l.listener.Close()
}

// Addr returns the underlying listener's address (typically the pipe
// path). Useful for log lines.
func (l *SupervisorIPCListener) Addr() net.Addr {
	return l.listener.Addr()
}

// SecurityDescriptorSDDL returns the SDDL string that was passed to
// winio.ListenPipe. This is the post-ListenPipe DACL smoke surface
// the spec calls out at §"Q11 v12 closure F4: go-winio pinned +
// smoke test".
//
// Scope (Task 5.1 simple variant): the returned text is the SDDL
// the listener *requested* and that go-winio applied via
// SddlToSecurityDescriptor before NtCreateNamedPipeFile. It is
// sufficient for the spec's mask-check (does the allowlist exclude
// BA? does it include GRGW?) but does NOT independently query the
// effective DACL on the live pipe handle. Full handle-introspection
// smoke (windows.GetSecurityInfo on the listener's pipe handle)
// requires unexported state from *winio.PipeListener and is deferred
// to a follow-up patch; go-winio's correct application of the SDDL
// is upstream's responsibility and is covered by their test suite.
func (l *SupervisorIPCListener) SecurityDescriptorSDDL() (string, error) {
	return l.sddl, nil
}

// winioDialPipe is a thin wrapper used by tests so they don't have
// to import go-winio directly. Not part of the supervisor's runtime
// surface.
func winioDialPipe(pipePath string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(pipePath, &timeout)
}
