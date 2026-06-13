package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// reapTestHarness wires a minimal in-memory controller for the orphan-reap
// tests. NO real process is spawned or killed and NO real state dir is
// touched: spawn/terminate are recording fakes, the tracker is in-memory, and
// statePath points inside a hardened temp dir.
//
// The harness drives the controller through a REAL running event loop (Run is
// started in a goroutine and ctrl.handleLoopEvent is registered as the
// handler), because the whole point of the pr302 r3 fix is that the reap
// lifecycle runs ON the loop. Determinism is preserved via the evReapBarrier
// synchronization event: every event-driving helper (refresh / postEvent /
// fireReapFollowup) is followed by h.sync(), which posts a barrier and blocks
// until the loop has drained every prior event. After sync() returns the loop
// is idle, so the test goroutine reads ctrl/tracker state race-free (the loop's
// writes happen-before the barrier signal, which happens-before the read).
//
// Running the loop concurrently means `go test -race` actually exercises the
// off-loop refreshSupervisorIntent → on-loop handleReapScan handoff and the
// timer → on-loop handleReapFollowup handoff that the fix introduced.
type reapTestHarness struct {
	ctrl           *supervisorController
	tracker        *DaemonRuntimeTracker
	loop           *api.EventLoop
	spawnCalls     *atomic.Int32
	terminateCalls *atomic.Int32
	terminatedPIDs chan int
	// terminateErr, when non-nil, makes the fake terminate return it WITHOUT
	// marking the tracker exited — modeling a FAILED terminate (PID query /
	// permission / escalation error; the process may still be alive). Used by
	// the finding-2 preserve-on-failure test. Guarded by terminateMu because the
	// terminate fake now runs on the loop goroutine while the test goroutine sets
	// it.
	terminateMu  sync.Mutex
	terminateErr error
	// freshIntent is the fake on-disk intent the follow-up handler re-reads
	// (finding A). The test sets it to model a remove (empty) or a re-add
	// (descriptor present) independently of the intentCache. Guarded by
	// freshIntentMu because reapIntentReader runs on the loop goroutine.
	// freshIntentErr, when non-nil, makes the fresh on-disk read FAIL — modeling a
	// transient supervisor-intent.json read glitch the follow-up handler must NOT
	// treat as confirmed-absence (#856). Guarded by the same mutex.
	freshIntentMu  sync.Mutex
	freshIntent    *api.SupervisorIntentFile
	freshIntentErr error
	// pendingFollowups is the ORDERED list of armed follow-up timer callbacks
	// captured by the injected reapAfterFunc (wall-clock-free). Each callback
	// closes over its own (taskName, generation) and POSTS evReapFollowup when
	// invoked. fireReapFollowup fires them FIFO; the production generation guard
	// + per-task scoping make a stale fire a deterministic no-op. Guarded by
	// followupMu because arm runs on the loop goroutine while the test fires on
	// its own goroutine.
	followupMu       sync.Mutex
	pendingFollowups []func()
}

func newReapTestHarness(t *testing.T) *reapTestHarness {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	tracker := NewDaemonRuntimeTracker()
	var spawnCalls atomic.Int32
	var terminateCalls atomic.Int32
	terminatedPIDs := make(chan int, 8)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	loop := api.NewEventLoop(64)
	h := &reapTestHarness{
		tracker:        tracker,
		loop:           loop,
		spawnCalls:     &spawnCalls,
		terminateCalls: &terminateCalls,
		terminatedPIDs: terminatedPIDs,
	}
	h.ctrl = &supervisorController{
		intentCache:  newIntentCache(),
		eventLoop:    loop,
		tracker:      tracker,
		events:       events,
		daemonIntent: newDaemonIntentCache(),
		spawn: func(d api.SupervisorDaemon) error {
			spawnCalls.Add(1)
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			entry, _ := tracker.Get(d.TaskName)
			terminateCalls.Add(1)
			select {
			case terminatedPIDs <- entry.CurrentPID:
			default:
			}
			h.terminateMu.Lock()
			termErr := h.terminateErr
			h.terminateMu.Unlock()
			if termErr != nil {
				// FAILED terminate: do NOT mark exited — the PID may still be
				// alive. The reap must preserve the entry for retry.
				return termErr
			}
			// Production terminate returns nil when the targeted PID is gone
			// (already-dead / identity-mismatch / confirmed-terminated) and
			// marks the tracker exited. Mirror that nil-return contract so the
			// reap's clearRemovedTaskRuntime sees a consistent entry.
			tracker.MarkExited(d.TaskName)
			return nil
		},
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		reapFollowupDelay:   time.Hour, // never fires on the wall clock; the test drives it
		reapAfterFunc: func(_ time.Duration, f func()) reapTimer {
			// Each callback closes over its own (taskName, generation) and posts
			// evReapFollowup when invoked. Record them in arm order; fireReapFollowup
			// fires FIFO. The production generation guard + per-task scoping make a
			// stale fire a deterministic no-op, so FIFO firing is sufficient to
			// exercise findings B/C.
			h.followupMu.Lock()
			h.pendingFollowups = append(h.pendingFollowups, f)
			h.followupMu.Unlock()
			return noopReapTimer{}
		},
		// reapIntentReader returns the harness's fake on-disk intent (finding A):
		// the follow-up handler confirms absence against THIS, not the cache.
		reapIntentReader: func() (*api.SupervisorIntentFile, error) {
			h.freshIntentMu.Lock()
			defer h.freshIntentMu.Unlock()
			if h.freshIntentErr != nil {
				return nil, h.freshIntentErr
			}
			return h.freshIntent, nil
		},
	}
	// Register the controller handler and start the real loop.
	loop.RegisterHandler(h.ctrl.handleLoopEvent)
	go loop.Run(ctx)
	return h
}

// noopReapTimer satisfies the reapTimer interface for the test fake. Stop is a
// no-op returning true; the generation guard in handleReapFollowup neutralizes a
// stale fire deterministically, so the test never depends on Stop actually
// canceling.
type noopReapTimer struct{}

func (noopReapTimer) Stop() bool { return true }

// sync posts an evReapBarrier and blocks until the loop has drained every prior
// event. After it returns the loop is idle and the test goroutine may read
// ctrl/tracker state race-free.
func (h *reapTestHarness) sync() {
	done := make(chan struct{})
	h.loop.Post(api.LoopEvent{
		Kind: evReapBarrier,
		Body: map[string]any{reapBarrierResultBodyKey: done},
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		panic("reap test harness sync timed out (loop wedged?)")
	}
}

// refresh drives an intent refresh through the production off-loop entry point
// (refreshSupervisorIntent posts evReapScan), then syncs so the on-loop
// handleReapScan has fully run. Stops are passed nil (preserve the prior stops
// cache) — the descriptor-set tests do not exercise stop intent; refreshWithStops
// is the variant that swaps the stops cache from the snapshot.
func (h *reapTestHarness) refresh(updated *api.SupervisorIntentFile) {
	h.refreshWithStops(updated, nil)
}

// refreshWithStops drives an intent refresh carrying a FRESH unified-stops snapshot
// so the on-loop handleReapScan swaps BOTH caches from the same snapshot (pr302 r4
// root fix). Used by the re-add-with-Desired=stopped test (the stops cache must
// reflect the re-add so a replayed event treats it as stopped, not default-running).
func (h *reapTestHarness) refreshWithStops(updated *api.SupervisorIntentFile, stops *api.DaemonIntentFile) {
	h.ctrl.refreshSupervisorIntent(updated, stops)
	h.sync()
}

// postEvent posts an SM event onto the loop and syncs.
func (h *reapTestHarness) postEvent(ev api.LoopEvent) {
	h.loop.Post(ev)
	h.sync()
}

// postNoSync posts an SM event onto the loop WITHOUT syncing, so a subsequent
// refreshNoSync can queue an evReapScan BEHIND it in the FIFO — the test then syncs
// once to drain both. Used by the queued-event-before-removal ordering test (the
// EvChildExit must drain first against the still-present cache, OR — if the order
// inverts — route via the shadow the on-loop scan installs before the swap).
func (h *reapTestHarness) postNoSync(ev api.LoopEvent) {
	h.loop.Post(ev)
}

// refreshNoSync drives an intent refresh through the production off-loop entry
// (posts evReapScan) WITHOUT syncing, so the caller controls the drain point.
func (h *reapTestHarness) refreshNoSync(updated *api.SupervisorIntentFile) {
	h.ctrl.refreshSupervisorIntent(updated, nil)
}

// setReapIntentReadErr toggles the fake fresh-on-disk-read failure mode (#856): a
// non-nil err makes the follow-up handler's reapIntentReader return an error, which
// must PRESERVE the reap + re-arm rather than confirm absence against the cache.
func (h *reapTestHarness) setReapIntentReadErr(err error) {
	h.freshIntentMu.Lock()
	h.freshIntentErr = err
	h.freshIntentMu.Unlock()
}

// setFreshIntent sets the fake on-disk intent the follow-up handler re-reads
// (finding A): pass emptyReapIntent() to model "still removed" or an intent
// carrying the descriptor to model a re-add landed on disk.
func (h *reapTestHarness) setFreshIntent(intent *api.SupervisorIntentFile) {
	h.freshIntentMu.Lock()
	h.freshIntent = intent
	h.freshIntentMu.Unlock()
}

// setTerminateErr toggles the fake terminate's failure mode (finding 2 / F).
func (h *reapTestHarness) setTerminateErr(err error) {
	h.terminateMu.Lock()
	h.terminateErr = err
	h.terminateMu.Unlock()
}

// fireReapFollowup invokes the OLDEST armed follow-up timer callback (FIFO)
// which POSTS evReapFollowup onto the loop, then syncs so the on-loop
// handleReapFollowup has fully run. Returns false when no follow-up is armed.
func (h *reapTestHarness) fireReapFollowup() bool {
	h.followupMu.Lock()
	if len(h.pendingFollowups) == 0 {
		h.followupMu.Unlock()
		return false
	}
	f := h.pendingFollowups[0]
	h.pendingFollowups = h.pendingFollowups[1:]
	h.followupMu.Unlock()
	f()      // posts evReapFollowup onto the loop
	h.sync() // drain the follow-up resolution
	return true
}

// pendingFollowupCount reports how many follow-up callbacks are currently armed
// (used by the finding-C test to assert a fresh window armed a new timer).
func (h *reapTestHarness) pendingFollowupCount() int {
	h.followupMu.Lock()
	defer h.followupMu.Unlock()
	return len(h.pendingFollowups)
}

// seedLiveDaemon installs `descriptor` into the intent cache and marks it live
// at `state` with a tracker PID, exactly as a steady-state running daemon
// would appear after a spawn. Called before any event is driven, so the direct
// smStates.Store is race-free (the loop has no work yet); a trailing sync()
// would also serve, but seeding precedes the first drive in every test.
func (h *reapTestHarness) seedLiveDaemon(descriptor api.SupervisorDaemon, state api.SMState, pid int) {
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}}
	h.ctrl.intentCache.Refresh(intent)
	h.ctrl.smStates.Store(descriptor.TaskName, state)
	if pid > 0 {
		h.tracker.MarkSpawned(descriptor.TaskName, pid, time.Now().UTC())
	}
	// Mirror the fresh on-disk intent so a follow-up before any removal sees the
	// daemon present (defensive; most tests set it explicitly before firing).
	h.setFreshIntent(intent)
}

