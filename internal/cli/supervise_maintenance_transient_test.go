// Tests for Task 9.2 — transient PID tracking + per-timer mutex +
// quiesce-drain state-shape compatibility.
//
// Spec §"Single-run guard + transient tracking"
// (docs/superpowers/specs/2026-05-16-v0.5.0-supervisor-architecture.md
// lines 466-468) + §"Graceful exit + quiesce drain" (lines 470-479).
// Plan Task 9.2 (docs/superpowers/plans/2026-05-16-v0.5.0-supervisor.md
// lines 2364-2368).
//
// TDD discipline: these tests are authored BEFORE Task 9.2 impl extends
// supervise_maintenance.go (Spawner interface, transient mutex map,
// claim/spawn/wait pipeline). They fail on first run; impl follows.
//
// Test runtime budget: <500ms total (no real subprocess; channel-based
// fakeSpawner.Wait gates only).
package cli

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// transientTestState extends testStateStore with the Task 9.2 transient
// PID surface. Stored as a flat slice (no map keyed by PID) so the test
// shape matches api.SupervisorStateFile.TransientPIDs exactly — the
// quiesce-drain state-shape test asserts that match.
type transientTestState struct {
	mu        sync.Mutex
	fired     map[string]string
	transient []api.TransientPID
	// Optional pointer the state-shape test wires to a real
	// SupervisorStateFile.TransientPIDs slice; if non-nil, every
	// transient mutation writes through to it too.
	mirror *[]api.TransientPID
}

func newTransientTestState() *transientTestState {
	return &transientTestState{fired: map[string]string{}}
}

func (s *transientTestState) GetMaintenanceFiredAt(kind string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.fired[kind]
	return v, ok
}

func (s *transientTestState) SetMaintenanceFiredAt(kind, rfc3339nanoUTC string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fired[kind] = rfc3339nanoUTC
	return nil
}

func (s *transientTestState) AddTransientPID(p api.TransientPID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transient = append(s.transient, p)
	if s.mirror != nil {
		*s.mirror = append(*s.mirror, p)
	}
}

func (s *transientTestState) RemoveTransientPID(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.transient[:0]
	for _, t := range s.transient {
		if t.PID == pid {
			continue
		}
		out = append(out, t)
	}
	s.transient = out

	if s.mirror != nil {
		mirror := (*s.mirror)[:0]
		for _, t := range *s.mirror {
			if t.PID == pid {
				continue
			}
			mirror = append(mirror, t)
		}
		*s.mirror = mirror
	}
}

func (s *transientTestState) snapshot() []api.TransientPID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]api.TransientPID, len(s.transient))
	copy(out, s.transient)
	return out
}

// fakeSpawner is the test injection point for Spawner. Start returns a
// caller-controllable PID (default: ascending int starting at 1000).
// Wait blocks on a per-PID channel the test closes to release the
// post-spawn goroutine.
type fakeSpawner struct {
	mu            sync.Mutex
	nextPID       int
	waitCh        map[int]chan struct{}
	waitErr       map[int]error
	startError    error
	startPanic    bool
	startCallback func(api.MaintenanceTimer) // observation hook, fires AFTER PID assignment, BEFORE return
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{
		nextPID: 1000,
		waitCh:  map[int]chan struct{}{},
		waitErr: map[int]error{},
	}
}

func (f *fakeSpawner) Start(t api.MaintenanceTimer) (int, error) {
	f.mu.Lock()
	if f.startPanic {
		f.mu.Unlock()
		panic("fakeSpawner.Start: synthetic panic")
	}
	if f.startError != nil {
		err := f.startError
		f.mu.Unlock()
		return 0, err
	}
	pid := f.nextPID
	f.nextPID++
	f.waitCh[pid] = make(chan struct{})
	cb := f.startCallback
	f.mu.Unlock()
	if cb != nil {
		cb(t)
	}
	return pid, nil
}

func (f *fakeSpawner) Wait(pid int) error {
	f.mu.Lock()
	ch := f.waitCh[pid]
	f.mu.Unlock()
	if ch == nil {
		return errors.New("fakeSpawner.Wait: unknown PID")
	}
	<-ch
	f.mu.Lock()
	err := f.waitErr[pid]
	f.mu.Unlock()
	return err
}

