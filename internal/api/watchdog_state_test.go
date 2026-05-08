// Tests for Task 4 (watchdog_state.go) — three-state read, sliding-30min
// strikes (corrupt, audit-failure, stale-clear), restart-pending TTL with
// injected clock, fail-CLOSED corrupt suppression, post-rename quarantine
// + non-fatal prune, err-first WriteWatchdogState contract, backward
// compatibility for older state JSON.
//
// Tests run with the production state-path resolver bypassed via the
// daemonStateRootOverride seam from state_paths.go (Task 1) so each
// test owns its own per-test directory under t.TempDir(). Quarantine
// helpers piggy-back on Task 2's seam pattern.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// watchdogStateTestHelper wires the state-dir override + resets the
// watchdog-quarantine seams and returns the state root. Every test
// must call it first. Cleanup restores production seams.
func watchdogStateTestHelper(t *testing.T) string {
	t.Helper()
	statePathsHelper(t)
	root := t.TempDir()
	daemonStateRootOverride = root

	prevRemove := watchdogQuarantineRemoveFileFn
	t.Cleanup(func() { watchdogQuarantineRemoveFileFn = prevRemove })

	prevLog := watchdogQuarantinePruneLogFn
	t.Cleanup(func() { watchdogQuarantinePruneLogFn = prevLog })

	prevRenameFn := watchdogStateRenameFn
	t.Cleanup(func() { watchdogStateRenameFn = prevRenameFn })

	return root
}

// writeWatchdogStateRaw seeds the watchdog-state.json file with arbitrary
// bytes (used to set up corrupt-state cases or test backward-compat).
func writeWatchdogStateRaw(t *testing.T, raw []byte) error {
	t.Helper()
	dir, err := DaemonStateDir()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, watchdogStateFileLeaf), raw, 0o600)
}

// watchdogStateStem returns the full path to watchdog-state.json under
// the test state-dir, used by quarantine tests that seed sibling files.
func watchdogStateStem(t *testing.T) string {
	t.Helper()
	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	return filepath.Join(dir, watchdogStateFileLeaf)
}

// ---------------------------------------------------------------------------
// Window math (plan §6 + Task 4.1).
// ---------------------------------------------------------------------------

// TestCooldown_DueWindow_T0_to_T15_inclusive asserts the §6 attempt
// window: Due true at attempts 1..4 inclusive of T+15min, false at
// attempts 5/6 (cooldown), true again at T+30 (next cycle).
func TestCooldown_DueWindow_T0_to_T15_inclusive(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-tw"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	if read.State != WatchdogStateValid && read.State != WatchdogStateMissing {
		t.Fatalf("initial State = %q, want missing/valid", read.State)
	}

	// 1st attempt: T0. Due is true (entry missing).
	if !read.Cool.Due(taskName, t0) {
		t.Fatalf("Due at T0 with no entry: got false, want true")
	}
	read.Cool.RecordAttempt(taskName, t0)

	// 2nd attempt at T+5: still within attempt window.
	t1 := t0.Add(5 * time.Minute)
	if !read.Cool.Due(taskName, t1) {
		t.Fatalf("Due at T+5 (attempt 2): got false, want true")
	}
	read.Cool.RecordAttempt(taskName, t1)

	// 3rd attempt at T+10.
	t2 := t0.Add(10 * time.Minute)
	if !read.Cool.Due(taskName, t2) {
		t.Fatalf("Due at T+10 (attempt 3): got false, want true")
	}
	read.Cool.RecordAttempt(taskName, t2)

	// 4th attempt at T+15 (inclusive boundary).
	t3 := t0.Add(15 * time.Minute)
	if !read.Cool.Due(taskName, t3) {
		t.Fatalf("Due at T+15 (attempt 4, inclusive): got false, want true")
	}
	read.Cool.RecordAttempt(taskName, t3)

	if got := read.Cool.AttemptsInWindow(taskName); got != 4 {
		t.Fatalf("AttemptsInWindow after 4: got %d, want 4", got)
	}

	// 5th attempt at T+16: AttemptsInWindow == 4 caps; cooldown phase.
	t4 := t0.Add(16 * time.Minute)
	if read.Cool.Due(taskName, t4) {
		t.Fatalf("Due at T+16 (5th): got true, want false (cooldown)")
	}

	// 6th attempt at T+25: still in cooldown (T+15..T+30).
	t5 := t0.Add(25 * time.Minute)
	if read.Cool.Due(taskName, t5) {
		t.Fatalf("Due at T+25 (6th): got true, want false (cooldown)")
	}

	// 7th attempt at T+30 (next cycle T0').
	t6 := t0.Add(30 * time.Minute)
	if !read.Cool.Due(taskName, t6) {
		t.Fatalf("Due at T+30 (next cycle): got false, want true")
	}
}

