package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

// F1 (pre-spawn port-owner gate) + F3 (quarantine self-heal sweep) — two NEW
// automatic triggers of the identity-gated port-squatter reap (decision D-A).
// After the codex-P1 fix F1 is SPLIT: the controller LOOP runs only a fast,
// deadline-bounded owner probe; an off-loop worker (runPortGateWorker, one
// goroutine, sole owner of squatterLimiter) runs the blocking classify+reap and
// maps the outcome back via EvManualRestart (+ the gateCleared one-shot flag).
// These tests reuse the shared classifier fixtures (globalDaemonDescriptor,
// serenaProxyDescriptor, setSquatterLookupForTest, squatterIdentityFor,
// alwaysExeMatch) from supervise_squatter_test.go — same package.

// setSquatterTerminateForTest swaps the kill primitive squatterTerminatePIDFn
// (shared by the F1 worker, F3, and the `daemon recover` verb) with a recorder
// so no real process is killed.
func setSquatterTerminateForTest(t *testing.T, fn func(process.PIDIdentityProof) error) {
	t.Helper()
	prev := squatterTerminatePIDFn
	squatterTerminatePIDFn = fn
	t.Cleanup(func() { squatterTerminatePIDFn = prev })
}

func setSelfPIDForTest(t *testing.T, pid int) {
	t.Helper()
	prev := supervisorSelfPIDFn
	supervisorSelfPIDFn = func() int { return pid }
	t.Cleanup(func() { supervisorSelfPIDFn = prev })
}

// f1Controller builds a controller wired for the FULL F1 gate — the loop probe
// (portOwnerFn) AND the off-loop worker (portGateCh + runPortGateWorker) — with
// the descriptor(s) loaded and the event loop running. Returns the controller,
// its loop, and the supervisor-events.log path.
func f1Controller(t *testing.T, portOwnerFn func(int) (int, bool, error), spawn func(api.SupervisorDaemon) error, daemons ...api.SupervisorDaemon) (*supervisorController, *api.EventLoop, string) {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })

	loop := api.NewEventLoop(16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctrl := &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		tracker:             NewDaemonRuntimeTracker(),
		events:              events,
		graceful:            &gracefulCounter{},
		daemonIntent:        newDaemonIntentCache(),
		spawn:               spawn,
		terminate:           func(api.SupervisorDaemon) error { return nil },
		statePath:           statePath,
		ctx:                 ctx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		squatterLimiter:     newSquatterReapLimiter(),
		portOwnerFn:         portOwnerFn,
		portGateCh:          make(chan portGateReq, portGateChCapacity),
	}
	ctrl.intentCache.Refresh(&api.SupervisorIntentFile{Daemons: daemons})

	// Deterministic selfPID so the classifier's gate-1 self-check is stable.
	setSelfPIDForTest(t, 1)

	loop.RegisterHandler(ctrl.handleLoopEvent)
	go loop.Run(ctx)
	go ctrl.runPortGateWorker(ctx)
	return ctrl, loop, eventsPath
}

func waitForCount(t *testing.T, get func() int32, want int32, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", what, get(), want)
}

func waitForEventInLog(t *testing.T, path, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.Contains(string(raw), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(path)
	t.Fatalf("event %q not found in supervisor-events.log:\n%s", substr, raw)
}

// --- F1 loop half: preSpawnPortGateHold runs ONLY the bounded probe -----------

// newProbeController is a minimal controller for direct preSpawnPortGateHold
// tests: it has the loop-half wiring (portOwnerFn + portGateCh + a cancelable
// ctx so the armed backoff timer goroutine exits on cleanup) but no running loop
// or worker.
func newProbeController(t *testing.T, portOwnerFn func(int) (int, bool, error)) *supervisorController {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &supervisorController{
		ctx:         ctx,
		portOwnerFn: portOwnerFn,
		portGateCh:  make(chan portGateReq, 4),
	}
}

func TestPreSpawnPortGateHold_PortFreeProceeds(t *testing.T) {
	d := globalDaemonDescriptor()
	c := newProbeController(t, func(int) (int, bool, error) { return 0, false, nil }) // unbound
	if err := c.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: d.TaskName}); err != nil {
		t.Fatalf("held = %v, want nil (port free → proceed to spawn)", err)
	}
	if len(c.portGateCh) != 0 {
		t.Fatalf("dispatched %d requests, want 0 (a free port must not dispatch to the worker)", len(c.portGateCh))
	}
}

func TestPreSpawnPortGateHold_ProbeErrorProceeds(t *testing.T) {
	d := globalDaemonDescriptor()
	c := newProbeController(t, func(int) (int, bool, error) { return 0, false, fmt.Errorf("netstat deadline") })
	if err := c.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: d.TaskName}); err != nil {
		t.Fatalf("held = %v, want nil (probe error/deadline → fail-open, proceed)", err)
	}
	if len(c.portGateCh) != 0 {
		t.Fatalf("dispatched %d requests, want 0 (a probe error must not dispatch)", len(c.portGateCh))
	}
}

func TestPreSpawnPortGateHold_OwnedHoldsAndDispatches(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	c := newProbeController(t, func(int) (int, bool, error) { return owner, true, nil })
	task := canonicalSupervisorTaskName(d.TaskName)
	err := c.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: task})
	if err == nil {
		t.Fatal("held = nil, want the sentinel (an owned port must hold + dispatch)")
	}
	select {
	case req := <-c.portGateCh:
		if req.ownerPID != owner {
			t.Fatalf("dispatched ownerPID %d, want %d", req.ownerPID, owner)
		}
		if req.d.Port != d.Port {
			t.Fatalf("dispatched port %d, want %d (the resolved effective port)", req.d.Port, d.Port)
		}
	default:
		t.Fatal("expected a worker dispatch for an owned port")
	}
	if st, ok := c.GetSMState(task); !ok || st != api.StBackoffWaiting {
		t.Fatalf("smState = %v (ok=%v), want StBackoffWaiting (held, no crash increment)", st, ok)
	}
}

