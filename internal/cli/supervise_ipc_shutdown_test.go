package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// heldAcceptExit starts the real supervisor with its closed-Accept result
// parked immediately before the existing ipc-accept-exit emission. Cleanup
// releases the barrier and joins the invocation even when an assertion fails.
type heldAcceptExit struct {
	stateDir     string
	logPath      string
	acceptClosed chan struct{}
	done         chan error
	release      func()
}

func startHeldAcceptExit(t *testing.T) *heldAcceptExit {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	acceptClosed := make(chan struct{})
	releaseAccept := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAccept) }) }
	done := make(chan error, 1)
	returned := make(chan struct{})
	logPath := filepath.Join(stateDir, "supervisor-events.log")
	go func() {
		done <- runSuperviseWithIPCOptions(ctx, false, false, false, ipcRunOptions{
			afterAccept: func(_ net.Conn, err error) {
				if errors.Is(err, net.ErrClosed) {
					close(acceptClosed)
					<-releaseAccept
				}
			},
		})
		close(returned)
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-returned:
		case <-time.After(3 * time.Second):
			t.Error("held-accept supervisor did not return during cleanup")
		}
		select {
		case <-acceptClosed:
			waitForSupervisorEvent(t, logPath, `"event":"ipc-accept-exit"`, 3*time.Second)
		default:
		}
	})
	waitForSupervisorEvent(t, logPath, `"event":"ipc-listener-bound"`, 3*time.Second)
	return &heldAcceptExit{stateDir: stateDir, logPath: logPath, acceptClosed: acceptClosed, done: done, release: release}
}

