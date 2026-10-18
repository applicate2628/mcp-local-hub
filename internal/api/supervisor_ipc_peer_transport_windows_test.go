//go:build windows

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const peerTransportChildEnv = "MCPHUB_TEST_PEER_TRANSPORT_CHILD"

// Transport evidence only: these are ordinary Go test-helper processes, never
// qualified supervise/Product actors and never issuers of a private control proof.
func TestSupervisorPeerStrictControlConnections(t *testing.T) {
	if os.Getenv(peerTransportChildEnv) != "" || os.Getenv(callerStartTimeChildStateDirEnv) != "" {
		t.Fatal("unexpected test-helper environment")
	}
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("the Root-owned bounded test runner must supply a deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := peerTransportRequireJob(windows.CurrentProcess()); err != nil {
		t.Fatal(err)
	}

	originalDir := hardenedTempDir(t)
	wrongDir := hardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", originalDir)
	originalAddress := requirePeerTransportAddress(t, originalDir)
	original := startPeerTransportChild(t, originalDir, originalAddress, [2]string{"status", "exit"})
	originalIdentity := original.identity
	originalOwner := SupervisorLockOwner{PID: originalIdentity.PID, StartedAt: originalIdentity.CreationTime}
	original.configure(t, originalOwner)
	waitPeerTransportFile(t, ctx, filepath.Join(originalDir, "transport-ready"))
	writeSupervisorOwnerForTest(t, originalDir, originalOwner)
	if err := VerifySupervisorControlGenerationForPeerFromStateDir(ctx, originalDir, originalOwner, originalIdentity); err != nil {
		t.Fatalf("strict original-peer readiness refused: %v", err)
	}
	waitPeerTransportReceipt(t, ctx, originalDir, 1)
	if err := DialSupervisorIPCExitForPeer(ctx, originalDir, originalOwner, originalIdentity, 5000); err != nil {
		t.Fatalf("strict original-peer fresh exit refused: %v", err)
	}
	waitPeerTransportReceipt(t, ctx, originalDir, 2)

	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", wrongDir)
	wrongAddress := requirePeerTransportAddress(t, wrongDir)
	wrong := startPeerTransportChild(t, wrongDir, wrongAddress, [2]string{"none", "none"})
	if wrong.identity.PID == originalIdentity.PID {
		t.Fatal("distinct retained creators resolved to the same process")
	}
	wrong.configure(t, originalOwner)
	waitPeerTransportFile(t, ctx, filepath.Join(wrongDir, "transport-ready"))
	writeSupervisorOwnerForTest(t, wrongDir, originalOwner)

	// Each operation must execute the actual native observer/validator inside its
	// OWN connection, refusing the retained wrong server before any request bytes.
	if err := VerifySupervisorControlGenerationForPeerFromStateDir(ctx, wrongDir, originalOwner, originalIdentity); !errors.Is(err, ErrSupervisorControlPeerIdentity) {
		t.Fatalf("strict wrong-peer readiness did not refuse: %v", err)
	}
	waitPeerTransportReceipt(t, ctx, wrongDir, 1)
	if err := DialSupervisorIPCExitForPeer(ctx, wrongDir, originalOwner, originalIdentity, 5000); !errors.Is(err, ErrSupervisorControlPeerIdentity) {
		t.Fatalf("strict wrong-peer fresh exit did not refuse: %v", err)
	}
	waitPeerTransportReceipt(t, ctx, wrongDir, 2)
	for _, missing := range []SupervisorProcessIdentityV1{{}, {PID: originalIdentity.PID, CreationTime: originalIdentity.CreationTime, ImagePath: originalIdentity.ImagePath}} {
		if err := VerifySupervisorControlGenerationForPeerFromStateDir(ctx, wrongDir, originalOwner, missing); !errors.Is(err, ErrSupervisorControlPeerIdentity) {
			t.Fatalf("missing original readiness expectation did not refuse: %v", err)
		}
		if err := DialSupervisorIPCExitForPeer(ctx, wrongDir, originalOwner, missing, 5000); !errors.Is(err, ErrSupervisorControlPeerIdentity) {
			t.Fatalf("missing original exit expectation did not refuse: %v", err)
		}
	}
	t.Log("Strict operation returns received; address, command-byte and connection-count evidence is checked after each original child Wait during cleanup")
}

type peerTransportChild struct {
	input    io.WriteCloser
	identity SupervisorProcessIdentityV1
}

func (child peerTransportChild) configure(t *testing.T, owner SupervisorLockOwner) {
	t.Helper()
	if err := json.NewEncoder(child.input).Encode(owner); err != nil {
		t.Fatalf("configure retained transport helper: %v", err)
	}
}

