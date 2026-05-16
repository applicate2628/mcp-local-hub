// Package cli — Task 6.1 supervise-command tests.
//
// Spec §"Q1 Lifecycle owner" + §"Q7 Supervisor invocation" + plan
// Task 6.1.
//
// These tests cover the Phase-6 scope: the supervise command MUST
// acquire <state-dir>/supervisor.lock, open the audit log, run the
// FIFO event loop, and exit cleanly when the graceful-exit pathway
// is triggered. IPC dispatch, reconciliation, and quiesce-drain
// belong to later tasks and are out of scope here — every test in
// this file passes `--no-ipc` so the listener does not bind a real
// pipe/socket.
//
// Why a test-only exit channel instead of `os.Process.Signal(Interrupt)`:
// Go's stdlib documents Process.Signal(os.Interrupt) as "not
// supported" for self-signaling on Windows (see Go source
// src/os/exec_windows.go). The plan's original TDD scaffold called
// `currentProc.Signal(os.Interrupt)` which works only on POSIX. The
// supervise body exposes `setSuperviseTestExitCh` as a package-private
// seam so cross-platform tests can trigger the same graceful-exit
// flow without relying on a working self-signal primitive. The seam
// is documented as test-only in supervise.go and is never wired by
// production callers.
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

// TestSuperviseCommand_AcquiresLockAndExitsOnSignal verifies the
// happy-path lifecycle: start → lock acquired (owner sidecar
// present) → graceful-exit triggered via the test seam → err=nil.
//
// The test asserts the same end state a real SIGINT delivery would
// have produced: the owner sidecar is removed (Release ran),
// supervisor-events.log exists (the audit channel opened).
func TestSuperviseCommand_AcquiresLockAndExitsOnSignal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	exitCh := make(chan struct{}, 1)
	cleanup := setSuperviseTestExitCh(exitCh)
	defer cleanup()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for the lock-owner sidecar to appear. AcquireSupervisorLock
	// writes <path>.owner.json after taking the flock, so its presence
	// is a reliable "startup reached the lock step" signal that does
	// not race with the open-event-log / IPC-bind phases.
	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 2*time.Second) {
		t.Fatalf("supervisor.lock.owner.json never appeared under %s", tmpHome)
	}

	// Trigger graceful exit via the test seam. The supervise body
	// selects on this channel in parallel with the real signal
	// channel and runs identical exit code on either path.
	exitCh <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervise did not exit on test-exit signal within 3s")
	}

	// After Release(), the lock sidecar is removed (see
	// SupervisorLock.Release in internal/api/supervisor_lock.go:104).
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar removed after graceful exit; stat err=%v", err)
	}

	// Audit log should exist with at least the supervisor-start
	// self-event. We don't assert on full content here — the audit
	// channel is exercised by its own tests in internal/api — but
	// presence proves the supervise body reached OpenSupervisorEventLog
	// + Emit before exiting.
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	if _, err := os.Stat(eventsPath); err != nil {
		t.Fatalf("supervisor-events.log never appeared: %v", err)
	}
}

// TestSuperviseCommand_RefusesSecondInstance verifies the singleton
// invariant: a second supervise process with the same state dir
// receives a non-nil error from AcquireSupervisorLock and exits
// before binding the IPC listener.
//
// On POSIX the second instance hits "supervisor.lock held by live
// PID N" (kill(self, 0) returns success → isOwnerLive=true).
// On Windows the second instance hits "flock reclaim" failure
// because Signal(0) is unsupported on Windows even for self-probe,
// so isOwnerLive returns false and the reclaim retry fails since
// the first holder's flock is still active. Both paths produce a
// non-nil error from runSupervise; the exact message diverges but
// the singleton invariant holds.
func TestSuperviseCommand_RefusesSecondInstance(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	exitCh := make(chan struct{}, 1)
	cleanup := setSuperviseTestExitCh(exitCh)
	defer cleanup()

	// Start the first supervise instance and wait for its sidecar
	// to appear so we know the lock is held.
	first := newSuperviseCmd()
	first.SetArgs([]string{"--no-ipc"})
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Execute() }()

	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 2*time.Second) {
		t.Fatalf("first supervisor.lock.owner.json never appeared")
	}

	// Attempt a second instance. It must return a non-nil error
	// quickly (no signal needed — lock-acquire failure is fail-fast).
	// Suppress cobra's auto-stderr usage dump so the test log stays
	// clean; the assertion below cares only about the returned error.
	second := newSuperviseCmd()
	second.SetArgs([]string{"--no-ipc"})
	second.SilenceErrors = true
	second.SilenceUsage = true
	secondErr := second.Execute()
	if secondErr == nil {
		t.Fatal("expected second supervise instance to refuse lock; got nil error")
	}

	// Clean up the first instance via the test seam.
	exitCh <- struct{}{}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first supervise exited with err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first supervise did not exit on test-exit during cleanup")
	}
}