func requireHeldAcceptReturnFence(t *testing.T, f *heldAcceptExit) {
	t.Helper()
	select {
	case <-f.acceptClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("closed Accept did not reach the pre-emit barrier")
	}
	select {
	case err := <-f.done:
		t.Fatalf("supervisor returned before ipc-accept-exit could emit: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	f.release()
	select {
	case err := <-f.done:
		if err != nil {
			t.Fatalf("supervisor return = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not return after accept release")
	}
	// The row must already be durable at return, not appended afterward.
	body, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var row struct {
			Event    string `json:"event"`
			Severity string `json:"severity"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decode event row: %v", err)
		}
		if row.Event == "ipc-accept-exit" {
			count++
			if row.Severity != "info" {
				t.Fatalf("ipc-accept-exit severity = %q, want info", row.Severity)
			}
		}
	}
	if count != 1 {
		t.Fatalf("ipc-accept-exit rows at return = %d, want 1", count)
	}
}

func TestRunSupervise_IPCShutdownJoinsAcceptExitOnTestExit(t *testing.T) {
	exitCh := make(chan struct{}, 1)
	t.Cleanup(setSuperviseTestExitCh(exitCh))
	f := startHeldAcceptExit(t)
	exitCh <- struct{}{}
	requireHeldAcceptReturnFence(t, f)
	body, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"signal":"test-exit"`) {
		t.Fatalf("test-exit lifecycle row missing: %s", body)
	}
}

func TestRunSupervise_IPCShutdownJoinsAcceptExitOnGracefulIPC(t *testing.T) {
	f := startHeldAcceptExit(t)
	conn, err := dialSuperviseIPC(defaultPipePathOS(f.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	waitForSupervisorReadyIPC(t, conn, reader)
	reqBody, err := json.Marshal(api.IPCRequest{ID: 43, Cmd: "exit", Args: map[string]any{"graceful": true, "timeout_ms": 5000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		t.Fatalf("write graceful exit: %v", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read graceful acknowledgement: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		t.Fatalf("decode graceful acknowledgement: %v", err)
	}
	result, ok := resp.Result.(map[string]any)
	if resp.ID != 43 || !resp.OK || !resp.Final || !ok || result["graceful_exit_initiated"] != true {
		t.Fatalf("graceful acknowledgement = %+v, want final OK id=43 and graceful_exit_initiated=true", resp)
	}
	requireHeldAcceptReturnFence(t, f)
	body, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"signal":"ipc-exit-graceful"`) {
		t.Fatalf("graceful IPC lifecycle row missing: %s", body)
	}
}

// A live handler's completion is part of the same return fence, including
// time after its connection is closed and its request loop has returned.
func TestRunSupervise_IPCShutdownJoinsHandler(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handlerReturning := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHandler) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseWithIPCOptions(ctx, false, false, false, ipcRunOptions{
			beforeHandlerDone: func(net.Conn) {
				close(handlerReturning)
				<-releaseHandler
			},
		})
	}()

	logPath := filepath.Join(stateDir, "supervisor-events.log")
	waitForSupervisorEvent(t, logPath, `"event":"ipc-listener-bound"`, 3*time.Second)
	conn, err := dialSuperviseIPC(defaultPipePathOS(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	cancel()
	select {
	case <-handlerReturning:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted handler did not reach completion barrier")
	}
	select {
	case err := <-done:
		t.Fatalf("runSupervise returned before handler settled: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("runSupervise error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not settle after handler release")
	}
}

func TestRunSupervise_IPCShutdownRejectsPendingAccept(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	accepted := make(chan struct{})
	sealed := make(chan struct{})
	handlerFinished := make(chan struct{}, 1)
	releaseAccept := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAccept) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseWithIPCOptions(ctx, false, false, false, ipcRunOptions{
			afterAccept: func(conn net.Conn, err error) {
				if err == nil && conn != nil {
					close(accepted)
					<-releaseAccept
				}
			},
			afterStopSeal:     func() { close(sealed) },
			beforeHandlerDone: func(net.Conn) { handlerFinished <- struct{}{} },
		})
	}()

	logPath := filepath.Join(stateDir, "supervisor-events.log")
	waitForSupervisorEvent(t, logPath, `"event":"ipc-listener-bound"`, 3*time.Second)
	conn, err := dialSuperviseIPC(defaultPipePathOS(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("client connection was not accepted")
	}
	cancel()
	select {
	case <-sealed:
	case <-time.After(3 * time.Second):
		t.Fatal("IPC admission was not sealed")
	}
	release()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("runSupervise error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not join pending Accept")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if line, err := bufio.NewReader(conn).ReadString('\n'); err == nil || line != "" {
		t.Fatalf("rejected connection received a hello frame: line=%q err=%v", line, err)
	}
	select {
	case <-handlerFinished:
		t.Fatal("rejected connection launched a handler")
	default:
	}
}

// A closed Accept result must finish its existing event emission before the
// supervisor returns and releases the event log's lifetime.
func TestRunSupervise_IPCShutdownJoinsAcceptExit(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	acceptClosed := make(chan struct{})
	releaseAccept := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAccept) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- runSuperviseWithIPCOptions(ctx, false, false, false, ipcRunOptions{
			afterAccept: func(_ net.Conn, err error) {
				if errors.Is(err, net.ErrClosed) {
					close(acceptClosed)
					<-releaseAccept
				}
			},
		})
	}()

	if !waitForFile(filepath.Join(stateDir, "supervisor.lock.owner.json"), 3*time.Second) {
		t.Fatal("supervisor lock owner did not appear")
	}
	// The listener binds after the lock; its bound event is the startup
	// checkpoint needed before cancellation closes Accept.
	logPath := filepath.Join(stateDir, "supervisor-events.log")
	waitForSupervisorEvent(t, logPath, `"event":"ipc-listener-bound"`, 3*time.Second)
	cancel()
	select {
	case <-acceptClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("closed Accept did not reach the barrier")
	}

	// On the old implementation the caller returns while the accept goroutine
	// is held here. The only time bound is a test watchdog, not a production
	// shutdown deadline.
	select {
	case err := <-done:
		t.Fatalf("runSupervise returned before ipc-accept-exit could emit: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("runSupervise error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runSupervise did not settle after accept release")
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), `"event":"ipc-accept-exit"`) != 1 || !strings.Contains(string(body), `"severity":"info"`) {
		t.Fatalf("joined closed-Accept event missing or changed: %s", body)
	}
}
