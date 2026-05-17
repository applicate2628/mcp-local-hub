// Package cli — Tests for Task 13.1 cold-start stale-child reaper.
//
// Cross-platform: every test uses injected fakes via ReaperDeps so the
// same suite runs on Windows (where ReapStaleTransients is a no-op
// stub returning ReaperResult{} immediately) and on POSIX (where the
// real implementation walks transient_pids, applies the 3-gate
// ownership check, kills via process-group, settles, and clears
// state). On Windows the production code path under test is the
// no-op stub — the suite still verifies the API surface compiles
// and that the function returns cleanly with no kills, no settle.
//
// Discipline (per Task 13.1 spec):
//   - DO NOT call real /proc/<pid>/* files (inject via ProcessIdentity).
//   - DO NOT call real syscall.Kill (inject via KillProcessGroup).
//   - DO NOT depend on any specific PID being alive in the test host.
package cli

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// fakeProc is one synthetic process the test surface knows about.
type fakeProc struct {
	pid      int
	alive    bool
	basename string
	cmdline  string
	uid      int
	idOK     bool // ProcessIdentity returns ok=this
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
	killed      []int    // PIDs the fake KillProcessGroup recorded
	killErrs    map[int]error
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
		SettleDuration: 50 * time.Millisecond, // small for tests
		Now: func() time.Time {
			f.nowCalls++
			return f.now
		},
	}
}

// addOwnedAlivePID adds a fake "alive + 3-gate-passes" process at pid.
func (f *reaperFakes) addOwnedAlivePID(pid int) {
	f.procs[pid] = fakeProc{
		pid:      pid,
		alive:    true,
		basename: "mcphub",
		cmdline:  "mcphub daemon --server memory --daemon default",
		uid:      f.currentUID,
		idOK:     true,
	}
	f.stateBefore.TransientPIDs = append(f.stateBefore.TransientPIDs, api.TransientPID{
		PID:       pid,
		Kind:      "test-fixture",
		StartedAt: f.now.Add(-1 * time.Minute).Format(time.RFC3339Nano),
	})
}

// addAliveWrongUIDPID adds a fake "alive but UID mismatch" process.
func (f *reaperFakes) addAliveWrongUIDPID(pid int) {
	f.procs[pid] = fakeProc{
		pid:      pid,
		alive:    true,
		basename: "mcphub",
		cmdline:  "mcphub daemon --server memory --daemon default",
		uid:      f.currentUID + 1, // mismatch
		idOK:     true,
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

// expectWindowsNoOp returns true when the test runs on Windows, where
// the reaper is a hard no-op (Job Object reaps automatically) and most
// assertions about kill/settle/state-write must collapse to "zero".
func expectWindowsNoOp() bool { return runtime.GOOS == "windows" }

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
// TestReaper_KillFailureNonFatal
//
// KillProcessGroup returns an error → the reaper still records the
// attempted kill, still settles, and still clears state. Reaping is
// best-effort: a failed kill should NOT preserve the stale PID in
// state (where it would only confuse the next supervisor start that
// reads the same stale entry).
// ---------------------------------------------------------------------
func TestReaper_KillFailureNonFatal(t *testing.T) {
	if expectWindowsNoOp() {
		t.Skip("Windows reaper is a no-op; kill failure path is not applicable")
	}
	f := newReaperFakes(t)
	f.addOwnedAlivePID(3333)
	f.killErrs[3333] = errors.New("synthetic kill failure")

	res, err := ReapStaleTransients(context.Background(), f.deps())
	if err != nil {
		t.Fatalf("ReapStaleTransients must not return kill errors as fatal; got %v", err)
	}
	// KilledPIDs records ATTEMPTED kills regardless of error so logs
	// surface "we tried to kill 3333" even on failure.
	if len(res.KilledPIDs) != 1 || res.KilledPIDs[0] != 3333 {
		t.Errorf("KilledPIDs = %v; want [3333] (attempt recorded even on failure)", res.KilledPIDs)
	}
	if f.stateAfter == nil {
		t.Fatalf("WriteState was not called even after kill failure")
	}
	if len(f.stateAfter.TransientPIDs) != 0 {
		t.Errorf("TransientPIDs after reap = %v; want empty even on kill failure", f.stateAfter.TransientPIDs)
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