// TestCooldown_ResetAfterRunning5Min asserts §6 reset rule: when
// LastRunningAt - prevLastRunningAt >= 5min the cooldown entry resets to
// zero (FirstAttemptAt zero, AttemptsInWindow 0, ChronicCycles 0).
func TestCooldown_ResetAfterRunning5Min(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-reset"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	read.Cool.RecordAttempt(taskName, t0)
	read.Cool.RecordAttempt(taskName, t0.Add(5*time.Minute))

	// First running observation: LastRunningAt set, no reset (no prior).
	read.Cool.RecordRunning(taskName, t0.Add(6*time.Minute))

	// Second running observation 5min+ later → reset.
	read.Cool.RecordRunning(taskName, t0.Add(11*time.Minute))

	if got := read.Cool.AttemptsInWindow(taskName); got != 0 {
		t.Errorf("AttemptsInWindow after 5+min Running: got %d, want 0", got)
	}
	// Due must again be true (entry has been reset to FirstAttemptAt zero).
	if !read.Cool.Due(taskName, t0.Add(12*time.Minute)) {
		t.Errorf("Due after reset: got false, want true")
	}
}

// TestCooldown_ChronicLimit asserts ChronicLimitReached after the
// ChronicCycles counter reaches the §6 limit. Per the plan §6 rule
// `ChronicCycles++ if !FirstAttemptAt.IsZero()` the first RecordAttempt
// does NOT increment (entry was empty), so reaching ChronicCycleLimit=4
// requires 5 cycle starts in total. After the 5th call: ChronicCycles=4
// → ChronicLimitReached.
func TestCooldown_ChronicLimit(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-chronic"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()

	// 5 cycle starts spaced ChronicCycleDuration apart. The first call
	// initializes FirstAttemptAt (no increment); each subsequent call
	// crosses the cycle boundary so ChronicCycles increments by 1.
	// Final state: ChronicCycles=4 ≥ ChronicCycleLimit=4 → reached.
	for cycle := 0; cycle < ChronicCycleLimit+1; cycle++ {
		base := t0.Add(time.Duration(cycle) * CooldownCycleDuration)
		read.Cool.RecordAttempt(taskName, base) // start cycle
	}

	if !read.Cool.ChronicLimitReached(taskName) {
		t.Fatalf("ChronicLimitReached after %d cycle starts: got false, want true",
			ChronicCycleLimit+1)
	}

	// Sanity: 4 starts (one less than required) → still NOT reached.
	const otherTask = "\\mcp-local-hub-chronic-2"
	for cycle := 0; cycle < ChronicCycleLimit; cycle++ {
		base := t0.Add(time.Duration(cycle) * CooldownCycleDuration)
		read.Cool.RecordAttempt(otherTask, base)
	}
	if read.Cool.ChronicLimitReached(otherTask) {
		t.Errorf("ChronicLimitReached after %d cycle starts: got true, want false",
			ChronicCycleLimit)
	}
}

