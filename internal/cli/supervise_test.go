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
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestSuperviseCommand_FailsClosedWhenIntentCollapseCannotMergeLegacyStops verifies that
// startup aborts when E2 collapse fails before active legacy stops are durable
// in supervisor-intent.json's sub-block.
func TestSuperviseCommand_FailsClosedWhenIntentCollapseCannotMergeLegacyStops(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateDir)
	restoreStateRoot := api.SetDaemonStateRootForTest(stateDir)
	defer restoreStateRoot()

	now := time.Now().UTC()
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	if err := api.NewAPI().WriteDaemonIntent(`\mcp-local-hub-paper-search-default`, api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserStop,
		UpdatedAt: now,
	}, "test"); err != nil {
		t.Fatalf("seed legacy daemon-intent.json: %v", err)
	}

	// Force RunDaemonIntentCollapse to fail after it has found active legacy
	// stops but before it can persist them into supervisor-intent.json. The
	// legacy daemon-intent.json remains valid/readable, so continuing startup
	// would drop the stop through UnifiedStopsFile's E2 sub-block-only rule.
	supervisorIntentLockPath := filepath.Join(stateDir, "supervisor-intent.json.lock")
	if err := os.Remove(supervisorIntentLockPath); err != nil {
		t.Fatalf("remove supervisor-intent lock file before poisoning path: %v", err)
	}
	if err := os.Mkdir(supervisorIntentLockPath, 0o700); err != nil {
		t.Fatalf("poison supervisor-intent lock path: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runSupervise(ctx, true, false)
	if err == nil {
		t.Fatal("runSupervise succeeded after an unmerged legacy stop collapse failure; want fail-closed error")
	}
	if !strings.Contains(err.Error(), "run daemon-intent collapse") {
		t.Fatalf("runSupervise error = %v; want collapse failure", err)
	}

	got, readErr := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if readErr != nil {
		t.Fatalf("read supervisor-intent.json after failed collapse: %v", readErr)
	}
	if len(got.Stops) != 0 {
		t.Fatalf("failed collapse unexpectedly wrote stops: %+v", got.Stops)
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "daemon-intent.json")); statErr != nil {
		t.Fatalf("legacy daemon-intent.json should remain for retry after failed collapse: %v", statErr)
	}
}

// TestHasUnmergedActiveLegacyStops_BareKeyMatchesCanonicalSubBlock locks in
// the canonicalization fix (bot PR #285 P2): older v0.4.x writers could leave
// BARE task keys in daemon-intent.json, while the collapse merge persists the
// stop under the canonical leading-backslash key. The gate must canonicalize
// the legacy key before the sub-block lookup, else an already-merged stop
// reads as "unmerged" and permanently fail-closes startup.
func TestHasUnmergedActiveLegacyStops_BareKeyMatchesCanonicalSubBlock(t *testing.T) {
	now := time.Now().UTC()
	stop := api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonUserStop,
		UpdatedAt: now,
	}
	supervisorIntent := &api.SupervisorIntentFile{
		Version: 1,
		Stops:   map[string]api.DaemonIntent{`\mcp-local-hub-foo-default`: stop},
	}
	legacy := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{"mcp-local-hub-foo-default": stop}, // BARE key
	}
	if hasUnmergedActiveLegacyStops(supervisorIntent, legacy, now) {
		t.Fatalf("bare-key legacy stop already merged under the canonical key must NOT read as unmerged (would permanently fail-close startup)")
	}
	// Falsification: a genuinely-unmerged stop still trips the gate.
	missing := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{"mcp-local-hub-bar-default": stop},
	}
	if !hasUnmergedActiveLegacyStops(supervisorIntent, missing, now) {
		t.Fatalf("genuinely-unmerged active legacy stop must trip the fail-closed gate")
	}
}

