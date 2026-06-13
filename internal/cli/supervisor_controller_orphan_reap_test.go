package cli

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// reapTestHarness wires a minimal in-memory controller for the orphan-reap
// tests. NO real process is spawned or killed and NO real state dir is
// touched: spawn/terminate are recording fakes, the tracker is in-memory, and
// statePath points inside a hardened temp dir. The controller is driven by
// calling refreshSupervisorIntent directly (the production reap owner) — the
// event loop exists only so executeSideEffect's PostSelf calls have a sink,
// but Run is NOT started, so no event is processed asynchronously and the
// assertions observe a deterministic synchronous outcome.
type reapTestHarness struct {
	ctrl           *supervisorController
	tracker        *DaemonRuntimeTracker
	spawnCalls     *atomic.Int32
	terminateCalls *atomic.Int32
	terminatedPIDs chan int
	// terminateErr, when non-nil, makes the fake terminate return it WITHOUT
	// marking the tracker exited — modeling a FAILED terminate (PID query /
	// permission / escalation error; the process may still be alive). Used by
	// the finding-2 preserve-on-failure test.
	terminateErr error
	// fireReapFollowup fires the most-recently-armed follow-up tick callback
	// synchronously (the injected reapAfterFunc never uses the wall clock). It
	// returns false when no follow-up is currently armed.
	fireReapFollowup func() bool
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

	h := &reapTestHarness{
		tracker:        tracker,
		spawnCalls:     &spawnCalls,
		terminateCalls: &terminateCalls,
		terminatedPIDs: terminatedPIDs,
	}
	// fakeReapTimer is a controllable, wall-clock-free timer: arming captures
	// the callback so the test fires it deterministically via
	// h.fireReapFollowup(). A real time.AfterFunc would make the follow-up tick
	// nondeterministic (and on a fast machine the test could race the timer).
	var pendingFollowup func()
	h.fireReapFollowup = func() bool {
		f := pendingFollowup
		if f == nil {
			return false
		}
		pendingFollowup = nil
		f()
		return true
	}
	h.ctrl = &supervisorController{
		intentCache:  newIntentCache(),
		eventLoop:    api.NewEventLoop(16),
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
			if h.terminateErr != nil {
				// FAILED terminate: do NOT mark exited — the PID may still be
				// alive. The reap must preserve the entry for retry.
				return h.terminateErr
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
			pendingFollowup = f
			return noopReapTimer{}
		},
	}
	return h
}

// noopReapTimer satisfies the reapTimer interface for the test fake; the
// controller never Stops the handle (it disarms via the armed flag), so Stop
// is a no-op returning true.
type noopReapTimer struct{}

func (noopReapTimer) Stop() bool { return true }

