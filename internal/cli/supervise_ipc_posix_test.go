//go:build !windows

package cli

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// TestSuperviseIPC_POSIXListenerMode0600 verifies that
// NewSupervisorIPCListener creates a unix-domain socket whose file
// mode is exactly 0600. Spec §"POSIX: unix socket at mode 0600" — the
// 0600 file mode IS the trust boundary on POSIX (Q11 POSIX
// equivalent of the Windows DACL allowlist).
func TestSuperviseIPC_POSIXListenerMode0600(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "supervisor.sock")
	listener, err := NewSupervisorIPCListener(socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("expected mode 0600, got %o", mode)
	}
	// Verify it's actually a socket file (not a regular file or
	// symlink — `net.Listen("unix", ...)` should produce a socket).
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("path %q is not a unix-domain socket (mode=%v)", socketPath, info.Mode())
	}
}

// TestSuperviseIPC_POSIXHelloHandshake verifies that Accept() sends
// the IPCHello frame containing version=1, the supervisor PID, and a
// non-empty StartedAt parseable as RFC3339Nano. Spec §"Wire format"
// + §"Handshake".
func TestSuperviseIPC_POSIXHelloHandshake(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "supervisor.sock")
	listener, err := NewSupervisorIPCListener(socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Spin up Accept in a goroutine so the test can dial concurrently.
	// Accept() writes the hello frame; the caller would normally drive
	// the request/response loop. Here we just hold the connection long
	// enough for the client to read the hello, then close.
	acceptErrCh := make(chan error, 1)
	go func() {
		serverConn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		// Hold the connection open just long enough for the client to
		// finish reading the hello line; closing immediately would
		// race the client's Read.
		time.Sleep(200 * time.Millisecond)
		_ = serverConn.Close()
		acceptErrCh <- nil
	}()

	clientConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))

	rdr := bufio.NewReader(clientConn)
	helloLine, err := rdr.ReadString('\n')
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}

	var frame struct {
		Hello struct {
			Version   int    `json:"version"`
			PID       int    `json:"pid"`
			StartedAt string `json:"started_at"`
		} `json:"hello"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(helloLine)), &frame); err != nil {
		t.Fatalf("parse hello (%q): %v", helloLine, err)
	}
	if frame.Hello.Version != 1 {
		t.Fatalf("hello.version = %d, want 1: %s", frame.Hello.Version, helloLine)
	}
	if frame.Hello.PID != os.Getpid() {
		t.Fatalf("hello.pid = %d, want %d: %s", frame.Hello.PID, os.Getpid(), helloLine)
	}
	if frame.Hello.StartedAt == "" {
		t.Fatalf("hello.started_at must be non-empty: %s", helloLine)
	}
	if _, err := time.Parse(time.RFC3339Nano, frame.Hello.StartedAt); err != nil {
		t.Fatalf("hello.started_at not RFC3339Nano (%q): %v", frame.Hello.StartedAt, err)
	}

	// Drain server goroutine.
	select {
	case err := <-acceptErrCh:
		if err != nil {
			t.Fatalf("server accept: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("server goroutine timed out")
	}
}

func TestSupervisorLockOwnerHelloConsistency(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "supervisor.sock")
	owner := api.SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-16T18:00:00.000000000Z"}
	listener, err := NewSupervisorIPCListener(socketPath, owner)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	acceptErrCh := make(chan error, 1)
	go func() {
		serverConn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		time.Sleep(200 * time.Millisecond)
		_ = serverConn.Close()
		acceptErrCh <- nil
	}()

	clientConn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))

	helloLine, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	var frame struct {
		Hello api.IPCHello `json:"hello"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(helloLine)), &frame); err != nil {
		t.Fatalf("parse hello (%q): %v", helloLine, err)
	}
	if frame.Hello.PID != owner.PID || frame.Hello.StartedAt != owner.StartedAt {
		t.Fatalf("hello owner mismatch: got pid=%d started_at=%q want pid=%d started_at=%q",
			frame.Hello.PID, frame.Hello.StartedAt, owner.PID, owner.StartedAt)
	}

	select {
	case err := <-acceptErrCh:
		if err != nil {
			t.Fatalf("server accept: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server goroutine timed out")
	}
}

// TestSuperviseIPC_POSIXStaleSocketRemoved verifies that
// NewSupervisorIPCListener succeeds even when a stale socket file
// already exists at the target path (typical case: previous
// supervisor crashed without unlinking).
func TestSuperviseIPC_POSIXStaleSocketRemoved(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "supervisor.sock")

	// First listener — establishes the socket file.
	l1, err := NewSupervisorIPCListener(socketPath)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	// Close WITHOUT removing the socket file; simulates the listener
	// closing but the inode still present (Go's *net.UnixListener
	// usually unlinks on Close, so we manually create one to simulate
	// a stale socket).
	_ = l1.Close()

	// Force-leave a stale plain-file at the socket path so the bind
	// would otherwise EADDRINUSE. Production stale-socket recovery is
	// already exercised; this test verifies the os.Remove pre-step.
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("create stale file: %v", err)
	}

	// Second listener — should succeed because the constructor
	// removes the stale entry before bind.
	l2, err := NewSupervisorIPCListener(socketPath)
	if err != nil {
		t.Fatalf("second listen (stale recovery): %v", err)
	}
	defer l2.Close()

	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat post-recovery: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("post-recovery path %q is not a socket (mode=%v)", socketPath, info.Mode())
	}
}

// TestSuperviseIPC_POSIXSocketPathTooLongRefusedWithActionableError verifies
// that NewSupervisorIPCListener pre-validates the socket path against the
// platform sun_path limit and refuses BEFORE calling net.Listen, returning an
// actionable error (naming "too long" and the limit) instead of letting
// net.Listen fail with an opaque ENAMETOOLONG-derived error that names
// neither. A long state-dir path (deep nesting, long username, corporate
// profile redirect) is the realistic trigger.
func TestSuperviseIPC_POSIXSocketPathTooLongRefusedWithActionableError(t *testing.T) {
	dir := t.TempDir()
	// Build a path comfortably past both platform limits (104 darwin / 108
	// linux) regardless of how long t.TempDir()'s own prefix is.
	longComponent := strings.Repeat("a", 200)
	socketPath := filepath.Join(dir, longComponent, "supervisor.sock")

	_, err := NewSupervisorIPCListener(socketPath)
	if err == nil {
		t.Fatalf("expected NewSupervisorIPCListener to refuse an over-limit socket path")
	}
	msg := err.Error()
	if !strings.Contains(msg, "too long") {
		t.Errorf("error message %q missing expected pre-validation wording \"too long\" — net.Listen may have run instead of the pre-validation gate", msg)
	}
}

// TestSuperviseIPC_POSIXSecurityDescriptorEmpty verifies that
// SecurityDescriptorSDDL returns an empty string on POSIX — the
// Q11 trust boundary is the 0600 file mode + owner-only parent dir,
// not an SDDL. Cross-platform call sites can switch on the empty
// return to skip Windows-only DACL assertions.
func TestSuperviseIPC_POSIXSecurityDescriptorEmpty(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "supervisor.sock")
	listener, err := NewSupervisorIPCListener(socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	sddl, err := listener.SecurityDescriptorSDDL()
	if err != nil {
		t.Fatalf("SecurityDescriptorSDDL: %v", err)
	}
	if sddl != "" {
		t.Fatalf("POSIX SDDL must be empty, got %q", sddl)
	}
}