func reapTestDescriptor() api.SupervisorDaemon {
	return api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-lsp-deadbeef-go`,
		Server:   "mcp-language-server",
		Daemon:   "lsp-deadbeef-go",
		Command:  "mcphub",
		Args:     []string{"daemon", "workspace-proxy"},
	}
}

func emptyReapIntent() *api.SupervisorIntentFile {
	return &api.SupervisorIntentFile{Version: 1}
}

// TestSupervisorController_OrphanReap_DescriptorDisappearTerminatesAfterWindow
// is the POSITIVE / falsifying control. A StRunning daemon whose descriptor
// disappears from intent must be driven through the SM-aware terminate — but
// only AFTER the one-tick verification window confirms the absence is real,
// not a replace-in-place blip.
//
// Pre-fix: refreshSupervisorIntent → clearRemovedTaskRuntime only cleared
// in-memory bookkeeping and NEVER terminated the live child, orphaning it on
// its TCP port until a supervisor cold-restart. This test fails pre-fix at the
// tick-2 terminate assertion.
func TestSupervisorController_OrphanReap_DescriptorDisappearTerminatesAfterWindow(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 41001)

	// Tick 1: descriptor absent for the first time → mark pendingReap only.
	// NO terminate; SM state PRESERVED so a re-add can absorb a replace-in-place.
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("tick-1 disappearance did not mark the live task pendingReap")
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("tick-1 must PRESERVE SM state across the verification window; got state=%v ok=%v", st, ok)
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("tick-1 must NOT terminate (verification window); terminate calls = %d", got)
	}

	// Tick 2: still absent → confirmed → SM-aware terminate fires exactly once,
	// then bookkeeping clears.
	h.refresh(emptyReapIntent())
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("tick-2 confirmed removal must terminate the orphaned child exactly once; terminate calls = %d", got)
	}
	select {
	case pid := <-h.terminatedPIDs:
		if pid != 41001 {
			t.Fatalf("reap terminated the wrong PID: got %d, want 41001 (the live child the descriptor was spawned with)", pid)
		}
	default:
		t.Fatalf("reap terminate did not record a target PID")
	}
	if _, ok := h.ctrl.GetSMState(d.TaskName); ok {
		t.Fatalf("confirmed reap left stale SM state tracked")
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("confirmed reap left a stale pendingReap mark")
	}
	if entry, ok := h.tracker.Get(d.TaskName); ok {
		t.Fatalf("confirmed reap left tracker entry: %+v", entry)
	}
}

// TestSupervisorController_OrphanReap_TransientAbsenceReappearNotTerminated is
// the CRITICAL negative control — the guard that prevents the reap from killing
// a live daemon during an operator/install REPLACE-IN-PLACE (remove + re-add
// landing in separate intent writes). A StRunning daemon absent in ONE refresh
// but PRESENT again in the NEXT must NOT be terminated; the verification window
// absorbs the blip and the daemon keeps running untouched.
//
// A wrong fix that reaps on the first observed absence terminates a live
// daemon here — this test is the falsifier for that mistake.
func TestSupervisorController_OrphanReap_TransientAbsenceReappearNotTerminated(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 42002)

	// Tick 1: descriptor momentarily absent (mid replace-in-place) → mark only.
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("tick-1 blip did not mark pendingReap")
	}

	// Tick 2: the re-add lands — descriptor PRESENT again → drop the mark, no
	// terminate. The daemon is preserved exactly as it was running.
	reappeared := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.refresh(reappeared)

	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("replace-in-place must NOT terminate the live daemon; terminate calls = %d (a wrong reap killed a live daemon)", got)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("re-appeared descriptor left a stale pendingReap mark; the next refresh would wrongly reap it")
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("replace-in-place must PRESERVE the running daemon; got state=%v ok=%v", st, ok)
	}
	if entry, ok := h.tracker.Get(d.TaskName); !ok || entry.CurrentPID != 42002 {
		t.Fatalf("replace-in-place disturbed the tracker entry: %+v (ok=%v)", entry, ok)
	}

	// And a subsequent normal refresh with the descriptor still present must
	// remain a no-op — the blip left no latent reap state.
	h.refresh(reappeared)
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("post-blip steady refresh terminated a live daemon; terminate calls = %d", got)
	}
}

// TestSupervisorController_OrphanReap_AlreadyStoppingNotDoubleReaped proves a
// task already at a non-live settled/terminating SM state is NOT reaped. A
// StIdle daemon (deliberately stopped, no live child) and a StExiting daemon
// (terminate already in flight, its own EvChildExit will finish it) must both
// skip the terminate — the reap only ever drives a terminate for a LIVE child
// with nothing else already terminating it.
func TestSupervisorController_OrphanReap_AlreadyStoppingNotDoubleReaped(t *testing.T) {
	t.Run("StIdle", func(t *testing.T) {
		h := newReapTestHarness(t)
		d := reapTestDescriptor()
		// StIdle: a deliberately-stopped daemon, no live child.
		h.seedLiveDaemon(d, api.StIdle, 0)

		// Tick 1: a non-live state is never marked pendingReap; bookkeeping is
		// cleared immediately (the prior behavior for non-live removals).
		h.refresh(emptyReapIntent())
		if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
			t.Fatalf("StIdle removal must NOT be marked pendingReap (nothing live to terminate)")
		}
		// Tick 2: confirm no terminate ever fires.
		h.refresh(emptyReapIntent())
		if got := h.terminateCalls.Load(); got != 0 {
			t.Fatalf("StIdle removal must never terminate; terminate calls = %d", got)
		}
	})

	t.Run("StExiting", func(t *testing.T) {
		h := newReapTestHarness(t)
		d := reapTestDescriptor()
		// StExiting: a terminate is already in flight (e.g. an operator stop
		// drove StRunning -> StExiting just before the descriptor was removed).
		h.seedLiveDaemon(d, api.StExiting, 43003)

		// Tick 1: StExiting IS a reapable (live) state, so it is marked
		// pendingReap — but reapRemovedDaemon must short-circuit on StExiting
		// (a terminate is already in flight) and NOT double-drive it.
		h.refresh(emptyReapIntent())
		// Tick 2: confirmed absent → reapRemovedDaemon runs but observes
		// StExiting and returns without a second terminate.
		h.refresh(emptyReapIntent())
		if got := h.terminateCalls.Load(); got != 0 {
			t.Fatalf("a StExiting (already-terminating) task must NOT be double-reaped; terminate calls = %d", got)
		}
	})
}

// TestSupervisorController_OrphanReap_PresentRunningRefreshUnchanged proves the
// common case: a normal refresh where the descriptor is STILL present leaves a
// running daemon completely untouched — no pendingReap mark, no terminate, no
// state change.
func TestSupervisorController_OrphanReap_PresentRunningRefreshUnchanged(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 44004)

	present := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	for i := 0; i < 3; i++ {
		h.refresh(present)
	}

	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("present-and-running refresh wrongly marked pendingReap")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("present-and-running refresh terminated a live daemon; terminate calls = %d", got)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("present-and-running refresh disturbed SM state; got state=%v ok=%v", st, ok)
	}
	if entry, ok := h.tracker.Get(d.TaskName); !ok || entry.CurrentPID != 44004 {
		t.Fatalf("present-and-running refresh disturbed tracker: %+v (ok=%v)", entry, ok)
	}
}

// TestSupervisorController_OrphanReap_FollowupTickTerminatesOnStableIntent is
// the FINDING 1 control: a pendingReap whose intent then stays STABLE (the
// common uninstall/remove case — the descriptor is removed once and never
// touched again) must STILL be terminated, driven by the self-armed bounded
// follow-up tick rather than a second intent-change refresh that never arrives.
//
// Pre-fix the two refreshSupervisorIntent call sites (IntentWatcher onChange +
// applyReconcileDrift) fire only on an intent MTIME change, so after the single
// remove there is no second tick and the orphan is never terminated. This test
// marks pendingReap on the ONLY refresh, then fires the follow-up tick (no
// further refresh) and asserts the terminate fired.
func TestSupervisorController_OrphanReap_FollowupTickTerminatesOnStableIntent(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 45005)

	// The ONE and only refresh: descriptor removed → mark pendingReap + arm the
	// follow-up tick. NO terminate yet (verification window).
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("removal did not mark pendingReap")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("first refresh must NOT terminate (verification window); terminate calls = %d", got)
	}

	// Intent now stays STABLE and the descriptor is still GONE on disk — model
	// that for the fresh-read confirmation the follow-up handler performs
	// (finding A). No further refreshSupervisorIntent call will ever happen; the
	// self-armed follow-up tick is the ONLY thing that can confirm + terminate.
	h.setFreshIntent(emptyReapIntent())
	if !h.fireReapFollowup() {
		t.Fatalf("a pendingReap mark must ARM a follow-up tick (finding 1); none was armed")
	}

	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("follow-up tick on a stable intent must terminate the orphan exactly once (finding 1); terminate calls = %d", got)
	}
	select {
	case pid := <-h.terminatedPIDs:
		if pid != 45005 {
			t.Fatalf("follow-up reap terminated the wrong PID: got %d, want 45005", pid)
		}
	default:
		t.Fatalf("follow-up reap terminate did not record a target PID")
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("follow-up reap left a stale pendingReap mark")
	}
	if _, ok := h.ctrl.GetSMState(d.TaskName); ok {
		t.Fatalf("follow-up reap left stale SM state tracked")
	}
}

// TestSupervisorController_OrphanReap_TerminateFailurePreservesStateForRetry is
// the FINDING 2 control: when the SM-aware terminate FAILS (the production
// TerminateFunc returns non-nil on a PID query / permission / escalation error,
// and the targeted process may still be alive), the reap must PRESERVE the
// SM/tracker entry + pendingReap so a later tick retries — NOT clear them.
//
// Pre-fix the StExiting side effect cleared the SM/tracker entry
// unconditionally; since the descriptor is already gone from intent and the
// liveness sweep only considers tracker rows that still have a descriptor,
// losing the recorded PID stranded the orphan with no supervisor handle. This
// test fails the terminate on the first confirmed reap, asserts the state
// survives, then succeeds the retry and asserts the orphan is finally reaped.
func TestSupervisorController_OrphanReap_TerminateFailurePreservesStateForRetry(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 46006)

	// Make the terminate FAIL on the first confirmed reap.
	h.setTerminateErr(errors.New("simulated terminate failure (PID query/permission error)"))

	// Tick 1: mark pendingReap. Tick 2: confirmed absent → terminate fires but
	// FAILS. The confirmation here comes through the scan path (the second
	// refresh's body `next` set), independent of the disk reader.
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())

	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("confirmed reap must attempt the terminate once; terminate calls = %d", got)
	}
	// FAILURE preservation: SM state, tracker PID, and pendingReap must all
	// SURVIVE so a later tick can retry. Losing any of them strands the orphan.
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok {
		t.Fatalf("terminate failure must PRESERVE the SM entry for retry; it was cleared (state=%v)", st)
	}
	if entry, ok := h.tracker.Get(d.TaskName); !ok || entry.CurrentPID != 46006 {
		t.Fatalf("terminate failure must PRESERVE the recorded PID for retry; entry=%+v ok=%v", entry, ok)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("terminate failure must PRESERVE pendingReap so a later tick retries; it was dropped")
	}

	// The retry path: a later follow-up tick fires with the terminate now
	// succeeding. The descriptor is still gone on disk (finding A read). The
	// orphan is finally reaped and bookkeeping clears.
	h.setTerminateErr(nil)
	h.setFreshIntent(emptyReapIntent())
	if !h.fireReapFollowup() {
		t.Fatalf("terminate failure must KEEP a follow-up tick armed for retry; none was armed")
	}

	if got := h.terminateCalls.Load(); got != 2 {
		t.Fatalf("retry tick must re-attempt the terminate; total terminate calls = %d, want 2", got)
	}
	if _, ok := h.ctrl.GetSMState(d.TaskName); ok {
		t.Fatalf("successful retry must clear the SM entry")
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("successful retry must clear pendingReap")
	}
	if entry, ok := h.tracker.Get(d.TaskName); ok {
		t.Fatalf("successful retry must clear the tracker entry: %+v", entry)
	}
}

// TestSupervisorController_OrphanReap_ChildExitDuringGapRespawns is the
// FINDING 3 control: a child that DIES while its descriptor is momentarily
// absent (the pendingReap window) must drive the normal crash/respawn path, NOT
// be left as a stale StRunning that apply/liveness treat as steady-state.
//
// The common-root reaping shadow makes this work: with the descriptor kept
// routable in the shadow, the in-flight EvChildExit routes through
// handleLoopEvent normally (StRunning + EvChildExit → StBackoffWaiting) instead
// of being orphan-dropped against the empty cache. On reappear the SM is
// already correctly at StBackoffWaiting (respawn pending), not a dead StRunning.
func TestSupervisorController_OrphanReap_ChildExitDuringGapRespawns(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 47007)

	// Tick 1: descriptor momentarily absent (replace-in-place blip) → marked
	// pendingReap, descriptor kept in the reaping shadow.
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); !ok {
		t.Fatalf("pendingReap window did not keep the descriptor in the reaping shadow (common-root fix)")
	}

	// The child crashes DURING the gap. Its real EvChildExit lands while the
	// descriptor is gone from intentCache. Without the shadow this would be
	// orphan-dropped; with it, the SM routes StRunning → StBackoffWaiting.
	h.postEvent(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StBackoffWaiting {
		t.Fatalf("EvChildExit during the absent window must route via the shadow to StBackoffWaiting (not a dropped event leaving a stale StRunning); got state=%v ok=%v", st, ok)
	}

	// Tick 2: the descriptor REAPPEARS (replace-in-place completed). The reap is
	// canceled; the daemon is already in backoff and will respawn — it was NOT
	// wrongly treated as a steady-state running daemon.
	reappeared := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.refresh(reappeared)

	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("replace-in-place must not terminate; terminate calls = %d", got)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StBackoffWaiting {
		t.Fatalf("after reappear the crashed daemon must remain in its respawn path (StBackoffWaiting), not a stale StRunning; got state=%v ok=%v", st, ok)
	}
}

// TestSupervisorController_OrphanReap_SpawningQueuedStopSurvives is the
// FINDING 4 control: a task removed while StSpawning records queued_action=stop
// (the SM terminates it later on the spawn-completion event). The caller must
// NOT clear runtime state on the reap — that would delete the queued stop, and
// the later completion event would be dropped against the empty cache, leaving
// the just-spawned child orphaned. The reaping shadow keeps the completion
// event routable so the queued stop actually terminates the child.
func TestSupervisorController_OrphanReap_SpawningQueuedStopSurvives(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	// StSpawning: a spawn is in flight, the child PID is recorded.
	h.seedLiveDaemon(d, api.StSpawning, 48008)

	// Tick 1: removed while StSpawning → mark pendingReap (StSpawning is
	// reapable). Tick 2: confirmed absent → reapRemovedDaemon drives
	// StSpawning + EvIntentUpdate(stopped) → "set queued_action=stop" (NO
	// terminate yet — deferred to the spawn-completion event).
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())

	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("a StSpawning reap must DEFER the terminate to the spawn-completion event, not terminate immediately; terminate calls = %d", got)
	}
	// The queued stop + the entry + the shadow must all SURVIVE so the
	// completion event can apply the stop.
	if v, ok := h.ctrl.queuedActions.Load(d.TaskName); !ok || v.(string) != "stop" {
		t.Fatalf("StSpawning reap must record + preserve queued_action=stop; got %v ok=%v", v, ok)
	}
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); !ok {
		t.Fatalf("StSpawning reap must keep the descriptor in the shadow so the spawn-completion event routes")
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StSpawning {
		t.Fatalf("StSpawning reap must stay StSpawning until the completion event; got state=%v ok=%v", st, ok)
	}

	// The spawn completes: EvHealthOK arrives. With the queued stop set, the SM
	// routes StSpawning + EvHealthOK → StExiting (issue terminate) — the
	// just-spawned child is terminated, NOT left orphaned. The event routes via
	// the reaping shadow (descriptor gone from intentCache).
	h.postEvent(api.LoopEvent{Kind: api.EvHealthOK, TaskName: d.TaskName})

	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("the spawn-completion event must apply the queued stop and terminate the child exactly once; terminate calls = %d", got)
	}
	select {
	case pid := <-h.terminatedPIDs:
		if pid != 48008 {
			t.Fatalf("queued-stop terminate hit the wrong PID: got %d, want 48008", pid)
		}
	default:
		t.Fatalf("queued-stop terminate did not record a target PID")
	}
}

// TestSupervisorController_OrphanReap_BackoffTimerDueDuringGapRetries is the
// FINDING 5 + FINDING E control, reconciled. A StBackoffWaiting task removed from
// intent has NO live child. Two invariants must BOTH hold while the removal is
// unconfirmed (the verification window):
//
//   - Finding E (anti-orphan): the daemon's existing backoff timer firing
//     EvTimerDue during the window MUST NOT respawn the old descriptor — that
//     would bind a port the user just removed (a transient orphan / port-collision
//     with a replacement daemon). The respawn is SUPPRESSED; the daemon stays
//     StBackoffWaiting; the suppressed timer is remembered.
//
//   - Finding 5 (anti-stranding): if the removal turns out to be a replace-in-
//     place BLIP (descriptor reappears), the suppressed timer is REPLAYED so the
//     reappeared daemon respawns and is NOT stranded in backoff forever (the
//     IntentWatcher posts no EvIntentUpdate for an unchanged-Desired re-add and
//     does not run Reconcile, so without the replay nothing would restart it).
//
// The earlier r1 form of this test asserted the OPPOSITE of finding E (spawn
// immediately during the window). Per the converged review (codex + consultant)
// that was the bug finding E identifies; this rewrite asserts the corrected
// behavior, which is STRICTLY STRONGER (it also closes the orphan-port window)
// while preserving the original anti-stranding invariant via the reappear replay.
func TestSupervisorController_OrphanReap_BackoffTimerDueDuringGapRetries(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	// StBackoffWaiting: a respawn is pending; backoff holds no live PID.
	h.seedLiveDaemon(d, api.StBackoffWaiting, 0)
	h.tracker.MarkBackoff(d.TaskName)

	// Tick 1: removed → marked pendingReap, descriptor in the shadow.
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); !ok {
		t.Fatalf("backoff reap did not keep the descriptor in the shadow")
	}

	// Finding E: the armed backoff timer fires DURING the gap. The respawn must be
	// SUPPRESSED (no spawn) and the daemon must stay StBackoffWaiting.
	spawnBefore := h.spawnCalls.Load()
	h.postEvent(api.LoopEvent{Kind: api.EvTimerDue, TaskName: d.TaskName})

	if got := h.spawnCalls.Load(); got != spawnBefore {
		t.Fatalf("finding E: EvTimerDue for a REMOVED backoff daemon must NOT respawn the old descriptor during the verification window (orphan port re-bind); spawn delta = %d, want 0", got-spawnBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StBackoffWaiting {
		t.Fatalf("finding E: a suppressed backoff EvTimerDue must leave the daemon in StBackoffWaiting (no transition); got state=%v ok=%v", st, ok)
	}

	// Finding 5 (anti-stranding): the removal was a BLIP — the descriptor
	// reappears. The suppressed timer must be REPLAYED so the daemon respawns and
	// is not stranded. The reappear routes through handleReapScan, which refreshes
	// the cache to the re-added descriptor, replays the deferred EvTimerDue, then
	// cancels the reap.
	reappeared := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.setFreshIntent(reappeared)
	h.refresh(reappeared)

	if got := h.spawnCalls.Load(); got != spawnBefore+1 {
		t.Fatalf("finding 5 anti-stranding: a reappeared backoff daemon must REPLAY the suppressed timer and respawn exactly once; spawn delta = %d, want 1", got-spawnBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st == api.StBackoffWaiting {
		t.Fatalf("finding 5 anti-stranding: after reappear+replay the daemon must LEAVE backoff (respawn in flight or healthy), not stay stuck; got state=%v ok=%v", st, ok)
	}
	// The reap must be fully canceled — no stale pendingReap/shadow/deferred marker.
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("reappear must cancel the reap; stale pendingReap left")
	}
	if _, ok := h.ctrl.reapDeferredTimerDue.Load(d.TaskName); ok {
		t.Fatalf("reappear replay must consume the deferred-timer marker; it leaked")
	}
}

func reapTestDescriptorB() api.SupervisorDaemon {
	return api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-lsp-cafef00d-go`,
		Server:   "mcp-language-server",
		Daemon:   "lsp-cafef00d-go",
		Command:  "mcphub",
		Args:     []string{"daemon", "workspace-proxy"},
	}
}

