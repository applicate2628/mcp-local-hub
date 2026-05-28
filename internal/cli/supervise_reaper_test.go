//go:build !windows

// Package cli — Tests for Task 13.1 cold-start stale-child reaper
// (POSIX behavior).
//
// POSIX-only: this suite exercises the POSIX reaper's 3-gate ownership
// check + process-group kill semantics via injected ReaperDeps fakes.
// Before PR #243 the Windows ReapStaleTransients was a no-op stub and
// this suite ran cross-platform, with expectWindowsNoOp() branches
// asserting that no-op. PR #243 round-2 P2 gave Windows its own active
// maintenance-transient reaper (start-time gate + tree-kill), covered
// by supervise_reaper_windows_test.go; this POSIX-oriented suite is now
// built only on non-Windows, so the expectWindowsNoOp() branches are
// dormant here.
//
// Discipline (per Task 13.1 spec):
//   - DO NOT call real /proc/<pid>/* files (inject via ProcessIdentity).
//   - DO NOT call real syscall.Kill (inject via KillProcessGroup).
//   - DO NOT depend on any specific PID being alive in the test host.
package cli

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// fakeProc is one synthetic process the test surface knows about.
type fakeProc struct {
	pid       int
	alive     bool
	basename  string
	cmdline   string
	uid       int
	idOK      bool // ProcessIdentity returns ok=this
	startTime time.Time
	startOK   bool // ProcessStartTime returns ok=this
}

// reaperFakes is a test seam that bundles all injectable deps into
// one place so each test can populate just the parts it cares about.
type reaperFakes struct {
	t           *testing.T
	stateBefore *api.SupervisorStateFile
	stateAfter  *api.SupervisorStateFile // captured by WriteState fake
	writeCalls  int
	readErr     error
	writeErr    error
	procs       map[int]fakeProc
	currentUID  int
	killed      []int // PIDs the fake KillProcessGroup recorded
	killErrs    map[int]error
	// killed1 records per-PID (non-pgroup) kill fallback invocations and
	// killErrs1 maps a per-PID kill error. Used by ESRCH+alive+fallback
	// tests (Lane F P0 #5).
	killed1     []int
	killErrs1   map[int]error
	now         time.Time
	nowCalls    int
	sleepCalled time.Duration // accumulated settle duration observed via Now() diffs
}

// newReaperFakes returns a reaperFakes pre-populated with a sane no-PID
// state and an empty proc table.
func newReaperFakes(t *testing.T) *reaperFakes {
	t.Helper()
	return &reaperFakes{
		t:           t,
		stateBefore: &api.SupervisorStateFile{Version: 1, Daemons: map[string]api.SupervisorDaemonState{}},
		procs:       map[int]fakeProc{},
		currentUID:  1000,
		killErrs:    map[int]error{},
		killErrs1:   map[int]error{},
		now:         time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
	}
}

