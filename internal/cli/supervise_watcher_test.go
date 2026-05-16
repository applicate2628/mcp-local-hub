// Package cli — Task 7.2 intent watcher tests.
//
// Spec §"Reconcile loop" + plan Task 7.2.
//
// These tests pin the three contracts the IntentWatcher must hold:
//
//  1. Mtime change on a tracked intent file fires onChange exactly once
//     per detected change (the watcher's re-baseline behavior prevents
//     a single change from firing repeatedly on subsequent idle ticks).
//  2. Idle state (no file changes) does NOT fire onChange. A spurious
//     fire would trigger a reconcile pass that needlessly walks every
//     daemon descriptor and competes with the spawn/terminate pipeline.
//  3. File presence transitions (absent → present, present → absent)
//     count as changes — operators sometimes delete daemon-intent.json
//     to clear all stops, and a freshly-installed supervisor-intent.json
//     must propagate to the reconcile loop without requiring a separate
//     IPC reload.
//
// Tests use a 50ms poll interval (vs the 60s production default) to
// keep wall-clock test time under 1s while still exercising the same
// code path. The clock-resolution sleep before the second write is
// necessary on Windows where the NTFS mtime resolution is ~10ms and
// two writes inside that window would produce identical mtimes and
// the watcher would correctly classify the second write as "no
// change" — which is not what the test is asserting.
package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestIntentWatcher_FiresOnSupervisorIntentChange asserts the primary
// contract: a mtime change on supervisor-intent.json triggers onChange.
// The initial baseline must NOT fire (a freshly-running watcher
// against a pre-existing file should sit quiet until the file is
// actually modified).
func TestIntentWatcher_FiresOnSupervisorIntentChange(t *testing.T) {
	dir := t.TempDir()
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"updated_at":"2026-05-17T00:00:00Z","daemons":[],"strict_mode":false}`), 0600); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var fireCount atomic.Int64
	w := NewIntentWatcher(dir, 50*time.Millisecond, func() {
		fireCount.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Wait several poll cycles to confirm the initial baseline did not
	// fire. 150ms covers 3 ticks at 50ms cadence with margin.
	time.Sleep(150 * time.Millisecond)
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("expected 0 fires before mtime change, got %d", got)
	}

	// Force a mtime change. NTFS resolution on Windows is ~10ms so a
	// brief sleep ensures the new write produces a distinct mtime
	// from the seed write above.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"updated_at":"2026-05-17T00:00:01Z","daemons":[],"strict_mode":true}`), 0600); err != nil {
		t.Fatalf("modify supervisor-intent.json: %v", err)
	}

	// Wait for at least one fire. 2s is generous — typical fire latency
	// is one poll interval (50ms) plus the os.Stat round trip.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fireCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fireCount.Load(); got == 0 {
		t.Fatalf("expected fire after supervisor-intent.json change, got 0")
	}
}

// TestIntentWatcher_FiresOnDaemonIntentChange asserts the same contract
// against the second tracked file. Both files are equal-priority intent
// inputs; a bug in the watchedIntentFiles slice or a typo in the
// snapshot loop would let one file regress while the other still works.
func TestIntentWatcher_FiresOnDaemonIntentChange(t *testing.T) {
	dir := t.TempDir()
	intentPath := filepath.Join(dir, "daemon-intent.json")
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"tasks":{}}`), 0600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}

	var fireCount atomic.Int64
	w := NewIntentWatcher(dir, 50*time.Millisecond, func() {
		fireCount.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("expected 0 fires before mtime change, got %d", got)
	}

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"tasks":{"\\foo":{"desired":"stopped"}}}`), 0600); err != nil {
		t.Fatalf("modify daemon-intent.json: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fireCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fireCount.Load(); got == 0 {
		t.Fatalf("expected fire after daemon-intent.json change, got 0")
	}
}