// finish unblocks Wait for the given PID. err may be nil.
func (f *fakeSpawner) finish(pid int, err error) {
	f.mu.Lock()
	if err != nil {
		f.waitErr[pid] = err
	}
	ch := f.waitCh[pid]
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// waitForCondition polls the predicate until it returns true or the
// budget expires. Returns true on success, false on budget exhaustion.
// 10ms granularity keeps the total wall-time bound tight.
func waitForCondition(budget time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

// fixedClock returns a constant time.Time, used to make StartedAt
// deterministic for assertions.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// sundayDue is a stable "now" value past Sun 03:00 local that fires both
// known kinds without ambiguity.
func sundayDue() time.Time { return time.Date(2026, 5, 17, 4, 0, 0, 0, time.Local) }

// TestMaintenance_Transient_RecordsPIDBeforeSpawn — when Spawner.Start
// panics, a TransientPID claim slot must already be in state when the
// panic is observed, proving the "record-before-syscall" invariant.
func TestMaintenance_Transient_RecordsPIDBeforeSpawn(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sp.startPanic = true
	sched.SetSpawner(sp)

	defer func() {
		if r := recover(); r == nil {
			// The scheduler should let the panic propagate
			// (or catch + log — either is acceptable as long
			// as the claim slot survives). If it caught the
			// panic, we still verify the claim.
		}
		snap := state.snapshot()
		// Either: (a) the scheduler's spawn-error rollback fired in
		// the defer and removed the claim — in which case the
		// invariant is NOT proven by this test path. Reject that.
		// (b) the claim survives — the invariant holds.
		//
		// We want (b): a panic during Spawner.Start must leave the
		// PID=0 claim slot in state so an outside observer sees
		// "fire was attempted" forensically.
		found := false
		for _, p := range snap {
			if p.Kind == "workspace-weekly-refresh" && p.PID == 0 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("panic during Start must leave PID=0 claim in state; got snap=%+v", snap)
		}
	}()

	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})
}