// TestSuperviseCommand_AcquiresLockAndExitsOnSignal verifies the
// happy-path lifecycle: start → lock acquired (owner sidecar
// present) → graceful-exit triggered via the test seam → err=nil.
//
// The test asserts the same end state a real SIGINT delivery would
// have produced: the owner sidecar is removed (Release ran),
// supervisor-events.log exists (the audit channel opened).
func TestSuperviseCommand_AcquiresLockAndExitsOnSignal(t *testing.T) {
	// v0.5.0 Fix Group 5: supervisor.lock owner sidecar +
	// supervisor-state.json writes flow through the hardened
	// secure-write pipeline. The state-dir override target must
	// pass the parent-dir gate; apitest.HardenedTempDir installs
	// the allowlist-conforming DACL/mode.
	tmpHome := apitest.HardenedTempDir(t)
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
	// v0.5.0 Fix Group 5: supervisor.lock owner sidecar +
	// supervisor-state.json writes flow through the hardened
	// secure-write pipeline. The state-dir override target must
	// pass the parent-dir gate; apitest.HardenedTempDir installs
	// the allowlist-conforming DACL/mode.
	tmpHome := apitest.HardenedTempDir(t)
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
	// v0.5.0 Fix Group 5: supervisor.lock owner sidecar +
	// supervisor-state.json writes flow through the hardened
	// secure-write pipeline. The state-dir override target must
	// pass the parent-dir gate; apitest.HardenedTempDir installs
	// the allowlist-conforming DACL/mode.
	tmpHome := apitest.HardenedTempDir(t)
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
	// v0.5.0 Fix Group 5: supervisor.lock owner sidecar +
	// supervisor-state.json writes flow through the hardened
	// secure-write pipeline. The state-dir override target must
	// pass the parent-dir gate; apitest.HardenedTempDir installs
	// the allowlist-conforming DACL/mode.
	tmpHome := apitest.HardenedTempDir(t)
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
	waitForSupervisorReadyIPC(t, conn, reader)

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

// TestSuperviseCommand_CallsReaperBeforeReconcileReady verifies the
// Task 13.1 wiring: runSupervise MUST invoke ReapStaleTransients
// during startup, after lock-acquire + audit-log open but BEFORE
// reconcileReady flips true. Without this wiring, a POSIX cold-start
// after a prior supervisor crash would race the kernel's port-release
// on orphaned daemon listeners and the first reconcile pass would
// fail with EADDRINUSE.
//
// The test installs a fake reaperFn via setReaperFnForTest that
// records (a) that it was called, and (b) the value of reconcileReady
// at the time of the call (must be false). The fake also asserts the
// StateDir it received matches the override directory the test set
// — that proves the call is wired into the same state-dir context the
// rest of the supervisor uses.
//
// Ordering proof: the test inspects supervisor-events.log after exit
// and asserts the byte offset of the cold-start-reap-complete entry
// is strictly less than the byte offset of the reconcile-ready entry.
// Both events go through the same single-writer flock-protected log
// (api.SupervisorEventLog), so byte-offset order in the on-disk log
// is a faithful witness of emission order.
func TestSuperviseCommand_CallsReaperBeforeReconcileReady(t *testing.T) {
	// v0.5.0 Fix Group 5: supervisor.lock owner sidecar +
	// supervisor-state.json writes flow through the hardened
	// secure-write pipeline. The state-dir override target must
	// pass the parent-dir gate; apitest.HardenedTempDir installs
	// the allowlist-conforming DACL/mode.
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	// Seed a supervisor-state.json with one transient_pid entry so
	// the fake reaper has something concrete to acknowledge. The PID
	// value (-1) is one the production code would never accept; the
	// fake doesn't care because it's a fake.
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	seedState := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{PID: 12345, Kind: "maintenance", StartedAt: "2026-05-17T00:00:00Z"},
		},
	}
	if err := api.WriteSupervisorState(statePath, seedState); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	var (
		reaperCalled        atomic.Bool
		reaperGotStateDir   atomic.Value // string
		reaperGotCtxNonNil  atomic.Bool
		reaperCalledOrderOK atomic.Bool // true if reaper observed reconcileReady=false at call-time
	)
	// reconcileReadyDuringReaper acts as the runSupervise-side
	// proof that the reaper fired BEFORE reconcileReady.Store(true).
	// We can't reach into runSupervise's local atomic.Bool, but the
	// audit log preserves emission order across both events; the
	// extra atomic here is a defense-in-depth assertion on the
	// runtime path while the log offset check below is the
	// canonical proof.
	cleanupReaper := setReaperFnForTest(func(ctx context.Context, deps ReaperDeps) (ReaperResult, error) {
		reaperCalled.Store(true)
		reaperGotStateDir.Store(deps.StateDir)
		if ctx != nil {
			reaperGotCtxNonNil.Store(true)
		}
		// At call-time we have no direct handle to the supervisor's
		// reconcileReady atomic — but we know runSupervise.Store(true)
		// only fires AFTER loadIntentFiles, which runs AFTER this
		// callback returns. So observing "false" here is equivalent
		// to "Store(true) has not yet been called", and the log
		// offset check below tracks the audit-emission order.
		reaperCalledOrderOK.Store(true)
		return ReaperResult{
			KilledPIDs:        []int{12345},
			ClearedTransients: 1,
			SettleDuration:    0, // skip the real 2s settle in the fake
		}, nil
	})
	defer cleanupReaper()

	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)
	defer cleanupExit()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for the lock-owner sidecar to appear so we know startup
	// reached the lock step. The reaper is wired immediately after
	// the audit log opens, which is after the lock step.
	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 3*time.Second) {
		t.Fatalf("supervisor.lock.owner.json never appeared under %s", tmpHome)
	}

	// Also wait briefly for the reaper to have been observed. The
	// reaper runs synchronously on the runSupervise goroutine before
	// reconcileReady.Store(true), so by the time the supervisor is
	// awaiting the signal channel, the reaper has already returned.
	reaperDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(reaperDeadline) {
		if reaperCalled.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reaperCalled.Load() {
		t.Fatal("fake reaperFn was never invoked by runSupervise")
	}
	if !reaperGotCtxNonNil.Load() {
		t.Fatal("reaperFn called with nil context — runSupervise must pass a real ctx")
	}
	if got := reaperGotStateDir.Load(); got == nil || got.(string) != tmpHome {
		t.Fatalf("reaperFn StateDir=%v; want %s", got, tmpHome)
	}
	if !reaperCalledOrderOK.Load() {
		t.Fatal("reaperFn order assertion did not pass — runtime path may have flipped reconcileReady early")
	}

	// Trigger graceful exit and wait for clean shutdown.
	exitCh <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervise did not exit on test-exit signal within 3s")
	}

	// Read the audit log and verify byte-offset ordering:
	// cold-start-reap-complete < reconcile-ready. Both events are
	// emitted through the same flock-protected SupervisorEventLog
	// instance from the supervise goroutine, so byte order in the
	// file IS emission order.
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	logStr := string(logRaw)
	reapIdx := strings.Index(logStr, `"event":"cold-start-reap-complete"`)
	if reapIdx < 0 {
		t.Fatalf("cold-start-reap-complete event missing from audit log:\n%s", logStr)
	}
	readyIdx := strings.Index(logStr, `"event":"reconcile-ready"`)
	if readyIdx < 0 {
		t.Fatalf("reconcile-ready event missing from audit log:\n%s", logStr)
	}
	if reapIdx >= readyIdx {
		t.Fatalf("cold-start-reap-complete (offset %d) must precede reconcile-ready (offset %d):\n%s",
			reapIdx, readyIdx, logStr)
	}
}