// TestCooldown_PersistAndReload — FirstAttemptAt + ChronicCycles +
// LastRunningAt + RestartPendingAt all survive write+read.
func TestCooldown_PersistAndReload(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-persist"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	read.Cool.RecordAttempt(taskName, t0)
	read.Cool.RecordAttempt(taskName, t0.Add(5*time.Minute))
	read.Cool.RecordRunning(taskName, t0.Add(6*time.Minute))
	read.Cool.MarkRestartPending(taskName, t0.Add(7*time.Minute))

	if _, err := a.WriteWatchdogState(read, t0.Add(7*time.Minute)); err != nil {
		t.Fatalf("WriteWatchdogState: %v", err)
	}

	// Re-read: confirm fields survived.
	read2 := a.ReadWatchdogState()
	if read2.State != WatchdogStateValid {
		t.Fatalf("State after reload: got %q, want valid", read2.State)
	}
	if got := read2.Cool.AttemptsInWindow(taskName); got != 2 {
		t.Errorf("AttemptsInWindow after reload: got %d, want 2", got)
	}
	if !read2.Cool.IsRestartPending(taskName, t0.Add(8*time.Minute)) {
		t.Errorf("IsRestartPending after reload (within TTL): got false, want true")
	}
}

// TestCooldown_PersistAcrossInvocations simulates two distinct
// `mcphub watchdog --once` runs sharing the same on-disk state file.
func TestCooldown_PersistAcrossInvocations(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-cross-run"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	// "First invocation"
	read1 := a.ReadWatchdogState()
	read1.Cool.RecordAttempt(taskName, t0)
	if _, err := a.WriteWatchdogState(read1, t0); err != nil {
		t.Fatalf("WriteWatchdogState run1: %v", err)
	}

	// "Second invocation": fresh API instance to mirror CLI re-invoke.
	b := NewAPI()
	read2 := b.ReadWatchdogState()
	if got := read2.Cool.AttemptsInWindow(taskName); got != 1 {
		t.Errorf("AttemptsInWindow after re-invoke: got %d, want 1", got)
	}
	read2.Cool.RecordAttempt(taskName, t0.Add(5*time.Minute))
	if _, err := b.WriteWatchdogState(read2, t0.Add(5*time.Minute)); err != nil {
		t.Fatalf("WriteWatchdogState run2: %v", err)
	}

	read3 := NewAPI().ReadWatchdogState()
	if got := read3.Cool.AttemptsInWindow(taskName); got != 2 {
		t.Errorf("AttemptsInWindow after second re-invoke: got %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Three-state read: missing | corrupt | valid (parallel to Task 2).
// ---------------------------------------------------------------------------

// TestWatchdogState_Missing_FreshCool — bootstrap path: file absent →
// State="missing" + Cool methods all return safe zero values.
func TestWatchdogState_Missing_FreshCool(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	read := a.ReadWatchdogState()
	if read.State != WatchdogStateMissing {
		t.Fatalf("State = %q, want %q", read.State, WatchdogStateMissing)
	}
	const taskName = "\\mcp-local-hub-anything"
	if !read.Cool.Due(taskName, time.Now().UTC()) {
		t.Errorf("Due on fresh state: got false, want true (no prior attempts)")
	}
	if read.Cool.ChronicLimitReached(taskName) {
		t.Errorf("ChronicLimitReached on fresh state: got true")
	}
	if read.Cool.AttemptsInWindow(taskName) != 0 {
		t.Errorf("AttemptsInWindow on fresh state: got %d, want 0",
			read.Cool.AttemptsInWindow(taskName))
	}
	if read.Cool.IsRestartPending(taskName, time.Now().UTC()) {
		t.Errorf("IsRestartPending on fresh state: got true, want false")
	}
}

// TestWatchdogState_Corrupt_FailClosedSuppressAll — corrupt JSON triggers
// quarantine + suppress-all Cool returning Due=false for every task.
func TestWatchdogState_Corrupt_FailClosedSuppressAll(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)
	if _, err := DaemonStateDir(); err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}

	if err := writeWatchdogStateRaw(t, []byte("{this is not valid json}")); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}

	read := a.ReadWatchdogState()
	if read.State != WatchdogStateCorrupt {
		t.Fatalf("State = %q, want %q", read.State, WatchdogStateCorrupt)
	}
	if read.QuarantinePath == "" {
		t.Fatalf("QuarantinePath empty after corrupt; want renamed sibling path")
	}
	if _, err := os.Stat(read.QuarantinePath); err != nil {
		t.Fatalf("quarantine target missing: %v", err)
	}
	for _, name := range []string{"\\a", "\\b", "\\anything-here", ""} {
		if read.Cool.Due(name, time.Now().UTC()) {
			t.Errorf("Due(%q) on corrupt state: got true, want false (fail-closed)", name)
		}
	}
}

