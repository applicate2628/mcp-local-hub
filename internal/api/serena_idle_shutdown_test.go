package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

// IntentReasonIdle is in the known-reason set so the unified schema validates
// an idle stop on read (validateIntentFields → isKnownIntentReason).
func TestIntentReasonIdle_IsKnownReason(t *testing.T) {
	if !isKnownIntentReason(IntentReasonIdle) {
		t.Fatalf("IntentReasonIdle (%q) must be a known intent reason", IntentReasonIdle)
	}
	// A full schema validation of an idle stop must pass.
	if err := validateIntentFields(DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonIdle, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("validateIntentFields rejected a valid idle stop: %v", err)
	}
}

// FALSIFICATION (spec §6): an idle stop is an ACTIVE stop that NEVER expires by
// the user-stop TTL. A user-stop of the same age DOES expire; an idle stop of
// the same age (and far past the TTL) stays active.
func TestIsActiveStop_Idle_NeverTTLExpires(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	// An idle stop updated 100 days ago — far past StopIntentTTL (24h) and
	// within the 365-day stale bound — must STILL be an active stop.
	idle := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonIdle,
		UpdatedAt: now.Add(-100 * 24 * time.Hour),
	}
	active, reason := idle.IsActiveStop(now)
	if !active {
		t.Fatalf("idle stop 100d old must remain ACTIVE (never TTL-expires); got inactive")
	}
	if reason != IntentReasonIdle {
		t.Fatalf("idle stop reason = %q, want %q", reason, IntentReasonIdle)
	}

	// Control: a user-stop of the SAME age expires (TTL only covers user-stop).
	userStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: now.Add(-100 * 24 * time.Hour),
	}
	if active, _ := userStop.IsActiveStop(now); active {
		t.Fatalf("user-stop 100d old must TTL-expire (control); got still active")
	}

	// An idle stop beyond the 365-day stale bound IS cleared (the stale-bound
	// branch applies to all reasons, as required).
	stale := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonIdle,
		UpdatedAt: now.Add(-400 * 24 * time.Hour),
	}
	if active, _ := stale.IsActiveStop(now); active {
		t.Fatalf("idle stop past the 365d stale bound must be inert; got still active")
	}

	// A future-dated idle stop fail-closes via the clock-skew branch (applies
	// to all reasons, with the synthetic clock-skew reason).
	future := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonIdle,
		UpdatedAt: now.Add(1 * time.Hour),
	}
	active, reason = future.IsActiveStop(now)
	if !active || reason != ClockSkewFutureSuspectReason {
		t.Fatalf("future-dated idle stop must fail closed as clock-skew-suspect; got active=%v reason=%q", active, reason)
	}
}

func TestSerenaIdleShutdownThreshold_Parse(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		enabled bool
	}{
		{"off", 0, false},
		{"15m", 15 * time.Minute, true},
		{"30m", 30 * time.Minute, true},
		{"1h", time.Hour, true},
		{"2h", 2 * time.Hour, true},
		{"", 0, false},
		{"garbage", 0, false},
	}
	for _, c := range cases {
		got, enabled := SerenaIdleShutdownThreshold(c.in)
		if got != c.want || enabled != c.enabled {
			t.Errorf("SerenaIdleShutdownThreshold(%q) = (%v,%v), want (%v,%v)", c.in, got, enabled, c.want, c.enabled)
		}
	}
}

// The registry default keeps idle-shutdown disabled. Stopping idle daemons releases
// their loopback ports before wake, so operators must explicitly opt in.
func TestSerenaIdleShutdown_RegistryDefaultIsOff(t *testing.T) {
	def := findDef(SerenaIdleShutdownSettingKey)
	if def == nil {
		t.Fatalf("setting %q missing from SettingsRegistry", SerenaIdleShutdownSettingKey)
	}
	if def.Default != "off" {
		t.Fatalf("default = %q, want off", def.Default)
	}
	d, enabled := SerenaIdleShutdownThreshold(def.Default)
	if enabled || d != 0 {
		t.Fatalf("default off must parse to disabled; got (%v,%v)", d, enabled)
	}
}

