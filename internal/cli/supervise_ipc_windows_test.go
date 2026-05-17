//go:build windows

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSuperviseIPC_ListenerDACL verifies that NewSupervisorIPCListener
// creates a Windows named pipe whose effective DACL matches the
// v0.5.0 supervisor allowlist (current user + LocalSystem only).
// Spec §"Q11 Control IPC trust boundary": BuiltinAdministrators is
// dropped from the pipe ACE set per v13 Q11 closure.
func TestSuperviseIPC_ListenerDACL(t *testing.T) {
	// Use unique pipe path per test to avoid collision with other
	// supervisor instances or parallel test runs.
	pipePath := `\\.\pipe\mcphub-supervisor-test-` + sanitizeForPipe(t.Name())
	listener, err := NewSupervisorIPCListener(pipePath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Smoke test: SecurityDescriptorSDDL() returns the SDDL applied
	// to the pipe handle. Spec §"Q11 v12 closure F4" requires the
	// allowlist to include GRGW (GENERIC_READ + GENERIC_WRITE) and
	// to exclude BuiltinAdministrators ("BA").
	sddl, err := listener.SecurityDescriptorSDDL()
	if err != nil {
		t.Fatalf("SecurityDescriptorSDDL: %v", err)
	}
	if !strings.Contains(sddl, ";GRGW;") {
		t.Fatalf("pipe DACL missing GRGW mask: %s", sddl)
	}
	if strings.Contains(sddl, "BA") {
		t.Fatalf("pipe DACL must NOT include BuiltinAdministrators (v13 Q11): %s", sddl)
	}
	// Sanity: SDDL must include both ACE entries (current user SID +
	// LocalSystem `SY`) under the protected DACL.
	if !strings.Contains(sddl, "D:P(") {
		t.Fatalf("pipe DACL must be protected (D:P): %s", sddl)
	}
	if !strings.Contains(sddl, ";SY)") {
		t.Fatalf("pipe DACL missing LocalSystem (SY): %s", sddl)
	}
}

// TestSuperviseIPC_HandshakeSent verifies that Accept() sends the
// IPCHello frame containing version=1, the supervisor PID, and a
// non-empty StartedAt. Spec §"Wire format" + §"Handshake".
func TestSuperviseIPC_HandshakeSent(t *testing.T) {
	pipePath := `\\.\pipe\mcphub-supervisor-test-handshake-` + sanitizeForPipe(t.Name())
	listener, err := NewSupervisorIPCListener(pipePath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Spin up the server-side Accept in a goroutine so the test
	// can dial concurrently. Accept() writes the hello frame, then
	// the caller would normally drive the request/response loop;
	// here we close immediately after the hello write.
	acceptErrCh := make(chan error, 1)
	go func() {
		serverConn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		// Hold the connection open just long enough for the client
		// to finish reading the hello line; closing here would race
		// the client's Read.
		time.Sleep(200 * time.Millisecond)
		_ = serverConn.Close()
		acceptErrCh <- nil
	}()

	// Connect via go-winio client.
	timeout := 5 * time.Second
	clientConn, err := winioDialPipe(pipePath, timeout)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	// Client reads exactly one hello line.
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	rdr := bufio.NewReader(clientConn)
	helloLine, err := rdr.ReadString('\n')
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}

	// Parse hello frame as JSON to verify schema.
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
	// RFC3339Nano parse sanity.
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

// sanitizeForPipe converts a test name into a string safe to embed in
// a Windows named-pipe path. Pipe names allow most characters but `/`
// is treated as a path separator; the test name from t.Name() may
// contain `/` for subtests, so we replace it with `-`.
func sanitizeForPipe(name string) string {
	// Replace path separators and other problematic chars.
	repl := strings.NewReplacer("/", "-", `\`, "-", " ", "_")
	return repl.Replace(fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
}