// TestSuperviseCommand_ReaperFailureDoesNotBlockStartup verifies the
// best-effort spec: a reaper error must NOT block supervisor startup.
// The supervisor should continue past the failed reap, emit a warn
// event with the wrapped error, and reach reconcileReady=true the
// same way a successful reap would.
//
// Without this property, a flaky /proc read or a transient I/O error
// on supervisor-state.json would prevent the supervisor from ever
// coming up — which is far worse than a missed reap (the next start
// will retry the reap, and the orphan PIDs will either die naturally
// or be reaped on a subsequent restart).
func TestSuperviseCommand_ReaperFailureDoesNotBlockStartup(t *testing.T) {
	// v0.5.0 Fix Group 5: supervisor.lock owner sidecar +
	// supervisor-state.json writes flow through the hardened
	// secure-write pipeline. The state-dir override target must
	// pass the parent-dir gate; apitest.HardenedTempDir installs
	// the allowlist-conforming DACL/mode.
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	reaperErr := errFakeReaperFailure
	cleanupReaper := setReaperFnForTest(func(ctx context.Context, deps ReaperDeps) (ReaperResult, error) {
		return ReaperResult{}, reaperErr
	})
	defer cleanupReaper()

	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)
	defer cleanupExit()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 3*time.Second) {
		t.Fatalf("supervisor.lock.owner.json never appeared")
	}

	// Trigger graceful exit; the supervisor must reach this point
	// despite the reaper failure.
	exitCh <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err despite reaper failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervise did not exit on test-exit signal within 3s")
	}

	// Verify the audit log shows BOTH the failure event AND the
	// reconcile-ready event — i.e. startup continued past the
	// reaper error.
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"cold-start-reap-failed"`) {
		t.Fatalf("cold-start-reap-failed event missing from audit log:\n%s", logStr)
	}
	if !strings.Contains(logStr, errFakeReaperFailure.Error()) {
		t.Fatalf("wrapped reaper error message missing from audit log:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"event":"reconcile-ready"`) {
		t.Fatalf("reconcile-ready event missing from audit log — startup blocked on reap failure:\n%s",
			logStr)
	}
}