func startPeerTransportChild(t *testing.T, stateDir, expectedAddress string, expectedCommands [2]string) peerTransportChild {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestSupervisorPeerNativeTransportChild$")
	// No SysProcAttr or breakaway flags: the children inherit Root's managed Job.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, "MCPHUB_STATE_DIR_OVERRIDE") && !strings.EqualFold(key, peerTransportChildEnv) && !strings.EqualFold(key, callerStartTimeChildStateDirEnv) {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, peerTransportChildEnv+"=1", "MCPHUB_STATE_DIR_OVERRIDE="+stateDir)
	// The exact parent's stderr/stdout retain helper failures through the existing
	// runner; there is no private transcript store or unbounded capture buffer.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		if closeErr := input.Close(); closeErr != nil {
			t.Errorf("close failed-start helper input: %v", closeErr)
		}
		t.Fatal(err)
	}
	// cmd.Process is the original creator-retained capability. This is its only
	// Wait. Cleanup closes stdin to stop its own listener/connections, never a PID.
	t.Cleanup(func() {
		closeErr := input.Close()
		waitErr := cmd.Wait()
		if err := errors.Join(closeErr, waitErr); err != nil {
			t.Errorf("original transport child settlement: %v", err)
		}
		// Exactly two connections per child follow from this accepted scenario:
		// original status+exit, or wrong status+exit. Invalid input must not dial.
		// Receive bytes only after this original child's Wait. Namespace existence
		// during the operation sequence is not a producer-completion fence.
		expectedFiles := []struct{ leaf, value string }{
			{"transport-ready", expectedAddress},
			{"transport-connections-total", "2"},
		}
		for i, command := range expectedCommands {
			expectedFiles = append(expectedFiles, struct{ leaf, value string }{
				fmt.Sprintf("transport-connection-%d-complete", i+1), command,
			})
		}
		for _, expected := range expectedFiles {
			raw, readErr := os.ReadFile(filepath.Join(stateDir, expected.leaf))
			if readErr != nil || string(raw) != expected.value {
				t.Errorf("settled transport evidence %s=%q, want %q; error=%v", expected.leaf, raw, expected.value, readErr)
			}
		}
	})
	identity, handle, err := peerTransportCreatorIdentity(cmd.Process)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Errorf("close retained query handle: %v", err)
		}
	})
	return peerTransportChild{input: input, identity: identity}
}

// Independent expected operand: query the original creator's still-live process,
// not a pipe peer or protocol frame. Retain this query-only handle during the test.
func peerTransportCreatorIdentity(proc *os.Process) (identity SupervisorProcessIdentityV1, handle windows.Handle, retErr error) {
	if proc == nil || proc.Pid <= 0 {
		return identity, 0, errors.New("missing original creator")
	}
	handle, retErr = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(proc.Pid))
	if retErr != nil {
		return identity, 0, retErr
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, windows.CloseHandle(handle))
			handle = 0
		}
	}()
	if retErr = peerTransportRequireJob(handle); retErr != nil {
		return identity, handle, retErr
	}
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return identity, handle, errors.Join(errors.New("original transport child is not confirmed alive"), err)
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return identity, handle, err
	}
	image, err := querySupervisorStatusImage(handle)
	if err != nil {
		return identity, handle, err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		return identity, handle, err
	}
	defer func() { retErr = errors.Join(retErr, token.Close()) }()
	tokenUser, err := token.GetTokenUser()
	if err != nil {
		return identity, handle, err
	}
	var session uint32
	if err := windows.ProcessIdToSessionId(uint32(proc.Pid), &session); err != nil {
		return identity, handle, err
	}
	integrity, err := supervisorStatusTokenIntegrityRID(token)
	if err != nil {
		return identity, handle, err
	}
	identity = SupervisorProcessIdentityV1{PID: proc.Pid, CreationTime: time.Unix(0, creation.Nanoseconds()).UTC().Format(time.RFC3339Nano), UserSID: tokenUser.User.Sid.String(), SessionID: session, IntegrityRID: integrity, ImagePath: image}
	return identity, handle, nil
}

func peerTransportRequireJob(handle windows.Handle) error {
	var member uint32
	probe := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	ok, _, err := probe.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return fmt.Errorf("transport helper Job membership unavailable: %w", err)
	}
	if member == 0 {
		return errors.New("transport fixture requires Root's managed Job")
	}
	return nil
}

func waitPeerTransportReceipt(t *testing.T, ctx context.Context, dir string, connection int) {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("transport-connection-%d-complete", connection))
	waitPeerTransportFile(t, ctx, path)
}

func waitPeerTransportFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond) // polling cadence, not an operation allowance.
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("transport helper evidence unavailable: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func requirePeerTransportAddress(t *testing.T, stateDir string) string {
	t.Helper()
	address := SupervisorIPCAddress(stateDir)
	if !strings.HasPrefix(address, `\\.\pipe\mcphub-supervisor-test-`) {
		t.Fatal("refusing a non-test supervisor transport")
	}
	return address
}