// WriteSerenaIdleStop lands Desired=stopped+IntentReasonIdle on the UNIFIED
// supervisor-intent stops sub-block (the §4/Phase-E path), NOT a second file.
func TestWriteSerenaIdleStop_LandsInUnifiedSubBlock(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-serena-alpha`
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("WriteSerenaIdleStop: %v", err)
	}
	sup := ReadSupervisorIntentForTest(t, stateDir)
	di, ok := sup.Stops[task]
	if !ok {
		t.Fatalf("idle stop not in unified stops sub-block; stops=%+v", sup.Stops)
	}
	if di.Desired != IntentDesiredStopped || di.Reason != IntentReasonIdle {
		t.Fatalf("sub-block idle stop mismatch: got %+v", di)
	}
	// And the unified reader resolves it as an active stop.
	active, reason := UnifiedStopsFile(sup, nil).Tasks[task].IsActiveStop(now)
	if !active || reason != IntentReasonIdle {
		t.Fatalf("UnifiedStopsFile idle stop must be active idle; got active=%v reason=%q", active, reason)
	}
}

// FIX-2a (operator stop must win over a stale idle write): seed a REAL
// user-disabled operator stop, then call WriteSerenaIdleStop (as the sweeper
// would from a stale snapshot). The on-disk reason must STAY user-disabled — the
// idle write is refused under the flock, so the next request never resurrects a
// deliberately-disabled daemon.
func TestWriteSerenaIdleStop_RefusesOverActiveOperatorStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-serena-disabled`

	// Operator disables the daemon.
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now,
	}, "operator"); err != nil {
		t.Fatalf("seed user-disabled stop: %v", err)
	}

	// The sweeper, from a stale status snapshot, tries to write an idle stop.
	if err := NewAPI().WriteSerenaIdleStop(task, now.Add(time.Minute)); err != nil {
		t.Fatalf("WriteSerenaIdleStop must not error when refusing over an operator stop; got %v", err)
	}

	// The operator stop must survive: reason still user-disabled, NOT idle.
	sup := ReadSupervisorIntentForTest(t, stateDir)
	di, ok := sup.Stops[task]
	if !ok {
		t.Fatalf("operator stop disappeared after idle write; stops=%+v", sup.Stops)
	}
	if di.Reason != IntentReasonUserDisabled {
		t.Fatalf("idle write OVERWROTE the operator stop: reason=%q, want user-disabled (operator stop must win)", di.Reason)
	}
	if !di.UpdatedAt.Equal(now) {
		t.Fatalf("operator stop UpdatedAt mutated by the refused idle write: got %v want %v", di.UpdatedAt, now)
	}
}

// FIX-2a control: an idle write OVER AN EXISTING IDLE stop still updates (it is
// not a non-idle operator stop), and an idle write where NO stop exists sets one.
func TestWriteSerenaIdleStop_SetsWhenNoOperatorStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	task := `\mcp-local-hub-serena-fresh`

	// No prior stop → idle SET.
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("WriteSerenaIdleStop (fresh): %v", err)
	}
	di, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[task]
	if !ok || di.Reason != IntentReasonIdle {
		t.Fatalf("fresh idle write must set an idle stop; got ok=%v stop=%+v", ok, di)
	}

	// An EXPIRED user-stop tombstone (inactive) must NOT block the idle write —
	// it is not an ACTIVE non-idle stop. Seed one that is past the user-stop TTL.
	task2 := `\mcp-local-hub-serena-expired-us`
	if err := NewAPI().WriteStopIntent(task2, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}, "operator"); err != nil {
		t.Fatalf("seed user-stop: %v", err)
	}
	// Write the idle stop far enough in the future that the user-stop has TTL-expired.
	future := now.Add(StopIntentTTL + time.Hour)
	if err := NewAPI().WriteSerenaIdleStop(task2, future); err != nil {
		t.Fatalf("WriteSerenaIdleStop over expired user-stop: %v", err)
	}
	di2, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[task2]
	if !ok || di2.Reason != IntentReasonIdle {
		t.Fatalf("idle write must replace an EXPIRED (inactive) user-stop; got ok=%v stop=%+v", ok, di2)
	}
}