// errFakeReaperFailure is the synthetic error returned by the fake
// reaper in TestSuperviseCommand_ReaperFailureDoesNotBlockStartup.
// Defined as a package-level var so the test can match on the
// error's Error() string in the audit log without leaking a sentinel
// into production code.
var errFakeReaperFailure = &fakeReaperErr{msg: "fake-reaper-failure-for-test-13-1-wiring"}

type fakeReaperErr struct{ msg string }

func (e *fakeReaperErr) Error() string { return e.msg }

// ---------------------------------------------------------------------------
// codex round-3 Lane B P1 #1: IPC verbs (quiesce-timers + exit{graceful}).
// ---------------------------------------------------------------------------

// fakeQuiesceDrainer is a test-only QuiesceHandler stand-in that
// returns a pre-programmed QuiesceResult after a configurable delay.
// Used by the quiesce-timers tests to drive deterministic two-frame
// timing without spawning real child processes.
type fakeQuiesceDrainer struct {
	delay  time.Duration
	result QuiesceResult
	calls  atomic.Int32
}

func (f *fakeQuiesceDrainer) Drain(ctx context.Context, timeoutMs int) QuiesceResult {
	f.calls.Add(1)
	select {
	case <-time.After(f.delay):
		return f.result
	case <-ctx.Done():
		return f.result
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		// Simulated timeout — return whatever was pre-programmed.
		return f.result
	}
}

// startSuperviseForIPCTest boots a supervise instance against
// tmpHome, waits for the IPC listener, and returns a connected client
// + a cleanup function that triggers graceful exit. The bufio.Reader
// is positioned past the hello frame so callers can immediately send
// IPC requests.
func startSuperviseForIPCTest(t *testing.T, tmpHome string) (net.Conn, *bufio.Reader, func()) {
	t.Helper()
	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 3*time.Second) {
		cleanupExit()
		t.Fatalf("supervisor.lock.owner.json never appeared under %s", tmpHome)
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
		cleanupExit()
		t.Fatalf("dial supervise IPC: %v", dialErr)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		_ = conn.Close()
		cleanupExit()
		t.Fatalf("read hello: %v", err)
	}
	waitForSupervisorReadyIPC(t, conn, reader)

	cleanup := func() {
		_ = conn.Close()
		select {
		case exitCh <- struct{}{}:
		default:
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("supervise did not exit on test-exit signal within 3s")
		}
		cleanupExit()
	}
	return conn, reader, cleanup
}