func TestPreSpawnPortGateHold_DisabledWithoutWorker(t *testing.T) {
	d := globalDaemonDescriptor()
	// portOwnerFn set but portGateCh nil → gate disabled (never hold a daemon with
	// no worker to reap it).
	c := &supervisorController{portOwnerFn: func(int) (int, bool, error) { return 44000, true, nil }}
	if err := c.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: d.TaskName}); err != nil {
		t.Fatalf("held = %v, want nil (no worker channel disables the gate)", err)
	}
	// portOwnerFn nil → also disabled.
	c2 := &supervisorController{portGateCh: make(chan portGateReq, 1)}
	if err := c2.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: d.TaskName}); err != nil {
		t.Fatalf("held = %v, want nil (nil portOwnerFn disables the gate)", err)
	}
}

// TestPreSpawnPortGateHold_LegacyPort0Resolved proves the loop probe resolves the
// effective port through api.EffectiveDaemonPort for a Port=0 descriptor (the
// 10-of-21 legacy rows). A serena-proxy row carries `--port` in argv, so the
// resolution is hermetic (no manifest needed).
func TestPreSpawnPortGateHold_LegacyPort0Resolved(t *testing.T) {
	d := serenaProxyDescriptor()
	d.Port = 0 // legacy row: no stamped port; argv still carries --port 9151
	const owner = 55001
	var probedPort int
	c := newProbeController(t, func(port int) (int, bool, error) { probedPort = port; return owner, true, nil })
	if err := c.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: canonicalSupervisorTaskName(d.TaskName)}); err == nil {
		t.Fatal("held = nil, want sentinel (owned resolved port must hold)")
	}
	if probedPort != 9151 {
		t.Fatalf("probed port %d, want 9151 (resolved from argv --port via EffectiveDaemonPort for a Port=0 row)", probedPort)
	}
	select {
	case req := <-c.portGateCh:
		if req.d.Port != 9151 {
			t.Fatalf("dispatched port %d, want 9151", req.d.Port)
		}
	default:
		t.Fatal("expected a dispatch on the resolved port")
	}
}

// --- F1 worker half: handlePortGateReq classify+reap + outcome mapping --------

// newWorkerController builds a controller with a running loop whose ONLY handler
// captures posted events, plus the squatterLimiter the worker owns. Worker tests
// call handlePortGateReq directly (synchronously) and assert the reap + the
// posted EvManualRestart + the gateCleared flag.
func newWorkerController(t *testing.T) (*supervisorController, <-chan api.LoopEvent) {
	t.Helper()
	tmpHome := apitest.HardenedTempDir(t)
	events, err := api.OpenSupervisorEventLog(filepath.Join(tmpHome, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	loop := api.NewEventLoop(16)
	captured := make(chan api.LoopEvent, 8)
	loop.RegisterHandler(func(e api.LoopEvent) { captured <- e })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go loop.Run(ctx)
	setSelfPIDForTest(t, 1)
	ctrl := &supervisorController{
		ctx:             ctx,
		eventLoop:       loop,
		events:          events,
		tracker:         NewDaemonRuntimeTracker(),
		squatterLimiter: newSquatterReapLimiter(),
	}
	return ctrl, captured
}

func assertEvent(t *testing.T, ch <-chan api.LoopEvent, kind api.SMEvent, task string) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Kind != kind {
			t.Fatalf("posted event kind = %v, want %v", ev.Kind, kind)
		}
		if canonicalSupervisorTaskName(ev.TaskName) != canonicalSupervisorTaskName(task) {
			t.Fatalf("posted event task = %q, want %q", ev.TaskName, task)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no %v event posted within 2s", kind)
	}
}

func assertNoEvent(t *testing.T, ch <-chan api.LoopEvent) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event posted: %+v (this outcome must post nothing — the 30s timer owns the re-probe)", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPortGateWorker_OwnReapedPostsManualRestartNoGateCleared(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	if reapCount.Load() != 1 {
		t.Fatalf("reap called %d times, want 1 (verified-own squatter reaped)", reapCount.Load())
	}
	assertEvent(t, captured, api.EvManualRestart, d.TaskName)
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("reaped path must NOT set gateCleared (the loop re-probes the freed port and only spawns if truly free)")
	}
}

func TestPortGateWorker_UnverifiedSetsGateClearedAndPosts(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, fmt.Errorf("simulated OpenProcess ACCESS_DENIED")
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (unverifiable owner is never killed)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); !ok {
		t.Fatal("unverified path MUST set gateCleared so the loop spawns as-today without re-probing the still-owned port")
	}
	assertEvent(t, captured, api.EvManualRestart, d.TaskName)
}

func TestPortGateWorker_ForeignPostsNothing(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false }) // exe gate → foreign
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (foreign owner must NEVER be killed)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("foreign path must NOT set gateCleared (no spawn over a foreign holder)")
	}
	assertNoEvent(t, captured)
}

// TestPortGateWorker_ForeignDynamicPoolRoutesToRealloc (F3): a FOREIGN holder at
// pre-spawn on a DYNAMIC-POOL proxy (serena / workspace-LSP) routes to the
// reallocation self-heal via an evPreSpawnRealloc post, instead of parking on the
// stolen port forever. It is NEVER killed (foreign) and does NOT set gateCleared.
//
// NON-VACUITY: this test is the DYNAMIC-POOL twin of
// TestPortGateWorker_ForeignPostsNothing (which uses a FIXED-global descriptor and
// asserts assertNoEvent). Same foreign classification, opposite routing — the
// contrast IS the F3 fix. Remove the isDynamicPoolProxyDescriptor gate in
// handlePortGateReq and the fixed-global twin starts posting (its assertNoEvent
// fails); remove the whole F3 post and THIS test's assertEvent fails.
func TestPortGateWorker_ForeignDynamicPoolRoutesToRealloc(t *testing.T) {
	d := serenaProxyDescriptor() // dynamic-pool proxy
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false }) // exe gate → foreign
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (foreign owner must NEVER be killed)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("foreign path must NOT set gateCleared (no spawn over a foreign holder)")
	}
	// F3: a FOREIGN holder on a DYNAMIC-POOL proxy routes to the reallocation self-heal.
	assertEvent(t, captured, evPreSpawnRealloc, d.TaskName)
}