// deps builds a ReaperDeps populated from the fake.
func (f *reaperFakes) deps() ReaperDeps {
	return ReaperDeps{
		StateDir: "/fake/state",
		ReadState: func(path string) (*api.SupervisorStateFile, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			// Return a deep-enough copy: tests rely on the fact that the
			// reaper does not mutate the read result in place before the
			// write. We hand back the same pointer; if the reaper mutates
			// it, that is the bug under test.
			return f.stateBefore, nil
		},
		WriteState: func(path string, s *api.SupervisorStateFile) error {
			f.writeCalls++
			if f.writeErr != nil {
				return f.writeErr
			}
			f.stateAfter = s
			return nil
		},
		PIDAlive: func(pid int) bool {
			p, ok := f.procs[pid]
			if !ok {
				return false
			}
			return p.alive
		},
		ProcessIdentity: func(pid int) (string, string, int, bool) {
			p, ok := f.procs[pid]
			if !ok {
				return "", "", 0, false
			}
			return p.basename, p.cmdline, p.uid, p.idOK
		},
		CurrentUID: func() int { return f.currentUID },
		KillProcessGroup: func(pid int) error {
			f.killed = append(f.killed, pid)
			if err, ok := f.killErrs[pid]; ok {
				return err
			}
			return nil
		},
		KillProcess: func(pid int) error {
			f.killed1 = append(f.killed1, pid)
			if err, ok := f.killErrs1[pid]; ok {
				return err
			}
			return nil
		},
		ProcessStartTime: func(pid int) (time.Time, bool) {
			p, ok := f.procs[pid]
			if !ok {
				return time.Time{}, false
			}
			// Default-true branch (codex r5 P3 fix): only fire the
			// "match f.now" default when the fake was registered with
			// startOK=true AND no explicit startTime — i.e., the test
			// did not opt out of the StartedAt gate at all. A fake
			// configured with startOK=false MUST return (zero, false)
			// so the StartedAt gate fails closed (Darwin/probe-failure
			// safety semantic). The previous default unconditionally
			// reported (f.now, true) when startTime was zero, masking
			// the fail-closed path in tests that explicitly set
			// startOK=false to model probe failure.
			if p.startOK && p.startTime.IsZero() {
				return f.now, true
			}
			return p.startTime, p.startOK
		},
		StartedAtTolerance: 2 * time.Second,
		SettleDuration:     50 * time.Millisecond, // small for tests
		Now: func() time.Time {
			f.nowCalls++
			return f.now
		},
	}
}

// addOwnedAlivePID adds a fake "alive + all-gates-pass" process at pid.
// Records StartedAt and ProcessStartTime both equal to f.now so the
// Lane F P0 #4 StartedAt gate passes by default; tests that want a
// mismatch use addOwnedAliveStaleStartPID instead.
func (f *reaperFakes) addOwnedAlivePID(pid int) {
	f.procs[pid] = fakeProc{
		pid:       pid,
		alive:     true,
		basename:  "mcphub",
		cmdline:   "mcphub daemon --server memory --daemon default",
		uid:       f.currentUID,
		idOK:      true,
		startTime: f.now,
		startOK:   true,
	}
	f.stateBefore.TransientPIDs = append(f.stateBefore.TransientPIDs, api.TransientPID{
		PID:       pid,
		Kind:      "test-fixture",
		StartedAt: f.now.Format(time.RFC3339Nano),
	})
}

// addOwnedAliveStaleStartPID adds a fake "all ownership gates pass,
// but computed start time is OUTSIDE the StartedAt tolerance window"
// process. Used by TestReaper_StartedAtMismatchSkipsRecycledPID to
// drive the Lane F P0 #4 PID-recycle skip path.
func (f *reaperFakes) addOwnedAliveStaleStartPID(pid int) {
	f.procs[pid] = fakeProc{
		pid:      pid,
		alive:    true,
		basename: "mcphub",
		cmdline:  "mcphub daemon --server memory --daemon default",
		uid:      f.currentUID,
		idOK:     true,
		// 1 hour drift = far outside the default 2s tolerance.
		startTime: f.now.Add(-1 * time.Hour),
		startOK:   true,
	}
	f.stateBefore.TransientPIDs = append(f.stateBefore.TransientPIDs, api.TransientPID{
		PID:       pid,
		Kind:      "test-fixture",
		StartedAt: f.now.Format(time.RFC3339Nano),
	})
}

// addAliveWrongUIDPID adds a fake "alive but UID mismatch" process.
// StartedAt/start time match by default so the gate that fails is the
// ownership UID gate, not the StartedAt gate (avoids ambiguity in the
// test's failing-gate signal).
func (f *reaperFakes) addAliveWrongUIDPID(pid int) {
	f.procs[pid] = fakeProc{
		pid:       pid,
		alive:     true,
		basename:  "mcphub",
		cmdline:   "mcphub daemon --server memory --daemon default",
		uid:       f.currentUID + 1, // mismatch
		idOK:      true,
		startTime: f.now,
		startOK:   true,
	}
	f.stateBefore.TransientPIDs = append(f.stateBefore.TransientPIDs, api.TransientPID{
		PID:       pid,
		Kind:      "test-fixture",
		StartedAt: f.now.Format(time.RFC3339Nano),
	})
}