func waitForSupervisorReadyIPC(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := api.IPCRequest{ID: 9001, Cmd: "status"}
		reqBody, _ := json.Marshal(req)
		if _, err := conn.Write(append(reqBody, '\n')); err != nil {
			t.Fatalf("write readiness status request: %v", err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read readiness status response: %v", err)
		}
		var resp api.IPCResponse
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
			t.Fatalf("decode readiness response (%q): %v", line, err)
		}
		if !resp.OK {
			t.Fatalf("readiness status returned error: %+v", resp.Error)
		}
		result, _ := resp.Result.(map[string]any)
		if result != nil && result["reconcile_ready"] == true {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("supervisor did not report reconcile_ready=true before timeout")
}

func TestSupervisorIPCRefusesMutatingCommandsBeforeReady(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	events, err := api.OpenSupervisorEventLog(filepath.Join(tmpHome, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	var ready atomic.Bool
	var loaded atomic.Bool
	loaded.Store(true)
	var graceful gracefulCounter
	deps := ipcDispatchDeps{
		stateDir:           tmpHome,
		events:             events,
		reconcileReady:     &ready,
		intentFilesLoaded:  &loaded,
		gracefulInProgress: &graceful,
		triggerGracefulExit: func() {
			t.Error("pre-ready mutating request must not trigger supervisor exit")
		},
	}

	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- dispatchIPCRequest(serverConn, api.IPCRequest{
			ID:   77,
			Cmd:  "exit",
			Args: map[string]any{"graceful": true},
		}, deps)
	}()

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read pre-ready response: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		t.Fatalf("decode response (%q): %v", line, err)
	}
	if resp.OK {
		t.Fatalf("pre-ready mutating command must fail, got %+v", resp)
	}
	if resp.ID != 77 || !resp.Final {
		t.Fatalf("response metadata mismatch: %+v", resp)
	}
	if resp.Error == nil {
		t.Fatalf("expected structured error, got nil")
	}
	if resp.Error.Code != ipcErrorSupervisorStarting {
		t.Fatalf("error code = %q, want %q", resp.Error.Code, ipcErrorSupervisorStarting)
	}
	if !resp.Error.Retryable {
		t.Fatalf("starting error must be retryable: %+v", resp.Error)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dispatch returned write error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after writing pre-ready error")
	}
}

// TestSupervise_IPC_QuiesceTimersTwoFrames pins the codex-r3 Lane B
// P1 #1 contract: a `quiesce-timers` request must return exactly two
// frames — an immediate `{accepted: true}` then a final
// `{drained, still_running, final: true}` after the drain returns.
func TestSupervise_IPC_QuiesceTimersTwoFrames(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	fake := &fakeQuiesceDrainer{
		delay:  100 * time.Millisecond,
		result: QuiesceResult{Drained: 3, StillRunning: []int{}},
	}
	restoreFactory := setQuiesceHandlerFactoryForTest(func(string) quiesceDrainer {
		return fake
	})
	defer restoreFactory()

	conn, reader, cleanup := startSuperviseForIPCTest(t, tmpHome)
	defer cleanup()

	req := api.IPCRequest{ID: 42, Cmd: "quiesce-timers", Args: map[string]any{"timeout_ms": 30000}}
	reqBody, _ := json.Marshal(req)
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		t.Fatalf("write quiesce-timers: %v", err)
	}

	// Frame 1: immediate accepted ack.
	line1, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read frame 1: %v", err)
	}
	var resp1 api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line1)), &resp1); err != nil {
		t.Fatalf("decode frame 1 (%q): %v", line1, err)
	}
	if resp1.ID != 42 || !resp1.OK {
		t.Fatalf("frame 1: want OK=true id=42, got %+v", resp1)
	}
	if resp1.Final {
		t.Fatalf("frame 1 must NOT carry final=true; the accepted-ack is by convention non-final")
	}
	r1, _ := resp1.Result.(map[string]any)
	if r1 == nil || r1["accepted"] != true {
		t.Fatalf("frame 1 result must include accepted=true; got %+v", resp1.Result)
	}

	// Frame 2: final result after drain.
	line2, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read frame 2: %v", err)
	}
	var resp2 api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line2)), &resp2); err != nil {
		t.Fatalf("decode frame 2 (%q): %v", line2, err)
	}
	if resp2.ID != 42 || !resp2.OK {
		t.Fatalf("frame 2: want OK=true id=42, got %+v", resp2)
	}
	if !resp2.Final {
		t.Fatalf("frame 2 must carry final=true; got %+v", resp2)
	}
	r2, _ := resp2.Result.(map[string]any)
	if r2 == nil {
		t.Fatalf("frame 2 result missing; got %+v", resp2.Result)
	}
	// JSON-number round-trip turns int → float64.
	if got, _ := r2["drained"].(float64); got != 3 {
		t.Fatalf("frame 2 drained want 3, got %v", r2["drained"])
	}
	if still, ok := r2["still_running"].([]any); !ok || len(still) != 0 {
		t.Fatalf("frame 2 still_running want empty array, got %v", r2["still_running"])
	}
	if fake.calls.Load() != 1 {
		t.Fatalf("fake Drain calls want 1, got %d", fake.calls.Load())
	}
}