// TestPortGateWorker_StaleOwnerChangedDropped proves the round-2 P2 staleness
// guard: when the intended port is owned by a DIFFERENT PID than the one dispatched
// (a prior request already reaped the squatter and the daemon respawned, so the
// respawned child now owns the port), the worker DROPS the stale request — no reap,
// no EvManualRestart (which would terminate+respawn the recovered daemon), no stale
// gateCleared bypass.
func TestPortGateWorker_StaleOwnerChangedDropped(t *testing.T) {
	d := globalDaemonDescriptor()
	const dispatchedOwner = 44000
	const differentOwnerNow = 55000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(dispatchedOwner, d), nil
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.portOwnerFn = func(int) (int, bool, error) { return differentOwnerNow, true, nil }
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: dispatchedOwner})

	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (stale request: owner changed → drop, no reap)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("stale request must NOT set gateCleared (would bypass a future F1 gate)")
	}
	assertNoEvent(t, captured)
}

// TestPortGateWorker_StalePortFreedDropped: the intended port is now FREE (a prior
// request reaped the squatter and the respawn has not yet bound) → drop, no reap,
// no gateCleared, no restart.
func TestPortGateWorker_StalePortFreedDropped(t *testing.T) {
	d := globalDaemonDescriptor()
	const dispatchedOwner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(dispatchedOwner, d), nil
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.portOwnerFn = func(int) (int, bool, error) { return 0, false, nil }
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: dispatchedOwner})

	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (stale request: port freed → drop)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("freed-port stale request must NOT set gateCleared")
	}
	assertNoEvent(t, captured)
}

// TestPortGateWorker_SameOwnerStillActs: the staleness guard PASSES when the same
// dispatched PID still owns the port (the squatter is still there) — the worker
// reaps and posts EvManualRestart as normal.
func TestPortGateWorker_SameOwnerStillActs(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.portOwnerFn = func(int) (int, bool, error) { return owner, true, nil }
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	if reapCount.Load() != 1 {
		t.Fatalf("reap called %d times, want 1 (same owner still holds the port → guard passes, reap)", reapCount.Load())
	}
	assertEvent(t, captured, api.EvManualRestart, d.TaskName)
}

// TestPortGateDispatch_DedupePerTask proves the round-2 P2 dedupe: a second dispatch
// for a task already in-flight is skipped, so stale duplicates cannot pile up.
func TestPortGateDispatch_DedupePerTask(t *testing.T) {
	d := globalDaemonDescriptor()
	ctrl, _ := newWorkerController(t)
	ctrl.portGateCh = make(chan portGateReq, 4)

	ctrl.tryDispatchPortGate(portGateReq{d: d, ownerPID: 44000})
	ctrl.tryDispatchPortGate(portGateReq{d: d, ownerPID: 44000}) // duplicate → skipped
	if got := len(ctrl.portGateCh); got != 1 {
		t.Fatalf("queued %d requests, want 1 (per-task dedupe skips the duplicate)", got)
	}
	// After the worker drains + clears the in-flight marker, a fresh dispatch queues.
	<-ctrl.portGateCh
	ctrl.portGateInFlight.Delete(canonicalSupervisorTaskName(d.TaskName))
	ctrl.tryDispatchPortGate(portGateReq{d: d, ownerPID: 44000})
	if got := len(ctrl.portGateCh); got != 1 {
		t.Fatalf("queued %d after clear, want 1 (dedupe releases once the in-flight marker clears)", got)
	}
}

// TestPortGateWorker_ReapFailedPostsNothing simulates the PID-reuse defense: the
// snapshot PID has been reused by a fresh process, so TerminatePIDWithIdentity's
// held-handle re-verify REFUSES the kill (here modeled by the injected terminate
// returning an identity-mismatch error). The worker must treat that as
// reap-failed → post NOTHING (the port is still held; the armed 30s timer owns
// the retry). The real held-handle re-verify lives inside TerminatePIDWithIdentity
// and is untouched by this change.
func TestPortGateWorker_ReapFailedPostsNothing(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error {
		reapCount.Add(1)
		return fmt.Errorf("%w: snapshot PID reused by a fresh child", process.ErrProcessIdentityMismatch)
	})

	ctrl, captured := newWorkerController(t)
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	if reapCount.Load() != 1 {
		t.Fatalf("reap attempted %d times, want 1 (the kill was tried and refused)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("reap-failed path must NOT set gateCleared")
	}
	assertNoEvent(t, captured)
}

// --- F1 end-to-end: loop probe → worker → outcome, via EvTimerDue -------------

// seedFailures records n crashes so the SM's StBackoffWaiting+EvTimerDue arm
// routes to StSpawning (n < 10), and the tests can assert the count is not pushed
// higher by F1's hold.
func seedFailures(c *supervisorController, task string, n int) {
	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		c.tracker.RecordCrashAndCountInWindow(task, now, respawnFailureWindow)
	}
}

func TestF1_ForeignRespawnHeldNoSpawnNoIncrement(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false }) // exe gate → foreign
	var spawnCount, reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, loop, eventsPath := f1Controller(t, func(int) (int, bool, error) { return owner, true, nil },
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	seedFailures(ctrl, d.TaskName, 3)
	ctrl.smStates.Store(canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: d.TaskName})

	// Loop held the spawn + dispatched; worker classified foreign and posted
	// nothing. The 30s re-arm will not fire in the test window.
	waitForEventInLog(t, eventsPath, `"event":"daemon-port-squatter-foreign"`)
	lostChildWaitForSMState(t, ctrl, canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	if spawnCount.Load() != 0 {
		t.Fatalf("spawn called %d times, want 0 (a spawn doomed to EADDRINUSE against a foreign holder must not fire)", spawnCount.Load())
	}
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (foreign owner never killed)", reapCount.Load())
	}
	if got := ctrl.tracker.CrashCountInWindow(d.TaskName, time.Now().UTC(), respawnFailureWindow); got != 3 {
		t.Fatalf("failure window = %d, want 3 (F1 hold must NOT increment the quarantine march)", got)
	}
}

