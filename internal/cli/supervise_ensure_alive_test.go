package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
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
	// CORRECTION #5 (test isolation): Part B's headless-fleet recovery added a
	// guiOwnerAliveFn() call to the `if running` branch. Without this fake,
	// this test would fall through to the REAL probeGUIOwnerAlive and probe
	// the developer's actual %LOCALAPPDATA% pidport — the fleet-wipe-class
	// incident this file's header (:26-28) warns about. GUI-alive keeps this
	// test on the pre-existing "supervisor running; no action" no-op path.
	liveGUIOwner(t, 9999, 9125)

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

// noLiveGUIOwner installs the GUI-incumbent probe seam reporting a
// CONFIRMED-dead GUI owner (guiOwnerStateConfirmedDead). This is the genuine
// OWNER-death topology the relaunch path is for, and it ALSO keeps the test
// off the real %LOCALAPPDATA% gui.pidport (state safety: the production probe
// reads the developer's running GUI). Every test that exercises a
// supervisor-down branch MUST install this (or its live-owner counterpart
// below) so the real pidport is never probed.
func noLiveGUIOwner(t *testing.T) {
	t.Helper()
	restore := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateConfirmedDead, 0, 0 })
	t.Cleanup(restore)
}

// liveGUIOwner installs the GUI-incumbent probe seam reporting a live GUI
// owner (guiOwnerStateAlive) at the given pid/port. Every test that holds the
// supervisor lock (running=true) and drives runEnsureAlive MUST install this
// (or noLiveGUIOwner) so the headless-fleet branch added alongside this
// helper never falls through to the REAL probeGUIOwnerAlive and touches the
// developer's actual %LOCALAPPDATA% pidport (the same §11.10 fleet-wipe
// safety this file's header already established for the supervisor-down
// side).
func liveGUIOwner(t *testing.T, pid, port int) {
	t.Helper()
	restore := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateAlive, pid, port })
	t.Cleanup(restore)
}

// unknownGUIOwnerState installs the GUI-incumbent probe seam reporting an
// AMBIGUOUS probe result (guiOwnerStateUnknown) — the pidport was
// missing/garbage/out-of-range, or its path could not be resolved. Every
// consumer of guiOwnerAliveFn MUST treat this exactly like a live owner
// (never authorize a GUI-owner-killing relaunch on an ambiguous read — P1-1
// review finding).
func unknownGUIOwnerState(t *testing.T, pid, port int) {
	t.Helper()
	restore := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateUnknown, pid, port })
	t.Cleanup(restore)
}

// TestEnsureAlive_FreeLock_RelaunchesOnce covers the recovery case: NO process
// holds the flock AND no live GUI owner → SupervisorRunningUnderStateDir
// reports not-running → the action relaunches the owner exactly once via the
// seam.
func TestEnsureAlive_FreeLock_RelaunchesOnce(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	noLiveGUIOwner(t)

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
	// Durable observability (PR #283 review P3-d): the relaunch-success outcome
	// is mirrored to supervisor-events.log so a tick is diagnosable even though
	// Task Scheduler discards stdout.
	assertSupervisorEvent(t, stateDir, "liveness-relaunched-owner")
}