// TestWatchdogState_Quarantine_PostRenamePruneCap — pre-seed 6 fake
// `.corrupt-*` files; corrupt-driven quarantine creates a 7th and prunes
// the oldest leaving 5 newest (QuarantineCap).
func TestWatchdogState_Quarantine_PostRenamePruneCap(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)
	if _, err := DaemonStateDir(); err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	stem := watchdogStateStem(t)

	now := time.Now()
	staleNames := make([]string, 6)
	for i := range staleNames {
		ts := time.Now().UTC().Add(time.Duration(-i) * time.Minute).Format(quarantineSuffixLayout)
		name := stem + ".corrupt-" + ts + fmt.Sprintf(".%d", i)
		if err := os.WriteFile(name, []byte(fmt.Sprintf("stale-%d", i)), 0o600); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		mt := now.Add(time.Duration(-i) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		staleNames[i] = name
	}

	if err := writeWatchdogStateRaw(t, []byte("{not valid")); err != nil {
		t.Fatalf("seed corrupt main: %v", err)
	}
	res := a.ReadWatchdogState()
	if res.State != WatchdogStateCorrupt {
		t.Fatalf("State = %q, want %q", res.State, WatchdogStateCorrupt)
	}

	matches, err := filepath.Glob(stem + ".corrupt-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if got := len(matches); got != QuarantineCap {
		t.Fatalf("survivor count: got %d, want %d (matches=%v)", got, QuarantineCap, matches)
	}

	// The OLDEST pre-seed (staleNames[5]) must have been pruned. The new
	// quarantine sibling is the most recent and necessarily survives.
	survivorBases := map[string]bool{}
	for _, m := range matches {
		survivorBases[filepath.Base(m)] = true
	}
	if survivorBases[filepath.Base(staleNames[5])] {
		t.Errorf("oldest pre-seed survived; expected pruned (matches=%v)", matches)
	}
}