func intentWith(daemons ...api.SupervisorDaemon) *api.SupervisorIntentFile {
	return &api.SupervisorIntentFile{Version: 1, Daemons: daemons}
}

// TestSupervisorController_OrphanReap_FollowupReReadsDiskNotStaleCache is the
// FINDING A control. The follow-up tick must confirm absence against a FRESH
// on-disk read of supervisor-intent.json, NOT the possibly-stale intentCache.
// The intentCache only refreshes on the 60s IntentWatcher poll, so after a
// remove → re-add-on-disk WITHIN the window the cache stays empty (showing the
// daemon removed) while disk shows it re-declared. A cache-only follow-up would
// TERMINATE the live re-declared daemon.
//
// Pre-fix the follow-up resolved against the cache; this test models a cache
// that still says "removed" while the fresh disk read says "present", and
// asserts NO terminate (the follow-up trusts disk and cancels).
func TestSupervisorController_OrphanReap_FollowupReReadsDiskNotStaleCache(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 51001)

	// Remove from intent → mark pendingReap. The intentCache now shows the
	// daemon GONE (refresh(empty) swapped it out).
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("removal did not mark pendingReap")
	}
	if _, cached := h.ctrl.intentCache.Lookup(d.TaskName); cached {
		t.Fatalf("precondition: intentCache should NOT hold the removed descriptor")
	}

	// The operator RE-ADDED the descriptor on disk WITHIN the window. The
	// intentCache has NOT yet been refreshed (no IntentWatcher poll), so it still
	// shows the daemon gone — but the fresh disk read sees the re-add.
	h.setFreshIntent(intentWith(d))

	// Fire the follow-up. A cache-only follow-up would see "absent" and TERMINATE
	// the live re-declared daemon. The disk-reading follow-up sees "present" and
	// cancels.
	if !h.fireReapFollowup() {
		t.Fatalf("removal must arm a follow-up tick")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("finding A: follow-up must re-read intent FROM DISK and cancel on the re-add, NOT terminate a live re-declared daemon (cache-only would kill it); terminate calls = %d", got)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("finding A: disk re-read showing the re-add must cancel the pendingReap")
	}
	// The daemon's SM state must be PRESERVED (it is live and re-declared).
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("finding A: a re-declared daemon must keep running; got state=%v ok=%v", st, ok)
	}
}

