package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
)

// ensureAliveTestStateDir creates a per-test temp state dir and routes EVERY
// state-path resolver at it, so the `--ensure-alive` action's
// SupervisorRunningUnderStateDir probe can NEVER touch the real
// %LOCALAPPDATA%\mcp-local-hub\supervisor.lock (the §11.10 fleet-wipe lesson —
// a test that forgets this reads/locks the LIVE supervisor lock and can
// disrupt the running fleet).
//
// Two layers of safety:
//  1. The action under test takes the stateDir as a DIRECT parameter, so the
//     test passes this temp dir and the real dir is never resolved.
//  2. SetDaemonStateRootForTest + the LOCALAPPDATA/USERPROFILE env overrides
//     redirect api.DaemonStateDir() too, so even an accidental real-dir
//     resolution lands in the temp tree.
func ensureAliveTestStateDir(t *testing.T) string {
	t.Helper()
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("USERPROFILE", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	return stateDir
}

// TestEnsureAlive_LiveLock_NoOp covers the common case: a supervisor holds the
// flock → SupervisorRunningUnderStateDir reports running → the action is a
// no-op and the relaunch seam is NOT called.
func TestEnsureAlive_LiveLock_NoOp(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	// Hold the REAL supervisor.lock flock so the probe reports running. This is
	// the same live-lock signal the §7.1 gate depends on (api/supervisor_lock_test.go).
	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()
	// Sanity: confirm the probe sees the held lock before exercising the action.
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || !running {
		t.Fatalf("precondition: probe must report running with the lock held; got running=%v err=%v", running, perr)
	}

	var relaunches int32
	restoreSeam := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&relaunches, 1)
		return nil
	})
	defer restoreSeam()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v (must always return nil / exit 0)", err)
	}
	if got := atomic.LoadInt32(&relaunches); got != 0 {
		t.Errorf("relaunch seam called %d times on a LIVE lock; want 0 (no-op on running supervisor)", got)
	}
	if !strings.Contains(out.String(), "supervisor running") {
		t.Errorf("output should report the running no-op; got %q", out.String())
	}
}

// TestEnsureAlive_FreeLock_RelaunchesOnce covers the recovery case: NO process
// holds the flock → SupervisorRunningUnderStateDir reports not-running → the
// action relaunches the owner exactly once via the seam.
func TestEnsureAlive_FreeLock_RelaunchesOnce(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	// No lock holder. Sanity-confirm the probe reports not-running, no error.
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || running {
		t.Fatalf("precondition: probe must report not-running with no lock holder; got running=%v err=%v", running, perr)
	}

	var relaunches int32
	restoreSeam := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&relaunches, 1)
		return nil
	})
	defer restoreSeam()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v (must always return nil / exit 0)", err)
	}
	if got := atomic.LoadInt32(&relaunches); got != 1 {
		t.Errorf("relaunch seam called %d times on a FREE lock; want exactly 1", got)
	}
	if !strings.Contains(out.String(), "relaunched owner") {
		t.Errorf("output should report the relaunch; got %q", out.String())
	}
}

// TestEnsureAlive_ProbeError_NoRelaunch covers the fail-closed guard-precondition:
// when the liveness probe itself cannot run (a state dir under a nonexistent
// parent chain, so the flock file cannot be opened), liveness is UNDETERMINABLE
// → the action must NOT relaunch (undeterminable != dead). This is the
// inverted-polarity guard: relaunching on an undeterminable probe could stack a
// second owner against a live-but-unprobeable supervisor.
func TestEnsureAlive_ProbeError_NoRelaunch(t *testing.T) {
	// Point at a path under a nonexistent parent chain so the flock create
	// fails → SupervisorRunningUnderStateDir returns a non-nil error (same
	// shape as api/supervisor_lock_test.go's fail-closed probe test).
	bogus := filepath.Join(t.TempDir(), "no-such-parent", "deeper", "state")
	// Sanity: confirm the probe genuinely errors at this path.
	if running, _, perr := api.SupervisorRunningUnderStateDir(bogus); perr == nil || running {
		t.Fatalf("precondition: probe against a nonexistent parent must error and report not-running; got running=%v err=%v", running, perr)
	}

	var relaunches int32
	restoreSeam := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&relaunches, 1)
		return nil
	})
	defer restoreSeam()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(bogus, out); err != nil {
		t.Fatalf("runEnsureAlive: %v (must always return nil / exit 0 even on probe error)", err)
	}
	if got := atomic.LoadInt32(&relaunches); got != 0 {
		t.Errorf("relaunch seam called %d times on a PROBE ERROR; want 0 (fail-closed: undeterminable != dead)", got)
	}
	if !strings.Contains(out.String(), "undeterminable") {
		t.Errorf("output should report the undeterminable no-op; got %q", out.String())
	}
}

// TestEnsureAlive_Falsification_HealthyNeverRelaunches is the polarity-proof:
// with a healthy supervisor (live lock held) the action is run for SEVERAL
// ticks and must produce ZERO relaunches. An inverted-polarity implementation
// (relaunch when running==true) would relaunch a HEALTHY supervisor every tick;
// this test fails loudly if that polarity ever slips in.
func TestEnsureAlive_Falsification_HealthyNeverRelaunches(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()

	var relaunches int32
	restoreSeam := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&relaunches, 1)
		return nil
	})
	defer restoreSeam()

	// Several ticks against the same live lock. A correct implementation
	// no-ops every time; an inverted polarity would relaunch every time.
	const ticks = 5
	for i := 0; i < ticks; i++ {
		if err := runEnsureAlive(stateDir, &bytes.Buffer{}); err != nil {
			t.Fatalf("runEnsureAlive tick %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&relaunches); got != 0 {
		t.Fatalf("relaunch seam fired %d times across %d ticks on a HEALTHY supervisor; want 0 "+
			"(any non-zero means inverted polarity — the action would relaunch a live supervisor)", got, ticks)
	}
}

// TestEnsureAlive_RelaunchFailure_StillExitsZero covers the best-effort
// contract: when the relaunch seam itself fails, the action logs and STILL
// returns nil (exit 0) so the ~1-min scheduled tick simply retries rather than
// surfacing a non-zero exit that would noise up the task's last-run record.
func TestEnsureAlive_RelaunchFailure_StillExitsZero(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	// No lock holder → the action will attempt a relaunch; the seam errors.
	var relaunches int32
	restoreSeam := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&relaunches, 1)
		return errors.New("synthetic relaunch failure")
	})
	defer restoreSeam()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive must return nil even when relaunch fails; got %v", err)
	}
	if got := atomic.LoadInt32(&relaunches); got != 1 {
		t.Errorf("relaunch seam called %d times; want exactly 1 (one attempt, then exit 0)", got)
	}
	if !strings.Contains(out.String(), "relaunch FAILED") {
		t.Errorf("output should report the relaunch failure; got %q", out.String())
	}
}