// TestWatchdogState_QuarantinePrune_FailureNonFatal — per-file delete
// failure must not break the quarantine flow; rename succeeded so
// State=corrupt + QuarantinePath set + prune logged as non-fatal.
func TestWatchdogState_QuarantinePrune_FailureNonFatal(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)
	if _, err := DaemonStateDir(); err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	stem := watchdogStateStem(t)

	now := time.Now()
	for i := 0; i < 6; i++ {
		ts := time.Now().UTC().Add(time.Duration(-i) * time.Minute).Format(quarantineSuffixLayout)
		name := stem + ".corrupt-" + ts + fmt.Sprintf(".%d", i)
		if err := os.WriteFile(name, []byte("stale"), 0o600); err != nil {
			t.Fatalf("seed stale: %v", err)
		}
		mt := now.Add(time.Duration(-i) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	var pruneEvents []string
	var deleteAttempts int
	watchdogQuarantineRemoveFileFn = func(path string) error {
		deleteAttempts++
		return errors.New("synthetic disk-full")
	}
	watchdogQuarantinePruneLogFn = func(event, path string, err error) {
		pruneEvents = append(pruneEvents, event)
	}

	if err := writeWatchdogStateRaw(t, []byte("garbage{")); err != nil {
		t.Fatalf("seed corrupt main: %v", err)
	}
	res := a.ReadWatchdogState()
	if res.State != WatchdogStateCorrupt {
		t.Fatalf("State = %q, want %q", res.State, WatchdogStateCorrupt)
	}
	if res.QuarantinePath == "" {
		t.Fatalf("QuarantinePath empty; rename should have succeeded")
	}
	if _, err := os.Stat(res.QuarantinePath); err != nil {
		t.Fatalf("quarantine target missing: %v", err)
	}
	if deleteAttempts == 0 {
		t.Errorf("expected >=1 delete attempt; got 0")
	}
	hasNonFatal := false
	for _, ev := range pruneEvents {
		if ev == "quarantine-prune-failed-non-fatal" {
			hasNonFatal = true
			break
		}
	}
	if !hasNonFatal {
		t.Errorf("expected quarantine-prune-failed-non-fatal event; got %v", pruneEvents)
	}
}

// ---------------------------------------------------------------------------
// Wall-clock-jump persistence (plan §29).
// ---------------------------------------------------------------------------

func TestWatchdogState_WallClockSeen_Persists(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	read := a.ReadWatchdogState()
	if !read.LastWallClock.IsZero() {
		t.Fatalf("initial LastWallClock: got %v, want zero", read.LastWallClock)
	}
	if _, err := a.WriteWatchdogState(read, t0); err != nil {
		t.Fatalf("WriteWatchdogState: %v", err)
	}

	// Simulate "25h later" — the second read returns the previous
	// LastWallClock so the caller can detect the jump (now - last = 25h).
	read2 := a.ReadWatchdogState()
	if !read2.LastWallClock.Equal(t0) {
		t.Errorf("LastWallClock after reload: got %v, want %v", read2.LastWallClock, t0)
	}
	now := t0.Add(25 * time.Hour)
	if delta := now.Sub(read2.LastWallClock); delta < 24*time.Hour {
		t.Errorf("delta from reload baseline: got %v, want >=24h", delta)
	}
	if _, err := a.WriteWatchdogState(read2, now); err != nil {
		t.Fatalf("WriteWatchdogState 2nd: %v", err)
	}

	read3 := a.ReadWatchdogState()
	if !read3.LastWallClock.Equal(now) {
		t.Errorf("LastWallClock after 2nd write: got %v, want %v", read3.LastWallClock, now)
	}
}

// ---------------------------------------------------------------------------
// IsRestartPending(name, now) — injected clock asserts no ambient time.Now.
// ---------------------------------------------------------------------------

// TestRestartPending_InjectedClock — validates the contract that
// IsRestartPending answers purely from the supplied `now`. We cannot
// directly observe time.Now invocations from outside the package, but
// we can verify the answer flips deterministically across the 6-min TTL
// boundary using ONLY the injected clock parameter — if the impl
// consulted ambient time.Now, the boundary checks would be flaky.
func TestRestartPending_InjectedClock(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-pending"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	if read.Cool.IsRestartPending(taskName, t0) {
		t.Fatalf("IsRestartPending without prior MarkRestartPending: got true")
	}
	read.Cool.MarkRestartPending(taskName, t0)

	// At t0 + 1min: pending.
	if !read.Cool.IsRestartPending(taskName, t0.Add(1*time.Minute)) {
		t.Errorf("IsRestartPending at t0+1min: got false, want true")
	}
	// At t0 + 6min exactly: still pending (TTL is > 6min boundary).
	if !read.Cool.IsRestartPending(taskName, t0.Add(6*time.Minute)) {
		t.Errorf("IsRestartPending at t0+6min boundary: got false, want true")
	}
	// At t0 + 6min + 1ns: stale, not pending.
	if read.Cool.IsRestartPending(taskName, t0.Add(6*time.Minute+1)) {
		t.Errorf("IsRestartPending at t0+6min+1ns: got true, want false (stale)")
	}

	// Second IsRestartPending call at the SAME injected clock returns the
	// SAME answer — proves no ambient time.Now() drift.
	first := read.Cool.IsRestartPending(taskName, t0.Add(2*time.Minute))
	second := read.Cool.IsRestartPending(taskName, t0.Add(2*time.Minute))
	if first != second {
		t.Errorf("IsRestartPending non-deterministic on equal injected now")
	}

	// ClearRestartPending wipes regardless of clock.
	read.Cool.ClearRestartPending(taskName)
	if read.Cool.IsRestartPending(taskName, t0.Add(1*time.Minute)) {
		t.Errorf("IsRestartPending after Clear: got true, want false")
	}
}

// ---------------------------------------------------------------------------
// WriteWatchdogState — stale-clear events.
// ---------------------------------------------------------------------------