// TestEnsureAlive_FreeLock_LiveGUIOwner_RelaunchesStandalone covers the §5
// permanent-fix PART 2 topology: the supervisor is down (free flock) BUT a live
// GUI owner still holds the single-instance lock (the dead-supervisor-child-
// under-live-GUI-owner case that was previously a SUPPRESSED no-op deadlock).
// The action MUST now recover the supervisor DIRECTLY via the GUI-independent
// standalone relaunch (a detached `mcphub supervise`), MUST NOT fire the
// autostart gui task (a no-op focus-steal under the live GUI), and MUST record
// the recovery event.
func TestEnsureAlive_FreeLock_LiveGUIOwner_RelaunchesStandalone(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	// No supervisor lock holder → supervisor reported down.
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || running {
		t.Fatalf("precondition: probe must report not-running with no lock holder; got running=%v err=%v", running, perr)
	}

	// A live GUI owner IS present (the dead-child-under-live-owner topology).
	restoreGUI := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateAlive, 4242, 0 })
	defer restoreGUI()

	var standaloneCalls, autostartCalls int32
	restoreStandalone := setStandaloneRelaunchFnForTest(func() error {
		atomic.AddInt32(&standaloneCalls, 1)
		return nil
	})
	defer restoreStandalone()
	restoreAutostart := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&autostartCalls, 1)
		return nil
	})
	defer restoreAutostart()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v (must always return nil / exit 0)", err)
	}
	if got := atomic.LoadInt32(&standaloneCalls); got != 1 {
		t.Errorf("standalone relaunch fired %d times under a live GUI owner; want 1 "+
			"(direct GUI-independent supervisor recovery)", got)
	}
	if got := atomic.LoadInt32(&autostartCalls); got != 0 {
		t.Errorf("autostart-task relaunch fired %d times under a live GUI owner; want 0 "+
			"(that path is a no-op focus-steal there)", got)
	}
	if !strings.Contains(out.String(), "standalone supervisor") || !strings.Contains(out.String(), "4242") {
		t.Errorf("output should report the standalone supervisor recovery (with the owner pid); got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "liveness-relaunched-supervisor-under-gui")
}

// TestEnsureAlive_FreeLock_LiveGUIOwner_StandaloneRelaunchFails covers the
// failure branch of the §5 PART 2 standalone recovery: supervisor down + a live
// GUI owner, but the detached `mcphub supervise` spawn fails. The action MUST
// still return nil (exit 0 — best-effort tick), MUST NOT fall through to the
// autostart task, MUST print a FAILED line, and MUST emit the durable
// liveness-standalone-relaunch-failed warn so a chronic failure is operator-
// visible despite Task Scheduler discarding stdout.
func TestEnsureAlive_FreeLock_LiveGUIOwner_StandaloneRelaunchFails(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || running {
		t.Fatalf("precondition: probe must report not-running; got running=%v err=%v", running, perr)
	}
	restoreGUI := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateAlive, 4242, 0 })
	defer restoreGUI()

	restoreStandalone := setStandaloneRelaunchFnForTest(func() error {
		return errors.New("synthetic standalone spawn failure")
	})
	defer restoreStandalone()
	var autostartCalls int32
	restoreAutostart := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&autostartCalls, 1)
		return nil
	})
	defer restoreAutostart()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive must return nil even on relaunch failure; got %v", err)
	}
	if got := atomic.LoadInt32(&autostartCalls); got != 0 {
		t.Errorf("autostart task must NOT fire when the standalone path is taken (live GUI); fired %d", got)
	}
	if !strings.Contains(out.String(), "FAILED") {
		t.Errorf("output should report the standalone relaunch FAILED; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "liveness-standalone-relaunch-failed")
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
	// CORRECTION #5 (test isolation): see TestEnsureAlive_LiveLock_NoOp's
	// identical fix above — without this fake, Part B's guiOwnerAliveFn()
	// call on the `if running` branch would probe the developer's real
	// %LOCALAPPDATA% pidport on every one of the ticks below.
	liveGUIOwner(t, 9999, 9125)

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
	noLiveGUIOwner(t)

	// No lock holder + no live GUI owner → the action will attempt a relaunch;
	// the seam errors.
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
	// Durable observability (PR #283 review P3-d): a chronically-failing
	// relaunch must be visible despite Task Scheduler discarding stdout.
	assertSupervisorEvent(t, stateDir, "liveness-relaunch-failed")
}

// ---------------------------------------------------------------------------
// Part B falsification suite: headless-fleet recovery (supervisor alive, GUI
// owner dead). Each test's doc comment names the exact mutation it detects.
// ---------------------------------------------------------------------------

// rewriteEnsureAliveSupervisorLockOwnerStartedAt overwrites the ALREADY-HELD
// supervisor.lock's owner sidecar with a fabricated StartedAt, without
// releasing the flock — SupervisorRunningUnderStateDir only inspects the
// flock (never the sidecar content) to decide running=true/false, so this
// lets a test control the boot-grace suppressor's uptime input independently
// of when AcquireSupervisorLock itself ran. The sidecar write goes through
// its own lock file (<path>.owner.json.lock), distinct from the held
// supervisor.lock.lock, so it does not contend with the caller's held lock.
func rewriteEnsureAliveSupervisorLockOwnerStartedAt(t *testing.T, stateDir string, startedAt time.Time) {
	t.Helper()
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner := api.SupervisorLockOwner{PID: os.Getpid(), StartedAt: startedAt.UTC().Format(time.RFC3339Nano)}
	if err := api.WriteStateFileAtomic(lockPath+".owner.json", owner); err != nil {
		t.Fatalf("rewrite supervisor.lock owner sidecar: %v", err)
	}
}

// TestEnsureAlive_HeadlessFleet_RelaunchesGUI covers the headless-fleet
// class this insertion exists for: the supervisor holds its flock (running)
// but no live GUI owner holds the pidport lock, the supervisor is well past
// its boot-grace window, and no restart-v3 handoff marker exists. The action
// MUST relaunch the GUI owner exactly once via the SAME seam genuine
// owner-death already uses, and MUST record both the detection and the
// relaunch as durable events.
//
// MUTATION: revert the `if running` insertion (supervise_ensure_alive.go)
// back to the bare no-op — guiOwnerAliveFn is never consulted, the relaunch
// seam is never called, and this test's "want exactly 1" assertion fails.
func TestEnsureAlive_HeadlessFleet_RelaunchesGUI(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()
	// Well past ensureAliveHeadlessFleetBootGrace (45s) so the boot-grace
	// suppressor does not mask the relaunch this test is proving.
	rewriteEnsureAliveSupervisorLockOwnerStartedAt(t, stateDir, time.Now().Add(-2*time.Minute))

	// GUI is dead: guiOwnerAliveFn reports a stale (but non-zero, for
	// realism — the REAL default GUI port 9125) pid/port recorded in the
	// last pidport write.
	restoreGUI := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateConfirmedDead, 5555, 9125 })
	defer restoreGUI()

	var relaunches int32
	restoreSeam := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&relaunches, 1)
		return nil
	})
	defer restoreSeam()

	// P1-3 review fix: the post-relaunch serving attestation MUST go
	// through the injectable seam, never a real HTTP dial — this fake
	// records the port it was called with (proving the widened
	// guiOwnerAliveFn port value threads through correctly) without ever
	// touching port 9125 on this or any other host, live fleet or not.
	var servingProbeCalls int32
	var servingProbePort int32
	restoreServingProbe := setGUIServingProbeFnForTest(func(port int) bool {
		atomic.AddInt32(&servingProbeCalls, 1)
		atomic.StoreInt32(&servingProbePort, int32(port))
		return true
	})
	defer restoreServingProbe()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v (must always return nil / exit 0)", err)
	}
	if got := atomic.LoadInt32(&relaunches); got != 1 {
		t.Errorf("relaunch seam called %d times on a headless fleet; want exactly 1", got)
	}
	if got := atomic.LoadInt32(&servingProbeCalls); got != 1 {
		t.Errorf("serving-probe seam called %d times; want exactly 1 (no real network dial)", got)
	}
	if got := atomic.LoadInt32(&servingProbePort); got != 9125 {
		t.Errorf("serving-probe seam called with port %d; want 9125 (the widened guiOwnerAliveFn port value must thread through unchanged)", got)
	}
	if !strings.Contains(out.String(), "headless fleet") || !strings.Contains(out.String(), "relaunched GUI owner") {
		t.Errorf("output should report the headless-fleet relaunch; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "gui-headless-fleet-detected")
	assertSupervisorEvent(t, stateDir, "liveness-relaunched-gui-headless-fleet")
	assertSupervisorEventBody(t, stateDir, "liveness-relaunched-gui-headless-fleet", `"serving_probe_ok":true`)
}

// TestEnsureAlive_HeadlessFleet_BootGraceSuppresses covers the boot-grace
// suppressor: the supervisor's own StartedAt is fresh (well inside
// ensureAliveHeadlessFleetBootGrace), so even with GUI reported dead the
// action MUST NOT relaunch this tick.
//
// MUTATION: delete the boot-grace check (or its uptime comparison) in
// runEnsureAliveHeadlessFleet — the action falls through to the relaunch
// unconditionally and this test's "want 0" assertion fails.
func TestEnsureAlive_HeadlessFleet_BootGraceSuppresses(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()
	// StartedAt defaults to "now" (AcquireSupervisorLock's own write) — well
	// inside the 45s boot-grace window. No rewrite needed.

	restoreGUI := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateConfirmedDead, 5555, 9125 })
	defer restoreGUI()

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
		t.Errorf("relaunch seam called %d times inside the boot-grace window; want 0", got)
	}
	if !strings.Contains(out.String(), "boot-grace") {
		t.Errorf("output should report the boot-grace suppression; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "gui-headless-fleet-relaunch-suppressed")
	assertSupervisorEventBody(t, stateDir, "gui-headless-fleet-relaunch-suppressed", `"reason":"boot-grace"`)
}

// TestEnsureAlive_HeadlessFleet_LiveHandoffSuppresses covers the live-handoff
// suppressor: an unexpired restart-v3 in-progress handoff marker means the
// GUI is mid-self-restart, not dead, so the action MUST NOT relaunch even
// though guiOwnerAliveFn reports no live owner. The supervisor's StartedAt is
// fabricated OLD (past boot-grace) so this test isolates the live-handoff
// check specifically — a broken live-handoff check cannot hide behind a
// boot-grace suppression that would mask it.
//
// MUTATION: delete the phaseDeadline/now.Before check in
// ensureAliveHeadlessFleetLiveHandoffSuppressed — the marker is read but
// never suppresses, the action falls through to the relaunch, and this
// test's "want 0" assertion fails.
func TestEnsureAlive_HeadlessFleet_LiveHandoffSuppresses(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()
	rewriteEnsureAliveSupervisorLockOwnerStartedAt(t, stateDir, time.Now().Add(-2*time.Minute))

	restoreGUI := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateConfirmedDead, 5555, 9125 })
	defer restoreGUI()

	// Plant a real, unexpired in-progress restart-v3 handoff marker via the
	// production HandoffMarkerStore (default Freshness is 3 minutes) — this
	// is the SAME store ensureAliveHeadlessFleetLiveHandoffSuppressed reads,
	// not a Phase-I fake, per this suppressor's deliberately independent read.
	deadlines := gui.DefaultRestartDeadlines()
	store := gui.NewHandoffMarkerStore(stateDir, deadlines)
	if _, err := store.Begin(gui.HandoffBegin{
		Generation: "headless-fleet-live-handoff",
		Route:      gui.HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     5555,
	}); err != nil {
		t.Fatalf("Begin handoff marker: %v", err)
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
		t.Errorf("relaunch seam called %d times during an unexpired restart-v3 handoff; want 0", got)
	}
	if !strings.Contains(out.String(), "live-handoff") {
		t.Errorf("output should report the live-handoff suppression; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "gui-headless-fleet-relaunch-suppressed")
	assertSupervisorEventBody(t, stateDir, "gui-headless-fleet-relaunch-suppressed", `"reason":"live-handoff"`)
}

// ---------------------------------------------------------------------------
// P1-1 review fix: the GUI-owner probe fails OPEN on an ambiguous read.
//
// Before this fix, guiOwnerAliveFn returned a bare bool that collapsed
// gui.VerdictMalformed (pidport unreadable/garbage/out-of-range) and a
// path-resolution failure into the SAME alive=false value a confirmed-dead
// owner (gui.VerdictDeadPID) produces. Both runEnsureAlive call sites
// AUTHORIZED a relaunch on alive=false — and the headless-fleet relaunch
// re-fires the autostart task against a MultipleInstances=StopExisting
// scheduled task, so an ambiguous probe (not a confirmed-dead owner) could
// terminate a perfectly healthy GUI every ~1-min tick. Each test below
// demonstrates the DANGEROUS behavior a bare-bool collapse would produce
// (a relaunch that must instead be suppressed) and is proven by the
// mutation described in its comment.
// ---------------------------------------------------------------------------

// TestProbeGUIOwnerAlive_MalformedPidportMapsToUnknown is a direct,
// non-seam-based test of probeGUIOwnerAlive's own Verdict.Class mapping: a
// genuinely corrupt pidport file (garbage bytes, not a parseable "<pid>
// <port>" line) must classify as guiOwnerStateUnknown, never
// guiOwnerStateConfirmedDead. Uses ensureAliveTestStateDir's LOCALAPPDATA
// redirection so gui.PidportPath() resolves inside the temp tree — this
// never touches the developer's real %LOCALAPPDATA% pidport.
//
// MUTATION: revert probeGUIOwnerAlive's Class switch to `return v.PIDAlive,
// v.PID, v.Port` (the pre-fix bare-bool shape widened back to a state) —
// VerdictMalformed's zero-value PIDAlive=false would then read as
// guiOwnerStateConfirmedDead-equivalent and this test's "want Unknown"
// assertion fails.
func TestProbeGUIOwnerAlive_MalformedPidportMapsToUnknown(t *testing.T) {
	ensureAliveTestStateDir(t)

	pidportPath, err := gui.PidportPath()
	if err != nil {
		t.Fatalf("resolve redirected pidport path: %v", err)
	}
	if err := os.WriteFile(pidportPath, []byte("not-a-valid-pidport-line"), 0o600); err != nil {
		t.Fatalf("write malformed pidport: %v", err)
	}

	state, _, _ := probeGUIOwnerAlive()
	if state != guiOwnerStateUnknown {
		t.Fatalf("probeGUIOwnerAlive on a malformed pidport = %v, want guiOwnerStateUnknown (a corrupt/garbage pidport must never be classified as a confirmed-dead owner)", state)
	}
}

// TestEnsureAlive_HeadlessFleet_UnknownGUIOwnerStateSuppresses reproduces the
// WORST P1-1 danger directly: the supervisor is alive and well past its
// boot-grace window, but the GUI-owner probe is AMBIGUOUS (guiOwnerStateUnknown)
// rather than confirmed-dead. The action MUST suppress — never authorize the
// headless-fleet relaunch, which would fire `schtasks /Run` against a
// StopExisting-policy task and could terminate a possibly-still-healthy GUI.
//
// MUTATION: change the `case guiOwnerStateUnknown:` branch in runEnsureAlive
// back to falling through to runEnsureAliveHeadlessFleet (the pre-fix
// bare-bool behavior collapsed Unknown and ConfirmedDead into one branch) —
// this test's "want 0" assertion fails (relaunches becomes 1).
func TestEnsureAlive_HeadlessFleet_UnknownGUIOwnerStateSuppresses(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)

	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()
	// Well past ensureAliveHeadlessFleetBootGrace (45s) so the boot-grace
	// suppressor cannot be the reason for the "no relaunch" outcome — only
	// the Unknown-state guard can be.
	rewriteEnsureAliveSupervisorLockOwnerStartedAt(t, stateDir, time.Now().Add(-2*time.Minute))

	unknownGUIOwnerState(t, 0, 0)

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
		t.Errorf("relaunch seam called %d times on an AMBIGUOUS GUI-owner probe; want 0 (an undeterminable read must never authorize a relaunch that could kill a live GUI)", got)
	}
	if !strings.Contains(out.String(), "undeterminable") {
		t.Errorf("output should report the GUI owner state as undeterminable; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "gui-owner-probe-undeterminable")
}

// TestEnsureAlive_FreeLock_UnknownGUIOwnerState_UsesStandaloneRelaunch covers
// the second P1-1 call site: the supervisor is DOWN (confirmed — the free
// flock) and the GUI-owner probe is ambiguous. Recovery is still warranted
// (nothing about the supervisor's own state is in doubt), but the action
// MUST pick the GUI-independent standalone relaunch, never the
// GUI-owner-killing autostart relaunch — the same "never risk killing a
// possibly-live GUI on an ambiguous read" invariant as the headless-fleet
// branch above.
//
// MUTATION: change `guiState != guiOwnerStateConfirmedDead` back to
// `guiState == guiOwnerStateAlive` (the narrower pre-fix-equivalent
// condition) — Unknown would then fall through to the autostart branch and
// this test's "want autostartCalls == 0" assertion fails.
func TestEnsureAlive_FreeLock_UnknownGUIOwnerState_UsesStandaloneRelaunch(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	if running, _, perr := api.SupervisorRunningUnderStateDir(stateDir); perr != nil || running {
		t.Fatalf("precondition: probe must report not-running with no lock holder; got running=%v err=%v", running, perr)
	}

	unknownGUIOwnerState(t, 0, 0)

	var standaloneCalls, autostartCalls int32
	restoreStandalone := setStandaloneRelaunchFnForTest(func() error {
		atomic.AddInt32(&standaloneCalls, 1)
		return nil
	})
	defer restoreStandalone()
	restoreAutostart := setLivenessRelaunchFnForTest(func() error {
		atomic.AddInt32(&autostartCalls, 1)
		return nil
	})
	defer restoreAutostart()

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v (must always return nil / exit 0)", err)
	}
	if got := atomic.LoadInt32(&standaloneCalls); got != 1 {
		t.Errorf("standalone relaunch fired %d times under an ambiguous GUI-owner probe; want 1", got)
	}
	if got := atomic.LoadInt32(&autostartCalls); got != 0 {
		t.Errorf("autostart-task (GUI-owner-killing) relaunch fired %d times under an ambiguous GUI-owner probe; want 0", got)
	}
	if !strings.Contains(out.String(), "undeterminable") {
		t.Errorf("output should report the GUI owner state as undeterminable; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "liveness-relaunched-supervisor-under-gui")
}

// TestEnsureAlive_HeadlessFleet_LiveHandoffSuppresses_EvenWithRestartV3GateOff
// reproduces the P1-2 review finding: ensureAliveHeadlessFleetLiveHandoffSuppressed
// used to gate its marker READ on gui.RestartV3Enabled() — the SAME feature
// flag that legitimately gates whether THIS process may INITIATE a v3
// restart (runEnsureAliveGUIRecovery's own gate). Recognition of ANOTHER
// process's already-in-flight handoff must never depend on that local
// resolution: this test forces gui.RestartV3Enabled() to resolve false in
// THIS process (the reviewer's exact reproduction:
// MCPHUB_GUI_RESTART_V3=0) while a real, unexpired handoff marker sits on
// disk, and confirms the headless-fleet relaunch is STILL suppressed.
//
// MUTATION: reintroduce `if !gui.RestartV3Enabled() { return false, nil }`
// at the top of ensureAliveHeadlessFleetLiveHandoffSuppressed — this test's
// "want 0 relaunches" assertion fails (the marker read is skipped, the
// suppressor never fires, and the dangerous relaunch proceeds).
func TestEnsureAlive_HeadlessFleet_LiveHandoffSuppresses_EvenWithRestartV3GateOff(t *testing.T) {
	gui.ResetRestartV3ResolvedForTest()
	t.Cleanup(gui.ResetRestartV3ResolvedForTest)
	t.Setenv("MCPHUB_GUI_RESTART_V3", "0")
	if gui.RestartV3Enabled() {
		t.Fatal("precondition: RestartV3Enabled() must resolve false for this reproduction (MCPHUB_GUI_RESTART_V3=0)")
	}

	stateDir := ensureAliveTestStateDir(t)

	lk, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()
	// Well past boot-grace so only the live-handoff suppressor (not
	// boot-grace) can explain a "no relaunch" outcome.
	rewriteEnsureAliveSupervisorLockOwnerStartedAt(t, stateDir, time.Now().Add(-2*time.Minute))

	restoreGUI := setGUIOwnerAliveFnForTest(func() (guiOwnerProbeState, int, int) { return guiOwnerStateConfirmedDead, 5555, 9125 })
	defer restoreGUI()

	// Plant a real, unexpired in-progress handoff marker via the production
	// HandoffMarkerStore — the SAME store the suppressor reads. Its
	// construction and Read() do not consult RestartV3Enabled() at all, so
	// this call succeeds regardless of the gate value above.
	deadlines := gui.DefaultRestartDeadlines()
	store := gui.NewHandoffMarkerStore(stateDir, deadlines)
	if _, err := store.Begin(gui.HandoffBegin{
		Generation: "gate-off-live-handoff",
		Route:      gui.HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     5555,
	}); err != nil {
		t.Fatalf("Begin handoff marker: %v", err)
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
		t.Errorf("relaunch seam called %d times despite an unexpired handoff marker (with this process's RestartV3Enabled()==false); want 0 (recognition of another process's marker must not depend on this process's own feature-gate resolution)", got)
	}
	if !strings.Contains(out.String(), "live-handoff") {
		t.Errorf("output should report the live-handoff suppression; got %q", out.String())
	}
	assertSupervisorEvent(t, stateDir, "gui-headless-fleet-relaunch-suppressed")
	assertSupervisorEventBody(t, stateDir, "gui-headless-fleet-relaunch-suppressed", `"reason":"live-handoff"`)
}

type ensureAliveGUIRecoveryMarkerFake struct {
	mu                    sync.Mutex
	record                *gui.HandoffMarkerRecord
	readErr               error
	readEntered           chan struct{}
	readContinue          chan struct{}
	interruptErr          error
	interruptCalls        int
	interruptEntered      chan struct{}
	interruptContinue     chan struct{}
	interruptBeforeCommit func()
}

func (s *ensureAliveGUIRecoveryMarkerFake) Read() (*gui.HandoffMarkerRecord, error) {
	s.mu.Lock()
	record := cloneEnsureAliveGUIRecoveryRecord(s.record)
	readErr := s.readErr
	entered := s.readEntered
	continueCh := s.readContinue
	s.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if continueCh != nil {
		<-continueCh
	}
	return record, readErr
}

func (s *ensureAliveGUIRecoveryMarkerFake) InterruptFromOwnedFreeProbe(generation string, expectedSequence uint64, reasonCode, operatorAction string) (*gui.HandoffMarkerRecord, error) {
	s.mu.Lock()
	s.interruptCalls++
	entered := s.interruptEntered
	continueCh := s.interruptContinue
	s.mu.Unlock()

	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if continueCh != nil {
		<-continueCh
	}

	s.mu.Lock()
	beforeCommit := s.interruptBeforeCommit
	s.mu.Unlock()
	if beforeCommit != nil {
		beforeCommit()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interruptErr != nil {
		return nil, s.interruptErr
	}
	if s.record == nil || s.record.Generation != generation || s.record.Sequence != expectedSequence {
		return nil, gui.ErrHandoffMarkerCASMismatch
	}
	s.record = cloneEnsureAliveGUIRecoveryRecord(s.record)
	s.record.Sequence++
	s.record.Phase = gui.HandoffPhaseInterrupted
	s.record.ReasonCode = reasonCode
	s.record.OperatorAction = operatorAction
	return cloneEnsureAliveGUIRecoveryRecord(s.record), nil
}

func (s *ensureAliveGUIRecoveryMarkerFake) snapshot() (*gui.HandoffMarkerRecord, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneEnsureAliveGUIRecoveryRecord(s.record), s.interruptCalls
}

func cloneEnsureAliveGUIRecoveryRecord(record *gui.HandoffMarkerRecord) *gui.HandoffMarkerRecord {
	if record == nil {
		return nil
	}
	copy := *record
	return &copy
}

type phaseGEnsureAliveLease struct{ releases int }

func (l *phaseGEnsureAliveLease) Release() { l.releases++ }

type phaseGEnsureAliveChild struct {
	terminates int
	detaches   int
}

func (c *phaseGEnsureAliveChild) PID() int { return 4270 }
func (c *phaseGEnsureAliveChild) TerminateBeforeRelease(context.Context) error {
	c.terminates++
	return nil
}
func (c *phaseGEnsureAliveChild) DetachAtRelease() error {
	c.detaches++
	return nil
}

type phaseGEnsureAliveListener struct{ full bool }

func (*phaseGEnsureAliveListener) EnterGrace(context.Context, http.Handler) error { return nil }
func (*phaseGEnsureAliveListener) CloseListener(context.Context) error            { return nil }
func (l *phaseGEnsureAliveListener) BindForRecovery(context.Context, int) (net.Listener, error) {
	return phaseGEnsureAliveNetListener{}, nil
}
func (l *phaseGEnsureAliveListener) ServeFull(net.Listener, http.Handler) error {
	l.full = true
	return nil
}
func (l *phaseGEnsureAliveListener) RestoreFull(http.Handler) error {
	l.full = true
	return nil
}

type phaseGEnsureAliveNetListener struct{}

func (phaseGEnsureAliveNetListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (phaseGEnsureAliveNetListener) Close() error              { return nil }
func (phaseGEnsureAliveNetListener) Addr() net.Addr            { return phaseGEnsureAliveAddr{} }

type phaseGEnsureAliveAddr struct{}

func (phaseGEnsureAliveAddr) Network() string { return "tcp" }
func (phaseGEnsureAliveAddr) String() string  { return "127.0.0.1:9125" }

type phaseGEnsureAliveStoreCounter struct {
	*gui.HandoffMarkerStore
	interrupts int
}

func (s *phaseGEnsureAliveStoreCounter) InterruptFromOwnedFreeProbe(generation string, sequence uint64, reason, action string) (*gui.HandoffMarkerRecord, error) {
	s.interrupts++
	return s.HandoffMarkerStore.InterruptFromOwnedFreeProbe(generation, sequence, reason, action)
}

func TestRestartV3_ProvedRollbackClearsMarkerAndEnsureAliveTickDoesNothing(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	now := time.Date(2026, 7, 18, 18, 0, 0, 0, time.UTC)
	deadlines := gui.DefaultRestartDeadlines()
	deadlines.Now = func() time.Time { return now }
	store := gui.NewHandoffMarkerStore(stateDir, deadlines)
	lease := &phaseGEnsureAliveLease{}
	child := &phaseGEnsureAliveChild{}
	listener := &phaseGEnsureAliveListener{}
	ids := []string{"handoff-g8", "generation-g8"}
	coordinator, err := gui.NewRestartCoordinator(gui.RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: stateDir,
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 9125, nil }, ParentPID: 1111,
		Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: store, Deadlines: deadlines,
		NewID:    func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce: func() ([]byte, error) { return bytes.Repeat([]byte{0x78}, 32), nil },
		Spawn:    func(gui.SelfRestartHandoff) (gui.RestartParentChild, error) { return child, nil },
		Confirm: func(context.Context, int, []byte, gui.AuthenticatedReadinessIdentity) error {
			return errors.New("standby confirmation timeout")
		},
		CloseHub: func(context.Context) {},
		Exit:     func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started.AcknowledgeResponseFlushed()
	result := <-started.Done
	if result.Err == nil || result.ParentLeaseReleased || lease.releases != 0 || !listener.full {
		t.Fatalf("rollback result=%+v releases=%d full=%t, want proved healthy retained parent", result, lease.releases, listener.full)
	}
	if child.terminates != 1 || child.detaches != 0 {
		t.Fatalf("unauthenticated child terminate=%d detach=%d, want 1/0", child.terminates, child.detaches)
	}
	marker, err := store.Read()
	if err != nil {
		t.Fatalf("Read after rollback: %v", err)
	}
	if marker != nil {
		t.Fatalf("proved rollback retained stale marker: %+v", marker)
	}

	counter := &phaseGEnsureAliveStoreCounter{HandoffMarkerStore: store}
	probeCalls := 0
	eventPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
	eventsBefore, beforeErr := os.ReadFile(eventPath)
	if beforeErr != nil && !os.IsNotExist(beforeErr) {
		t.Fatalf("read event baseline: %v", beforeErr)
	}
	restore := setEnsureAliveGUIRecoveryDependenciesForTest(ensureAliveGUIRecoveryDependencies{
		restartV3Enabled: func() bool { return true },
		restartDeadlines: func() gui.RestartDeadlines { return deadlines },
		markerStore:      func(string, gui.RestartDeadlines) ensureAliveGUIRecoveryStore { return counter },
		probeOwnerLease: func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
			probeCalls++
			return gui.GUIOwnerLeaseProbeResult{State: gui.GUIOwnerLeaseStateHeld}
		},
	})
	t.Cleanup(restore)
	var out bytes.Buffer
	runEnsureAliveGUIRecovery(stateDir, &out)
	if out.Len() != 0 || probeCalls != 0 || counter.interrupts != 0 {
		t.Fatalf("ensure-alive tick output=%q probes=%d mutations=%d, want zero emit/probe/mutation", out.String(), probeCalls, counter.interrupts)
	}
	eventsAfter, afterErr := os.ReadFile(eventPath)
	if afterErr != nil && !os.IsNotExist(afterErr) {
		t.Fatalf("read events after ensure-alive tick: %v", afterErr)
	}
	if !bytes.Equal(eventsAfter, eventsBefore) || bytes.Contains(eventsAfter, []byte("gui-restart-live-holder-wedged")) {
		t.Fatalf("ensure-alive event log changed or contains false held-owner degrade: before=%q after=%q", eventsBefore, eventsAfter)
	}
}

type ensureAliveGUIRecoveryLeaseFake struct {
	once     sync.Once
	releases atomic.Int32
	released chan struct{}
}

type ensureAliveGUIRecoveryReleaseCheckingWriter struct {
	bytes.Buffer
	lease              *ensureAliveGUIRecoveryLeaseFake
	wroteBeforeRelease bool
}

func (w *ensureAliveGUIRecoveryReleaseCheckingWriter) Write(p []byte) (int, error) {
	if w.lease.releases.Load() == 0 {
		w.wroteBeforeRelease = true
	}
	return w.Buffer.Write(p)
}

func (l *ensureAliveGUIRecoveryLeaseFake) Release() {
	l.once.Do(func() {
		l.releases.Add(1)
		if l.released != nil {
			close(l.released)
		}
	})
}

func expiredEnsureAliveGUIRecoveryRecord(now time.Time, phase gui.HandoffPhase) *gui.HandoffMarkerRecord {
	deadline := now.Add(-time.Second)
	record := &gui.HandoffMarkerRecord{
		Version:        "3.1",
		Generation:     "ensure-alive-generation",
		Sequence:       2,
		Phase:          phase,
		Route:          gui.HandoffRouteSamePort,
		OldPort:        9125,
		NewPort:        9125,
		OldPID:         101,
		ChildPID:       202,
		CreatedAt:      now.Add(-time.Minute),
		UpdatedAt:      now.Add(-30 * time.Second),
		FreshUntil:     deadline,
		ReasonCode:     "",
		OperatorAction: "",
	}
	if phase == gui.HandoffPhaseReserved {
		record.DesignatedChildHash = "sha256:" + strings.Repeat("0", 64)
		record.ReservationExpiresAt = deadline
	}
	return record
}

func ensureAliveGUIRecoveryTestDeadlines(now time.Time) gui.RestartDeadlines {
	deadlines := gui.DefaultRestartDeadlines()
	deadlines.Now = func() time.Time { return now }
	deadlines.RecordLock = 100 * time.Millisecond
	return deadlines
}

func installEnsureAliveGUIRecoveryTestDependencies(
	t *testing.T,
	deadlines gui.RestartDeadlines,
	store ensureAliveGUIRecoveryStore,
	probe func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult,
) {
	t.Helper()
	restore := setEnsureAliveGUIRecoveryDependenciesForTest(ensureAliveGUIRecoveryDependencies{
		restartV3Enabled: func() bool { return true },
		restartDeadlines: func() gui.RestartDeadlines { return deadlines },
		markerStore: func(string, gui.RestartDeadlines) ensureAliveGUIRecoveryStore {
			return store
		},
		probeOwnerLease: probe,
	})
	t.Cleanup(restore)
}

// holdEnsureAliveSupervisorLock holds the REAL supervisor.lock flock so
// SupervisorRunningUnderStateDir reports running=true, for tests exercising
// the Phase-I GUI-restart marker classifier layered on top of a healthy
// running supervisor. It ALSO installs a live-GUI-owner fake: since Part B
// (headless-fleet recovery) added a guiOwnerAliveFn() call to the `if
// running` branch, a caller here that drove runEnsureAlive without this fake
// would fall through to the REAL probeGUIOwnerAlive and probe the
// developer's actual %LOCALAPPDATA% pidport — the exact fleet-wipe-class
// risk this file's header (:26-28) warns about. GUI-alive is the correct
// fixture for these tests regardless: it keeps runEnsureAlive on its
// existing "supervisor running; no action" no-op line (Part B's
// headless-fleet path only fires when GUI is dead), so none of the
// Phase-I-focused assertions in this file's other tests change.
func holdEnsureAliveSupervisorLock(t *testing.T, stateDir string) {
	t.Helper()
	lock, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	t.Cleanup(lock.Release)
	liveGUIOwner(t, 9999, 9125)
}

func TestEnsureAliveGUIRecovery_ConcurrentTicksUseProductionFlockExclusion(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	stateDir := ensureAliveTestStateDir(t)
	holdEnsureAliveSupervisorLock(t, stateDir)
	deadlines := ensureAliveGUIRecoveryTestDeadlines(now)
	deadlines.RecordLock = time.Second

	store := &ensureAliveGUIRecoveryMarkerFake{
		record:            expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseReserved),
		interruptEntered:  make(chan struct{}, 1),
		interruptContinue: make(chan struct{}),
	}
	var probeCalls atomic.Int32
	var freeResults atomic.Int32
	var heldResults atomic.Int32
	probe := func(ctx context.Context, request gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
		probeCalls.Add(1)
		result := gui.ProbeGUIOwnerLease(ctx, request)
		switch result.State {
		case gui.GUIOwnerLeaseStateFree:
			freeResults.Add(1)
		case gui.GUIOwnerLeaseStateHeld:
			heldResults.Add(1)
		}
		return result
	}
	installEnsureAliveGUIRecoveryTestDependencies(t, deadlines, store, probe)

	var guiAutostartCalls atomic.Int32
	restoreAutostart := setLivenessRelaunchFnForTest(func() error {
		guiAutostartCalls.Add(1)
		return nil
	})
	t.Cleanup(restoreAutostart)
	var standaloneCalls atomic.Int32
	restoreStandalone := setStandaloneRelaunchFnForTest(func() error {
		standaloneCalls.Add(1)
		return nil
	})
	t.Cleanup(restoreStandalone)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_ = runEnsureAlive(stateDir, &bytes.Buffer{})
	}()
	select {
	case <-store.interruptEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first ensure-alive tick did not reach the owned-free interrupt CAS")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_ = runEnsureAlive(stateDir, &bytes.Buffer{})
	}()
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent ensure-alive tick did not complete while the first held the probe lease")
	}
	close(store.interruptContinue)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first ensure-alive tick did not complete after the CAS was released")
	}

	gotRecord, interruptCalls := store.snapshot()
	if interruptCalls != 1 {
		t.Fatalf("InterruptFromOwnedFreeProbe calls = %d, want exactly 1", interruptCalls)
	}
	if gotRecord.Phase != gui.HandoffPhaseInterrupted || gotRecord.Sequence != 3 {
		t.Fatalf("terminal marker = phase %q sequence %d, want interrupted/3", gotRecord.Phase, gotRecord.Sequence)
	}
	if probeCalls.Load() != 2 || freeResults.Load() != 1 || heldResults.Load() != 1 {
		t.Fatalf("production owner-probe results: calls=%d free=%d held=%d, want 2/1/1", probeCalls.Load(), freeResults.Load(), heldResults.Load())
	}
	if guiAutostartCalls.Load() != 0 || standaloneCalls.Load() != 0 {
		t.Fatalf("ensure-alive GUI recovery spawned via relaunch seams: autostart=%d standalone=%d, want 0/0", guiAutostartCalls.Load(), standaloneCalls.Load())
	}
}