// TestSuperviseCommand_StatusIPC_ReconcileReady verifies the Task 6.2
// contract: a `status` IPC request returns reconcile_ready=true after
// the supervisor has read its intent files AND scheduled the first
// reconcile pass. Spec §"Wire format" + §"Migration step 14:
// reconcile-ready not all-daemons-healthy".
//
// Task 6.2 stubs the reconcile-pass scheduling — Task 7.1 replaces it
// with a real diff-and-apply tick. For this phase the supervisor flips
// reconcileReady=true immediately after intent-file reads complete, so
// the migrate-side wait-loop has a positive ready-marker to anchor its
// 30-second timeout against.
//
// The test runs WITH IPC (no `--no-ipc` flag) so the listener actually
// binds the unix socket / named pipe, and the test client dials it via
// the platform-specific dialSuperviseIPC helper. The hello-frame
// handshake from Task 5.1 / 5.2 must complete before the status
// request frame can be sent — the helper reads it via a dedicated
// bufio.Reader call.
//
// The test does NOT need to assert exact intent-files-loaded=true
// because that flip happens on the same code path as reconcile_ready;
// the spec contract that migration polls against is reconcile_ready,
// and intent_files_loaded is the observability companion (operators
// debugging a stuck startup can see which sub-step is incomplete).
func TestSuperviseCommand_StatusIPC_ReconcileReady(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	exitCh := make(chan struct{}, 1)
	cleanup := setSuperviseTestExitCh(exitCh)
	defer cleanup()

	cmd := newSuperviseCmd()
	// No --no-ipc flag — we WANT the listener bound so the client
	// can dial it.
	cmd.SetArgs([]string{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Drain the supervisor at test end via the test-exit seam. Done
	// here as a defer so a mid-test failure (t.Fatalf) still unblocks
	// the goroutine — otherwise the test binary would hang on exit.
	defer func() {
		select {
		case exitCh <- struct{}{}:
		default:
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("supervise did not exit on test-exit signal within 3s")
		}
	}()

	// Wait for the lock-owner sidecar to appear AND give the listener
	// goroutine a moment to bind. The sidecar appears after
	// AcquireSupervisorLock, which is BEFORE the IPC listener binds;
	// the post-sidecar sleep is a small grace window so the dial
	// below doesn't race the bind.
	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 3*time.Second) {
		t.Fatalf("supervisor.lock.owner.json never appeared under %s", tmpHome)
	}

	// Poll-dial loop: try to dial up to 2 seconds. The listener binds
	// after the sidecar appears but before the reconcile-ready flag
	// flips; either succeeds within the window. A flat sleep would
	// race the listener-bind on slow CI hosts.
	pipePath := defaultPipePathOS(tmpHome)
	var conn net.Conn
	var dialErr error
	dialDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(dialDeadline) {
		conn, dialErr = dialSuperviseIPC(pipePath)
		if dialErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("dial supervise IPC (%s): %v", pipePath, dialErr)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	// Read hello frame — per spec §"Handshake" the supervisor sends
	// one JSON line on connect before accepting any client request.
	reader := bufio.NewReader(conn)
	helloLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if !strings.Contains(helloLine, `"hello":`) {
		t.Fatalf("unexpected first frame (want hello), got: %s", helloLine)
	}

	// Send status request. The supervisor may not yet have flipped
	// reconcile_ready=true when the first request arrives — the
	// runSupervise body schedules the listener BEFORE loadIntentFiles
	// runs, which is correct (clients connecting during startup get
	// reconcile_ready=false and re-poll). Drive a small retry loop
	// here so the test asserts the steady-state contract: once the
	// supervisor has reached the post-loadIntentFiles checkpoint,
	// reconcile_ready stays true forever.
	statusDeadline := time.Now().Add(5 * time.Second)
	var lastResult map[string]any
	for time.Now().Before(statusDeadline) {
		req := api.IPCRequest{ID: 1, Cmd: "status"}
		reqBody, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		if _, err := conn.Write(append(reqBody, '\n')); err != nil {
			t.Fatalf("write status request: %v", err)
		}

		respLine, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read status response: %v", err)
		}
		var resp api.IPCResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(respLine)), &resp); err != nil {
			t.Fatalf("parse response (%q): %v", respLine, err)
		}
		if !resp.OK {
			t.Fatalf("status response not OK: %+v", resp.Error)
		}
		result, ok := resp.Result.(map[string]any)
		if !ok {
			t.Fatalf("result not object: %T", resp.Result)
		}
		lastResult = result
		if result["reconcile_ready"] == true {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if lastResult == nil {
		t.Fatal("no status response captured")
	}
	if lastResult["reconcile_ready"] != true {
		t.Fatalf("expected reconcile_ready=true after startup, got %v in result %+v",
			lastResult["reconcile_ready"], lastResult)
	}
	if lastResult["intent_files_loaded"] != true {
		t.Fatalf("expected intent_files_loaded=true after startup, got %v in result %+v",
			lastResult["intent_files_loaded"], lastResult)
	}
	if lastResult["state"] != "running" {
		t.Fatalf("expected state=running, got %v in result %+v",
			lastResult["state"], lastResult)
	}
	daemons, ok := lastResult["daemons"].([]any)
	if !ok {
		t.Fatalf("daemons field not an array: %T (Task 7.1 populates this; Task 6.2 keeps it empty)", lastResult["daemons"])
	}
	if len(daemons) != 0 {
		t.Fatalf("expected empty daemons array in Task 6.2 stub, got %d entries", len(daemons))
	}
}

// TestSuperviseCommand_StatusIPC_UnknownCommand verifies the dispatch
// surface returns a structured UNKNOWN_COMMAND error for verbs not yet
// implemented (reload / restart / quiesce-timers / exit — those land
// in later tasks). The contract per spec §"Wire format" is an
// IPCResponse with an IPCErr envelope, not a closed connection — so
// clients can fail-fast on missing verbs without re-dialing.
func TestSuperviseCommand_StatusIPC_UnknownCommand(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	exitCh := make(chan struct{}, 1)
	cleanup := setSuperviseTestExitCh(exitCh)
	defer cleanup()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	defer func() {
		select {
		case exitCh <- struct{}{}:
		default:
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("supervise did not exit on test-exit signal within 3s")
		}
	}()

	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 3*time.Second) {
		t.Fatalf("supervisor.lock.owner.json never appeared")
	}

	pipePath := defaultPipePathOS(tmpHome)
	var conn net.Conn
	var dialErr error
	dialDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(dialDeadline) {
		conn, dialErr = dialSuperviseIPC(pipePath)
		if dialErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		t.Fatalf("dial supervise IPC: %v", dialErr)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read hello: %v", err)
	}

	req := api.IPCRequest{ID: 99, Cmd: "no-such-cmd-12345"}
	reqBody, _ := json.Marshal(req)
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		t.Fatalf("write request: %v", err)
	}
	respLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(respLine)), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected !OK for unknown cmd; got %+v", resp)
	}
	if resp.Error == nil {
		t.Fatalf("expected error envelope; got nil")
	}
	if resp.Error.Code != "UNKNOWN_COMMAND" {
		t.Fatalf("expected error.code=UNKNOWN_COMMAND, got %q", resp.Error.Code)
	}
	if resp.ID != 99 {
		t.Fatalf("expected correlation id=99, got %d", resp.ID)
	}
}

// waitForFile polls for the named file's existence up to timeout.
// Returns true on first observation, false on timeout. Used in
// supervise tests as a cheap "startup reached this step" probe.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err := os.Stat(path)
	return err == nil
}