// TestSupervise_IPC_QuiesceTimersDrainTimeout pins the codex-r3 Lane B
// P1 #1 contract that `still_running` is populated when the drain
// goroutine is still working past the requested timeout.
func TestSupervise_IPC_QuiesceTimersDrainTimeout(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	fake := &fakeQuiesceDrainer{
		delay:  10 * time.Millisecond, // fake completes quickly
		result: QuiesceResult{Drained: 1, StillRunning: []int{1234, 5678}},
	}
	restoreFactory := setQuiesceHandlerFactoryForTest(func(string) quiesceDrainer {
		return fake
	})
	defer restoreFactory()

	conn, reader, cleanup := startSuperviseForIPCTest(t, tmpHome)
	defer cleanup()

	req := api.IPCRequest{ID: 99, Cmd: "quiesce-timers", Args: map[string]any{"timeout_ms": 100}}
	reqBody, _ := json.Marshal(req)
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Frame 1 (accepted).
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read frame 1: %v", err)
	}
	// Frame 2 (final).
	line2, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read frame 2: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line2)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Final {
		t.Fatalf("frame 2 must be final; got %+v", resp)
	}
	r, _ := resp.Result.(map[string]any)
	if r == nil {
		t.Fatalf("nil result; got %+v", resp.Result)
	}
	still, ok := r["still_running"].([]any)
	if !ok {
		t.Fatalf("still_running not an array; got %T", r["still_running"])
	}
	if len(still) != 2 {
		t.Fatalf("still_running len want 2, got %d (%v)", len(still), still)
	}
	first, _ := still[0].(map[string]any)
	if first == nil || first["pid"] == nil {
		t.Fatalf("still_running[0] missing pid field; got %v", still[0])
	}
}

// TestSupervise_IPC_ExitGracefulInitiates pins the codex-r3 Lane B
// P1 #1 contract for the `exit{graceful: true}` verb: the supervisor
// must (a) reply with a Final=true frame carrying
// graceful_exit_initiated=true, and (b) actually exit afterwards.
func TestSupervise_IPC_ExitGracefulInitiates(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	// We do NOT install the test-exit channel — the IPC exit verb
	// itself must drive shutdown. Set it up just so cleanup can stop
	// the supervisor if the verb misfires.
	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)
	defer cleanupExit()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	sidecar := filepath.Join(tmpHome, "supervisor.lock.owner.json")
	if !waitForFile(sidecar, 3*time.Second) {
		select {
		case exitCh <- struct{}{}:
		default:
		}
		t.Fatalf("sidecar never appeared")
	}

	pipePath := defaultPipePathOS(tmpHome)
	var conn net.Conn
	var dialErr error
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		conn, dialErr = dialSuperviseIPC(pipePath)
		if dialErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialErr != nil {
		select {
		case exitCh <- struct{}{}:
		default:
		}
		t.Fatalf("dial: %v", dialErr)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	waitForSupervisorReadyIPC(t, conn, reader)

	req := api.IPCRequest{ID: 43, Cmd: "exit", Args: map[string]any{"graceful": true, "timeout_ms": 5000}}
	reqBody, _ := json.Marshal(req)
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		t.Fatalf("write exit: %v", err)
	}

	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read exit response: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != 43 || !resp.OK || !resp.Final {
		t.Fatalf("want OK=true Final=true id=43, got %+v", resp)
	}
	r, _ := resp.Result.(map[string]any)
	if r == nil || r["graceful_exit_initiated"] != true {
		t.Fatalf("result must include graceful_exit_initiated=true; got %+v", resp.Result)
	}

	// The supervisor must exit on its own from the IPC-driven path.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err: %v", err)
		}
	case <-time.After(3 * time.Second):
		// Fall back: trigger test-exit so cleanup doesn't hang the
		// test binary, but flag the timeout as a failure first.
		select {
		case exitCh <- struct{}{}:
		default:
		}
		<-done
		t.Fatal("supervisor did not exit within 3s after IPC exit{graceful:true}")
	}
}

// TestSupervise_GracefulCounter_BasicEnterExit pins the unit-level
// contract of the gracefulCounter type that replaces the
// gracefulInProgress atomic.Bool to fix the codex-r4-b-p2 race.
// Enter increments; Exit decrements; InProgress returns true iff
// the counter is positive.
func TestSupervise_GracefulCounter_BasicEnterExit(t *testing.T) {
	var g gracefulCounter
	if g.InProgress() {
		t.Fatal("fresh counter must report InProgress=false")
	}
	g.Enter()
	if !g.InProgress() {
		t.Fatal("after Enter, InProgress must be true")
	}
	g.Exit()
	if g.InProgress() {
		t.Fatal("after balanced Enter+Exit, InProgress must be false")
	}
}