// addDeadPID adds a fake "PID no longer alive" entry.
func (f *reaperFakes) addDeadPID(pid int) {
	f.procs[pid] = fakeProc{
		pid:   pid,
		alive: false,
	}
	f.stateBefore.TransientPIDs = append(f.stateBefore.TransientPIDs, api.TransientPID{
		PID:       pid,
		Kind:      "test-fixture",
		StartedAt: f.now.Format(time.RFC3339Nano),
	})
}

// expectWindowsNoOp is retained as a dormant guard from when this suite
// ran cross-platform. The file is now built only on non-Windows (the
// Windows reaper has its own active behavior + test — see the package
// comment and supervise_reaper_windows_test.go), so this always returns
// false here and the `if expectWindowsNoOp()` branches never fire.
func expectWindowsNoOp() bool { return false }

// ---------------------------------------------------------------------
// TestReaper_EmptyStateNoOp
//
// transient_pids empty → reaper must not kill anything, must not sleep
// the settle interval. On POSIX the state write IS allowed (a defensive
// rewrite of an empty TransientPIDs slice is harmless and idempotent),
// but it is not required. On Windows the whole function is a no-op
// stub, so no write either.
// ---------------------------------------------------------------------
func TestReaper_EmptyStateNoOp(t *testing.T) {
	f := newReaperFakes(t)
	// stateBefore.TransientPIDs is already nil — no additions.

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty", res.KilledPIDs)
	}
	if len(res.SkippedPIDs) != 0 {
		t.Errorf("SkippedPIDs = %v; want empty", res.SkippedPIDs)
	}
	if len(res.DeadPIDs) != 0 {
		t.Errorf("DeadPIDs = %v; want empty", res.DeadPIDs)
	}
	if res.ClearedTransients != 0 {
		t.Errorf("ClearedTransients = %d; want 0", res.ClearedTransients)
	}
	if res.SettleDuration != 0 {
		t.Errorf("SettleDuration = %v; want 0", res.SettleDuration)
	}
	if len(f.killed) != 0 {
		t.Errorf("KillProcessGroup called %v; want no calls", f.killed)
	}
}

// ---------------------------------------------------------------------
// TestReaper_AliveAndOwnedKilled (POSIX-only effect)
//
// One alive+owned PID → reaper kills it, settles, and clears state.
// On Windows the no-op stub returns zero result; assertions are
// adjusted accordingly.
// ---------------------------------------------------------------------
func TestReaper_AliveAndOwnedKilled(t *testing.T) {
	f := newReaperFakes(t)
	f.addOwnedAlivePID(4242)

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}

	if expectWindowsNoOp() {
		if len(res.KilledPIDs) != 0 || res.ClearedTransients != 0 || res.SettleDuration != 0 {
			t.Fatalf("Windows reaper must be a hard no-op; got %+v", res)
		}
		if len(f.killed) != 0 {
			t.Errorf("Windows reaper must not invoke kill; got %v", f.killed)
		}
		return
	}

	if len(res.KilledPIDs) != 1 || res.KilledPIDs[0] != 4242 {
		t.Errorf("KilledPIDs = %v; want [4242]", res.KilledPIDs)
	}
	if res.ClearedTransients != 1 {
		t.Errorf("ClearedTransients = %d; want 1", res.ClearedTransients)
	}
	if res.SettleDuration <= 0 {
		t.Errorf("SettleDuration = %v; want >0 after a kill", res.SettleDuration)
	}
	if len(f.killed) != 1 || f.killed[0] != 4242 {
		t.Errorf("KillProcessGroup invoked with %v; want [4242]", f.killed)
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState was not called; expected a state-clear write")
	}
	if len(f.stateAfter.TransientPIDs) != 0 {
		t.Errorf("stateAfter.TransientPIDs = %v; want empty", f.stateAfter.TransientPIDs)
	}
}