func TestF1_OwnSquatterReapedThenSpawnsNoIncrement(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)
	var spawnCount, reapCount atomic.Int32
	var portFreed atomic.Bool
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error {
		reapCount.Add(1)
		portFreed.Store(true) // TerminatePIDWithIdentity waits for exit → LISTEN socket freed
		return nil
	})

	portOwnerFn := func(int) (int, bool, error) {
		if portFreed.Load() {
			return 0, false, nil
		}
		return owner, true, nil
	}
	ctrl, loop, eventsPath := f1Controller(t, portOwnerFn,
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	seedFailures(ctrl, d.TaskName, 3)
	ctrl.smStates.Store(canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: d.TaskName})

	// Loop held + dispatched; worker reaped the own squatter (freeing the port) and
	// posted EvManualRestart; the loop re-probed the now-free port and spawned —
	// all without a crash increment.
	waitForEventInLog(t, eventsPath, `"event":"daemon-port-squatter-reaped"`)
	waitForCount(t, reapCount.Load, 1, "reap count")
	waitForCount(t, spawnCount.Load, 1, "spawn count (proceeds after worker reap + EvManualRestart)")
	lostChildWaitForSMState(t, ctrl, canonicalSupervisorTaskName(d.TaskName), api.StRunning)
	if got := reapCount.Load(); got != 1 {
		t.Fatalf("reap count = %d, want exactly 1 (no double reap of an already-freed port)", got)
	}
	if got := ctrl.tracker.CrashCountInWindow(d.TaskName, time.Now().UTC(), respawnFailureWindow); got != 3 {
		t.Fatalf("failure window = %d, want 3 (reap+respawn must NOT increment the march)", got)
	}
}

func TestF1_UnverifiedSpawnsAsToday(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, fmt.Errorf("simulated lookup failure")
	}, alwaysExeMatch)
	var spawnCount, reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	// Owner stays present the whole time: only the gateCleared one-shot lets the
	// loop spawn over the still-owned port (today's unverified→proceed contract).
	ctrl, loop, _ := f1Controller(t, func(int) (int, bool, error) { return owner, true, nil },
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	seedFailures(ctrl, d.TaskName, 2)
	ctrl.smStates.Store(canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: d.TaskName})

	waitForCount(t, spawnCount.Load, 1, "spawn count (unverified fail-open via gateCleared)")
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (unverified owner never killed)", reapCount.Load())
	}
}

// TestF1_LoopNotBlockedByWorkerClassify is the codex-P1 proof: while task A's
// identity classify BLOCKS on the off-loop worker, a SECOND daemon B's respawn
// event still dispatches on the loop (B spawns). Pre-fix the classify ran INLINE
// on the loop, so a blocked A froze the whole fleet and B would never spawn.
func TestF1_LoopNotBlockedByWorkerClassify(t *testing.T) {
	a := globalDaemonDescriptor() // port 9123
	b := a
	b.TaskName = `\mcp-local-hub-time-default`
	b.Server = "time"
	b.Daemon = "default"
	b.Args = []string{"daemon", "--server", "time", "--daemon", "default"}
	b.Port = 9124

	block := make(chan struct{})
	var released atomic.Bool
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		// A's classify blocks here until the test releases it; B never classifies
		// (its port is free, so the loop never dispatches B to the worker).
		<-block
		return squatterIdentityFor(44000, a), nil
	}, alwaysExeMatch)
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { return nil })

	// A's port (9123) is owned; B's port (9124) is free.
	portOwnerFn := func(port int) (int, bool, error) {
		if port == a.Port && !released.Load() {
			return 44000, true, nil
		}
		return 0, false, nil
	}
	var spawnA, spawnB atomic.Int32
	spawn := func(d api.SupervisorDaemon) error {
		if canonicalSupervisorTaskName(d.TaskName) == canonicalSupervisorTaskName(b.TaskName) {
			spawnB.Add(1)
		} else {
			spawnA.Add(1)
		}
		return nil
	}
	ctrl, loop, _ := f1Controller(t, portOwnerFn, spawn, a, b)

	seedFailures(ctrl, a.TaskName, 3)
	seedFailures(ctrl, b.TaskName, 3)
	ctrl.smStates.Store(canonicalSupervisorTaskName(a.TaskName), api.StBackoffWaiting)
	ctrl.smStates.Store(canonicalSupervisorTaskName(b.TaskName), api.StBackoffWaiting)

	// A: loop probes (owned, fast) → holds + dispatches → worker BLOCKS on classify.
	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: a.TaskName})
	// B: loop probes (free, fast) → spawns immediately — proving the loop is NOT
	// frozen by A's blocked worker classify.
	loop.Post(api.LoopEvent{Kind: api.EvTimerDue, TaskName: b.TaskName})

	waitForCount(t, spawnB.Load, 1, "daemon B spawn count while A's classify blocks the worker")
	if spawnA.Load() != 0 {
		t.Fatalf("daemon A spawned %d times while its classify is still blocked, want 0", spawnA.Load())
	}

	// Release A so the worker + its goroutines drain cleanly before teardown.
	released.Store(true)
	close(block)
	waitForCount(t, spawnA.Load, 1, "daemon A spawn count after the classify unblocks (own reaped → respawn)")
}

// --- F3 integration tests (quarantine self-heal sweep) — unchanged by P1 ------