func TestEnsureAliveGUIRecovery_FreeMessageReconcilesSupervisorLiveness(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name             string
		supervisorAlive  bool
		wantMessage      string
		wantEvent        string
		wantRelaunches   int32
		wantManualAction bool
	}{
		{
			name:             "supervisor alive keeps manual recovery guidance",
			supervisorAlive:  true,
			wantMessage:      ensureAliveGUIFreeMessage,
			wantEvent:        "gui-restart-interrupted-free-flock",
			wantManualAction: true,
		},
		{
			name:           "both dead reports automatic owner recovery",
			wantMessage:    "GUI restart interrupted; the supervisor owner is being recovered automatically.",
			wantEvent:      "gui-restart-interrupted-owner-recovering",
			wantRelaunches: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := ensureAliveTestStateDir(t)
			if tc.supervisorAlive {
				holdEnsureAliveSupervisorLock(t, stateDir)
			} else {
				noLiveGUIOwner(t)
			}
			store := &ensureAliveGUIRecoveryMarkerFake{
				record: expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseReserved),
			}
			lease := &ensureAliveGUIRecoveryLeaseFake{}
			installEnsureAliveGUIRecoveryTestDependencies(t, ensureAliveGUIRecoveryTestDeadlines(now), store, func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
				return gui.GUIOwnerLeaseProbeResult{
					State:  gui.GUIOwnerLeaseStateFree,
					Lease:  lease,
					Record: cloneEnsureAliveGUIRecoveryRecord(store.record),
				}
			})

			var relaunches atomic.Int32
			restoreRelaunch := setLivenessRelaunchFnForTest(func() error {
				relaunches.Add(1)
				return nil
			})
			t.Cleanup(restoreRelaunch)
			var standaloneRelaunches atomic.Int32
			restoreStandalone := setStandaloneRelaunchFnForTest(func() error {
				standaloneRelaunches.Add(1)
				return nil
			})
			t.Cleanup(restoreStandalone)

			out := &bytes.Buffer{}
			if err := runEnsureAlive(stateDir, out); err != nil {
				t.Fatalf("runEnsureAlive: %v", err)
			}
			if !strings.Contains(out.String(), tc.wantMessage) {
				t.Fatalf("output %q missing reconciled message %q", out.String(), tc.wantMessage)
			}
			if got := relaunches.Load(); got != tc.wantRelaunches {
				t.Fatalf("owner relaunch calls = %d, want %d", got, tc.wantRelaunches)
			}
			if got := standaloneRelaunches.Load(); got != 0 {
				t.Fatalf("standalone relaunch calls = %d, want 0", got)
			}
			if gotManual := strings.Contains(out.String(), "run `mcphub gui`"); gotManual != tc.wantManualAction {
				t.Fatalf("manual mcphub-gui guidance present = %t, want %t; output=%q", gotManual, tc.wantManualAction, out.String())
			}
			interrupted, interruptCalls := store.snapshot()
			if interruptCalls != 1 || interrupted == nil || interrupted.Phase != gui.HandoffPhaseInterrupted {
				t.Fatalf("interrupt result = record=%+v calls=%d, want one terminal interrupted write", interrupted, interruptCalls)
			}
			if gotManual := interrupted.OperatorAction == "mcphub gui"; gotManual != tc.wantManualAction {
				t.Fatalf("durable operator action = %q, manual=%t want %t", interrupted.OperatorAction, gotManual, tc.wantManualAction)
			}
			assertSupervisorEvent(t, stateDir, tc.wantEvent)
			logRaw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
			if err != nil {
				t.Fatalf("read supervisor event log: %v", err)
			}
			if strings.Contains(string(logRaw), `"handoff_id"`) {
				t.Fatalf("Phase-I event aliased generation into distinct Phase-F handoff_id: %s", logRaw)
			}
		})
	}
}