// ---------------------------------------------------------------------
// TestReaper_AliveButNotOwnedSkipped
//
// PID alive but the 3-gate ownership check fails (UID mismatch in this
// case — same logic applies to basename mismatch or cmdline mismatch).
// Reaper must NOT kill it; SkippedPIDs must list it; state still
// cleared (we cannot leave that PID in the transient list since we
// have no recourse — the operator must intervene).
// ---------------------------------------------------------------------
func TestReaper_AliveButNotOwnedSkipped(t *testing.T) {
	f := newReaperFakes(t)
	f.addAliveWrongUIDPID(5555)

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}

	if expectWindowsNoOp() {
		if len(res.SkippedPIDs) != 0 {
			t.Fatalf("Windows reaper must be a hard no-op; got SkippedPIDs=%v", res.SkippedPIDs)
		}
		return
	}

	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty (gate failed)", res.KilledPIDs)
	}
	if len(res.SkippedPIDs) != 1 || res.SkippedPIDs[0] != 5555 {
		t.Errorf("SkippedPIDs = %v; want [5555]", res.SkippedPIDs)
	}
	if len(f.killed) != 0 {
		t.Errorf("KillProcessGroup invoked %v; want no calls when ownership gate fails", f.killed)
	}
}

// ---------------------------------------------------------------------
// TestReaper_DeadPIDsClearedWithoutKill
//
// PID is gone — no kill, no skip, but DeadPIDs records it and the
// state is still cleared.
// ---------------------------------------------------------------------
func TestReaper_DeadPIDsClearedWithoutKill(t *testing.T) {
	f := newReaperFakes(t)
	f.addDeadPID(9999)

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}

	if expectWindowsNoOp() {
		if len(res.DeadPIDs) != 0 {
			t.Fatalf("Windows reaper must be a hard no-op; got DeadPIDs=%v", res.DeadPIDs)
		}
		return
	}

	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty", res.KilledPIDs)
	}
	if len(res.DeadPIDs) != 1 || res.DeadPIDs[0] != 9999 {
		t.Errorf("DeadPIDs = %v; want [9999]", res.DeadPIDs)
	}
	if len(f.killed) != 0 {
		t.Errorf("KillProcessGroup invoked %v; want no calls on dead PID", f.killed)
	}
	if res.ClearedTransients != 1 {
		t.Errorf("ClearedTransients = %d; want 1", res.ClearedTransients)
	}
}

// ---------------------------------------------------------------------
// TestReaper_StatePersistedAfterReap
//
// After a successful reap of a mixed set (one owned-alive + one dead),
// the persisted state must have an empty (nil or zero-length)
// TransientPIDs slice — explicit nil is also acceptable since
// `omitempty` in the struct tag elides empty slices.
// ---------------------------------------------------------------------
func TestReaper_StatePersistedAfterReap(t *testing.T) {
	f := newReaperFakes(t)
	f.addOwnedAlivePID(1111)
	f.addDeadPID(2222)

	if _, err := ReapStaleTransients(context.Background(), f.deps()); err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}

	if expectWindowsNoOp() {
		// no-op: no write expected
		if f.writeCalls != 0 {
			t.Fatalf("Windows reaper must not write state; writeCalls=%d", f.writeCalls)
		}
		return
	}

	if f.writeCalls != 1 {
		t.Fatalf("WriteState called %d times; want exactly 1", f.writeCalls)
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState received nil state")
	}
	if len(f.stateAfter.TransientPIDs) != 0 {
		t.Errorf("TransientPIDs after reap = %v; want empty", f.stateAfter.TransientPIDs)
	}
}

// ---------------------------------------------------------------------
// TestReaper_SettleDurationRespected
//
// On POSIX the reaper must wait the full configured settle duration
// between the kill burst and the state-write/return so the parent
// supervisor's first reconcile spawn doesn't collide with TCP TIME_WAIT
// on the recently-killed daemon's port. The Now() fake returns a
// monotonic-ish wall clock; the reaper observes the elapsed time
// via Now()-before and Now()-after.
//
// To avoid flakiness from real-clock skew the test uses a tiny settle
// (50ms) configured in newReaperFakes.deps(). We verify the result's
// reported SettleDuration is at least the configured value, and that
// at minimum the elapsed wall time covers the settle.
// ---------------------------------------------------------------------
func TestReaper_SettleDurationRespected(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; settle duration is not applicable")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(7777)

	t0 := time.Now()
	res, err := ReapStaleTransients(context.Background(), f.deps())
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if res.SettleDuration < 50*time.Millisecond {
		t.Errorf("reported SettleDuration = %v; want >= 50ms", res.SettleDuration)
	}
	if elapsed < 50*time.Millisecond {
		t.Errorf("wall elapsed = %v; want >= 50ms (settle must be observed)", elapsed)
	}
}