func runF3Sweep(t *testing.T, d api.SupervisorDaemon, tracker *DaemonRuntimeTracker, events *api.SupervisorEventLog) []api.LoopEvent {
	t.Helper()
	reap := &squatterSweepReaper{
		// F3 must reap via squatterTerminatePIDFn directly, never the sweep's
		// reapFn closure (which is the P2a-mismatch path). Fail loud if it does.
		reapFn: func(api.SupervisorDaemon, process.PIDIdentityProof) error {
			t.Fatal("F3 must not use reap.reapFn")
			return nil
		},
		limiter: newSquatterReapLimiter(),
	}
	return runSweepOnce(t, d, tracker, events, reap)
}

func TestF3_QuarantinedOwnSquatterReapedAndManualRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })
	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName) // the exact state the running-pass skips

	events := runF3Sweep(t, d, tracker, nil)

	if reapCount.Load() != 1 {
		t.Fatalf("reap called %d times, want 1 (F3 reaps the verified-own squatter of a quarantined daemon)", reapCount.Load())
	}
	if len(events) != 1 || events[0].Kind != api.EvManualRestart {
		t.Fatalf("events = %+v, want exactly one EvManualRestart (legal quarantine exit)", events)
	}
	if canonicalSupervisorTaskName(events[0].TaskName) != canonicalSupervisorTaskName(d.TaskName) {
		t.Fatalf("EvManualRestart task = %q, want %q", events[0].TaskName, d.TaskName)
	}
}

func TestF3_QuarantinedForeignObserveOnlyNoRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false }) // foreign
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })
	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName)

	events := runF3Sweep(t, d, tracker, nil)
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (foreign owner of a quarantined daemon is never killed)", reapCount.Load())
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (foreign is observe-only — no self-heal restart)", events)
	}
}

func TestF3_QuarantinedUnverifiedObserveOnlyNoRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, fmt.Errorf("simulated lookup failure")
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })
	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName)

	events := runF3Sweep(t, d, tracker, nil)
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (unverifiable owner fails closed)", reapCount.Load())
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (unverified is observe-only)", events)
	}
}

// TestF3_QuarantinedPortFreeNoRestart proves F3 does NOT fire when the
// quarantined daemon's port is unbound — cooldown recovery there is F2's parole,
// not a squatter reap.
func TestF3_QuarantinedPortFreeNoRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		t.Fatal("classify must not run when the port is free")
		return process.ProcessIdentity{}, nil
	}, alwaysExeMatch)
	// Port owner probe reports unbound.
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 0, false, nil },
	})
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName)
	events := runF3Sweep(t, d, tracker, nil)
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (a quarantined daemon with a free port is F2 parole's job, not F3)", events)
	}
}

// --- H1 forensic bounding for the new automatic sources -----------------------