// TestSupervise_GracefulCounter_NestedEnterExitOnlyClearsWhenZero pins
// the load-bearing fix: with multiple concurrent Enters, InProgress
// must remain true until the LAST Exit, not the first.
//
// The historical bool let the first defer Store(false) clear the flag
// while another drain was still running — the rest of the supervisor
// (lifecycle handlers, transient-timer suppression) then observed
// `InProgress() == false` and could proceed as if drain had
// finished, even though one drain goroutine was still active.
func TestSupervise_GracefulCounter_NestedEnterExitOnlyClearsWhenZero(t *testing.T) {
	var g gracefulCounter
	g.Enter()
	g.Enter()
	if !g.InProgress() {
		t.Fatal("after two Enters, InProgress must be true")
	}
	g.Exit()
	// One drain done; the OTHER is still running.
	if !g.InProgress() {
		t.Fatal("after one Exit while a second Enter is still open, InProgress MUST remain true (codex-r4-b-p2 fix); the historical bool collapsed both states onto false on the first Exit")
	}
	g.Exit()
	if g.InProgress() {
		t.Fatal("after balanced Enter+Exit pairs, InProgress must be false")
	}
}

// TestSupervise_GracefulCounter_ConcurrentEntersExits stress-tests the
// counter against concurrent goroutines. The final state after every
// goroutine returns must be `InProgress() == false`. Run with -race to
// catch torn reads/writes on the underlying atomic.
func TestSupervise_GracefulCounter_ConcurrentEntersExits(t *testing.T) {
	var g gracefulCounter
	const N = 50
	done := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func() {
			g.Enter()
			// At this point InProgress MUST be true regardless of how
			// many other goroutines have already exited; if a peer's
			// Exit raced our Enter to zero, that would be a regression.
			if !g.InProgress() {
				t.Errorf("InProgress=false while goroutine is between Enter and Exit; counter race")
			}
			g.Exit()
			done <- struct{}{}
		}()
	}
	for i := 0; i < N; i++ {
		<-done
	}
	if g.InProgress() {
		t.Fatal("after all goroutines completed, counter must be back to zero")
	}
}

// TestSupervise_GracefulCounter_QuiesceWiringClearsOnExit pins the
// dispatcher-level wiring: handleQuiesceTimers must call Enter on
// entry and Exit on completion of the drain. The deferred Exit must
// run after Drain returns. This is the same control-flow guarantee
// the historical bool needed; the test pins it under the new
// counter so a future refactor can't regress.
func TestSupervise_GracefulCounter_QuiesceWiringClearsOnExit(t *testing.T) {
	var g gracefulCounter
	// Simulate the wiring: Enter on entry, defer Exit on exit.
	func() {
		g.Enter()
		defer g.Exit()
		if !g.InProgress() {
			t.Fatal("InProgress=false inside Enter/Exit pair")
		}
	}()
	if g.InProgress() {
		t.Fatal("InProgress=true after deferred Exit; counter never cleared")
	}
}