// TestSupervisorController_OrphanReap_FollowupScopedToOwnTaskNotSiblings is the
// FINDING B control. A follow-up timer fired for task A must resolve ONLY task A.
// It must NOT confirm+terminate a SIBLING task B that is also momentarily absent
// — B has its own verification window and its own follow-up timer.
//
// Pre-fix the follow-up resolved EVERY pendingReap entry, so A's timer firing
// could terminate B without B's own window elapsing.
func TestSupervisorController_OrphanReap_FollowupScopedToOwnTaskNotSiblings(t *testing.T) {
	h := newReapTestHarness(t)
	a := reapTestDescriptor()
	b := reapTestDescriptorB()
	h.ctrl.intentCache.Refresh(intentWith(a, b))
	h.ctrl.smStates.Store(a.TaskName, api.StRunning)
	h.ctrl.smStates.Store(b.TaskName, api.StRunning)
	h.tracker.MarkSpawned(a.TaskName, 52001, time.Now().UTC())
	h.tracker.MarkSpawned(b.TaskName, 52002, time.Now().UTC())

	// BOTH disappear in the same refresh → both marked pendingReap (two timers
	// armed, FIFO: A first, then B).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(a.TaskName); !ok {
		t.Fatalf("task A not marked pendingReap")
	}
	if _, ok := h.ctrl.pendingReap.Load(b.TaskName); !ok {
		t.Fatalf("task B not marked pendingReap")
	}

	// Fire exactly ONE follow-up timer (one task's). It must terminate EXACTLY
	// that one task and leave the OTHER task's pendingReap UNTOUCHED — the sibling
	// has its own verification window. The arm order of A vs B is non-deterministic
	// (Go map range), so the test is order-agnostic: it identifies the fired task
	// by the recorded terminate PID and asserts the OTHER survives.
	if !h.fireReapFollowup() {
		t.Fatalf("no follow-up armed")
	}
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("finding B: one task's follow-up must terminate EXACTLY ONE task, not also its sibling; terminate calls = %d", got)
	}
	var firedPID int
	select {
	case firedPID = <-h.terminatedPIDs:
	default:
		t.Fatalf("finding B: the follow-up did not record a terminate target")
	}
	// Map the fired PID back to the task; assert the OTHER task's reap survived.
	var firedTask, survivorTask string
	switch firedPID {
	case 52001:
		firedTask, survivorTask = a.TaskName, b.TaskName
	case 52002:
		firedTask, survivorTask = b.TaskName, a.TaskName
	default:
		t.Fatalf("finding B: follow-up terminated an unexpected PID %d (neither task A=52001 nor B=52002)", firedPID)
	}
	if _, ok := h.ctrl.pendingReap.Load(survivorTask); !ok {
		t.Fatalf("finding B: firing %s's follow-up must NOT resolve the sibling %s — its pendingReap must SURVIVE (its own window has not elapsed)", firedTask, survivorTask)
	}
	if _, ok := h.ctrl.GetSMState(survivorTask); !ok {
		t.Fatalf("finding B: the sibling %s's SM state must be untouched by %s's follow-up", survivorTask, firedTask)
	}
}

