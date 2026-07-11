//go:build windows

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"

	"golang.org/x/sys/windows"
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

	sddl, err := listener.SecurityDescriptorSDDL()
	if err != nil {
		t.Fatalf("SecurityDescriptorSDDL: %v", err)
	}
	if !strings.Contains(sddl, "O:") {
		t.Fatalf("pipe security descriptor must come from the live handle and include owner info: %s", sddl)
	}

	wantSDDL, err := api.BuildAllowlistSDDL(api.AllowlistMaskPipe)
	if err != nil {
		t.Fatalf("BuildAllowlistSDDL: %v", err)
	}
	wantSD, err := windows.SecurityDescriptorFromString(wantSDDL)
	if err != nil {
		t.Fatalf("parse expected SDDL: %v", err)
	}
	gotDACL := mustSDDLDACLSection(t, sddl)
	wantDACL := mustSDDLDACLSection(t, wantSD.String())
	gotACEs := mustDACLAllowACEs(t, gotDACL)
	wantACEs := mustDACLAllowACEs(t, wantDACL)
	if len(gotACEs) != len(wantACEs) {
		t.Fatalf("pipe live DACL ACE count mismatch:\n got: %s\nwant: %s\nfull: %s", gotDACL, wantDACL, sddl)
	}
	for trustee, wantMask := range wantACEs {
		gotMask, ok := gotACEs[trustee]
		if !ok {
			t.Fatalf("pipe live DACL missing trustee %s:\n got: %s\nwant: %s", trustee, gotDACL, wantDACL)
		}
		if mustPipeReadWriteMask(t, gotMask) != mustPipeReadWriteMask(t, wantMask) {
			t.Fatalf("pipe live DACL mask mismatch for %s: got %s want %s", trustee, gotMask, wantMask)
		}
	}
	if strings.Contains(sddl, "BA") {
		t.Fatalf("pipe DACL must NOT include BuiltinAdministrators (v13 Q11): %s", sddl)
	}
}

func mustSDDLDACLSection(t *testing.T, sddl string) string {
	t.Helper()
	start := strings.Index(sddl, "D:")
	if start < 0 {
		t.Fatalf("SDDL missing DACL section: %s", sddl)
	}
	end := len(sddl)
	for _, marker := range []string{"O:", "G:", "S:"} {
		if idx := strings.Index(sddl[start+2:], marker); idx >= 0 && start+2+idx < end {
			end = start + 2 + idx
		}
	}
	return sddl[start:end]
}

func mustDACLAllowACEs(t *testing.T, dacl string) map[string]string {
	t.Helper()
	if !strings.HasPrefix(dacl, "D:P") {
		t.Fatalf("DACL must be protected: %s", dacl)
	}
	rest := strings.TrimPrefix(dacl, "D:P")
	aces := map[string]string{}
	for rest != "" {
		if !strings.HasPrefix(rest, "(") {
			t.Fatalf("unexpected DACL ACE encoding in %s", dacl)
		}
		end := strings.Index(rest, ")")
		if end < 0 {
			t.Fatalf("unterminated DACL ACE in %s", dacl)
		}
		fields := strings.Split(rest[1:end], ";")
		if len(fields) != 6 {
			t.Fatalf("unexpected DACL ACE field count in %q", rest[1:end])
		}
		if fields[0] != "A" {
			t.Fatalf("unexpected non-allow ACE in %q", rest[1:end])
		}
		aces[fields[5]] = fields[2]
		rest = rest[end+1:]
	}
	return aces
}

func mustPipeReadWriteMask(t *testing.T, mask string) uint32 {
	t.Helper()
	switch mask {
	case "GRGW", "GWGR":
		return uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE)
	default:
		n, err := strconv.ParseUint(mask, 0, 32)
		if err != nil {
			t.Fatalf("unsupported pipe DACL mask %q: %v", mask, err)
		}
		return uint32(n)
	}
}

// TestSuperviseIPC_HandshakeSent verifies that the supervisor sends
// the IPCHello frame containing version=1, the supervisor PID, and a
// non-empty StartedAt. Spec §"Wire format" + §"Handshake". The hello
// frame is written by WriteHello after Accept (production drives this
// from serveIPCConn); the server goroutine below mirrors that ordering.
func TestSuperviseIPC_HandshakeSent(t *testing.T) {
	pipePath := `\\.\pipe\mcphub-supervisor-test-handshake-` + sanitizeForPipe(t.Name())
	listener, err := NewSupervisorIPCListener(pipePath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Spin up the server-side Accept in a goroutine so the test
	// can dial concurrently. WriteHello writes the hello frame (moved
	// off Accept into the per-connection serving goroutine), then the
	// caller would normally drive the request/response loop; here we
	// close immediately after the hello write.
	acceptErrCh := make(chan error, 1)
	go func() {
		serverConn, err := listener.Accept()
		if err != nil {
			acceptErrCh <- err
			return
		}
		if err := listener.WriteHello(serverConn); err != nil {
			_ = serverConn.Close()
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

func TestSupervisorLockOwnerHelloConsistency(t *testing.T) {
	pipePath := `\\.\pipe\mcphub-supervisor-test-owner-` + sanitizeForPipe(t.Name())
	owner := api.SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-16T18:00:00.000000000Z"}
	listener, err := NewSupervisorIPCListener(pipePath, owner)
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
		if err := listener.WriteHello(serverConn); err != nil {
			_ = serverConn.Close()
			acceptErrCh <- err
			return
		}
		time.Sleep(200 * time.Millisecond)
		_ = serverConn.Close()
		acceptErrCh <- nil
	}()

	clientConn, err := winioDialPipe(pipePath, 5*time.Second)
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

// sanitizeForPipe converts a test name into a string safe to embed in
// a Windows named-pipe path. Pipe names allow most characters but `/`
// is treated as a path separator; the test name from t.Name() may
// contain `/` for subtests, so we replace it with `-`.
func sanitizeForPipe(name string) string {
	// Replace path separators and other problematic chars.
	repl := strings.NewReplacer("/", "-", `\`, "-", " ", "_")
	return repl.Replace(fmt.Sprintf("%s-%d", name, time.Now().UnixNano()))
}
