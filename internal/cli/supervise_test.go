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
	"os"
	"path/filepath"
	"testing"
	"time"
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