func TestEnsureAliveGUIRecovery_FreeVsHeldSelectsExactOperatorCommand(t *testing.T) {
	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		probeState         gui.GUIOwnerLeaseState
		probeReason        error
		interruptErr       error
		wantMessage        string
		wantCommand        string
		forbidCommand      string
		wantEvent          string
		wantInterruptCalls int
		wantInterrupted    bool
	}{
		{
			name:               "free",
			probeState:         gui.GUIOwnerLeaseStateFree,
			wantMessage:        ensureAliveGUIFreeMessage,
			wantCommand:        "mcphub gui",
			forbidCommand:      "mcphub gui --force --kill",
			wantEvent:          "gui-restart-interrupted-free-flock",
			wantInterruptCalls: 1,
			wantInterrupted:    true,
		},
		{
			name:               "held",
			probeState:         gui.GUIOwnerLeaseStateHeld,
			probeReason:        gui.ErrSingleInstanceBusy,
			wantMessage:        ensureAliveGUIHeldMessage,
			wantCommand:        "mcphub gui --force --kill",
			wantEvent:          "gui-restart-live-holder-wedged",
			wantInterruptCalls: 0,
		},
		{
			name:               "unknown",
			probeState:         gui.GUIOwnerLeaseStateUnknown,
			probeReason:        errors.New("synthetic owner uncertainty"),
			wantMessage:        ensureAliveGUIUnknownMessage,
			forbidCommand:      "mcphub gui",
			wantEvent:          "gui-restart-owner-unknown",
			wantInterruptCalls: 0,
		},
		{
			name:               "free marker write failure",
			probeState:         gui.GUIOwnerLeaseStateFree,
			interruptErr:       errors.New("synthetic interrupted marker write failure"),
			wantMessage:        ensureAliveGUIMarkerWriteFailedMessage,
			wantCommand:        "mcphub gui",
			forbidCommand:      "mcphub gui --force --kill",
			wantEvent:          "gui-restart-interrupted-marker-write-failed",
			wantInterruptCalls: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := ensureAliveTestStateDir(t)
			holdEnsureAliveSupervisorLock(t, stateDir)
			store := &ensureAliveGUIRecoveryMarkerFake{
				record:       expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseReserved),
				interruptErr: tc.interruptErr,
			}
			lease := &ensureAliveGUIRecoveryLeaseFake{}
			probe := func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
				result := gui.GUIOwnerLeaseProbeResult{
					State:  tc.probeState,
					Reason: tc.probeReason,
					Record: cloneEnsureAliveGUIRecoveryRecord(store.record),
				}
				if tc.probeState == gui.GUIOwnerLeaseStateFree {
					result.Lease = lease
				}
				return result
			}
			installEnsureAliveGUIRecoveryTestDependencies(t, ensureAliveGUIRecoveryTestDeadlines(now), store, probe)

			out := &bytes.Buffer{}
			if err := runEnsureAlive(stateDir, out); err != nil {
				t.Fatalf("runEnsureAlive: %v", err)
			}
			if !strings.Contains(out.String(), tc.wantMessage) {
				t.Fatalf("output %q missing exact message %q", out.String(), tc.wantMessage)
			}
			if tc.wantCommand != "" && !strings.Contains(out.String(), tc.wantCommand) {
				t.Fatalf("output %q missing operator command %q", out.String(), tc.wantCommand)
			}
			if tc.forbidCommand != "" && strings.Contains(out.String(), tc.forbidCommand) {
				t.Fatalf("output %q selected forbidden operator command %q", out.String(), tc.forbidCommand)
			}
			assertSupervisorEvent(t, stateDir, tc.wantEvent)

			gotRecord, interruptCalls := store.snapshot()
			if interruptCalls != tc.wantInterruptCalls {
				t.Fatalf("InterruptFromOwnedFreeProbe calls = %d, want %d", interruptCalls, tc.wantInterruptCalls)
			}
			if got := gotRecord.Phase == gui.HandoffPhaseInterrupted; got != tc.wantInterrupted {
				t.Fatalf("marker interrupted = %t, want %t (record=%+v)", got, tc.wantInterrupted, gotRecord)
			}
			if tc.probeState == gui.GUIOwnerLeaseStateFree && lease.releases.Load() != 1 {
				t.Fatalf("owned Free probe lease releases = %d, want 1", lease.releases.Load())
			}
		})
	}
}