// TestSquatterAutoTrigger_OversizedCommandLinePreservesIdentity proves the H1
// field pre-bounding survives through the F1/F3 shared reap path: an oversized
// hostile CommandLine still yields a reaped event carrying squatter_pid +
// bounded executable_path (not evicted by the 16 KB whole-body truncation), with
// the NEW "prespawn" body-source tag.
func TestSquatterAutoTrigger_OversizedCommandLinePreservesIdentity(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	log, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer log.Close()
	d := globalDaemonDescriptor()
	const owner = 44000
	// A VALID own-task argv (so the classify verdict is own_task) with a 100 KB
	// hostile tail — unbounded it would blow past the 16 KB whole-body cap and
	// evict the identity into a sentinel.
	huge := joinCmdLine(append([]string{d.Command}, d.Args...)) + " " + strings.Repeat("A", 100*1024)
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{PID: owner, Basename: "mcphub.exe", CommandLine: huge, ExecutablePath: `C:\mcphub.exe`, CreationDateUnix: time.Now().Unix()}, nil
	}, alwaysExeMatch)
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { return nil })
	setSelfPIDForTest(t, 1)

	if got := reapSquatterForAutomaticTrigger(d, owner, 1, nil, newSquatterReapLimiter(), log, squatterSourcePreSpawn, time.Now().UTC()); got != squatterAutoReaped {
		t.Fatalf("outcome = %v, want squatterAutoReaped", got)
	}
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	line := strings.TrimSpace(string(data))
	if strings.Contains(line, "_truncated_note") {
		t.Fatalf("whole-body truncation fired — identity evicted (H1 violated); line:\n%.200s", line)
	}
	for _, want := range []string{`"squatter_pid":44000`, `"executable_path":"C:\\mcphub.exe"`, `"verdict":"own_task"`, `"source":"prespawn"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("bounded event missing %q; line:\n%.400s", want, line)
		}
	}
	// Envelope source reflects the F1 worker trigger (OBS-2), not "liveness".
	if !strings.Contains(line, `"source":"restart-policy"`) {
		t.Fatalf("envelope source not restart-policy (OBS-2); line:\n%.400s", line)
	}
	if len(line) > 12*1024 {
		t.Fatalf("event line %d bytes — expected well under the 16 KB cap after field bounding", len(line))
	}
}

func TestRound6AutomaticAlreadyExitedEmitsDistinctEventWithoutReapedClaim(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	log, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = log.Close()
		}
	}()

	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch)
	terminateCalls := 0
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error {
		terminateCalls++
		return process.ErrProcessAlreadyExited
	})
	setSelfPIDForTest(t, 1)

	if got := reapSquatterForAutomaticTrigger(d, owner, 1, nil, newSquatterReapLimiter(), log, squatterSourcePreSpawn, time.Now().UTC()); got != squatterAutoReaped {
		t.Fatalf("outcome=%v want squatterAutoReaped so the caller may continue recovery", got)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close event log: %v", err)
	}
	closed = true
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	text := string(data)
	if got := strings.Count(text, `"event":"daemon-port-squatter-reaped"`); got != 0 {
		t.Fatalf("reaped event count=%d want=0; log=%s", got, text)
	}
	if got := strings.Count(text, `"event":"daemon-port-squatter-already-exited"`); got != 1 {
		t.Fatalf("already-exited event count=%d want=1; log=%s", got, text)
	}
	if terminateCalls != 1 {
		t.Fatalf("squatterTerminatePIDFn calls=%d want=1", terminateCalls)
	}
}

// --- Codex PR-3 P2 regression tests ------------------------------------------

// TestF1_GatesEvIntentUpdateRespawn (P2-1): api.Transition ALSO creates a process
// for StBackoffWaiting+EvIntentUpdate(running) (operator re-enable/edit). Before
// the fix the F1 gate was scoped to EvTimerDue|EvManualRestart only, so a
// re-enable while a lost child squatted the port spawned into it. The gate now
// covers EvIntentUpdate: an owned+foreign port is held, not spawned.
func TestF1_GatesEvIntentUpdateRespawn(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false }) // foreign → worker posts nothing
	var spawnCount, reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, loop, eventsPath := f1Controller(t, func(int) (int, bool, error) { return owner, true, nil },
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	ctrl.smStates.Store(canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	// EvIntentUpdate(running) at StBackoffWaiting → StSpawning (create-process); the
	// F1 gate must fire (P2-1). Empty daemonIntent defaults to running.
	loop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: d.TaskName})

	waitForEventInLog(t, eventsPath, `"event":"daemon-port-squatter-foreign"`)
	lostChildWaitForSMState(t, ctrl, canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	if spawnCount.Load() != 0 {
		t.Fatalf("spawn called %d times, want 0 (EvIntentUpdate respawn must be gated, not spawned over a squatter)", spawnCount.Load())
	}
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (foreign owner never killed)", reapCount.Load())
	}
}

// TestF1_GateClearedClearedOnSettle (P2-2): a gateCleared bypass set by the worker
// but never consumed (the task settles out of the respawn cycle before the
// EvManualRestart lands) must be dropped, or the NEXT respawn would skip the port
// probe and spawn over whatever now owns the port.
func TestF1_GateClearedClearedOnSettle(t *testing.T) {
	d := globalDaemonDescriptor()
	task := canonicalSupervisorTaskName(d.TaskName)
	ctrl, loop, _ := f1Controller(t, nil, func(api.SupervisorDaemon) error { return nil }, d)

	// Simulate the worker stale bypass, with the task about to settle.
	ctrl.gateCleared.Store(task, struct{}{})
	ctrl.smStates.Store(task, api.StBackoffWaiting)

	// A graceful request settles StBackoffWaiting → StIdle (a matched, non-StSpawning
	// transition) — the flag must be cleared as the task leaves the respawn cycle.
	loop.Post(api.LoopEvent{Kind: api.EvRequestGraceful, TaskName: d.TaskName})
	lostChildWaitForSMState(t, ctrl, task, api.StIdle)

	if _, ok := ctrl.gateCleared.Load(task); ok {
		t.Fatal("gateCleared survived a non-StSpawning settle transition (P2-2): the next respawn would skip the port probe")
	}
}

// runF3SweepStopped mirrors runF3Sweep but wires a stoppedFn returning `stopped`.
func runF3SweepStopped(t *testing.T, d api.SupervisorDaemon, tracker *DaemonRuntimeTracker, stopped bool) []api.LoopEvent {
	t.Helper()
	reap := &squatterSweepReaper{
		reapFn: func(api.SupervisorDaemon, process.PIDIdentityProof) error {
			t.Fatal("F3 must not use reap.reapFn")
			return nil
		},
		limiter:   newSquatterReapLimiter(),
		stoppedFn: func(string) bool { return stopped },
	}
	return runSweepOnce(t, d, tracker, nil, reap)
}

// TestF3_StoppedDaemonNotSelfHealed (P2-3): F3 must NOT reap + auto-restart a
// quarantined daemon the operator STOPPED, even with a verified-own squatter on
// its port — the stop owns cleanup.
func TestF3_StoppedDaemonNotSelfHealed(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, alwaysExeMatch) // own — would be reaped if not for the stop gate
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })
	restore := setSupervisorLivenessProbeForTest(mismatchProbe(owner))
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName)

	events := runF3SweepStopped(t, d, tracker, true) // operator STOPPED this daemon
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (a stopped daemon's squatter must not be reaped)", reapCount.Load())
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (F3 must not auto-restart a stopped daemon)", events)
	}
}

// TestPreSpawnPortGateHold_OccupiedHiddenHolds (P2-4): a probe result of (0, true)
// — the port is OCCUPIED by an unreadable different-UID process (Linux
// occupied-hidden) — must HOLD + dispatch, NOT be treated as free and spawned over.
func TestPreSpawnPortGateHold_OccupiedHiddenHolds(t *testing.T) {
	d := globalDaemonDescriptor()
	c := newProbeController(t, func(int) (int, bool, error) { return 0, true, nil }) // occupied-hidden
	task := canonicalSupervisorTaskName(d.TaskName)
	if err := c.preSpawnPortGateHold(&d, api.LoopEvent{TaskName: task}); err == nil {
		t.Fatal("held = nil, want the sentinel ((0,true) occupied-hidden must hold, not spawn as free)")
	}
	select {
	case req := <-c.portGateCh:
		if req.ownerPID != 0 {
			t.Fatalf("dispatched ownerPID %d, want 0 (occupied-hidden)", req.ownerPID)
		}
	default:
		t.Fatal("expected a worker dispatch for an occupied-hidden port")
	}
	if st, ok := c.GetSMState(task); !ok || st != api.StBackoffWaiting {
		t.Fatalf("smState = %v (ok=%v), want StBackoffWaiting", st, ok)
	}
}

// TestPortGateWorker_OccupiedHiddenHoldsNoSpawn (P2-4): the worker, given an
// occupied-hidden dispatch (req.ownerPID<=0), must HOLD — no reap, no gateCleared,
// no EvManualRestart — so the loop never spawns over the cross-user-held port.
func TestPortGateWorker_OccupiedHiddenHoldsNoSpawn(t *testing.T) {
	d := globalDaemonDescriptor()
	var reapCount atomic.Int32
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		t.Fatal("identity lookup must not run for a non-positive owner PID (gate 1 rejects first)")
		return process.ProcessIdentity{}, nil
	}, alwaysExeMatch)
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, captured := newWorkerController(t)
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: 0}) // occupied-hidden

	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (occupied-hidden is unkillable, never reaped)", reapCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); ok {
		t.Fatal("occupied-hidden must NOT set gateCleared (the loop must not spawn over a cross-user-held port)")
	}
	assertNoEvent(t, captured)
}

// TestF3_OccupiedHiddenObserveOnly (P2-4): a quarantined daemon whose port probe
// returns (0, true) (occupied-hidden) is observe-only — no reap, no restart — NOT
// skipped as free.
func TestF3_OccupiedHiddenObserveOnly(t *testing.T) {
	d := globalDaemonDescriptor()
	var reapCount atomic.Int32
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		t.Fatal("identity lookup must not run for a non-positive owner PID")
		return process.ProcessIdentity{}, nil
	}, alwaysExeMatch)
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:     func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) { return 0, true, nil }, // occupied-hidden
	})
	defer restore()

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName)
	events := runF3Sweep(t, d, tracker, nil)
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (occupied-hidden owner is never killed)", reapCount.Load())
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (occupied-hidden is observe-only — no restart, and NOT skipped as free)", events)
	}
}

// --- Codex PR-3 round-3 P2 follow-on tests -----------------------------------

// TestF3_LoopSideStopReCheckDropsRestart (P2-A): a flagged (require_running_intent)
// EvManualRestart posted by F3 is DROPPED on the loop when the daemon is stopped —
// the loop-side re-check closes the off-loop stoppedFn race so F3 never resurrects
// a just-stopped daemon.
func TestF3_LoopSideStopReCheckDropsRestart(t *testing.T) {
	d := globalDaemonDescriptor()
	task := canonicalSupervisorTaskName(d.TaskName)
	var spawnCount atomic.Int32
	// nil portOwnerFn → F1 gate disabled, so only the P2-A stop gate decides.
	ctrl, loop, eventsPath := f1Controller(t, nil, func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		task: {Desired: api.IntentDesiredStopped}, // operator stopped it
	}})
	ctrl.smStates.Store(task, api.StQuarantined)

	loop.Post(api.LoopEvent{
		Kind:     api.EvManualRestart,
		TaskName: d.TaskName,
		Body:     map[string]any{autoRestartRequireRunningIntentBodyKey: true},
	})

	waitForEventInLog(t, eventsPath, `"event":"automatic-restart-skipped-stopped"`)
	// The daemon must stay quarantined (dropped before the SM, no spawn).
	time.Sleep(50 * time.Millisecond)
	if spawnCount.Load() != 0 {
		t.Fatalf("spawn called %d times, want 0 (F3 restart must be dropped for a stopped daemon)", spawnCount.Load())
	}
	if st, ok := ctrl.GetSMState(task); !ok || st != api.StQuarantined {
		t.Fatalf("smState = %v, want StQuarantined (the flagged restart was dropped before the SM)", st)
	}
}

// TestF3_UnflaggedManualRestartUnconditional (P2-A): an UNFLAGGED EvManualRestart
// (as `mcphub daemon recover` posts) still restarts a quarantined daemon
// unconditionally, even with a stopped intent — the P2-A gate keys on the flag.
func TestF3_UnflaggedManualRestartUnconditional(t *testing.T) {
	d := globalDaemonDescriptor()
	task := canonicalSupervisorTaskName(d.TaskName)
	var spawnCount atomic.Int32
	ctrl, loop, _ := f1Controller(t, nil, func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		task: {Desired: api.IntentDesiredStopped},
	}})
	ctrl.smStates.Store(task, api.StQuarantined)

	// No require_running_intent flag → daemon-recover semantics: unconditional.
	loop.Post(api.LoopEvent{Kind: api.EvManualRestart, TaskName: d.TaskName})

	waitForCount(t, spawnCount.Load, 1, "spawn count (unflagged EvManualRestart is unconditional)")
}

// TestF1_GateClearedClearedOnNotFoundDrop (P2-B): a gateCleared flag left for a
// task whose descriptor was removed is cleared when its now-orphan EvManualRestart
// is dropped at the descriptor-not-found path, so a same-name re-registration
// cannot inherit a stale probe-skip bypass.
func TestF1_GateClearedClearedOnNotFoundDrop(t *testing.T) {
	d := globalDaemonDescriptor()
	// The loop is running with only `d` in intent; `removed` is NOT in intent.
	ctrl, loop, _ := f1Controller(t, nil, func(api.SupervisorDaemon) error { return nil }, d)
	removed := canonicalSupervisorTaskName(`\mcp-local-hub-removed-default`)

	ctrl.gateCleared.Store(removed, struct{}{})
	loop.Post(api.LoopEvent{Kind: api.EvManualRestart, TaskName: removed})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := ctrl.gateCleared.Load(removed); !ok {
			return // cleared — success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gateCleared survived the descriptor-not-found drop (P2-B): a re-add would skip the port probe")
}

// --- Codex PR-3 round-4 P2 follow-on tests -----------------------------------

// TestF1_GatesEvStartRespawn (P2-i): StIdle+EvStart creates a process; the
// converged gate (all create-process events except EvChildExit) must cover it, so
// a cold-start EvStart into an own-squatter-occupied port is held+dispatched, not
// spawned over.
func TestF1_GatesEvStartRespawn(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil
	}, func(int, string) bool { return false }) // foreign → worker posts nothing
	var spawnCount, reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	ctrl, loop, eventsPath := f1Controller(t, func(int) (int, bool, error) { return owner, true, nil },
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	// Task starts idle (default); EvStart from the reconciler → StSpawning (create-process).
	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: d.TaskName})

	waitForEventInLog(t, eventsPath, `"event":"daemon-port-squatter-foreign"`)
	lostChildWaitForSMState(t, ctrl, canonicalSupervisorTaskName(d.TaskName), api.StBackoffWaiting)
	if spawnCount.Load() != 0 {
		t.Fatalf("spawn called %d times, want 0 (EvStart into an occupied port must be gated)", spawnCount.Load())
	}
}

// TestF1_ColdStartEvStartFreePortSpawns (P2-i): the same EvStart cold start on a
// FREE port proceeds (probe → free → spawn) — the gate is fail-open.
func TestF1_ColdStartEvStartFreePortSpawns(t *testing.T) {
	d := globalDaemonDescriptor()
	var spawnCount atomic.Int32
	ctrl, loop, _ := f1Controller(t, func(int) (int, bool, error) { return 0, false, nil }, // free
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)
	_ = ctrl
	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: d.TaskName})
	waitForCount(t, spawnCount.Load, 1, "spawn count (cold-start EvStart on a free port proceeds)")
}

// TestPortGateWorker_RestartArmsCarryRunningIntentFlag (P2-ii): BOTH the reaped and
// the unverified arms of the F1 worker post their EvManualRestart carrying
// require_running_intent, so the loop-side stop re-check gates them.
func TestPortGateWorker_RestartArmsCarryRunningIntentFlag(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000

	t.Run("reaped arm", func(t *testing.T) {
		setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
			return squatterIdentityFor(owner, d), nil
		}, alwaysExeMatch)
		setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { return nil })
		ctrl, captured := newWorkerController(t)
		ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})
		assertRestartFlagged(t, captured, d.TaskName)
	})

	t.Run("unverified arm", func(t *testing.T) {
		setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
			return process.ProcessIdentity{}, fmt.Errorf("simulated ACCESS_DENIED")
		}, alwaysExeMatch)
		setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { return nil })
		ctrl, captured := newWorkerController(t)
		ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})
		assertRestartFlagged(t, captured, d.TaskName)
		if _, ok := ctrl.gateCleared.Load(canonicalSupervisorTaskName(d.TaskName)); !ok {
			t.Fatal("unverified arm must set gateCleared")
		}
	})
}

func assertRestartFlagged(t *testing.T, ch <-chan api.LoopEvent, task string) {
	t.Helper()
	select {
	case ev := <-ch:
		if ev.Kind != api.EvManualRestart {
			t.Fatalf("posted %v, want EvManualRestart", ev.Kind)
		}
		if flag, _ := ev.Body[autoRestartRequireRunningIntentBodyKey].(bool); !flag {
			t.Fatalf("EvManualRestart Body = %+v, want require_running_intent=true (P2-ii)", ev.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no EvManualRestart posted within 2s")
	}
}

// TestF1_WorkerRestartDroppedOnStopClearsGateCleared (P2-ii integration): the F1
// worker's unverified arm sets gateCleared + posts a flagged EvManualRestart; with
// a stopped intent the loop-side gate DROPS the restart (no spawn) AND clears the
// stale gateCleared bypass.
func TestF1_WorkerRestartDroppedOnStopClearsGateCleared(t *testing.T) {
	d := globalDaemonDescriptor()
	task := canonicalSupervisorTaskName(d.TaskName)
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return process.ProcessIdentity{}, fmt.Errorf("simulated ACCESS_DENIED")
	}, alwaysExeMatch)
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { return nil })
	var spawnCount atomic.Int32
	ctrl, _, eventsPath := f1Controller(t, func(int) (int, bool, error) { return owner, true, nil },
		func(api.SupervisorDaemon) error { spawnCount.Add(1); return nil }, d)

	ctrl.daemonIntent.Refresh(&api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		task: {Desired: api.IntentDesiredStopped},
	}})
	ctrl.smStates.Store(task, api.StBackoffWaiting)

	// Simulate a dispatched request: the worker's unverified arm sets gateCleared +
	// posts the flagged EvManualRestart; the loop-side gate drops it (stopped).
	ctrl.handlePortGateReq(ctrl.ctx, portGateReq{d: d, ownerPID: owner})

	waitForEventInLog(t, eventsPath, `"event":"automatic-restart-skipped-stopped"`)
	time.Sleep(50 * time.Millisecond)
	if spawnCount.Load() != 0 {
		t.Fatalf("spawn called %d times, want 0 (stopped daemon must not be restarted by the F1 worker)", spawnCount.Load())
	}
	if _, ok := ctrl.gateCleared.Load(task); ok {
		t.Fatal("gateCleared not cleared after the flagged restart was dropped (P2-ii): a re-enable would skip the port probe")
	}
}

// TestF3_StaleSnapshotDoesNotReapRestartedChild (P2-iii): if the task is restarted
// between the sweep-start snapshot and F3's port probe, the fresh re-read must drop
// the reap so F3 never kills the healthy just-restarted child.
func TestF3_StaleSnapshotDoesNotReapRestartedChild(t *testing.T) {
	d := globalDaemonDescriptor()
	const owner = 44000
	setSquatterLookupForTest(t, func(int) (process.ProcessIdentity, error) {
		return squatterIdentityFor(owner, d), nil // would be verified-own against a STALE snap
	}, alwaysExeMatch)
	var reapCount atomic.Int32
	setSquatterTerminateForTest(t, func(process.PIDIdentityProof) error { reapCount.Add(1); return nil })

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkQuarantined(d.TaskName) // sweep-start snapshot sees Quarantined

	// The port probe simulates the race: the task is restarted (F2 parole / recover)
	// between the sweep-start snapshot and this probe, so by reap time the tracker
	// shows a fresh running child (CurrentPID = owner) owning the port.
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) {
			tracker.MarkSpawned(d.TaskName, owner, time.Now().UTC())
			return owner, true, nil
		},
	})
	defer restore()

	events := runF3Sweep(t, d, tracker, nil)
	if reapCount.Load() != 0 {
		t.Fatalf("reap called %d times, want 0 (F3 must NOT reap a just-restarted healthy child — friendly-fire)", reapCount.Load())
	}
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 0 (no restart of an already-recovered task)", events)
	}
}