// FIX-2b (compare-and-clear): seed an IDLE stop, then simulate a concurrent
// operator stop landing (overwriting the idle entry on disk) BEFORE the wake's
// clear fires. The wake's ClearStopIntentIfReason(IntentReasonIdle) must REFUSE
// to delete because the current entry is now user-disabled, so the operator stop
// survives the wake.
func TestWakeIdleSerenaDaemon_CompareAndClear_OperatorStopSurvives(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-toctou`

	// Step 1: an idle stop exists (the daemon was idle-stopped).
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	// Step 2: the wake reads the stop LOCK-FREE and classifies it as idle. We
	// emulate the TOCTOU by overriding the read seam to return the IDLE snapshot
	// the wake saw, while the ON-DISK entry is then replaced by an operator stop
	// (as a concurrent operator-stop write would do between read and clear).
	origRead := serenaWakeReadStopFn
	serenaWakeReadStopFn = func(string) (DaemonIntent, error) {
		// The stale snapshot the wake observed: an idle stop.
		return DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonIdle, UpdatedAt: now}, nil
	}
	t.Cleanup(func() { serenaWakeReadStopFn = origRead })

	// Concurrent operator stop lands on disk (overwrites the idle entry).
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now,
	}, "operator"); err != nil {
		t.Fatalf("concurrent operator stop write: %v", err)
	}

	reconcileCalled, readyCalled := false, false
	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		reconcileCalled = true
		return ReconcileResponse{}, nil
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error {
		readyCalled = true
		return nil
	}
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	err := NewAPI().WakeIdleSerenaDaemon(context.Background(), task, 0, "tester")
	if !errors.Is(err, ErrWakeRefusedOperatorStop) {
		t.Fatalf("wake that loses the idle-clear race to an operator stop must refuse; got %v", err)
	}
	if reconcileCalled || readyCalled {
		t.Fatalf("lost-clear wake must not nudge reconcile (%v) or probe readiness (%v)", reconcileCalled, readyCalled)
	}

	sup := ReadSupervisorIntentForTest(t, stateDir)
	di, ok := sup.Stops[task]
	if !ok {
		t.Fatalf("operator stop ERASED by the wake's clear (TOCTOU); stops=%+v", sup.Stops)
	}
	if di.Reason != IntentReasonUserDisabled {
		t.Fatalf("wake clear must not downgrade/erase the operator stop; reason=%q want user-disabled", di.Reason)
	}
}

// FIX-2b direct: ClearStopIntentIfReason deletes ONLY when the current entry's
// reason matches; a mismatched reason or absent entry is a no-op.
func TestClearStopIntentIfReason_CompareAndClear(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	a := NewAPI()

	// Matching reason → cleared.
	taskIdle := `\mcp-local-hub-serena-idle-clear`
	if err := a.WriteSerenaIdleStop(taskIdle, now); err != nil {
		t.Fatalf("seed idle: %v", err)
	}
	clearAllowed, err := a.ClearStopIntentIfReason(taskIdle, IntentReasonIdle, "tester")
	if err != nil {
		t.Fatalf("clear idle: %v", err)
	}
	if !clearAllowed {
		t.Fatalf("clear idle returned clearAllowed=false, want true for matching idle stop")
	}
	if _, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[taskIdle]; ok {
		t.Fatalf("idle stop must be cleared by a matching-reason compare-and-clear")
	}

	// Mismatched reason → refused (entry survives).
	taskOp := `\mcp-local-hub-serena-op-clear`
	if err := a.WriteStopIntent(taskOp, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now,
	}, "operator"); err != nil {
		t.Fatalf("seed user-stop: %v", err)
	}
	clearAllowed, err = a.ClearStopIntentIfReason(taskOp, IntentReasonIdle, "tester")
	if err != nil {
		t.Fatalf("compare-and-clear (mismatch) must be a no-op success; got %v", err)
	}
	if clearAllowed {
		t.Fatalf("compare-and-clear mismatch returned clearAllowed=true, want false so wake refuses operator races")
	}
	di, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[taskOp]
	if !ok || di.Reason != IntentReasonUserStop {
		t.Fatalf("a user-stop must survive a clear-if-idle; got ok=%v stop=%+v", ok, di)
	}

	// Absent entry → no-op success.
	clearAllowed, err = a.ClearStopIntentIfReason(`\mcp-local-hub-serena-absent`, IntentReasonIdle, "tester")
	if err != nil {
		t.Fatalf("compare-and-clear on an absent entry must be a no-op success; got %v", err)
	}
	if !clearAllowed {
		t.Fatalf("compare-and-clear on an absent entry returned clearAllowed=false, want true for idempotent concurrent wakes")
	}
}

// FIX-5 (hot-path cache correctness): readSerenaUnifiedStopForTask must reflect
// writes (the mtime/size-keyed cache busts on any change) AND serve repeated
// reads of an unchanged file from cache. The optimization must never serve a
// stale classification across a write.
func TestReadSerenaUnifiedStopForTask_CacheReflectsWrites(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := canonicalIntentTaskKey(`\mcp-local-hub-serena-cache`)

	// No file yet → zero DaemonIntent (not a stop), cache populated as empty.
	di, err := readSerenaUnifiedStopForTask(task)
	if err != nil {
		t.Fatalf("read (no file): %v", err)
	}
	if di.Desired != "" {
		t.Fatalf("no-stop read must return zero DaemonIntent; got %+v", di)
	}

	// Two consecutive reads with no write: the second is a cache HIT (valid flag
	// stays set, same stat key). Both return the same value.
	if !serenaStopReadCache.valid {
		t.Fatalf("first read must populate the cache (valid=true)")
	}
	di2, _ := readSerenaUnifiedStopForTask(task)
	if di2.Desired != "" {
		t.Fatalf("cache-hit read must still return no-stop; got %+v", di2)
	}

	// Write an idle stop → the cache must bust and the next read sees it.
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("WriteSerenaIdleStop: %v", err)
	}
	di3, err := readSerenaUnifiedStopForTask(task)
	if err != nil {
		t.Fatalf("read (after write): %v", err)
	}
	if di3.Desired != IntentDesiredStopped || di3.Reason != IntentReasonIdle {
		t.Fatalf("cache must reflect the written idle stop; got %+v (stale cache?)", di3)
	}

	// Clear it via the compare-and-clear → the next read must see it GONE.
	clearAllowed, err := NewAPI().ClearStopIntentIfReason(task, IntentReasonIdle, "tester")
	if err != nil {
		t.Fatalf("ClearStopIntentIfReason: %v", err)
	}
	if !clearAllowed {
		t.Fatalf("ClearStopIntentIfReason returned clearAllowed=false, want true for matching idle stop")
	}
	di4, err := readSerenaUnifiedStopForTask(task)
	if err != nil {
		t.Fatalf("read (after clear): %v", err)
	}
	if di4.Desired != "" {
		t.Fatalf("cache must reflect the clear; got %+v (stale stop served after clear?)", di4)
	}
}

// ReadSupervisorIntentForTest is a tiny test reader so the idle tests do not
// import a per-file unexported helper from a sibling test file.
func ReadSupervisorIntentForTest(t *testing.T, stateDir string) *SupervisorIntentFile {
	t.Helper()
	got, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-intent.json: %v", err)
	}
	return got
}

// ---------------------------------------------------------------------------
// WakeIdleSerenaDaemon — the router's next-request wake.
// ---------------------------------------------------------------------------

// withWakeSeams swaps the three wake seams and restores them on cleanup.
func withWakeSeams(t *testing.T, readStop func(string) (DaemonIntent, error), reconcile func(context.Context, bool) (ReconcileResponse, error), readiness func(context.Context, string, int, time.Duration) error) {
	t.Helper()
	origRead, origRec, origReady := serenaWakeReadStopFn, serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReadStopFn = readStop
	serenaWakeReconcileFn = reconcile
	serenaWakeReadinessFn = readiness
	t.Cleanup(func() {
		serenaWakeReadStopFn = origRead
		serenaWakeReconcileFn = origRec
		serenaWakeReadinessFn = origReady
	})
}

// No active stop → fast no-op success (the daemon is presumed up; the steady-
// state hot path). The clear/reconcile/readiness seams must NOT be invoked.
func TestWakeIdleSerenaDaemon_NoStop_NoOp(t *testing.T) {
	reconcileCalled, readyCalled := false, false
	withWakeSeams(t,
		func(string) (DaemonIntent, error) { return DaemonIntent{}, nil }, // no stop
		func(context.Context, bool) (ReconcileResponse, error) {
			reconcileCalled = true
			return ReconcileResponse{}, nil
		},
		func(context.Context, string, int, time.Duration) error { readyCalled = true; return nil },
	)
	if err := NewAPI().WakeIdleSerenaDaemon(context.Background(), `\mcp-local-hub-serena-up`, 9201, "tester"); err != nil {
		t.Fatalf("wake of an up daemon must be a no-op success; got %v", err)
	}
	if reconcileCalled || readyCalled {
		t.Fatalf("no-stop wake must not nudge reconcile (%v) or probe readiness (%v)", reconcileCalled, readyCalled)
	}
}

// FALSIFICATION (spec §6): a user-disabled daemon is NEVER woken by an idle
// wake. The wake REFUSES (ErrWakeRefusedOperatorStop) and does NOT clear the
// stop or spawn.
func TestWakeIdleSerenaDaemon_UserDisabled_Refused(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-disabled`
	// Seed a REAL user-disabled stop on the unified sub-block so we exercise the
	// production read path (no readStop seam override).
	if err := NewAPI().WriteStopIntent(task, DaemonIntent{
		Desired: IntentDesiredStopped, Reason: IntentReasonUserDisabled, UpdatedAt: now,
	}, "operator"); err != nil {
		t.Fatalf("seed user-disabled stop: %v", err)
	}

	reconcileCalled, readyCalled := false, false
	// Only override reconcile/readiness so we can assert they are NOT called;
	// leave the stop reader as the real on-disk read.
	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		reconcileCalled = true
		return ReconcileResponse{}, nil
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error { readyCalled = true; return nil }
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	err := NewAPI().WakeIdleSerenaDaemon(context.Background(), task, 9202, "tester")
	if !errors.Is(err, ErrWakeRefusedOperatorStop) {
		t.Fatalf("user-disabled wake must return ErrWakeRefusedOperatorStop; got %v", err)
	}
	if reconcileCalled || readyCalled {
		t.Fatalf("user-disabled wake must NOT nudge reconcile (%v) or probe readiness (%v)", reconcileCalled, readyCalled)
	}
	// The user-disabled stop must still be on disk (the wake did not clear it).
	sup := ReadSupervisorIntentForTest(t, stateDir)
	di, ok := sup.Stops[task]
	if !ok || di.Reason != IntentReasonUserDisabled {
		t.Fatalf("user-disabled stop must survive the refused wake; got ok=%v stop=%+v", ok, di)
	}
}

