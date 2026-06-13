package cli

import (
	"context"
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
			// Production terminate returns nil when the targeted PID is gone
			// (already-dead / identity-mismatch / confirmed-terminated) and
			// marks the tracker exited. Mirror that nil-return contract so the
			// reap's clearRemovedTaskRuntime sees a consistent entry.
			tracker.MarkExited(d.TaskName)
			terminateCalls.Add(1)
			select {
			case terminatedPIDs <- entry.CurrentPID:
			default:
			}
			return nil
		},
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	return h
}

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