// ---------------------------------------------------------------------
// TestReaper_KillErrorsRetained (Lane F P0 #5 / Lane B P0)
//
// KillProcessGroup returns a non-ESRCH error (EPERM, synthetic
// generic failure, etc.) → the reaper records a KillErrors entry AND
// retains the TransientPID in state so the next supervisor cold start
// can retry. KilledPIDs MUST NOT list the PID (no successful kill
// occurred); SettleDuration MUST be zero (nothing to settle for).
//
// Replaces the prior TestReaper_KillFailureNonFatal which asserted
// "still clear state on kill failure" — that semantic was a defect
// per Lane F P0 #5 (failed kills must survive so they retry).
// ---------------------------------------------------------------------
func TestReaper_KillErrorsRetained(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; kill failure path is not applicable")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(3333)
	f.killErrs[3333] = syscall.EPERM // non-ESRCH error class

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients must not return kill errors as fatal; got %v", err)
	}
	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty (kill failed, no success to record)", res.KilledPIDs)
	}
	if len(res.KillErrors) != 1 {
		t.Fatalf("KillErrors = %v; want one entry for pid 3333", res.KillErrors)
	}
	gotErr, ok := res.KillErrors[3333]
	if !ok {
		t.Fatalf("KillErrors missing pid 3333 entry: %v", res.KillErrors)
	}
	if !errors.Is(gotErr, syscall.EPERM) {
		t.Errorf("KillErrors[3333] = %v; want syscall.EPERM", gotErr)
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState was not called even after kill failure")
	}
	if len(f.stateAfter.TransientPIDs) != 1 || f.stateAfter.TransientPIDs[0].PID != 3333 {
		t.Errorf("TransientPIDs after reap = %v; want [{PID:3333}] retained for retry", f.stateAfter.TransientPIDs)
	}
}

// ---------------------------------------------------------------------
// TestReaper_ContextCancellation
//
// ctx canceled before/during reap → returns ctx.Err() and does NOT
// write back the state. Justification: a partially-completed reap
// leaves the kill side-effect in place (the kill syscall already
// fired by the time we noticed cancellation) but preserving the
// transient_pids[] entry on disk means the NEXT supervisor start can
// re-attempt the reap pass and confirm the PIDs are gone via the
// dead-PID path. That is strictly safer than clearing state on a
// cancel because clearing would mask any PIDs we never got to.
// ---------------------------------------------------------------------
func TestReaper_ContextCancellation(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; context cancellation has nothing to interrupt")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(8888)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before call

	_, err := ReapStaleTransients(ctx, f.deps())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
	if f.writeCalls != 0 {
		t.Errorf("WriteState called %d times after ctx cancel; want 0 (state preserved)", f.writeCalls)
	}
}

// ---------------------------------------------------------------------
// TestReaper_StartedAtMismatchSkipsRecycledPID (Lane F P0 #4)
//
// All ownership gates pass (basename, cmdline tokens, UID), but the
// fake ProcessStartTime returns a wall-clock 1 hour before the
// recorded StartedAt. The reaper must classify this as PID recycling,
// add the PID to SkippedPIDs, and NOT invoke KillProcessGroup.
// State is cleared (no recourse — operator must intervene).
// ---------------------------------------------------------------------
func TestReaper_StartedAtMismatchSkipsRecycledPID(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; StartedAt gate has no effect")
	}
	f := newReaperFakes(t)
	f.addOwnedAliveStaleStartPID(6060)

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty (StartedAt mismatch must skip kill)", res.KilledPIDs)
	}
	if len(res.SkippedPIDs) != 1 || res.SkippedPIDs[0] != 6060 {
		t.Errorf("SkippedPIDs = %v; want [6060]", res.SkippedPIDs)
	}
	if len(f.killed) != 0 {
		t.Errorf("KillProcessGroup invoked %v; want no calls when StartedAt gate fails", f.killed)
	}
	if len(f.killed1) != 0 {
		t.Errorf("KillProcess fallback invoked %v; want no calls when StartedAt gate fails", f.killed1)
	}
	if len(res.KillErrors) != 0 {
		t.Errorf("KillErrors = %v; want empty (no kill was attempted)", res.KillErrors)
	}
}