func TestEnsureAliveGUIRecovery_IneligibleOrUnreadableMarkersNeverProbeMutateOrChooseCommand(t *testing.T) {
	now := time.Date(2026, 7, 18, 13, 30, 0, 0, time.UTC)
	tests := []struct {
		name        string
		record      *gui.HandoffMarkerRecord
		readErr     error
		wantUnknown bool
	}{
		{name: "absent"},
		{name: "committed", record: expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseCommitted)},
		{name: "interrupted", record: expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseInterrupted)},
		{
			name: "fresh in-progress",
			record: func() *gui.HandoffMarkerRecord {
				record := expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseInProgress)
				record.FreshUntil = now.Add(time.Minute)
				return record
			}(),
		},
		{name: "unknown schema", readErr: errors.New("unknown marker version"), wantUnknown: true},
		{name: "state-dir mismatch", readErr: errors.New("marker state directory mismatch"), wantUnknown: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := ensureAliveTestStateDir(t)
			store := &ensureAliveGUIRecoveryMarkerFake{record: tc.record, readErr: tc.readErr}
			var probeCalls atomic.Int32
			installEnsureAliveGUIRecoveryTestDependencies(t, ensureAliveGUIRecoveryTestDeadlines(now), store, func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
				probeCalls.Add(1)
				return gui.GUIOwnerLeaseProbeResult{State: gui.GUIOwnerLeaseStateFree, Lease: &ensureAliveGUIRecoveryLeaseFake{}}
			})
			out := &bytes.Buffer{}

			runEnsureAliveGUIRecovery(stateDir, out)

			if probeCalls.Load() != 0 {
				t.Fatalf("owner probe calls = %d, want 0", probeCalls.Load())
			}
			_, interruptCalls := store.snapshot()
			if interruptCalls != 0 {
				t.Fatalf("marker interrupt calls = %d, want 0", interruptCalls)
			}
			if strings.Contains(out.String(), "mcphub gui") {
				t.Fatalf("ineligible/unreadable marker selected an operator command: %q", out.String())
			}
			if gotUnknown := strings.Contains(out.String(), ensureAliveGUIUnknownMessage); gotUnknown != tc.wantUnknown {
				t.Fatalf("unknown diagnostic present = %t, want %t; output=%q", gotUnknown, tc.wantUnknown, out.String())
			}
			if tc.wantUnknown {
				assertSupervisorEvent(t, stateDir, "gui-restart-owner-unknown")
			}
		})
	}
}