// TestSupervisorController_OrphanReap_StaleFollowupGenerationIsNoOp is the
// FINDING C control. After a remove → reappear → remove-again for the SAME task,
// the FIRST removal's follow-up timer (generation 1) must be a NO-OP when it
// fires — it must NOT confirm/terminate the SECOND removal (generation 2) early,
// bypassing the second removal's own verification window.
//
// Pre-fix the follow-up was not generation-tagged and the old time.AfterFunc was
// not Stopped on disarm, so the first timer firing during the second window
// confirmed the second removal early.
func TestSupervisorController_OrphanReap_StaleFollowupGenerationIsNoOp(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 53001)

	// Remove #1 → pendingReap generation 1, timer1 armed.
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	gen1 := h.ctrl.currentReapGeneration(d.TaskName)
	if gen1 == 0 {
		t.Fatalf("remove #1 did not open a reap generation")
	}

	// Reappear → cancel reap (the descriptor is back). The cache is refreshed to
	// the re-add by handleReapScan.
	h.setFreshIntent(intentWith(d))
	h.refresh(intentWith(d))
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("reappear must cancel remove #1's pendingReap")
	}

	// Remove #2 → pendingReap generation 2, timer2 armed (a DISTINCT generation).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	gen2 := h.ctrl.currentReapGeneration(d.TaskName)
	if gen2 == gen1 {
		t.Fatalf("remove #2 must open a NEW reap generation distinct from remove #1 (got %d == %d)", gen2, gen1)
	}

	// Now fire the FIRST (stale, generation-1) timer. It must be a NO-OP: it must
	// NOT terminate the generation-2 removal (which is only one tick into its OWN
	// verification window). FIFO firing returns timer1 first.
	termBefore := h.terminateCalls.Load()
	if !h.fireReapFollowup() {
		t.Fatalf("expected a pending follow-up timer to fire")
	}
	if got := h.terminateCalls.Load(); got != termBefore {
		t.Fatalf("finding C: the STALE generation-1 follow-up must be a no-op and NOT terminate the generation-2 removal early; terminate delta = %d, want 0", got-termBefore)
	}
	// The generation-2 pendingReap must SURVIVE (its window has not been confirmed).
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("finding C: a stale-generation follow-up must NOT resolve the live generation-2 reap")
	}

	// Now fire the generation-2 timer — THAT one confirms + terminates.
	if !h.fireReapFollowup() {
		t.Fatalf("generation-2 follow-up timer must be armed")
	}
	if got := h.terminateCalls.Load(); got != termBefore+1 {
		t.Fatalf("finding C: the generation-2 follow-up must terminate the second removal exactly once; terminate delta = %d, want 1", got-termBefore)
	}
}

// TestSupervisorController_OrphanReap_SpawningReappearClearsDeferredStop is the
// FINDING D control. A task removed while StSpawning records queued_action=stop
// (the terminate is deferred to the spawn-completion event). If the descriptor
// REAPPEARS before the spawn completes (replace-in-place), the reap-originated
// queued stop MUST be cleared — otherwise the next EvHealthOK drives the re-added
// daemon (which the operator wants RUNNING) to StExiting → terminate.
//
// Pre-fix the cancel path dropped only the marker/shadow; the queued stop
// survived and the spawn-completion event killed the re-added daemon.
func TestSupervisorController_OrphanReap_SpawningReappearClearsDeferredStop(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StSpawning, 54001)

	// Tick 1 + Tick 2: removed while StSpawning → reap drives StSpawning +
	// EvIntentUpdate(stopped) → queued_action=stop (deferred).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if v, ok := h.ctrl.queuedActions.Load(d.TaskName); !ok || v.(string) != "stop" {
		t.Fatalf("precondition: StSpawning reap must record queued_action=stop; got %v ok=%v", v, ok)
	}
	if _, ok := h.ctrl.reapDeferredStop.Load(d.TaskName); !ok {
		t.Fatalf("precondition: StSpawning reap must mark the stop as reap-originated (finding D marker)")
	}

	// The descriptor REAPPEARS before the spawn completes (replace-in-place). The
	// reap-originated queued stop must be cleared.
	reappeared := intentWith(d)
	h.setFreshIntent(reappeared)
	h.refresh(reappeared)
	if v, ok := h.ctrl.queuedActions.Load(d.TaskName); ok && v.(string) == "stop" {
		t.Fatalf("finding D: a reap-originated queued_action=stop must be CLEARED when the descriptor reappears before the spawn completes; it survived as %q", v)
	}

	// The spawn completes: EvHealthOK arrives. With the queued stop cleared, the
	// re-added daemon must go to StRunning, NOT be terminated.
	termBefore := h.terminateCalls.Load()
	h.postEvent(api.LoopEvent{Kind: api.EvHealthOK, TaskName: d.TaskName})
	if got := h.terminateCalls.Load(); got != termBefore {
		t.Fatalf("finding D: the spawn-completion event must NOT terminate the re-added daemon (the reap stop was cleared on reappear); terminate delta = %d, want 0", got-termBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("finding D: the re-added daemon must reach StRunning after EvHealthOK, not be killed; got state=%v ok=%v", st, ok)
	}
}

// TestSupervisorController_OrphanReap_TerminateAlreadyGoneClearsNotLoops is the
// FINDING F control. When the production TerminateFunc returns an error whose
// cause proves the process is ALREADY GONE (no running PID; or the kill succeeded
// but a post-kill persist failed — both wrapped with errTerminateTargetGone), the
// reap must classify it CONFIRMED-DEAD and CLEAR the bookkeeping — NOT preserve
// the entry + pendingReap forever (which would loop and leave a later
// re-registration stuck at stale StRunning-no-PID).
//
// Pre-fix every non-nil terminate error was treated as "may still be alive" and
// the reap preserved + retried forever.
func TestSupervisorController_OrphanReap_TerminateAlreadyGoneClearsNotLoops(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 55001)

	// Terminate returns a "gone" error (errTerminateTargetGone-wrapped). The fake
	// does NOT MarkExited (mirrors the no-PID path where there is nothing to mark),
	// so only the reap's own classification can clear the entry.
	h.setTerminateErr(fmt.Errorf("%w: no running PID recorded", errTerminateTargetGone))

	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent()) // tick 1: mark
	h.refresh(emptyReapIntent()) // tick 2: confirmed → terminate returns gone-error

	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("confirmed reap must attempt terminate once; terminate calls = %d", got)
	}
	// Finding F: a "gone" error is CONFIRMED-DEAD → bookkeeping cleared, NOT
	// preserved-for-retry.
	if _, ok := h.ctrl.GetSMState(d.TaskName); ok {
		t.Fatalf("finding F: an already-gone terminate error must CLEAR the SM entry (confirmed-dead), not preserve it for a retry loop")
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("finding F: an already-gone terminate error must CLEAR pendingReap, not keep retrying forever")
	}
	if entry, ok := h.tracker.Get(d.TaskName); ok {
		t.Fatalf("finding F: an already-gone terminate error must clear the tracker row: %+v", entry)
	}
}

// TestSupervisorController_OrphanReap_OwnSpawnedDefersClearUntilReaper is the
// FINDING G control. For an OWN-spawned daemon (its cmd.Wait reaper is still
// outstanding), the confirmed reap terminates the child but must DEFER the
// tracker/shadow clear until the daemon's OWN reaper posts the real EvChildExit.
// Clearing immediately would let a late MarkExited resurrect a stale idle tracker
// row (removal not durable) AND orphan-drop the real EvChildExit against a missing
// shadow.
//
// This test marks the daemon own-spawned + reaper-outstanding, confirms the reap
// (which must NOT clear yet), then delivers the real EvChildExit (routed via the
// surviving shadow) and a follow-up tick, after which the bookkeeping clears.
func TestSupervisorController_OrphanReap_OwnSpawnedDefersClearUntilReaper(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 56001)
	// Mark own-spawned with a live reaper outstanding (as executeSideEffect's
	// spawn-success branch would).
	h.ctrl.ownSpawned.Store(d.TaskName, true)
	h.ctrl.reaperOutstanding.Store(d.TaskName, true)

	// Tick 1 + Tick 2: confirmed removal → reap drives terminate. Because the task
	// is own-spawned with a reaper outstanding, the clear is DEFERRED.
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())

	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("confirmed reap must terminate the own-spawned child once; terminate calls = %d", got)
	}
	// Finding G: the tracker row + shadow must SURVIVE (deferred) so the real
	// EvChildExit routes and a late MarkExited cannot resurrect a stale row.
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); !ok {
		t.Fatalf("finding G: own-spawned reap must KEEP the shadow until the real EvChildExit arrives, not drop it immediately")
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("finding G: own-spawned reap must DEFER (keep pendingReap) until the reaper posts the real exit")
	}
	if _, ok := h.tracker.Get(d.TaskName); !ok {
		t.Fatalf("finding G: own-spawned reap must NOT remove the tracker row before the reaper runs (a late MarkExited would resurrect it)")
	}

	// The daemon's own reaper runs LAST: MarkExited (idle, pid 0) then posts the
	// real EvChildExit. The exit routes via the surviving shadow (StExiting +
	// EvChildExit -> StIdle).
	h.tracker.MarkExited(d.TaskName)
	h.postEvent(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})

	// A follow-up tick now confirms the task settled (StIdle, non-reapable) and
	// clears the bookkeeping — AFTER MarkExited ran, so the clear is durable.
	h.setFreshIntent(emptyReapIntent())
	if !h.fireReapFollowup() {
		t.Fatalf("finding G: a deferred own-spawned reap must keep a follow-up armed to finish the clear")
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("finding G: after the real EvChildExit settled the task, the follow-up must clear pendingReap")
	}
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); ok {
		t.Fatalf("finding G: after settle the shadow must be dropped")
	}
	if entry, ok := h.tracker.Get(d.TaskName); ok {
		t.Fatalf("finding G: after the reaper ran + settle, the tracker row must be cleared durably: %+v", entry)
	}
}