// A user-STOP (not disabled) is likewise refused — only IntentReasonIdle is
// wakeable.
func TestWakeIdleSerenaDaemon_UserStop_Refused(t *testing.T) {
	now := time.Now().UTC()
	withWakeSeams(t,
		func(string) (DaemonIntent, error) {
			return DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now}, nil
		},
		func(context.Context, bool) (ReconcileResponse, error) {
			t.Fatal("reconcile must not be called for user-stop")
			return ReconcileResponse{}, nil
		},
		func(context.Context, string, int, time.Duration) error {
			t.Fatal("readiness must not be called for user-stop")
			return nil
		},
	)
	if err := NewAPI().WakeIdleSerenaDaemon(context.Background(), `\mcp-local-hub-serena-us`, 9203, "tester"); !errors.Is(err, ErrWakeRefusedOperatorStop) {
		t.Fatalf("user-stop wake must be refused; got %v", err)
	}
}

// An IDLE stop IS woken: the wake clears the stop (ClearStopIntent on disk),
// nudges reconcile, probes readiness, and returns nil.
func TestWakeIdleSerenaDaemon_Idle_ClearsNudgesAndReady(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-idle`
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	if _, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[task]; !ok {
		t.Fatalf("precondition: idle stop should be set")
	}

	reconcileApply, readyCalled := false, false
	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(_ context.Context, apply bool) (ReconcileResponse, error) {
		reconcileApply = apply
		return ReconcileResponse{}, nil
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error { readyCalled = true; return nil }
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	if err := NewAPI().WakeIdleSerenaDaemon(context.Background(), task, 9204, "tester"); err != nil {
		t.Fatalf("idle wake must succeed; got %v", err)
	}
	if !reconcileApply {
		t.Fatalf("idle wake must nudge reconcile with apply=true")
	}
	if !readyCalled {
		t.Fatalf("idle wake must probe readiness")
	}
	// The idle stop must be CLEARED from the sub-block.
	sup := ReadSupervisorIntentForTest(t, stateDir)
	if _, ok := sup.Stops[task]; ok {
		t.Fatalf("idle wake must clear the idle stop; still present: %+v", sup.Stops)
	}
}

// A readiness timeout on an idle wake returns an error (→ router 503, client
// retries) but the stop was still cleared (the daemon IS coming up).
func TestWakeIdleSerenaDaemon_Idle_ReadinessTimeout_StopStillCleared(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-slow`
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error { return errors.New("not ready in time") }
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	err := NewAPI().WakeIdleSerenaDaemon(context.Background(), task, 9205, "tester")
	if err == nil {
		t.Fatalf("a readiness failure must surface an error so the router 503s")
	}
	if _, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[task]; ok {
		t.Fatalf("idle stop must already be cleared even when readiness times out (the daemon IS coming up)")
	}
}