func TestEnsureAliveGUIRecovery_ReservationWindowSuppressesAndSupervisorLiveStillNoOps(t *testing.T) {
	now := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	stateDir := ensureAliveTestStateDir(t)
	holdEnsureAliveSupervisorLock(t, stateDir)
	record := expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseReserved)
	record.ReservationExpiresAt = now.Add(time.Minute)
	store := &ensureAliveGUIRecoveryMarkerFake{record: record}
	var probeCalls atomic.Int32
	installEnsureAliveGUIRecoveryTestDependencies(t, ensureAliveGUIRecoveryTestDeadlines(now), store, func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
		probeCalls.Add(1)
		return gui.GUIOwnerLeaseProbeResult{State: gui.GUIOwnerLeaseStateHeld, Reason: gui.ErrHandoffReserved}
	})

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v", err)
	}
	if probeCalls.Load() != 0 {
		t.Fatalf("reservation-aware probe calls inside healthy reservation window = %d, want 0", probeCalls.Load())
	}
	if !strings.Contains(out.String(), "supervisor running") || strings.Contains(out.String(), "GUI restart") {
		t.Fatalf("supervisor-live output changed or reservation was not suppressed: %q", out.String())
	}
	_, interruptCalls := store.snapshot()
	if interruptCalls != 0 {
		t.Fatalf("marker interrupts inside healthy reservation window = %d, want 0", interruptCalls)
	}
}