// stopsFor builds a unified-stops snapshot declaring `taskName` stopped with a
// non-expiring idle reason — used by the re-add-with-Desired=stopped test (d).
func stopsFor(taskName string) *api.DaemonIntentFile {
	return &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		canonicalSupervisorTaskName(taskName): {
			Desired:   api.IntentDesiredStopped,
			Reason:    api.IntentReasonIdle,
			UpdatedAt: time.Now().UTC(),
		},
	}}
}

// (a) TestSupervisorController_OrphanReap_R4_ChildExitQueuedBeforeRemovalNotDropped
// is the ROOT-FIX (#564/#535) falsifier for the on-loop cache swap. An EvChildExit
// is QUEUED on the loop BEFORE the removal's evReapScan. Pre-r4 the cache was
// swapped OFF the loop the instant refreshSupervisorIntent ran, so the already-queued
// EvChildExit drained against the EMPTY cache with NO shadow → orphan-dropped → the
// SM wedged at stale StRunning. Post-r4 the swap runs ON the loop inside
// handleReapScan with the shadow installed BEFORE the swap, so the queued
// EvChildExit routes (whichever order it drains in): it must reach StBackoffWaiting
// (StRunning + EvChildExit), never be orphan-dropped.
func TestSupervisorController_OrphanReap_R4_ChildExitQueuedBeforeRemovalNotDropped(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 61001)

	// Queue the child's crash exit FIRST (no sync), then the removal refresh (no
	// sync), then drain both with a single sync. The FIFO order is EvChildExit,
	// evReapScan — but the invariant must hold regardless: the exit routes.
	h.postNoSync(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})
	h.refreshNoSync(emptyReapIntent())
	h.sync()

	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StBackoffWaiting {
		t.Fatalf("#564/#535: an EvChildExit queued before the removal scan must route via the shadow to StBackoffWaiting, NOT be orphan-dropped leaving a stale StRunning; got state=%v ok=%v", st, ok)
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("#564/#535: the queued exit must not have triggered a terminate; terminate calls = %d", got)
	}
}

// (b) TestSupervisorController_OrphanReap_R4_ScanAbsentThenReaddedBeforeHandlingNotTerminated
// is the #614 falsifier. A removal evReapScan is posted (carrying the post-time
// snapshot), but BEFORE it is handled the descriptor is re-added to the cache (a
// prior on-loop event applied the re-add). handleReapScan must re-diff against the
// CURRENT cache at handling time and NOT terminate / NOT mark pendingReap — the task
// is present now.
func TestSupervisorController_OrphanReap_R4_ScanAbsentThenReaddedBeforeHandlingNotTerminated(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 62001)

	// Post the removal scan WITHOUT syncing. Then, before draining, re-add the
	// descriptor to the cache as a prior on-loop event would have. The scan's body
	// still carries the post-time "absent" snapshot, but handleReapScan recomputes
	// next from the FRESH intent in the body AND re-diffs against the current cache.
	// Model the on-disk re-add by posting a SECOND scan (the re-add) so the absent
	// scan, when handled, sees the descriptor present in the union/current cache and
	// the re-add scan restores it. The robust assertion: after both drain, no
	// terminate fired and the daemon is still running.
	readded := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.refreshNoSync(emptyReapIntent()) // scan #1: removal
	h.refreshNoSync(readded)           // scan #2: re-add (lands before #1's reap could confirm)
	h.sync()

	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("#614: a descriptor re-added before the removal scan confirms must NOT be terminated; terminate calls = %d", got)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("#614: the re-added daemon must keep running; got state=%v ok=%v", st, ok)
	}
	if _, ok := h.ctrl.intentCache.Lookup(d.TaskName); !ok {
		t.Fatalf("#614: the re-add must leave the descriptor in the cache")
	}
}

// (c) TestSupervisorController_OrphanReap_R4_SnapshotReaddsAAndFirstRemovesB is the
// #826-rescan falsifier. ONE fresh snapshot simultaneously re-adds task A (resolving
// A's pending reap) AND first-removes task B. handleReapScan must run the FULL diff
// for ALL tasks in the snapshot, so B gets its shadow + pendingReap installed — the
// reappear path must not blind-replace the cache and lose B's first removal.
func TestSupervisorController_OrphanReap_R4_SnapshotReaddsAAndFirstRemovesB(t *testing.T) {
	h := newReapTestHarness(t)
	a := reapTestDescriptor()
	b := reapTestDescriptorB()
	// Both A and B start live and present.
	h.ctrl.intentCache.Refresh(intentWith(a, b))
	h.ctrl.smStates.Store(a.TaskName, api.StRunning)
	h.ctrl.smStates.Store(b.TaskName, api.StRunning)
	h.tracker.MarkSpawned(a.TaskName, 63001, time.Now().UTC())
	h.tracker.MarkSpawned(b.TaskName, 63002, time.Now().UTC())

	// Scan 1: A disappears (B still present) → A marked pendingReap.
	h.refresh(intentWith(b))
	if _, ok := h.ctrl.pendingReap.Load(a.TaskName); !ok {
		t.Fatalf("scan-1 must mark A pendingReap")
	}

	// Scan 2: ONE snapshot re-adds A AND first-removes B. The reappear of A must NOT
	// blind-replace the cache and lose B's first removal: B must get shadow +
	// pendingReap installed in the same scan.
	h.refresh(intentWith(a))

	if _, ok := h.ctrl.pendingReap.Load(a.TaskName); ok {
		t.Fatalf("#826-rescan: A's reappear must cancel A's pendingReap")
	}
	if _, ok := h.ctrl.pendingReap.Load(b.TaskName); !ok {
		t.Fatalf("#826-rescan: the same snapshot that re-added A must FIRST-REMOVE B → B must be marked pendingReap (the reappear path must run the full diff, not blind-replace the cache)")
	}
	if _, ok := h.ctrl.reapShadow.Load(b.TaskName); !ok {
		t.Fatalf("#826-rescan: B's first removal must install B's reaping shadow in the same scan")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("#826-rescan: neither A (reappeared) nor B (first removal, verification window) may terminate yet; terminate calls = %d", got)
	}
}

// (d) TestSupervisorController_OrphanReap_R4_ReaddWithDesiredStoppedUpdatesStopsCache
// is the #826/#829 falsifier. When a descriptor is re-added with Desired=stopped, the
// stops cache must be swapped from the SAME fresh snapshot so a replayed
// EvTimerDue/EvHealthOK treats it as stopped (not default-running). Pre-r4 the stops
// cache was swapped on a SEPARATE off-loop path that could lag the reap scan.
func TestSupervisorController_OrphanReap_R4_ReaddWithDesiredStoppedUpdatesStopsCache(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 64001)

	// Scan 1: removed → pendingReap.
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("scan-1 must mark pendingReap")
	}

	// Scan 2: re-added WITH Desired=stopped, carrying the matching stops snapshot. The
	// on-loop handleReapScan swaps BOTH caches; the stops cache must now report the
	// task stopped.
	readded := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.refreshWithStops(readded, stopsFor(d.TaskName))

	di := h.ctrl.daemonIntent.Lookup(d.TaskName)
	if di.Desired != api.IntentDesiredStopped {
		t.Fatalf("#826/#829: a re-add with Desired=stopped must update the stops cache from the same snapshot; cache desired=%q want %q", di.Desired, api.IntentDesiredStopped)
	}
	if stop, _ := di.IsActiveStop(time.Now().UTC()); !stop {
		t.Fatalf("#826/#829: the swapped stops cache must report the re-added task as an active stop")
	}
}