// seedLiveDaemon installs `descriptor` into the intent cache and marks it live
// at `state` with a tracker PID, exactly as a steady-state running daemon
// would appear after a spawn.
func (h *reapTestHarness) seedLiveDaemon(descriptor api.SupervisorDaemon, state api.SMState, pid int) {
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}}
	h.ctrl.intentCache.Refresh(intent)
	h.ctrl.smStates.Store(descriptor.TaskName, state)
	if pid > 0 {
		h.tracker.MarkSpawned(descriptor.TaskName, pid, time.Now().UTC())
	}
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
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
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
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
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
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("tick-1 blip did not mark pendingReap")
	}

	// Tick 2: the re-add lands — descriptor PRESENT again → drop the mark, no
	// terminate. The daemon is preserved exactly as it was running.
	reappeared := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.ctrl.refreshSupervisorIntent(reappeared)

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
	h.ctrl.refreshSupervisorIntent(reappeared)
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
		h.ctrl.refreshSupervisorIntent(emptyReapIntent())
		if _, ok := h.ctrl.pendingReap.Load(d.TaskName); ok {
			t.Fatalf("StIdle removal must NOT be marked pendingReap (nothing live to terminate)")
		}
		// Tick 2: confirm no terminate ever fires.
		h.ctrl.refreshSupervisorIntent(emptyReapIntent())
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
		h.ctrl.refreshSupervisorIntent(emptyReapIntent())
		// Tick 2: confirmed absent → reapRemovedDaemon runs but observes
		// StExiting and returns without a second terminate.
		h.ctrl.refreshSupervisorIntent(emptyReapIntent())
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
		h.ctrl.refreshSupervisorIntent(present)
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
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
	if _, ok := h.ctrl.pendingReap.Load(d.TaskName); !ok {
		t.Fatalf("removal did not mark pendingReap")
	}
	if got := h.terminateCalls.Load(); got != 0 {
		t.Fatalf("first refresh must NOT terminate (verification window); terminate calls = %d", got)
	}

	// Intent now stays STABLE — no further refreshSupervisorIntent call will
	// ever happen. The self-armed follow-up tick is the ONLY thing that can
	// confirm + terminate. Fire it.
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
	h.terminateErr = errors.New("simulated terminate failure (PID query/permission error)")

	// Tick 1: mark pendingReap.
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
	// Tick 2: confirmed absent → terminate fires but FAILS.
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())

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
	// succeeding. The orphan is finally reaped and bookkeeping clears.
	h.terminateErr = nil
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
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); !ok {
		t.Fatalf("pendingReap window did not keep the descriptor in the reaping shadow (common-root fix)")
	}

	// The child crashes DURING the gap. Its real EvChildExit lands while the
	// descriptor is gone from intentCache. Without the shadow this would be
	// orphan-dropped; with it, the SM routes StRunning → StBackoffWaiting.
	h.ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvChildExit, TaskName: d.TaskName})
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StBackoffWaiting {
		t.Fatalf("EvChildExit during the absent window must route via the shadow to StBackoffWaiting (not a dropped event leaving a stale StRunning); got state=%v ok=%v", st, ok)
	}

	// Tick 2: the descriptor REAPPEARS (replace-in-place completed). The reap is
	// canceled; the daemon is already in backoff and will respawn — it was NOT
	// wrongly treated as a steady-state running daemon.
	reappeared := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{d}}
	h.ctrl.refreshSupervisorIntent(reappeared)

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
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())

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
	h.ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvHealthOK, TaskName: d.TaskName})

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
// FINDING 5 control: a StBackoffWaiting task whose armed backoff timer fires
// (EvTimerDue) DURING the absent window must still retry — its EvTimerDue must
// route, not be orphan-dropped, otherwise the daemon is stuck in backoff with
// no respawn ever fired.
//
// The reaping shadow keeps the descriptor routable, so EvTimerDue routes
// StBackoffWaiting + EvTimerDue → StSpawning (create-process) and the spawn
// re-fires against the shadow descriptor.
func TestSupervisorController_OrphanReap_BackoffTimerDueDuringGapRetries(t *testing.T) {
	h := newReapTestHarness(t)
	d := reapTestDescriptor()
	// StBackoffWaiting: a respawn is pending; backoff holds no live PID.
	h.seedLiveDaemon(d, api.StBackoffWaiting, 0)
	h.tracker.MarkBackoff(d.TaskName)

	// Tick 1: removed → marked pendingReap, descriptor in the shadow.
	h.ctrl.refreshSupervisorIntent(emptyReapIntent())
	if _, ok := h.ctrl.reapShadow.Load(d.TaskName); !ok {
		t.Fatalf("backoff reap did not keep the descriptor in the shadow")
	}

	// The armed backoff timer fires DURING the gap. Without the shadow its
	// EvTimerDue would be orphan-dropped and the daemon would never respawn.
	spawnBefore := h.spawnCalls.Load()
	h.ctrl.handleLoopEvent(api.LoopEvent{Kind: api.EvTimerDue, TaskName: d.TaskName})

	if got := h.spawnCalls.Load(); got != spawnBefore+1 {
		t.Fatalf("EvTimerDue during the absent window must route via the shadow and re-fire the spawn (finding 5); spawn delta = %d, want 1", got-spawnBefore)
	}
	if st, ok := h.ctrl.GetSMState(d.TaskName); !ok || st != api.StSpawning {
		t.Fatalf("backoff EvTimerDue must drive StBackoffWaiting → StSpawning, not stay stuck in backoff; got state=%v ok=%v", st, ok)
	}
}