func TestWakeIdleSerenaDaemon_IPCUnavailableRestoresIdleStopAndNextWakeRetries(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-ipc-down`
	if err := NewAPI().WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	var reconcileCalls int32
	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error {
		t.Fatal("readiness must not run when the supervisor IPC nudge is unavailable")
		return nil
	}
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	a := NewAPI()
	err := a.WakeIdleSerenaDaemon(context.Background(), task, 9301, "tester")
	if !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Fatalf("wake error = %v, want ErrSupervisorIPCUnavailable", err)
	}
	if got := ReadSupervisorIntentForTest(t, stateDir).Stops[task]; got.Reason != IntentReasonIdle {
		t.Fatalf("idle stop was not restored after IPC-unavailable nudge; got %+v", got)
	}

	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		return ReconcileResponse{}, nil
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error { return nil }
	if err := a.WakeIdleSerenaDaemon(context.Background(), task, 9301, "tester"); err != nil {
		t.Fatalf("second wake should retry the restored idle stop and succeed; got %v", err)
	}
	if atomic.LoadInt32(&reconcileCalls) != 2 {
		t.Fatalf("reconcile calls = %d, want 2 (second wake retried instead of fast no-op)", reconcileCalls)
	}
	if _, ok := ReadSupervisorIntentForTest(t, stateDir).Stops[task]; ok {
		t.Fatalf("idle stop should be cleared after the successful retry")
	}
}

func TestWakeIdleSerenaDaemon_IPCUnavailableRestoreDoesNotClobberOperatorStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	var captured []IntentAuditEntry
	installTestAuditFn(t, &captured, nil)

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-operator-race`
	a := NewAPI()
	if err := a.WriteSerenaIdleStop(task, now); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	captured = nil

	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		if err := a.WriteStopIntent(task, DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now.Add(time.Minute),
		}, "operator"); err != nil {
			t.Fatalf("operator stop during wake: %v", err)
		}
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error {
		t.Fatal("readiness must not run when the supervisor IPC nudge is unavailable")
		return nil
	}
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	err := a.WakeIdleSerenaDaemon(context.Background(), task, 9302, "tester")
	if !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Fatalf("wake error = %v, want ErrSupervisorIPCUnavailable", err)
	}
	got := ReadSupervisorIntentForTest(t, stateDir).Stops[task]
	if got.Reason != IntentReasonUserStop {
		t.Fatalf("operator stop was clobbered by idle restore; got %+v", got)
	}
	foundRefusal := false
	for _, entry := range captured {
		if entry.Action == "idle-stop-refused-operator-stop-active" && entry.Task == task {
			foundRefusal = true
			break
		}
	}
	if !foundRefusal {
		t.Fatalf("restore did not use the idle-guarded writer; audit entries=%+v", captured)
	}
}