func TestSupervisorPeerNativeTransportChild(t *testing.T) {
	if os.Getenv(peerTransportChildEnv) != "1" {
		return
	}
	if err := peerTransportRequireJob(windows.CurrentProcess()); err != nil {
		t.Fatal(err)
	}
	stateDir := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE")
	if !filepath.IsAbs(stateDir) {
		t.Fatal("transport helper requires an explicit private state directory")
	}
	address := SupervisorIPCAddress(stateDir)
	if !strings.HasPrefix(address, `\\.\pipe\mcphub-supervisor-test-`) {
		t.Fatal("transport helper refuses a non-test pipe")
	}
	var hello SupervisorLockOwner
	if err := json.NewDecoder(os.Stdin).Decode(&hello); err != nil {
		// EOF is the parent cancelling a child before configuration.
		if !errors.Is(err, io.EOF) {
			t.Error(err)
		}
		return
	}
	listener, err := winio.ListenPipe(address, &winio.PipeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var closeListener sync.Once
	closePipe := func() {
		closeListener.Do(func() {
			if err := listener.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	defer closePipe()
	var mu sync.Mutex
	var closeCurrent func()
	stopping := false
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Error(err)
		}
		mu.Lock()
		stopping = true
		if closeCurrent != nil {
			closeCurrent()
		}
		mu.Unlock()
		closePipe()
	}()
	defer func() { <-controlDone }()
	if err := writePeerTransportEvidence(filepath.Join(stateDir, "transport-ready"), address); err != nil {
		t.Fatal(err)
	}
	for connection := 1; ; connection++ {
		conn, err := listener.Accept()
		if err != nil {
			mu.Lock()
			expected := stopping
			mu.Unlock()
			if !expected {
				t.Error(err)
			}
			if err := writePeerTransportEvidence(filepath.Join(stateDir, "transport-connections-total"), fmt.Sprintf("%d", connection-1)); err != nil {
				t.Error(err)
			}
			return
		}
		var closeOnce sync.Once
		closeConn := func() {
			closeOnce.Do(func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			})
		}
		mu.Lock()
		closeCurrent = closeConn
		if stopping {
			closeConn()
		}
		mu.Unlock()
		command, serveErr := servePeerTransportConn(conn, hello)
		closeConn()
		if serveErr != nil {
			t.Error(serveErr)
		}
		if err := writePeerTransportEvidence(filepath.Join(stateDir, fmt.Sprintf("transport-connection-%d-complete", connection)), command); err != nil {
			t.Error(err)
		}
		mu.Lock()
		closeCurrent = nil
		mu.Unlock()
	}
}

func servePeerTransportConn(conn net.Conn, hello SupervisorLockOwner) (string, error) {
	helloErr := json.NewEncoder(conn).Encode(map[string]any{"hello": IPCHello{Version: 1, PID: hello.PID, StartedAt: hello.StartedAt}})
	// Even if the strict client closed before receiving hello, read through EOF
	// and count actual received bytes before recording a no-request connection.
	counted := &peerTransportByteCounter{Reader: conn}
	var request IPCRequest
	if err := json.NewDecoder(counted).Decode(&request); err != nil {
		if peerTransportCleanDisconnect(err) && counted.bytes == 0 && peerTransportCleanDisconnect(helloErr) {
			return "none", nil
		}
		return "request-bytes-without-valid-frame", errors.Join(helloErr, err)
	}
	if helloErr != nil {
		return request.Cmd, helloErr
	}
	result := map[string]any{"state": "running", "daemons": []any{}}
	if request.Cmd == "exit" {
		if request.Args["graceful"] != true || request.Args["timeout_ms"] != float64(5000) {
			return request.Cmd, errors.New("strict exit changed graceful/timeout operands")
		}
		result = map[string]any{"graceful_exit_initiated": true}
	} else if request.Cmd != "status" {
		return request.Cmd, errors.New("unexpected transport command")
	}
	if err := json.NewEncoder(conn).Encode(IPCResponse{ID: request.ID, OK: true, Final: true, Result: result}); err != nil {
		return request.Cmd, err
	}
	return request.Cmd, nil
}

func peerTransportCleanDisconnect(err error) bool {
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)
}

type peerTransportByteCounter struct {
	io.Reader
	bytes int
}

func (r *peerTransportByteCounter) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func writePeerTransportEvidence(path, value string) error {
	// Each test-owned namespace has one writer and previously absent leaves.
	// Publish only after all bytes are written, so readiness is no timing proxy.
	if err := os.WriteFile(path+".pending", []byte(value), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".pending", path)
}