// TestMaintenance_Transient_RemovesPIDOnExit — successful spawn + Wait
// returns nil → TransientPID drained from state.
func TestMaintenance_Transient_RemovesPIDOnExit(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sched.SetSpawner(sp)

	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	// Wait until a PID > 0 is recorded.
	if !waitForCondition(200*time.Millisecond, func() bool {
		for _, p := range state.snapshot() {
			if p.Kind == "workspace-weekly-refresh" && p.PID > 0 {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("expected PID > 0 to be recorded after spawn; got %+v", state.snapshot())
	}

	// Capture the assigned PID and finish the wait.
	var pid int
	for _, p := range state.snapshot() {
		if p.Kind == "workspace-weekly-refresh" && p.PID > 0 {
			pid = p.PID
			break
		}
	}
	sp.finish(pid, nil)

	if !waitForCondition(200*time.Millisecond, func() bool {
		return len(state.snapshot()) == 0
	}) {
		t.Fatalf("TransientPID must be drained after Wait returns; got %+v", state.snapshot())
	}
}

// TestMaintenance_Transient_PerTimerMutexPreventsOverlap — first fire
// is still in-flight (Wait hasn't returned); second Tick for the same
// Kind must skip with a warn log AND must NOT add a second
// TransientPID.
func TestMaintenance_Transient_PerTimerMutexPreventsOverlap(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sched.SetSpawner(sp)

	// First Tick — fires and blocks on Wait.
	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	// Wait for the first fire to register its PID in state.
	if !waitForCondition(200*time.Millisecond, func() bool {
		for _, p := range state.snapshot() {
			if p.Kind == "workspace-weekly-refresh" && p.PID > 0 {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("first fire never recorded PID; got %+v", state.snapshot())
	}

	// Reset the last_fired entry so the second Tick would WANT to fire
	// if the per-timer mutex wasn't enforced — we want this test to
	// fail on missing mutex, not on cadence math.
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", "2020-01-01T00:00:00Z")

	// Second Tick — must be skipped.
	beforeSnap := state.snapshot()
	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	// Give the impl a brief window to perform any (incorrect) second
	// spawn. If it did, snap len grew.
	time.Sleep(40 * time.Millisecond)

	afterSnap := state.snapshot()
	if len(afterSnap) != len(beforeSnap) {
		t.Fatalf("overlap protection failed: snap grew from %d to %d entries (%+v)", len(beforeSnap), len(afterSnap), afterSnap)
	}

	// Cleanup: unblock the first Wait so the goroutine exits.
	var pid int
	for _, p := range beforeSnap {
		if p.PID > 0 {
			pid = p.PID
			break
		}
	}
	sp.finish(pid, nil)
}

// TestMaintenance_Transient_DifferentKindsRunConcurrently — two
// different Kinds fire on the same Tick; both record PIDs without
// serialization. Verified by holding BOTH Waits blocked simultaneously
// and observing two PIDs in state.
func TestMaintenance_Transient_DifferentKindsRunConcurrently(t *testing.T) {
	timers := []api.MaintenanceTimer{
		{Kind: "workspace-weekly-refresh"},
		{Kind: "server-weekly-refresh"},
	}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sched.SetSpawner(sp)

	sched.Tick(sundayDue(), timers)

	if !waitForCondition(200*time.Millisecond, func() bool {
		ws := false
		sv := false
		for _, p := range state.snapshot() {
			if p.PID > 0 && p.Kind == "workspace-weekly-refresh" {
				ws = true
			}
			if p.PID > 0 && p.Kind == "server-weekly-refresh" {
				sv = true
			}
		}
		return ws && sv
	}) {
		t.Fatalf("two different kinds must run concurrently; got snap=%+v", state.snapshot())
	}

	// Cleanup: finish both Waits.
	for _, p := range state.snapshot() {
		if p.PID > 0 {
			sp.finish(p.PID, nil)
		}
	}
}

// TestMaintenance_Transient_SpawnFailureRollsBackClaim — Spawner.Start
// returns an error; the PID=0 claim must be removed (rollback) and the
// per-timer slot released so the next Tick can retry.
func TestMaintenance_Transient_SpawnFailureRollsBackClaim(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sp.startError = errors.New("synthetic spawn failure")
	sched.SetSpawner(sp)

	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	if !waitForCondition(200*time.Millisecond, func() bool {
		return len(state.snapshot()) == 0
	}) {
		t.Fatalf("spawn error must roll back TransientPID claim; got %+v", state.snapshot())
	}

	// Slot is released — clear the start error and re-tick; verify the
	// scheduler is willing to fire again.
	sp.mu.Lock()
	sp.startError = nil
	sp.mu.Unlock()

	// Reset last_fired so the cadence guard doesn't block the retry.
	state.SetMaintenanceFiredAt("workspace-weekly-refresh", "2020-01-01T00:00:00Z")

	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	if !waitForCondition(200*time.Millisecond, func() bool {
		for _, p := range state.snapshot() {
			if p.PID > 0 && p.Kind == "workspace-weekly-refresh" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("retry after rollback must succeed; got %+v", state.snapshot())
	}

	// Cleanup.
	for _, p := range state.snapshot() {
		if p.PID > 0 {
			sp.finish(p.PID, nil)
		}
	}
}

// TestMaintenance_Transient_FireHookStillInvoked — SetFireHook callback
// still fires (post-spawn, for observability) so existing Task 9.1
// consumers and structured logging keep working.
func TestMaintenance_Transient_FireHookStillInvoked(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sched.SetSpawner(sp)

	var fired atomic.Int32
	sched.SetFireHook(func(api.MaintenanceTimer) { fired.Add(1) })

	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	if !waitForCondition(200*time.Millisecond, func() bool {
		return fired.Load() == 1
	}) {
		t.Fatalf("FireHook must fire once after spawn; got %d", fired.Load())
	}

	// Cleanup.
	for _, p := range state.snapshot() {
		if p.PID > 0 {
			sp.finish(p.PID, nil)
		}
	}
}

// TestMaintenance_Transient_StateShapeMatchesAPIStruct — recorded
// api.TransientPID after success has Kind populated, PID > 0, and
// StartedAt as RFC3339Nano UTC (Z-suffix).
func TestMaintenance_Transient_StateShapeMatchesAPIStruct(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	startedAt := time.Date(2026, 5, 17, 4, 0, 0, 0, time.UTC)
	sched.SetClock(fixedClock(startedAt))

	sp := newFakeSpawner()
	sched.SetSpawner(sp)

	// Mirror state writes into a real *api.SupervisorStateFile slice
	// so we prove the wire shape that QuiesceHandler.Drain consumes.
	supervisorState := &api.SupervisorStateFile{Version: 1, Daemons: map[string]api.SupervisorDaemonState{}}
	state.mirror = &supervisorState.TransientPIDs

	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	if !waitForCondition(200*time.Millisecond, func() bool {
		for _, p := range state.snapshot() {
			if p.PID > 0 {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("PID never recorded; got %+v", state.snapshot())
	}

	snap := state.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected one TransientPID entry; got %d (%+v)", len(snap), snap)
	}
	p := snap[0]
	if p.Kind != "workspace-weekly-refresh" {
		t.Fatalf("Kind = %q; want %q", p.Kind, "workspace-weekly-refresh")
	}
	if p.PID <= 0 {
		t.Fatalf("PID = %d; want > 0", p.PID)
	}
	if !strings.HasSuffix(p.StartedAt, "Z") {
		t.Fatalf("StartedAt must be UTC (Z-suffix); got %q", p.StartedAt)
	}
	if _, err := time.Parse(time.RFC3339Nano, p.StartedAt); err != nil {
		t.Fatalf("StartedAt must parse as RFC3339Nano; got %q err=%v", p.StartedAt, err)
	}
	if !strings.Contains(p.StartedAt, "2026") {
		t.Fatalf("StartedAt should reflect injected clock (2026); got %q", p.StartedAt)
	}

	// State-shape: the mirror SupervisorStateFile.TransientPIDs must
	// match exactly (the slice QuiesceHandler.Drain consumes).
	if len(supervisorState.TransientPIDs) != 1 {
		t.Fatalf("mirrored TransientPIDs len = %d; want 1", len(supervisorState.TransientPIDs))
	}
	if supervisorState.TransientPIDs[0].PID != p.PID {
		t.Fatalf("mirrored PID = %d; want %d", supervisorState.TransientPIDs[0].PID, p.PID)
	}

	// Cleanup.
	sp.finish(p.PID, nil)
}

// TestMaintenance_Transient_NilSpawnerFallsBackToFireHook — Task 9.1
// compat: when SetSpawner is never called, fireHook still fires and no
// TransientPID is recorded (no spawn pipeline engaged).
func TestMaintenance_Transient_NilSpawnerFallsBackToFireHook(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	var fired atomic.Int32
	sched.SetFireHook(func(api.MaintenanceTimer) { fired.Add(1) })

	// NO SetSpawner call.
	sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

	if fired.Load() != 1 {
		t.Fatalf("nil-spawner fallback must fire hook once; got %d", fired.Load())
	}
	if len(state.snapshot()) != 0 {
		t.Fatalf("nil-spawner must not record TransientPID; got %+v", state.snapshot())
	}
}

// TestMaintenance_Transient_GoroutineLeakBaseline — after N spawns
// complete and all Waits return, the goroutine count should return to
// baseline. This is a leak-hygiene smoke; absolute counts vary across
// runtimes, so the assertion compares before/after deltas.
func TestMaintenance_Transient_GoroutineLeakBaseline(t *testing.T) {
	timer := api.MaintenanceTimer{Kind: "workspace-weekly-refresh"}
	state := newTransientTestState()
	sched := NewMaintenanceScheduler(state)

	sp := newFakeSpawner()
	sched.SetSpawner(sp)

	// Settle any stray goroutines from prior tests.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Fire 5 sequential transients, completing each before the next.
	for i := 0; i < 5; i++ {
		state.SetMaintenanceFiredAt("workspace-weekly-refresh", "2020-01-01T00:00:00Z")
		sched.Tick(sundayDue(), []api.MaintenanceTimer{timer})

		if !waitForCondition(200*time.Millisecond, func() bool {
			for _, p := range state.snapshot() {
				if p.PID > 0 {
					return true
				}
			}
			return false
		}) {
			t.Fatalf("iter %d: PID never recorded; got %+v", i, state.snapshot())
		}
		var pid int
		for _, p := range state.snapshot() {
			if p.PID > 0 {
				pid = p.PID
				break
			}
		}
		sp.finish(pid, nil)

		if !waitForCondition(200*time.Millisecond, func() bool {
			return len(state.snapshot()) == 0
		}) {
			t.Fatalf("iter %d: TransientPID never drained; got %+v", i, state.snapshot())
		}
	}

	// Allow goroutine scheduler to settle.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	after := runtime.NumGoroutine()
	// Allow some slack: the runtime may keep a few extra goroutines
	// alive across iterations (GC sweep, test framework). The check
	// catches a per-spawn leak (would be at least 5 new goroutines).
	if after-baseline > 3 {
		t.Fatalf("goroutine leak suspected: baseline=%d after=%d delta=%d", baseline, after, after-baseline)
	}
}