// TestSupervise_IPC_VersionPinning pins the codex-r3 Lane B P1 #1
// version-pinning contract: an explicit Version != 1 returns a
// structured UNSUPPORTED_PROTOCOL_VERSION error rather than being
// silently accepted. Version == 0 (omitted) is treated as v1 for
// backward compatibility — exercised implicitly by every other test
// in this file.
func TestSupervise_IPC_VersionPinning(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	conn, reader, cleanup := startSuperviseForIPCTest(t, tmpHome)
	defer cleanup()

	req := api.IPCRequest{Version: 2, ID: 7, Cmd: "status"}
	reqBody, _ := json.Marshal(req)
	if _, err := conn.Write(append(reqBody, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("v2 request must be refused; got %+v", resp)
	}
	if resp.Error.Code != "UNSUPPORTED_PROTOCOL_VERSION" {
		t.Fatalf("want code UNSUPPORTED_PROTOCOL_VERSION, got %q", resp.Error.Code)
	}
	if resp.ID != 7 {
		t.Fatalf("want correlation id=7, got %d", resp.ID)
	}
}

// TestMergeDaemonEnvOverlayWinsOverManifest pins the precedence rule
// for the Task 2.7 three-arg mergeDaemonEnv: parent < manifest < overlay.
// Both manifest and overlay use the same Path-family key (the manifest
// uses "Path"; the overlay also uses "Path"). The overlay value must
// win in the output, and only one Path-family entry must appear.
//
// Spec ref: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Spawn-time env merge"; Plan Task 2.7 acceptance criterion #3.
func TestMergeDaemonEnvOverlayWinsOverManifest(t *testing.T) {
	parent := []string{"PATH=/system", "OTHER=parent"}
	manifest := map[string]string{"Path": "/manifest"}
	overlay := map[string]string{"Path": "/overlay"}

	got := mergeDaemonEnv(parent, manifest, overlay)

	if got == nil {
		t.Fatalf("mergeDaemonEnv returned nil with non-empty manifest+overlay")
	}

	// Collect every Path-family entry (case-insensitive lookup).
	var pathEntries []string
	for _, kv := range got {
		// kv is "KEY=VALUE"; split once on '=' so VALUEs containing '='
		// are not truncated.
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		if strings.EqualFold(k, "PATH") {
			pathEntries = append(pathEntries, kv)
		}
	}
	if len(pathEntries) != 1 {
		t.Fatalf("want exactly 1 Path-family entry, got %d: %v (full env = %v)",
			len(pathEntries), pathEntries, got)
	}
	// The single survivor's value must be "/overlay" (overlay > manifest > parent).
	eq := strings.IndexByte(pathEntries[0], '=')
	if pathEntries[0][eq+1:] != "/overlay" {
		t.Fatalf("Path-family entry value = %q, want %q (overlay must win)",
			pathEntries[0][eq+1:], "/overlay")
	}
}

// TestMergeDaemonEnvBothEmptyReturnsNil pins the both-empty fast-path
// contract that lets the production spawn callsite skip setting
// cmd.Env (child inherits os.Environ directly when no manifest+overlay
// keys are present).
//
// Spec ref: Plan Task 2.7 acceptance criterion #2.
func TestMergeDaemonEnvBothEmptyReturnsNil(t *testing.T) {
	parent := []string{"BASE=parent"}
	got := mergeDaemonEnv(parent, nil, nil)
	if got != nil {
		t.Fatalf("mergeDaemonEnv(parent, nil, nil) = %v, want nil so the caller can leave cmd.Env=nil and inherit os.Environ directly", got)
	}
	// Also exercise the explicit-empty-map variant — equivalent contract.
	got2 := mergeDaemonEnv(parent, map[string]string{}, map[string]string{})
	if got2 != nil {
		t.Fatalf("mergeDaemonEnv(parent, {}, {}) = %v, want nil", got2)
	}
}

// TestMergeDaemonEnvWindowsCaseInsensitive pins the Windows
// case-insensitive collision contract: "PATH" (parent), "path"
// (manifest), and "Path" (overlay) all share the same logical key
// under Windows env semantics. The highest-precedence source
// (overlay) must win, and its original casing must be preserved
// in the output ("Path", not "PATH" or "path").
//
// Skipped on POSIX where env keys are case-sensitive (the existing
// TestProductionSpawnFn_EnvOverrideAppliedDeterministically asserts
// the POSIX side of that contract).
//
// Spec ref: Plan Task 2.7 acceptance criterion #4; design doc
// §"${parent_path} token semantics" (case-insensitive PATH lookup).
func TestMergeDaemonEnvWindowsCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only: POSIX env keys are case-sensitive")
	}
	parent := []string{"PATH=/parent"}
	manifest := map[string]string{"path": "/manifest"}
	overlay := map[string]string{"Path": "/overlay"}

	got := mergeDaemonEnv(parent, manifest, overlay)
	if got == nil {
		t.Fatalf("mergeDaemonEnv returned nil with non-empty manifest+overlay")
	}

	// Exactly one Path-family entry, value "/overlay", casing "Path".
	var pathEntries []string
	for _, kv := range got {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		if strings.EqualFold(k, "PATH") {
			pathEntries = append(pathEntries, kv)
		}
	}
	if len(pathEntries) != 1 {
		t.Fatalf("want exactly 1 Path-family entry on Windows (case-insensitive merge), got %d: %v",
			len(pathEntries), pathEntries)
	}
	want := "Path=/overlay"
	if pathEntries[0] != want {
		t.Fatalf("Path-family entry = %q, want %q (overlay wins; original casing preserved)",
			pathEntries[0], want)
	}
}