// ---------------------------------------------------------------------
// TestReaper_StartedAtGateRejectsProbeFailure (Lane F P0 #4 — Darwin
// safety mode)
//
// On Darwin (and any host without /proc) processStartTime returns
// ok=false. The reaper must fail closed: gate fails, PID skipped, no
// kill. This test simulates the Darwin failure mode via a fakeProc
// with startOK=false.
// ---------------------------------------------------------------------
func TestReaper_StartedAtGateRejectsProbeFailure(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; ProcessStartTime gate has no effect")
	}
	f := newReaperFakes(t)
	// Construct a manual fakeProc with startOK=false (simulates Darwin
	// fallback). Use addOwnedAlivePID then override the proc map.
	f.addOwnedAlivePID(7070)
	p := f.procs[7070]
	p.startOK = false
	p.startTime = time.Time{}
	f.procs[7070] = p

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty (StartedAt probe failure must skip kill)", res.KilledPIDs)
	}
	if len(res.SkippedPIDs) != 1 || res.SkippedPIDs[0] != 7070 {
		t.Errorf("SkippedPIDs = %v; want [7070]", res.SkippedPIDs)
	}
	if len(f.killed) != 0 {
		t.Errorf("KillProcessGroup invoked %v; want no calls", f.killed)
	}
}

// ---------------------------------------------------------------------
// TestReaper_ESRCHFromMissingPgroupPreservesState (Lane F P0 #5 /
// Lane B P0)
//
// KillProcessGroup returns ESRCH (no such process group leader —
// happens when spawn never called Setpgid). PIDAlive still reports
// the process is alive on the post-kill re-check, so the reaper
// falls back to per-PID kill (deps.KillProcess). The fake makes the
// per-PID kill fail too (EPERM). Expected result:
//
//   - KilledPIDs empty
//   - KillErrors[pid] = EPERM
//   - TransientPIDs retained in state for retry
//   - KillProcessGroup AND KillProcess both invoked
//
// ---------------------------------------------------------------------
func TestReaper_ESRCHFromMissingPgroupPreservesState(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; ESRCH path is not applicable")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(4040)
	f.killErrs[4040] = syscall.ESRCH
	// PIDAlive remains true (the alive flag is still set on procs[4040])
	// so the post-ESRCH re-check classifies as "no pgroup leader" and
	// the per-PID fallback fires. Make the per-PID kill ALSO fail so
	// the entry retains in state.
	f.killErrs1[4040] = syscall.EPERM

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty (both kill attempts failed)", res.KilledPIDs)
	}
	if len(f.killed) != 1 || f.killed[0] != 4040 {
		t.Errorf("KillProcessGroup invocations = %v; want [4040] (one pgroup attempt)", f.killed)
	}
	if len(f.killed1) != 1 || f.killed1[0] != 4040 {
		t.Errorf("KillProcess invocations = %v; want [4040] (one per-PID fallback)", f.killed1)
	}
	if len(res.KillErrors) != 1 {
		t.Fatalf("KillErrors = %v; want one entry for pid 4040", res.KillErrors)
	}
	if !errors.Is(res.KillErrors[4040], syscall.EPERM) {
		t.Errorf("KillErrors[4040] = %v; want syscall.EPERM (fallback error)", res.KillErrors[4040])
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState was not called")
	}
	if len(f.stateAfter.TransientPIDs) != 1 || f.stateAfter.TransientPIDs[0].PID != 4040 {
		t.Errorf("TransientPIDs after reap = %v; want [{PID:4040}] retained", f.stateAfter.TransientPIDs)
	}
}