// (e) TestSupervisorController_OrphanReap_R4_FollowupFreshReadErrorPreservesReap is
// the #856 falsifier. When the follow-up tick's fresh on-disk read FAILS, the reap
// must NOT fall back to the (emptied) cache to confirm absence and terminate — it
// must PRESERVE pendingReap + re-arm and retry. Pre-#856 a transient read glitch
// confirmed absence against the emptied cache and terminated a possibly-re-declared
// daemon.
func TestSupervisorController_OrphanReap_R4_FollowupFreshReadErrorPreservesReap(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 65001)

	// Remove → mark pendingReap, arm a follow-up.
	h.refresh(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("removal did not mark pendingReap")
	}

	// Make the follow-up's fresh on-disk read FAIL.
	h.setReapIntentReadErr(errors.New("simulated transient supervisor-intent.json read error"))

	if !h.fireReapFollowup() {
		t.Fatalf("removal must arm a follow-up tick")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("#856: a follow-up fresh-read FAILURE must NOT confirm absence against the emptied cache and terminate; terminate calls = %d", got)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("#856: a follow-up fresh-read FAILURE must PRESERVE pendingReap for retry")
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StRunning {
		t.Fatalf("#856: the daemon's SM state must be preserved across a transient read glitch; got state=%v ok=%v", st, ok)
	}

	// The retry: the read recovers and still shows the daemon gone → NOW it terminates.
	h.setReapIntentReadErr(nil)
	h.setFreshIntent(emptyReapIntent())
	if !h.fireReapFollowup() {
		t.Fatalf("#856: a fresh-read failure must keep a follow-up armed for the retry")
	}
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("#856: the recovered retry tick must finally terminate the confirmed orphan exactly once; terminate calls = %d", got)
	}
}

// (f) TestSupervisorController_OrphanReap_R4_FiredTimerTerminateFailureReArms is the
// #966/#967 falsifier. After a follow-up timer FIRES and the terminate FAILS
// (process may still be alive), resolveConfirmedReap calls armReapFollowup to retry.
// Pre-#966/#967 the fired timer's armed-flag still held the same generation, so the
// re-arm was a no-op and the transient terminate failure was NEVER retried. Post-fix
// the fired-timer bookkeeping is cleared when the timer fires, so the retry actually
// schedules a NEW follow-up timer.
func TestSupervisorController_OrphanReap_R4_FiredTimerTerminateFailureReArms(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 66001)

	// Remove → mark pendingReap (timer #1 armed).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if h.pendingFollowupCount() != 1 {
		t.Fatalf("removal must arm exactly one follow-up; armed=%d", h.pendingFollowupCount())
	}

	// Make the terminate FAIL (non-gone → reapTerminateFailed → must re-arm).
	h.setTerminateErr(errors.New("simulated terminate failure (process may still be alive)"))

	// Fire timer #1: confirms absence (fresh read empty), terminate fails. The fix
	// must re-arm a NEW follow-up timer for the retry.
	if !h.fireReapFollowup() {
		t.Fatalf("timer #1 must be armed")
	}
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("#966/#967: the fired timer must attempt the terminate once; terminate calls = %d", got)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("#966/#967: a failed terminate must preserve pendingReap")
	}
	if h.pendingFollowupCount() != 1 {
		t.Fatalf("#966/#967: after a fired timer + terminate failure, a NEW retry timer must be armed (pre-fix the re-arm no-op'd and left 0); armed=%d", h.pendingFollowupCount())
	}

	// The retry timer fires with the terminate now succeeding → orphan reaped.
	h.setTerminateErr(nil)
	if !h.fireReapFollowup() {
		t.Fatalf("#966/#967: the re-armed retry timer must be fireable")
	}
	if got := h.terminateCalls.Load(); got != 2 {
		t.Fatalf("#966/#967: the retry must re-attempt the terminate; total terminate calls = %d, want 2", got)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("#966/#967: the successful retry must clear pendingReap")
	}
}

// (h) TestSupervisorController_OrphanReap_R4_OwnSpawnedReaddBeforeExitQueuesRespawn is
// the #625 falsifier. An own-spawned daemon confirmed absent is driven StRunning →
// StExiting (the terminate fires; the real cmd.Wait exit is awaited). If the
// descriptor is RE-ADDED (declared running) before that exit, the cancel path must
// QUEUE a respawn so the EvChildExit drives StExiting → StSpawning and the
// re-declared daemon comes back RUNNING. Pre-#625 the cancel only cleared reap
// markers; the EvChildExit went to StIdle (no respawn) and the daemon stayed stopped.
func TestSupervisorController_OrphanReap_R4_OwnSpawnedReaddBeforeExitQueuesRespawn(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 67001)
	// Own-spawned with a live reaper outstanding.
	h.ctrl.ownSpawned.Store(d.TaskName, true)
	h.ctrl.reaperOutstanding.Store(d.TaskName, true)

	// Scan 1 + Scan 2: confirmed removal → terminate fires → StExiting. Because the
	// task is own-spawned, the clear is DEFERRED (the real EvChildExit is awaited).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("confirmed reap must terminate the own-spawned child once; terminate calls = %d", got)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StExiting {
		t.Fatalf("after the reap terminate the own-spawned daemon must be StExiting awaiting its real exit; got state=%v ok=%v", st, ok)
	}

	// The descriptor is RE-ADDED (declared running) BEFORE the real cmd.Wait exit.
	readded := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.setFreshIntent(readded)
	h.refresh(readded)
	if v, ok := h.ctrl.queuedActions.Load(d.TaskName); !ok || v.(string) != "respawn" {
		t.Fatalf("#625: re-adding a running descriptor while the own-spawned daemon is mid-terminate must QUEUE a respawn (queued_action=respawn); got %v ok=%v", v, ok)
	}

	// The real cmd.Wait exit now arrives. With queued_action=respawn the SM drives
	// StExiting → StSpawning (consume the queued respawn) → the daemon comes back.
	spawnBefore := h.spawnCalls.Load()
	h.postEvent(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})
	if got := h.spawnCalls.Load(); got != spawnBefore+1 {
		t.Fatalf("#625: the EvChildExit must consume the queued respawn and re-spawn the re-declared daemon exactly once; spawn delta = %d, want 1", got-spawnBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || (st != api.StSpawning && st != api.StRunning) {
		t.Fatalf("#625: after the respawn the re-declared daemon must be live (StSpawning/StRunning), not stopped; got state=%v ok=%v", st, ok)
	}
}

// =====================================================================
// pr302 r5 — single-snapshot-applier: the FOLLOW-UP reappear path must
// mirror handleReapScan (it no longer runs a weaker raw-Refresh+cancel).
// =====================================================================

// (F1a) TestSupervisorController_OrphanReap_R5_FollowupReappearOwnSpawnedExitingQueuesRespawn
// is the #946 falsifier for the FOLLOW-UP (timer) reappear path. An own-spawned
// daemon confirmed absent is driven StRunning → StExiting (the terminate fired; the
// real cmd.Wait exit is awaited). If the descriptor is RE-ADDED (declared running) on
// disk and the reappear is observed by the FOLLOW-UP TICK (not a scan), the follow-up
// must route the fresh snapshot through handleReapScan so queueRespawnOnReapCancelIfNeeded
// fires and queues a respawn — exactly as the scan path does (#625).
//
// Pre-r5 the follow-up's reappear branch did a raw intentCache.Refresh(fresh) + ad-hoc
// cancelPendingReap and called NEITHER queueRespawnOnReapCancelIfNeeded NOR the stops
// swap, so the pending EvChildExit drove StExiting → StIdle (NO respawn) and the
// re-declared daemon stayed stopped. This test fires the reappear via the follow-up tick
// and asserts queued_action=respawn, then that the EvChildExit re-spawns the daemon.
func TestSupervisorController_OrphanReap_R5_FollowupReappearOwnSpawnedExitingQueuesRespawn(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 71001)
	h.ctrl.ownSpawned.Store(d.TaskName, true)
	h.ctrl.reaperOutstanding.Store(d.TaskName, true)

	// Scan 1 + Scan 2: confirmed removal → terminate fires → StExiting (own-spawned,
	// clear deferred until the real exit). The follow-up tick stays armed.
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("confirmed reap must terminate the own-spawned child once; terminate calls = %d", got)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StExiting {
		t.Fatalf("after the reap terminate the own-spawned daemon must be StExiting awaiting its real exit; got state=%v ok=%v", st, ok)
	}

	// The descriptor is RE-ADDED (declared running) on disk BEFORE the real exit. The
	// reappear is observed by the FOLLOW-UP TICK (not a scan): the cache is still empty
	// (no IntentWatcher poll), so a cache-only / raw-Refresh follow-up would miss the
	// respawn. Routed through handleReapScan, the StExiting reappear queues a respawn.
	readded := intentWith(d)
	h.setFreshIntent(readded)
	if !h.fireReapFollowup() {
		t.Fatalf("the deferred own-spawned reap must keep a follow-up armed")
	}
	if v, ok := h.ctrl.queuedActions.Load(d.TaskName); !ok || v.(string) != "respawn" {
		t.Fatalf("#946 (F1a): a FOLLOW-UP-observed reappear of a mid-terminate own-spawned daemon must route through handleReapScan and QUEUE a respawn (queued_action=respawn); got %v ok=%v", v, ok)
	}
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("#946 (F1a): the reappear must NOT terminate again; terminate calls = %d", got)
	}

	// The real cmd.Wait exit arrives → StExiting + queued respawn → StSpawning → live.
	spawnBefore := h.spawnCalls.Load()
	h.tracker.MarkExited(d.TaskName)
	h.postEvent(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})
	if got := h.spawnCalls.Load(); got != spawnBefore+1 {
		t.Fatalf("#946 (F1a): the EvChildExit must consume the queued respawn and re-spawn the re-declared daemon exactly once; spawn delta = %d, want 1", got-spawnBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st == api.StIdle {
		t.Fatalf("#946 (F1a): after the respawn the re-declared daemon must be live (NOT StIdle); got state=%v ok=%v", st, ok)
	}
}

// (F1b) TestSupervisorController_OrphanReap_R5_FollowupReappearDesiredStoppedRefreshesStopsCache
// is the #946 stops-half falsifier for the FOLLOW-UP reappear path. When the follow-up
// observes the task re-added WITH Desired=stopped, the stops cache must be swapped from
// the SAME fresh snapshot (via handleReapScan's both-cache swap) so the daemon STAYS
// stopped and no respawn is queued.
//
// Pre-r5 the follow-up did a raw intentCache.Refresh(fresh) that swapped ONLY the
// descriptor cache, never the stops cache, so a replayed timer/health event would treat
// the re-add as default-running. This test re-adds with Desired=stopped via the follow-up
// tick and asserts the stops cache now reports the task stopped + NO respawn queued.
func TestSupervisorController_OrphanReap_R5_FollowupReappearDesiredStoppedRefreshesStopsCache(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 72001)
	h.ctrl.ownSpawned.Store(d.TaskName, true)
	h.ctrl.reaperOutstanding.Store(d.TaskName, true)

	// Scan 1 + Scan 2: confirmed removal → terminate → StExiting (deferred).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StExiting {
		t.Fatalf("precondition: own-spawned reap must reach StExiting; got state=%v ok=%v", st, ok)
	}

	// Re-added WITH Desired=stopped, observed by the follow-up tick. The fresh on-disk
	// intent carries the matching stops sub-block (fresh.Stops), which handleReapScan
	// swaps into the stops cache.
	readded := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{d},
		Stops: map[string]api.DaemonIntent{
			canonicalSupervisorTaskName(d.TaskName): {
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonIdle,
				UpdatedAt: time.Now().UTC(),
			},
		},
	}
	h.setFreshIntent(readded)
	if !h.fireReapFollowup() {
		t.Fatalf("the deferred own-spawned reap must keep a follow-up armed")
	}

	di := h.ctrl.daemonIntent.Lookup(d.TaskName)
	if di.Desired != api.IntentDesiredStopped {
		t.Fatalf("#946 (F1b): a FOLLOW-UP-observed re-add with Desired=stopped must swap the stops cache from the SAME fresh snapshot; cache desired=%q want %q", di.Desired, api.IntentDesiredStopped)
	}
	if stop, _ := di.IsActiveStop(time.Now().UTC()); !stop {
		t.Fatalf("#946 (F1b): the swapped stops cache must report the re-added task as an active stop")
	}
	// A Desired=stopped re-add must NOT queue a respawn — it should stay stopped.
	if v, ok := h.ctrl.queuedActions.Load(d.TaskName); ok && v.(string) == "respawn" {
		t.Fatalf("#946 (F1b): a Desired=stopped re-add must NOT queue a respawn; queued_action=%q", v)
	}
}

