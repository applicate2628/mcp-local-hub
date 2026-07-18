package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// noLiveGUIOwner installs the GUI-incumbent probe seam reporting NO live GUI
// owner. This is the genuine OWNER-death topology the relaunch path is for, and
// it ALSO keeps the test off the real %LOCALAPPDATA% gui.pidport (state safety:
// the production probe reads the developer's running GUI). Every test that
// exercises a supervisor-down branch MUST install this (or its live-owner
// counterpart below) so the real pidport is never probed.
func noLiveGUIOwner(t *testing.T) {
	t.Helper()
	restore := setGUIOwnerAliveFnForTest(func() (bool, int) { return false, 0 })
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
	restoreGUI := setGUIOwnerAliveFnForTest(func() (bool, int) { return true, 4242 })
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
	restoreGUI := setGUIOwnerAliveFnForTest(func() (bool, int) { return true, 4242 })
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

func holdEnsureAliveSupervisorLock(t *testing.T, stateDir string) {
	t.Helper()
	lock, err := api.AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	t.Cleanup(lock.Release)
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

func TestEnsureAliveGUIRecovery_GateOffSkipsBranchAndPreservesSupervisorLivePath(t *testing.T) {
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
