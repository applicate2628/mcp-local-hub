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
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
)

// SupervisorIPCListener wraps a go-winio named-pipe listener with the
// Q11 trust-boundary DACL and the spec-required hello-frame
// handshake. The zero value is unusable; construct via
// NewSupervisorIPCListener.
type SupervisorIPCListener struct {
	listener  net.Listener
	pipePath  string
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

// SecurityDescriptorSDDL returns the live pipe handle's effective security
// descriptor in SDDL form. This is the post-ListenPipe DACL smoke surface the
// spec calls out at §"Q11 v12 closure F4: go-winio pinned + smoke test".
func (l *SupervisorIPCListener) SecurityDescriptorSDDL() (string, error) {
	serverConn, clientConn, err := l.openSecurityProbePipe()
	if err != nil {
		return "", err
	}
	defer serverConn.Close()
	defer clientConn.Close()
	fdConn, ok := serverConn.(interface{ Fd() uintptr })
	if !ok {
		return "", fmt.Errorf("security probe pipe %T does not expose Fd", serverConn)
	}
	handle := windows.Handle(fdConn.Fd())
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("GetSecurityInfo pipe handle: %w", err)
	}
	if sd == nil {
		return "", fmt.Errorf("GetSecurityInfo pipe handle returned nil security descriptor")
	}
	sddl := sd.String()
	if sddl == "" {
		return "", fmt.Errorf("convert live pipe security descriptor to SDDL")
	}
	return sddl, nil
}

func (l *SupervisorIPCListener) openSecurityProbePipe() (net.Conn, net.Conn, error) {
	if l == nil || l.listener == nil {
		return nil, nil, fmt.Errorf("supervisor IPC listener is nil")
	}
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		conn, err := l.listener.Accept()
		acceptCh <- acceptResult{conn: conn, err: err}
	}()

	timeout := 2 * time.Second
	clientConn, err := winio.DialPipe(l.pipePath, &timeout)
	if err != nil {
		return nil, nil, fmt.Errorf("dial security probe pipe: %w", err)
	}

	select {
	case res := <-acceptCh:
		if res.err != nil {
			_ = clientConn.Close()
			return nil, nil, fmt.Errorf("accept security probe pipe: %w", res.err)
		}
		return res.conn, clientConn, nil
	case <-time.After(timeout):
		_ = clientConn.Close()
		return nil, nil, fmt.Errorf("accept security probe pipe timed out")
	}
}

// winioDialPipe is a thin wrapper used by tests so they don't have
// to import go-winio directly. Not part of the supervisor's runtime
// surface.
func winioDialPipe(pipePath string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(pipePath, &timeout)
}