func TestEnsureAliveGUIRecovery_LateStandbyRejectsInterruptedMarker(t *testing.T) {
	clockNow := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	stateDir := ensureAliveTestStateDir(t)
	holdEnsureAliveSupervisorLock(t, stateDir)
	deadlines := ensureAliveGUIRecoveryTestDeadlines(clockNow)
	deadlines.Now = func() time.Time { return clockNow }
	store := gui.NewHandoffMarkerStore(stateDir, deadlines)
	nonce := []byte("late-standby-child")
	hash := sha256.Sum256(nonce)
	begin, err := store.Begin(gui.HandoffBegin{
		Generation: "late-standby-generation",
		Route:      gui.HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     101,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Reserve(begin.Generation, begin.Sequence, clockNow.Add(time.Second), "sha256:"+hex.EncodeToString(hash[:]), 202); err != nil {
		t.Fatalf("Reserve handoff: %v", err)
	}
	clockNow = clockNow.Add(2 * time.Second)
	installEnsureAliveGUIRecoveryTestDependencies(t, deadlines, store, gui.ProbeGUIOwnerLease)

	if err := runEnsureAlive(stateDir, &bytes.Buffer{}); err != nil {
		t.Fatalf("runEnsureAlive: %v", err)
	}
	interrupted, err := store.Read()
	if err != nil {
		t.Fatalf("Read interrupted marker: %v", err)
	}
	if interrupted == nil || interrupted.Phase != gui.HandoffPhaseInterrupted {
		t.Fatalf("ensure-alive marker = %+v, want interrupted", interrupted)
	}

	lease, err := gui.AcquireSingleInstanceAt(filepath.Join(stateDir, gui.PidportFileLeaf), 9125, gui.SingleInstanceAcquireOptions{
		RestartV3Enabled:     true,
		MarkerStore:          store,
		DesignatedChildNonce: nonce,
		Deadlines:            deadlines,
	})
	if lease != nil {
		lease.Release()
		t.Fatal("late standby child acquired the GUI flock after ensure-alive wrote interrupted")
	}
	if !errors.Is(err, gui.ErrHandoffReserved) {
		t.Fatalf("late standby child error = %v, want ErrHandoffReserved", err)
	}
}

func TestEnsureAliveGUIRecovery_TotalBudgetCannotStarveSupervisorLiveness(t *testing.T) {
	now := time.Date(2026, 7, 18, 16, 0, 0, 0, time.UTC)
	stateDir := ensureAliveTestStateDir(t)
	noLiveGUIOwner(t)
	deadlines := ensureAliveGUIRecoveryTestDeadlines(now)
	deadlines.RecordLock = 40 * time.Millisecond
	readContinue := make(chan struct{})
	unblockRead := sync.OnceFunc(func() { close(readContinue) })
	defer unblockRead()
	store := &ensureAliveGUIRecoveryMarkerFake{
		readEntered:  make(chan struct{}, 1),
		readContinue: readContinue,
	}
	installEnsureAliveGUIRecoveryTestDependencies(t, deadlines, store, func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
		t.Fatal("marker-read timeout reached owner probe")
		return gui.GUIOwnerLeaseProbeResult{}
	})
	var relaunches atomic.Int32
	restoreRelaunch := setLivenessRelaunchFnForTest(func() error {
		relaunches.Add(1)
		return nil
	})
	t.Cleanup(restoreRelaunch)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runEnsureAlive(stateDir, &bytes.Buffer{})
	}()
	select {
	case <-store.readEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("ensure-alive did not reach the synthetic wedged marker read")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		unblockRead()
		<-done
		t.Fatal("wedged marker read starved the supervisor-liveness recovery past the total classifier budget")
	}
	if got := relaunches.Load(); got != 1 {
		t.Fatalf("owner relaunch calls after classifier deadline = %d, want 1", got)
	}
}