func TestWakeIdleSerenaDaemon_FollowerIPCUnavailableDoesNotRestoreStaleIdleStop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	now := time.Now().UTC()
	task := `\mcp-local-hub-serena-follower-race`
	taskKey := canonicalIntentTaskKey(task)
	a := NewAPI()
	a.serenaWakeInFlightMu.Lock()
	a.serenaWakeInFlight = map[string]bool{taskKey: true}
	a.serenaWakeInFlightMu.Unlock()

	withWakeSeams(t,
		func(gotTask string) (DaemonIntent, error) {
			if gotTask != taskKey {
				t.Fatalf("readStop task = %q, want %q", gotTask, taskKey)
			}
			return DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonIdle, UpdatedAt: now}, nil
		},
		func(context.Context, bool) (ReconcileResponse, error) {
			return ReconcileResponse{}, ErrSupervisorIPCUnavailable
		},
		func(context.Context, string, int, time.Duration) error {
			t.Fatal("readiness must not run when the supervisor IPC nudge is unavailable")
			return nil
		},
	)

	err := a.WakeIdleSerenaDaemon(context.Background(), task, 9303, "tester")
	if !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Fatalf("wake error = %v, want ErrSupervisorIPCUnavailable", err)
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	got, readErr := ReadSupervisorIntent(intentPath)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return
		}
		t.Fatalf("ReadSupervisorIntent(%s): %v", intentPath, readErr)
	}
	if _, ok := got.Stops[taskKey]; ok {
		t.Fatalf("follower restored stale idle stop despite not owning the clear: %+v", got.Stops)
	}
}