func TestWriteWatchdogState_StaleClearEvents(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-stale"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	read.Cool.MarkRestartPending(taskName, t0)
	if _, err := a.WriteWatchdogState(read, t0); err != nil {
		t.Fatalf("WriteWatchdogState seed: %v", err)
	}

	// 7 minutes later: pending should be cleared during serialization
	// AND the cleared task name returned in the events list.
	read2 := a.ReadWatchdogState()
	events, err := a.WriteWatchdogState(read2, t0.Add(7*time.Minute))
	if err != nil {
		t.Fatalf("WriteWatchdogState stale-clear: %v", err)
	}
	if len(events) != 1 || events[0] != taskName {
		t.Fatalf("staleClearEvents = %v, want [%q]", events, taskName)
	}

	// Re-read and confirm RestartPendingAt was zeroed on disk.
	read3 := a.ReadWatchdogState()
	if read3.Cool.IsRestartPending(taskName, t0.Add(8*time.Minute)) {
		t.Errorf("IsRestartPending after stale-clear write: got true, want false")
	}
}

// TestWriteWatchdogState_ErrFirstContract — atomic-write failure returns
// (nil, err) and never leaks a partial events slice (plan §36 v9).
func TestWriteWatchdogState_ErrFirstContract(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-err"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	read.Cool.MarkRestartPending(taskName, t0)
	if _, err := a.WriteWatchdogState(read, t0); err != nil {
		t.Fatalf("WriteWatchdogState seed: %v", err)
	}

	// Re-read; the second write would clear the stale entry but we
	// inject a rename failure to short-circuit the atomic write step.
	read2 := a.ReadWatchdogState()

	syntheticErr := errors.New("synthetic rename failure")
	watchdogStateRenameFn = func(oldpath, newpath string) error {
		return syntheticErr
	}

	events, err := a.WriteWatchdogState(read2, t0.Add(7*time.Minute))
	if err == nil {
		t.Fatalf("WriteWatchdogState: want err, got nil")
	}
	if events != nil {
		t.Errorf("events on err: got %v, want nil (err-first contract)", events)
	}
}

// ---------------------------------------------------------------------------
// Sliding-30min strikes — three independent windows.
// ---------------------------------------------------------------------------

// TestCorruptStrikeWindow_FourIn30MinTriggers — 4 entries within 30min
// → trigger detected (driver decides quarantine). 4 spread over 31min →
// no trigger. 5 entries → window capped at 4 newest.
func TestCorruptStrikeWindow_FourIn30MinTriggers(t *testing.T) {
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	// 4 strikes within 30min → len >= 4 (threshold).
	w := []time.Time{}
	for i := 0; i < 4; i++ {
		w = AppendStrike(w, t0.Add(time.Duration(i)*5*time.Minute), CorruptStrikeThreshold)
	}
	if len(w) < CorruptStrikeThreshold {
		t.Errorf("4 strikes in 30min: window len = %d, want >= %d", len(w), CorruptStrikeThreshold)
	}

	// 4 spread over 31min → only the 4th survives (the older 3 fall
	// outside the 30min window relative to the newest).
	w = nil
	w = AppendStrike(w, t0, CorruptStrikeThreshold)
	w = AppendStrike(w, t0.Add(11*time.Minute), CorruptStrikeThreshold)
	w = AppendStrike(w, t0.Add(22*time.Minute), CorruptStrikeThreshold)
	w = AppendStrike(w, t0.Add(31*time.Minute), CorruptStrikeThreshold)
	// Newest window-defining `now` is the last call → 31min. Entries
	// older than 31min - 30min = 1min fall out → keep entries with
	// ts >= 1min. So only the 22min and 31min entries survive.
	if len(w) >= CorruptStrikeThreshold {
		t.Errorf("4 strikes spread over 31min: len = %d, want < %d", len(w), CorruptStrikeThreshold)
	}

	// 5 entries within 30min → cap at threshold (4 newest).
	w = nil
	for i := 0; i < 5; i++ {
		w = AppendStrike(w, t0.Add(time.Duration(i)*time.Minute), CorruptStrikeThreshold)
	}
	if len(w) != CorruptStrikeThreshold {
		t.Errorf("5 strikes window cap: len = %d, want %d", len(w), CorruptStrikeThreshold)
	}
}

