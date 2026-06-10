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

	"mcp-local-hub/internal/api"
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

// TestIntentWatcher_DoesNotFireOnDaemonIntentChange pins the Phase 4-E2
// contract: daemon-intent.json is DROPPED from watchedIntentFiles, so a
// change to a (stale/leftover) daemon-intent.json must NOT trigger a
// reconcile. After E2 the supervisor-intent.json `stops` sub-block is the
// sole stop source; the watcher tracks only that file. A regression that
// re-added daemon-intent.json to watchedIntentFiles would fire here and the
// test would catch it.
func TestIntentWatcher_DoesNotFireOnDaemonIntentChange(t *testing.T) {
	dir := t.TempDir()
	// Seed BOTH a supervisor-intent.json (the tracked file) and a leftover
	// daemon-intent.json so the watcher has a stable baseline and only the
	// daemon-intent.json mutates below.
	supPath := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(supPath, []byte(`{"version":1,"updated_at":"2026-05-17T00:00:00Z","daemons":[],"strict_mode":false}`), 0600); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	daemonPath := filepath.Join(dir, "daemon-intent.json")
	if err := os.WriteFile(daemonPath, []byte(`{"version":1,"tasks":{}}`), 0600); err != nil {
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

	// Mutate ONLY daemon-intent.json. The watcher must stay quiet.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(daemonPath, []byte(`{"version":1,"tasks":{"\\foo":{"desired":"stopped"}}}`), 0600); err != nil {
		t.Fatalf("modify daemon-intent.json: %v", err)
	}

	// Wait several poll cycles; assert NO fire (daemon-intent.json untracked).
	time.Sleep(250 * time.Millisecond)
	if got := fireCount.Load(); got != 0 {
		t.Fatalf("expected 0 fires after daemon-intent.json change (E2: untracked), got %d", got)
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
// transition: deleting the tracked supervisor-intent.json (e.g. an operator
// clearing the intent) must trigger reconcile. Phase 4-E2: the tracked file
// is supervisor-intent.json (daemon-intent.json is no longer watched).
func TestIntentWatcher_FiresOnFileDeletion(t *testing.T) {
	dir := t.TempDir()
	intentPath := filepath.Join(dir, "supervisor-intent.json")
	if err := os.WriteFile(intentPath, []byte(`{"version":1,"updated_at":"2026-05-17T00:00:00Z","daemons":[],"strict_mode":false}`), 0600); err != nil {
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
		t.Fatalf("remove supervisor-intent.json: %v", err)
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

// TestResolveWatcherDaemonIntent_FailClosedContract pins the IntentWatcher
// onChange stop-resolution contract (adversarial review P2-B). It is the
// watcher-path analogue of
// TestReconcileIPC_ApplyPreservesDaemonIntentCacheOnCorruptRead: when both
// stop sources are unavailable this tick, the resolver MUST keep the prior
// snapshot rather than synthesize an EMPTY UnifiedStopsFile(nil, nil) that
// would clear the cache and let a sub-block-sourced stop un-suppress (the SM
// would then revive a deliberately-stopped daemon).
//
// Before the fix the watcher only kept `previous` on a daemon-intent.json read
// FAILURE; a supervisor-intent.json read failure with a MISSING
// daemon-intent.json fell through to UnifiedStopsFile(nil, nil) → empty → cache
// cleared. That is the latent-in-E1 / live-in-E2 fail-quiet regression this
// test locks out.
func TestResolveWatcherDaemonIntent_FailClosedContract(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	prevStop := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	liveStop := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	supWithBaselineStop := &api.SupervisorIntentFile{
		Version: 1,
		Stops: map[string]api.DaemonIntent{
			taskName: {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserStop,
				UpdatedAt: time.Now().UTC(),
			},
		},
	}

	// stopped reports whether the resolved file keeps taskName suppressed.
	stopped := func(f *api.DaemonIntentFile) bool {
		if f == nil || f.Tasks == nil {
			return false
		}
		di, ok := f.Tasks[taskName]
		return ok && di.Desired == api.IntentDesiredStopped
	}

	tests := []struct {
		name         string
		supRead      *api.SupervisorIntentFile
		rawDaemon    *api.DaemonIntentFile
		daemonFailed bool
		supFailed    bool
		// wantPrevious asserts the resolver returned `previous` verbatim
		// (pointer-identity), the fail-closed branch.
		wantPrevious bool
		// wantStopped asserts the resolved file keeps taskName suppressed
		// (whether via `previous` or via a live/baseline source).
		wantStopped bool
	}{
		{
			// The load-bearing P2-B case: supervisor read failed AND
			// daemon-intent.json missing → keep previous (do NOT clear).
			name:         "supFailed_and_missing_daemon_intent_keeps_previous",
			supRead:      nil, // caller nils supRead when supFailed
			rawDaemon:    nil, // missing daemon-intent.json
			daemonFailed: false,
			supFailed:    true,
			wantPrevious: true,
			wantStopped:  true,
		},
		{
			// daemon-intent.json read failure → keep previous (pre-existing
			// guard; must still hold).
			name:         "daemon_intent_read_failure_keeps_previous",
			supRead:      supWithBaselineStop,
			rawDaemon:    nil,
			daemonFailed: true,
			supFailed:    false,
			wantPrevious: true,
			wantStopped:  true,
		},
		{
			// Phase 4-E2: supervisor-intent.json read FAILED → keep previous
			// UNCONDITIONALLY. Even a (stale) live daemon-intent.json present is
			// IGNORED now that the sub-block is the sole source — a failed read
			// of the sole source must not let a stale leftover decide, so the
			// resolver fails closed to the cached snapshot. (E1 used the live
			// file here; E2 inverts that precedence.)
			name:         "supFailed_keeps_previous_even_with_stale_daemon_intent",
			supRead:      nil,
			rawDaemon:    liveStop,
			daemonFailed: false,
			supFailed:    true,
			wantPrevious: true,
			wantStopped:  true,
		},
		{
			// Healthy both: daemon-intent.json missing → fall back to the
			// supervisor-intent stops sub-block (recovery baseline).
			name:         "missing_daemon_intent_falls_back_to_baseline",
			supRead:      supWithBaselineStop,
			rawDaemon:    nil,
			daemonFailed: false,
			supFailed:    false,
			wantPrevious: false,
			wantStopped:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveWatcherDaemonIntent(tc.supRead, tc.rawDaemon, tc.daemonFailed, tc.supFailed, prevStop)
			if tc.wantPrevious && got != prevStop {
				t.Fatalf("expected resolver to keep previous snapshot (pointer-identity), got %p want %p", got, prevStop)
			}
			if !tc.wantPrevious && got == prevStop {
				t.Fatalf("expected resolver to NOT return previous, but it did")
			}
			if gotStopped := stopped(got); gotStopped != tc.wantStopped {
				t.Fatalf("resolved stop-suppression = %v, want %v (resolved=%+v)", gotStopped, tc.wantStopped, got)
			}
		})
	}
}