func TestEnsureAliveGUIRecovery_ClassifierTimeoutRetainsLeaseUntilCASCompletes(t *testing.T) {
	now := time.Date(2026, 7, 18, 16, 15, 0, 0, time.UTC)
	stateDir := ensureAliveTestStateDir(t)
	noLiveGUIOwner(t)
	deadlines := ensureAliveGUIRecoveryTestDeadlines(now)
	deadlines.RecordLock = 40 * time.Millisecond
	lease := &ensureAliveGUIRecoveryLeaseFake{released: make(chan struct{})}
	interruptContinue := make(chan struct{})
	unblockInterrupt := sync.OnceFunc(func() { close(interruptContinue) })
	defer unblockInterrupt()
	var committedAfterRelease atomic.Bool
	store := &ensureAliveGUIRecoveryMarkerFake{
		record:            expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseReserved),
		interruptEntered:  make(chan struct{}, 1),
		interruptContinue: interruptContinue,
		interruptBeforeCommit: func() {
			if lease.releases.Load() != 0 {
				committedAfterRelease.Store(true)
			}
		},
	}
	installEnsureAliveGUIRecoveryTestDependencies(t, deadlines, store, func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
		return gui.GUIOwnerLeaseProbeResult{
			State:  gui.GUIOwnerLeaseStateFree,
			Lease:  lease,
			Record: cloneEnsureAliveGUIRecoveryRecord(store.record),
		}
	})
	var relaunches atomic.Int32
	restoreRelaunch := setLivenessRelaunchFnForTest(func() error {
		relaunches.Add(1)
		return nil
	})
	t.Cleanup(restoreRelaunch)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runEnsureAlive(stateDir, &bytes.Buffer{})
	}()
	select {
	case <-store.interruptEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("ensure-alive did not reach the deterministically blocked marker CAS")
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		unblockInterrupt()
		<-done
		t.Fatal("blocked marker CAS starved the supervisor-liveness recovery past the total classifier budget")
	}
	if got := relaunches.Load(); got != 1 {
		t.Fatalf("owner relaunch calls after classifier deadline = %d, want 1", got)
	}
	if got := lease.releases.Load(); got != 0 {
		t.Fatalf("owned probe lease released while CAS was still executing = %d, want 0", got)
	}

	unblockInterrupt()
	select {
	case <-lease.released:
	case <-time.After(2 * time.Second):
		t.Fatal("owned probe lease was not released after the CAS completed")
	}
	if committedAfterRelease.Load() {
		t.Fatal("marker CAS committed after its owned GUI probe lease was released")
	}
	interrupted, interruptCalls := store.snapshot()
	if interruptCalls != 1 || interrupted == nil || interrupted.Phase != gui.HandoffPhaseInterrupted {
		t.Fatalf("interrupt result = record=%+v calls=%d, want one terminal interrupted write", interrupted, interruptCalls)
	}
}

func TestEnsureAliveGUIRecovery_FreeProbeContractFailureReleasesBeforeDiagnostics(t *testing.T) {
	now := time.Date(2026, 7, 18, 16, 30, 0, 0, time.UTC)
	stateDir := ensureAliveTestStateDir(t)
	store := &ensureAliveGUIRecoveryMarkerFake{
		record: expiredEnsureAliveGUIRecoveryRecord(now, gui.HandoffPhaseReserved),
	}
	lease := &ensureAliveGUIRecoveryLeaseFake{}
	mismatched := cloneEnsureAliveGUIRecoveryRecord(store.record)
	mismatched.Sequence++
	installEnsureAliveGUIRecoveryTestDependencies(t, ensureAliveGUIRecoveryTestDeadlines(now), store, func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
		return gui.GUIOwnerLeaseProbeResult{
			State:  gui.GUIOwnerLeaseStateFree,
			Lease:  lease,
			Record: mismatched,
		}
	})
	out := &ensureAliveGUIRecoveryReleaseCheckingWriter{lease: lease}

	runEnsureAliveGUIRecovery(stateDir, out)

	if lease.releases.Load() != 1 {
		t.Fatalf("mismatched Free probe lease releases = %d, want 1", lease.releases.Load())
	}
	if out.wroteBeforeRelease {
		t.Fatalf("mismatched Free probe emitted diagnostics before releasing its owned lease: %q", out.String())
	}
	if !strings.Contains(out.String(), ensureAliveGUIUnknownMessage) {
		t.Fatalf("mismatched Free probe output = %q, want unknown diagnostic", out.String())
	}
	_, interruptCalls := store.snapshot()
	if interruptCalls != 0 {
		t.Fatalf("mismatched Free probe marker interrupts = %d, want 0", interruptCalls)
	}
}

func TestRestartV3_FeatureGateInertMatrix(t *testing.T) {
	stateDir := ensureAliveTestStateDir(t)
	holdEnsureAliveSupervisorLock(t, stateDir)
	restore := setEnsureAliveGUIRecoveryDependenciesForTest(ensureAliveGUIRecoveryDependencies{
		restartV3Enabled: func() bool { return false },
		restartDeadlines: func() gui.RestartDeadlines { panic("gate-off resolved restart deadlines") },
		markerStore: func(string, gui.RestartDeadlines) ensureAliveGUIRecoveryStore {
			panic("gate-off constructed a handoff marker store")
		},
		probeOwnerLease: func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult {
			panic("gate-off probed the GUI owner lease")
		},
	})
	t.Cleanup(restore)

	out := &bytes.Buffer{}
	if err := runEnsureAlive(stateDir, out); err != nil {
		t.Fatalf("runEnsureAlive: %v", err)
	}
	if !strings.Contains(out.String(), "ensure-alive: supervisor running") || strings.Contains(out.String(), "GUI restart") {
		t.Fatalf("gate-off supervisor-live path output changed: %q", out.String())
	}
}

// assertSupervisorEvent fails the test unless supervisor-events.log under
// stateDir contains a JSONL row whose "event" discriminator equals wantEvent.
// Used to prove the durable-observability fix (PR #283 review P3-d) actually
// writes the diagnostic the operator needs.
func assertSupervisorEvent(t *testing.T, stateDir, wantEvent string) {
	t.Helper()
	logPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read supervisor-events.log %q: %v", logPath, err)
	}
	needle := `"event":"` + wantEvent + `"`
	if !strings.Contains(string(raw), needle) {
		t.Fatalf("supervisor-events.log missing event %q; log body=%q", wantEvent, string(raw))
	}
}

// assertSupervisorEventBody fails the test unless supervisor-events.log
// under stateDir contains a line carrying BOTH the "event" discriminator AND
// the given body substring (e.g. `"reason":"boot-grace"`) — used where two
// call sites share one event name and only the body distinguishes them
// (gui-headless-fleet-relaunch-suppressed's machine-filterable reason field).
func assertSupervisorEventBody(t *testing.T, stateDir, wantEvent, wantBodySubstring string) {
	t.Helper()
	logPath := filepath.Join(stateDir, api.SupervisorEventLogFileLeaf)
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read supervisor-events.log %q: %v", logPath, err)
	}
	eventNeedle := `"event":"` + wantEvent + `"`
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, eventNeedle) && strings.Contains(line, wantBodySubstring) {
			return
		}
	}
	t.Fatalf("supervisor-events.log has no %q row carrying %q; log body=%q", wantEvent, wantBodySubstring, string(raw))
}