func TestWakeIdleSerenaDaemon_NoStopWaitsForInFlightWake(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-serena-inflight`
	if err := NewAPI().WriteSerenaIdleStop(task, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}
	firstReadinessStarted := make(chan struct{})
	secondReadinessStarted := make(chan struct{})
	releaseReadiness := make(chan struct{})
	var readinessCalls int32
	serenaWakeReadinessFn = func(ctx context.Context, taskName string, port int, timeout time.Duration) error {
		switch atomic.AddInt32(&readinessCalls, 1) {
		case 1:
			close(firstReadinessStarted)
		case 2:
			close(secondReadinessStarted)
		}
		select {
		case <-releaseReadiness:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	a := NewAPI()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- a.WakeIdleSerenaDaemon(ctx, task, 9400, "tester")
	}()

	select {
	case <-firstReadinessStarted:
	case err := <-firstDone:
		t.Fatalf("first wake returned before entering readiness: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first wake did not reach readiness")
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- a.WakeIdleSerenaDaemon(ctx, task, 9400, "tester")
	}()

	select {
	case err := <-secondDone:
		close(releaseReadiness)
		<-firstDone
		t.Fatalf("second wake returned before the in-flight readiness probe completed: %v", err)
	case <-secondReadinessStarted:
	case <-time.After(time.Second):
		close(releaseReadiness)
		<-firstDone
		t.Fatal("second wake did not wait on the in-flight readiness probe")
	}

	select {
	case err := <-secondDone:
		close(releaseReadiness)
		<-firstDone
		t.Fatalf("second wake returned before readiness was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseReadiness)
	if err := <-firstDone; err != nil {
		t.Fatalf("first wake after readiness release: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second wake after readiness release: %v", err)
	}
	if got := atomic.LoadInt32(&readinessCalls); got != 2 {
		t.Fatalf("readiness calls = %d, want 2 (original wake + in-flight fast-path wait)", got)
	}
}

func TestWakeIdleSerenaDaemon_RefusesForeignPortOwnerDespiteSerenaResponse(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("OS-level loopback port-owner proof is enforced on Windows and Linux")
	}
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-serena-owner-mismatch`
	if err := NewAPI().WriteSerenaIdleStop(task, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	_, port := newReadinessHTTPTestServer(t, perfectSerenaInitializeHandler)

	const supervisorPID = 4242
	const attackerPID = 7331
	origRec, origStatus, origOwner := serenaWakeReconcileFn, supervisorIPCStatusFn, loopbackPortOwnerFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: task, State: "Running", PID: supervisorPID, Port: port}}, nil
	}
	loopbackPortOwnerFn = func(gotPort int) (int, bool, error) {
		if gotPort != port {
			t.Errorf("owner lookup got port %d, want %d", gotPort, port)
		}
		return attackerPID, true, nil
	}
	t.Cleanup(func() {
		serenaWakeReconcileFn = origRec
		supervisorIPCStatusFn = origStatus
		loopbackPortOwnerFn = origOwner
	})

	err := NewAPI().WakeIdleSerenaDaemon(context.Background(), task, port, "tester")
	if err == nil {
		t.Fatal("foreign process serving a perfect serena initialize response must be refused")
	}
	msg := err.Error()
	if !strings.Contains(msg, "owned by PID 7331") || !strings.Contains(msg, "supervisor-reported daemon PID 4242") {
		t.Fatalf("expected explicit owner-PID mismatch diagnostic, got: %v", err)
	}
}