// TestIntentWatcher_NoFireOnIdle asserts the watcher does NOT fire
// when nothing in the tracked file set changes. A spurious fire here
// would propagate to a wasted reconcile pass — cheap individually but
// at 60s cadence over a long-running supervisor a single steady-state
// false-positive becomes hundreds of unnecessary reconciles per day.
func TestIntentWatcher_NoFireOnIdle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "supervisor-intent.json"), []byte(`{}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var fireCount atomic.Int64
	w := NewIntentWatcher(dir, 50*time.Millisecond, func() {
		fireCount.Add(1)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// 250ms covers 5 polls at 50ms; ample window to detect a stuck
	// "fire every tick" bug if the re-baseline logic regressed.
	time.Sleep(250 * time.Millisecond)
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("expected 0 fires on idle, got %d", got)
	}
}

// TestIntentWatcher_FiresOnFileCreation asserts the presence-transition
// path: a watcher started against a state dir with NO intent file
// should fire onChange when the file is created. This is the
// fresh-install path — `mcphub install` writes supervisor-intent.json
// for the first time after the supervisor is already running.
func TestIntentWatcher_FiresOnFileCreation(t *testing.T) {
	dir := t.TempDir()
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	// Do NOT pre-seed the file — let the baseline observe "absent".

	var fireCount atomic.Int64
	w := NewIntentWatcher(dir, 50*time.Millisecond, func() {
		fireCount.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Brief baseline window.
	time.Sleep(100 * time.Millisecond)
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("expected 0 fires before file creation, got %d", got)
	}

	// Create the file. The watcher must observe the absent → present
	// transition and fire onChange.
	if err := os.WriteFile(intentPath, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatalf("create supervisor-intent.json: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fireCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fireCount.Load(); got == 0 {
		t.Fatalf("expected fire after file creation, got 0")
	}
}

// TestIntentWatcher_FiresOnFileDeletion asserts the inverse presence
// transition: deleting a tracked file (operator clears all stops by
// removing daemon-intent.json) must trigger reconcile.
func TestIntentWatcher_FiresOnFileDeletion(t *testing.T) {
	dir := t.TempDir()
	intentPath := filepath.Join(dir, "daemon-intent.json")
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"tasks":{}}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var fireCount atomic.Int64
	w := NewIntentWatcher(dir, 50*time.Millisecond, func() {
		fireCount.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(150 * time.Millisecond)
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("expected 0 fires before deletion, got %d", got)
	}

	if err := os.Remove(intentPath); err != nil {
		t.Fatalf("remove daemon-intent.json: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fireCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := fireCount.Load(); got == 0 {
		t.Fatalf("expected fire after file deletion, got 0")
	}
}

// TestIntentWatcher_DefaultsPollIntervalTo60s asserts the spec-mandated
// 60s default applies when caller passes 0 or negative. A regression
// (e.g. accidentally defaulting to 0 — busy-loop) would crater CPU on
// every supervisor host.
func TestIntentWatcher_DefaultsPollIntervalTo60s(t *testing.T) {
	w := NewIntentWatcher(t.TempDir(), 0, func() {})
	if w.pollInterval != 60*time.Second {
		t.Fatalf("expected default pollInterval 60s, got %v", w.pollInterval)
	}
	w2 := NewIntentWatcher(t.TempDir(), -5*time.Millisecond, func() {})
	if w2.pollInterval != 60*time.Second {
		t.Fatalf("expected negative pollInterval clamped to 60s, got %v", w2.pollInterval)
	}
}

// TestIntentWatcher_NilOnChangeIsSafe asserts the watcher does not
// panic when onChange is nil (a defensive contract — tests that only
// want to assert the watcher does NOT panic on a missing state dir
// can pass nil; production callers MUST pass a real callback).
func TestIntentWatcher_NilOnChangeIsSafe(t *testing.T) {
	dir := t.TempDir()
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte(`{}`), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := NewIntentWatcher(dir, 50*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	// Mutate the file — the watcher should detect the change but
	// must NOT panic when invoking the nil callback.
	if err := os.WriteFile(intentPath, []byte(`{"changed":true}`), 0600); err != nil {
		t.Fatalf("modify: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	// No assertion needed — surviving this far without panic IS the
	// assertion. A panic would have killed the test goroutine and the
	// outer test would have reported the runtime error.
}