// ---------------------------------------------------------------------
// TestReaper_ESRCHFromProcessAlreadyGoneClearsState (Lane F P0 #5)
//
// KillProcessGroup returns ESRCH AND the post-kill PIDAlive re-check
// reports the process is gone. The reaper must classify this as the
// benign race (kernel reaped the process between alive-check and
// kill), record it in DeadPIDs, NOT fall back to per-PID kill, and
// clear state.
// ---------------------------------------------------------------------
func TestReaper_ESRCHFromProcessAlreadyGoneClearsState(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; ESRCH path is not applicable")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(5050)
	// Make KillProcessGroup return ESRCH; the SAME act of "kill returned
	// ESRCH" implies the kernel decided the process group is gone. We
	// simulate the post-kill re-check by flipping PIDAlive false via a
	// custom override on the proc table after the seam captures the
	// initial alive state.
	f.killErrs[5050] = syscall.ESRCH
	// Override PIDAlive to flip on the SECOND call: first call (pre-gate)
	// returns true; subsequent call (post-ESRCH) returns false. Easiest
	// approach: rebuild deps with a closure that counts calls.
	deps := f.deps()
	calls := 0
	deps.PIDAlive = func(pid int) bool {
		calls++
		if pid == 5050 && calls > 1 {
			return false
		}
		p, ok := f.procs[pid]
		if !ok {
			return false
		}
		return p.alive
	}

	res, err := ReapStaleTransients(context.Background(), deps)
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if len(res.KilledPIDs) != 0 {
		t.Errorf("KilledPIDs = %v; want empty (kernel already reaped)", res.KilledPIDs)
	}
	if len(res.DeadPIDs) != 1 || res.DeadPIDs[0] != 5050 {
		t.Errorf("DeadPIDs = %v; want [5050] (ESRCH + post-kill dead = benign race)", res.DeadPIDs)
	}
	if len(f.killed1) != 0 {
		t.Errorf("KillProcess fallback invoked %v; want no calls (process gone, no fallback needed)", f.killed1)
	}
	if len(res.KillErrors) != 0 {
		t.Errorf("KillErrors = %v; want empty (ESRCH on gone process is not an error)", res.KillErrors)
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState was not called")
	}
	if len(f.stateAfter.TransientPIDs) != 0 {
		t.Errorf("TransientPIDs after reap = %v; want empty (state cleared on gone process)", f.stateAfter.TransientPIDs)
	}
}

// ---------------------------------------------------------------------
// TestReaper_ESRCHFromMissingPgroupFallbackSucceeds (Lane F P0 #5)
//
// KillProcessGroup returns ESRCH (no pgroup leader), PIDAlive on the
// re-check reports the process is still alive, deps.KillProcess
// succeeds. The reaper must record the PID in KilledPIDs and clear
// state — the per-PID fallback closed the orphan.
// ---------------------------------------------------------------------
func TestReaper_ESRCHFromMissingPgroupFallbackSucceeds(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; ESRCH fallback path is not applicable")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(2020)
	f.killErrs[2020] = syscall.ESRCH
	// KillProcess fallback returns nil (success) — no entry in killErrs1.

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients: %v", err)
	}
	if len(res.KilledPIDs) != 1 || res.KilledPIDs[0] != 2020 {
		t.Errorf("KilledPIDs = %v; want [2020] (per-PID fallback succeeded)", res.KilledPIDs)
	}
	if len(f.killed) != 1 || f.killed[0] != 2020 {
		t.Errorf("KillProcessGroup invocations = %v; want [2020] (one initial attempt)", f.killed)
	}
	if len(f.killed1) != 1 || f.killed1[0] != 2020 {
		t.Errorf("KillProcess invocations = %v; want [2020] (one fallback)", f.killed1)
	}
	if len(res.KillErrors) != 0 {
		t.Errorf("KillErrors = %v; want empty (fallback succeeded)", res.KillErrors)
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState was not called")
	}
	if len(f.stateAfter.TransientPIDs) != 0 {
		t.Errorf("TransientPIDs after reap = %v; want empty (state cleared on successful kill)", f.stateAfter.TransientPIDs)
	}
}