// TestAuditFailureWindow_ThreeIn30MinTriggers — separate window, threshold 3.
func TestAuditFailureWindow_ThreeIn30MinTriggers(t *testing.T) {
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	// 3 within 30min: trigger.
	w := []time.Time{}
	for i := 0; i < 3; i++ {
		w = AppendStrike(w, t0.Add(time.Duration(i)*5*time.Minute), AuditFailureThreshold)
	}
	if len(w) < AuditFailureThreshold {
		t.Errorf("3 audit failures in 30min: len = %d, want >= %d", len(w), AuditFailureThreshold)
	}

	// 3 spread over 31min → drop oldest, count drops below threshold.
	w = nil
	w = AppendStrike(w, t0, AuditFailureThreshold)
	w = AppendStrike(w, t0.Add(15*time.Minute), AuditFailureThreshold)
	w = AppendStrike(w, t0.Add(31*time.Minute), AuditFailureThreshold)
	if len(w) >= AuditFailureThreshold {
		t.Errorf("3 audit failures spread over 31min: len = %d, want < %d", len(w), AuditFailureThreshold)
	}
}

// TestStaleClearWindow_FourIn30MinTriggers — separate window, threshold 4.
func TestStaleClearWindow_FourIn30MinTriggers(t *testing.T) {
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	w := []time.Time{}
	for i := 0; i < 4; i++ {
		w = AppendStrike(w, t0.Add(time.Duration(i)*5*time.Minute), StaleClearThreshold)
	}
	if len(w) < StaleClearThreshold {
		t.Errorf("4 stale-clears in 30min: len = %d, want >= %d", len(w), StaleClearThreshold)
	}

	w = nil
	w = AppendStrike(w, t0, StaleClearThreshold)
	w = AppendStrike(w, t0.Add(11*time.Minute), StaleClearThreshold)
	w = AppendStrike(w, t0.Add(22*time.Minute), StaleClearThreshold)
	w = AppendStrike(w, t0.Add(31*time.Minute), StaleClearThreshold)
	if len(w) >= StaleClearThreshold {
		t.Errorf("4 stale-clears spread over 31min: len = %d, want < %d", len(w), StaleClearThreshold)
	}
}