func TestWakeIdleSerenaDaemon_AcceptsMatchingPortOwnerAndSerenaResponse(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("OS-level loopback port-owner proof is enforced on Windows and Linux")
	}
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-serena-owner-match`
	if err := NewAPI().WriteSerenaIdleStop(task, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	_, port := newReadinessHTTPTestServer(t, perfectSerenaInitializeHandler)

	const supervisorPID = 4242
	origRec, origStatus, origOwner := serenaWakeReconcileFn, supervisorIPCStatusFn, loopbackPortOwnerFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: task, State: "Running", PID: supervisorPID, Port: port}}, nil
	}
	loopbackPortOwnerFn = func(gotPort int) (int, bool, error) {
		if gotPort != port {
			t.Errorf("owner lookup got port %d, want %d", gotPort, port)
		}
		return supervisorPID, true, nil
	}
	t.Cleanup(func() {
		serenaWakeReconcileFn = origRec
		supervisorIPCStatusFn = origStatus
		loopbackPortOwnerFn = origOwner
	})

	if err := NewAPI().WakeIdleSerenaDaemon(context.Background(), task, port, "tester"); err != nil {
		t.Fatalf("matching supervisor PID + port owner + serena response must pass: %v", err)
	}
}

func TestWakeIdleSerenaDaemon_StatusPIDNotLiveFailsAfterPolling(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("OS-level loopback port-owner proof is enforced on Windows and Linux")
	}
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-serena-no-live-pid`
	if err := NewAPI().WriteSerenaIdleStop(task, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}
	_, port := newReadinessHTTPTestServer(t, perfectSerenaInitializeHandler)

	statusCalls := 0
	origRec, origStatus, origOwner := serenaWakeReconcileFn, supervisorIPCStatusFn, loopbackPortOwnerFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		statusCalls++
		if statusCalls == 1 {
			return []DaemonStatus{{TaskName: task, State: "Restarting", PID: 0, Port: port}}, nil
		}
		return []DaemonStatus{}, nil
	}
	loopbackPortOwnerFn = func(int) (int, bool, error) {
		t.Fatal("owner lookup must wait until supervisor status reports a live PID")
		return 0, false, nil
	}
	t.Cleanup(func() {
		serenaWakeReconcileFn = origRec
		supervisorIPCStatusFn = origStatus
		loopbackPortOwnerFn = origOwner
	})

	ctx, cancel := context.WithTimeout(context.Background(), 140*time.Millisecond)
	defer cancel()
	err := NewAPI().WakeIdleSerenaDaemon(ctx, task, port, "tester")
	if err == nil {
		t.Fatal("wake readiness must fail when supervisor status never reports a live PID")
	}
	if statusCalls < 2 {
		t.Fatalf("status should be polled until the wake deadline; calls=%d", statusCalls)
	}
	if !strings.Contains(err.Error(), "supervisor status") || !strings.Contains(err.Error(), "live PID") {
		t.Fatalf("expected honest no-live-PID diagnostic, got: %v", err)
	}
}

func perfectSerenaInitializeHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","serverInfo":{"name":"Serena","version":"1"},"capabilities":{}}}`))
}