// (F2) TestSupervisorController_OrphanReap_R5_FollowupReappearReaddsAFirstRemovesBInstallsSiblingShadow
// is the #942 falsifier for the FOLLOW-UP reappear path. A's follow-up fires and its
// FRESH on-disk read shows A re-added AND a DIFFERENT live task B removed in the SAME
// snapshot. Routing the fresh snapshot through handleReapScan installs B's shadow +
// pendingReap; a raw intentCache.Refresh(fresh) would drop B from the cache WITHOUT a
// shadow, so the later watcher pass would diff from a cache already missing B and never
// terminate it → orphaned daemon.
func TestSupervisorController_OrphanReap_R5_FollowupReappearReaddsAFirstRemovesBInstallsSiblingShadow(t *testing.T) {
	h := newReapTestHarness(t)
	a := reapTestDescriptor()
	b := reapTestDescriptorB()
	// Both A and B start live and present.
	h.ctrl.intentCache.Refresh(intentWith(a, b))
	h.ctrl.smStates.Store(a.TaskName, api.StRunning)
	h.ctrl.smStates.Store(b.TaskName, api.StRunning)
	h.tracker.MarkSpawned(a.TaskName, 73001, time.Now().UTC())
	h.tracker.MarkSpawned(b.TaskName, 73002, time.Now().UTC())

	// Scan 1: A disappears (B still present) → A marked pendingReap, A's follow-up armed.
	h.setFreshIntent(intentWith(b))
	h.refresh(intentWith(b))
	if _, ok := h.ctrl.pendingReap.Load(a.TaskName); !ok {
		t.Fatalf("scan-1 must mark A pendingReap")
	}

	// A's FOLLOW-UP fires and its fresh on-disk read shows ONE snapshot that re-adds A
	// AND first-removes B. The reappear of A must NOT blind-replace the cache and lose
	// B's first removal: routed through handleReapScan, B gets shadow + pendingReap.
	h.setFreshIntent(intentWith(a))
	if !h.fireReapFollowup() {
		t.Fatalf("A's removal must arm a follow-up tick")
	}

	if _, ok := h.ctrl.pendingReap.Load(a.TaskName); ok {
		t.Fatalf("#942 (F2): A's follow-up reappear must cancel A's pendingReap")
	}
	if _, ok := h.ctrl.pendingReap.Load(b.TaskName); !ok {
		t.Fatalf("#942 (F2): the fresh snapshot that re-added A also first-removed B → B must be marked pendingReap by the follow-up routing through handleReapScan (a raw Refresh would have orphaned B)")
	}
	if _, ok := h.ctrl.reapShadow.Load(b.TaskName); !ok {
		t.Fatalf("#942 (F2): B's first removal must install B's reaping shadow in the same follow-up scan")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("#942 (F2): neither A (reappeared) nor B (first removal, verification window) may terminate yet; terminate calls = %d", got)
	}
}

// (F3) TestSupervisorController_OrphanReap_R5_FollowupReappearIdleAfterReapPostsRespawn
// is the #748 falsifier. A confirmed reap kills an own-spawned daemon; its REAL
// EvChildExit is processed (StExiting → StIdle) BEFORE the descriptor is re-added. The
// task is now StIdle but pendingReap is still set (the deferred-clear follow-up had not
// run). When the re-add (declared running) is then observed, the reap-cancel path must
// POST an EvStart so StIdle → StSpawning — the watcher posts NO EvStart for a descriptor
// addition, so without this the re-declared daemon stays stopped until a manual restart.
//
// Pre-#748 the reappear handling early-returned at StIdle (queueRespawnOnReapCancelIfNeeded
// only handled StExiting) and cancelPendingReap dropped the only bookkeeping, leaving the
// re-declared running daemon permanently stopped. This test drives the idle-after-reap
// state, re-adds running, fires the follow-up, and asserts a respawn actually fires.
func TestSupervisorController_OrphanReap_R5_FollowupReappearIdleAfterReapPostsRespawn(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	h.seedLiveDaemon(d, api.StRunning, 74001)
	h.ctrl.ownSpawned.Store(d.TaskName, true)
	h.ctrl.reaperOutstanding.Store(d.TaskName, true)

	// Scan 1 + Scan 2: confirmed removal → terminate → StExiting (own-spawned deferred).
	h.setFreshIntent(emptyReapIntent())
	h.refresh(emptyReapIntent())
	h.refresh(emptyReapIntent())
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StExiting {
		t.Fatalf("precondition: own-spawned reap must reach StExiting; got state=%v ok=%v", st, ok)
	}

	// The REAL own-spawned EvChildExit arrives BEFORE the re-add: StExiting + EvChildExit
	// (no queued respawn) → StIdle. pendingReap SURVIVES (own-spawned deferred-clear).
	h.tracker.MarkExited(d.TaskName)
	h.postEvent(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StIdle {
		t.Fatalf("the reap-kill exit must settle the own-spawned daemon to StIdle; got state=%v ok=%v", st, ok)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("precondition (#748): pendingReap must SURVIVE the deferred own-spawned exit until the follow-up resolves it")
	}

	// NOW the descriptor is re-added (declared running) on disk, observed by the
	// follow-up tick. StIdle + pendingReap + re-added-running → the reap-cancel path
	// must POST EvStart so the daemon respawns (the watcher delivers no EvStart here).
	spawnBefore := h.spawnCalls.Load()
	readded := intentWith(d)
	h.setFreshIntent(readded)
	if !h.fireReapFollowup() {
		t.Fatalf("the deferred own-spawned reap must keep a follow-up armed")
	}
	if got := h.terminateCalls.Load(); got != 1 {
		t.Fatalf("#748: the idle-after-reap reappear must NOT terminate again; terminate calls = %d", got)
	}
	if got := h.spawnCalls.Load(); got != spawnBefore+1 {
		t.Fatalf("#748: an idle-after-reap re-add declared running must POST EvStart and re-spawn the re-declared daemon exactly once (pre-fix it stayed stopped forever); spawn delta = %d, want 1", got-spawnBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st == api.StIdle {
		t.Fatalf("#748: after the EvStart-driven respawn the re-declared daemon must be live (NOT StIdle); got state=%v ok=%v", st, ok)
	}
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
		t.Fatalf("#748: the reappear must cancel the pendingReap")
	}
}