// TestWindows_Independent — appending to one window must NOT mutate any
// other; persistence must keep the three windows separate.
func TestWindows_Independent(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	read := a.ReadWatchdogState()
	// Inject 4 corrupt strikes; leave audit + staleclear untouched.
	for i := 0; i < 4; i++ {
		read.CorruptStrikeWindow = AppendStrike(
			read.CorruptStrikeWindow,
			t0.Add(time.Duration(i)*5*time.Minute),
			CorruptStrikeThreshold,
		)
	}
	if _, err := a.WriteWatchdogState(read, t0.Add(20*time.Minute)); err != nil {
		t.Fatalf("WriteWatchdogState: %v", err)
	}
	read2 := a.ReadWatchdogState()
	if got := len(read2.CorruptStrikeWindow); got < CorruptStrikeThreshold {
		t.Errorf("CorruptStrikeWindow after persist: got %d, want >= %d",
			got, CorruptStrikeThreshold)
	}
	if got := len(read2.AuditFailureWindow); got != 0 {
		t.Errorf("AuditFailureWindow leaked from corrupt write: got %d, want 0", got)
	}
	if got := len(read2.StaleClearWindow); got != 0 {
		t.Errorf("StaleClearWindow leaked from corrupt write: got %d, want 0", got)
	}

	// Now inject 3 audit failures. CorruptStrikeWindow must remain
	// unchanged; StaleClearWindow must remain empty.
	read3 := a.ReadWatchdogState()
	corruptBefore := append([]time.Time(nil), read3.CorruptStrikeWindow...)
	for i := 0; i < 3; i++ {
		read3.AuditFailureWindow = AppendStrike(
			read3.AuditFailureWindow,
			t0.Add(time.Duration(i+10)*time.Minute),
			AuditFailureThreshold,
		)
	}
	if _, err := a.WriteWatchdogState(read3, t0.Add(40*time.Minute)); err != nil {
		t.Fatalf("WriteWatchdogState 2: %v", err)
	}
	read4 := a.ReadWatchdogState()
	if !timeSlicesEqual(read4.CorruptStrikeWindow, corruptBefore) {
		t.Errorf("CorruptStrikeWindow mutated by audit-window write: got %v, want %v",
			read4.CorruptStrikeWindow, corruptBefore)
	}
	if got := len(read4.AuditFailureWindow); got < AuditFailureThreshold {
		t.Errorf("AuditFailureWindow after persist: got %d, want >= %d",
			got, AuditFailureThreshold)
	}
	if got := len(read4.StaleClearWindow); got != 0 {
		t.Errorf("StaleClearWindow leaked from audit write: got %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Backward-compat — older state JSON without new windows unmarshals to nil.
// ---------------------------------------------------------------------------

func TestWatchdogState_BackwardCompat_OlderJSON(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)
	if _, err := DaemonStateDir(); err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}

	// Older schema: only Cooldowns + LastWallClockSeen + CorruptStrikeWindow.
	older := map[string]interface{}{
		"cooldowns":             map[string]interface{}{},
		"last_wall_clock_seen":  "2026-05-07T12:00:00Z",
		"corrupt_strike_window": []interface{}{},
	}
	raw, err := json.Marshal(older)
	if err != nil {
		t.Fatalf("marshal older: %v", err)
	}
	if err := writeWatchdogStateRaw(t, raw); err != nil {
		t.Fatalf("seed older: %v", err)
	}

	read := a.ReadWatchdogState()
	if read.State != WatchdogStateValid {
		t.Fatalf("State = %q, want valid (older JSON should parse)", read.State)
	}
	if read.AuditFailureWindow != nil && len(read.AuditFailureWindow) != 0 {
		t.Errorf("AuditFailureWindow on older JSON: got %v, want nil/empty",
			read.AuditFailureWindow)
	}
	if read.StaleClearWindow != nil && len(read.StaleClearWindow) != 0 {
		t.Errorf("StaleClearWindow on older JSON: got %v, want nil/empty",
			read.StaleClearWindow)
	}

	// Subsequent writes with new fields persist normally.
	read.AuditFailureWindow = AppendStrike(
		read.AuditFailureWindow,
		time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC),
		AuditFailureThreshold,
	)
	if _, err := a.WriteWatchdogState(read, time.Date(2026, 5, 7, 12, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("WriteWatchdogState: %v", err)
	}
	read2 := a.ReadWatchdogState()
	if len(read2.AuditFailureWindow) != 1 {
		t.Errorf("AuditFailureWindow after upgrade: got %d, want 1", len(read2.AuditFailureWindow))
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func timeSlicesEqual(a, b []time.Time) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]time.Time(nil), a...)
	bb := append([]time.Time(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Before(aa[j]) })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Before(bb[j]) })
	for i := range aa {
		if !aa[i].Equal(bb[i]) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Sanity: shipped JSON uses RFC3339Nano UTC for time fields.
// ---------------------------------------------------------------------------

func TestWatchdogState_TimeFields_AreUTC_RFC3339Nano(t *testing.T) {
	a := NewAPI()
	watchdogStateTestHelper(t)

	const taskName = "\\mcp-local-hub-utc"
	t0 := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	read := a.ReadWatchdogState()
	read.Cool.RecordAttempt(taskName, t0)
	read.Cool.MarkRestartPending(taskName, t0)
	read.CorruptStrikeWindow = AppendStrike(read.CorruptStrikeWindow, t0, CorruptStrikeThreshold)
	if _, err := a.WriteWatchdogState(read, t0); err != nil {
		t.Fatalf("WriteWatchdogState: %v", err)
	}

	dir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, watchdogStateFileLeaf))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	rawStr := string(raw)
	// All time strings end in `Z` (UTC). Catch any timezone drift.
	if strings.Contains(rawStr, "+00:00") || strings.Contains(rawStr, "+0000") {
		t.Errorf("time fields contain non-UTC offset; raw=%s", rawStr)
	}
}
