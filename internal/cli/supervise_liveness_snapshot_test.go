package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// installProductionPortOwnerProbeForSweep sets the GLOBAL liveness probe so the
// SWEEP takes the snapshot branch. The sweep only swaps in the single-snapshot
// probe when PortOwnerPID is the PRODUCTION per-port probe
// (supervisorPortOwnerPID); a test-injected per-port closure is deliberately
// left intact (see supervisorLivenessUsesProductionPortOwnerProbe). So to
// exercise the snapshot path the probe must carry the real production
// PortOwnerPID — the snapshot-backed closure inside sweepSupervisorLivenessOnce
// then replaces it, resolving every daemon against the injected
// loopbackPortOwnersSnapshotFn. PIDAlive=true and NO PIDIdentity proof so the
// per-daemon liveness reduces to the port-owner check.
func installProductionPortOwnerProbeForSweep(t *testing.T) {
	t.Helper()
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:     func(pid int) bool { return true },
		PortLive:     func(port int) bool { return true },
		PortOwnerPID: supervisorPortOwnerPID,
	})
	t.Cleanup(restore)
}

// runSweepCollectingEvents runs ONE sweep against a buffered event loop and
// returns every LoopEvent posted within a short drain window. A snapshot that
// resolves every daemon Running posts ZERO events (the perf-fix happy path);
// any restart/teardown verdict posts an event the caller can inspect.
func runSweepCollectingEvents(t *testing.T, stateDir string, intent *api.SupervisorIntentFile, tracker *DaemonRuntimeTracker, n int) []api.LoopEvent {
	t.Helper()
	loop := api.NewEventLoop(n + 4)
	got := make(chan api.LoopEvent, n+4)
	loop.RegisterHandler(func(e api.LoopEvent) { got <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil, nil)

	// Drain: collect every event posted during the sweep, then stop on the
	// first quiet window.
	var events []api.LoopEvent
	for {
		select {
		case e := <-got:
			events = append(events, e)
		case <-time.After(200 * time.Millisecond):
			return events
		}
	}
}

// sweepIntentFromTracker rebuilds the supervisor-intent the sweep consumes from
// the seeded tracker + meta. seedRunningDaemons already wrote
// supervisor-intent.json, but the sweep takes the intent as an argument, so
// reconstruct an equivalent in-memory intent with the same task/port set.
func sweepIntentFromMeta(meta map[string]struct{ pid, port int }) *api.SupervisorIntentFile {
	intent := &api.SupervisorIntentFile{Version: 1}
	for task, m := range meta {
		intent.Daemons = append(intent.Daemons, api.SupervisorDaemon{
			TaskName: task,
			Server:   "srv",
			Daemon:   "default",
			Port:     m.port,
		})
	}
	return intent
}

// TestSupervisorLivenessSweepTakesPortOwnerSnapshotExactlyOnce is the core perf
// guarantee for the SWEEP (mirrors TestSupervisorStatusTakesPortOwnerSnapshotExactlyOnce):
// resolving N(>=3) running daemons takes ONE OS port-owner snapshot, not N
// per-port netstat spawns EVERY 5 seconds. Every daemon's port maps to its own
// tracked PID in the snapshot, so the sweep posts ZERO events (all live), and
// the snapshot fn is invoked EXACTLY ONCE regardless of daemon count.
func TestSupervisorLivenessSweepTakesPortOwnerSnapshotExactlyOnce(t *testing.T) {
	const n = 4
	stateDir, tracker, meta := seedRunningDaemons(t, n)
	installProductionPortOwnerProbeForSweep(t)

	snap := map[int]int{}
	for _, m := range meta {
		snap[m.port] = m.pid // every port owned by its OWN tracked pid → live
	}
	calls := withFakePortOwnersSnapshot(t, func() (map[int]int, error) { return snap, nil })

	intent := sweepIntentFromMeta(meta)
	events := runSweepCollectingEvents(t, stateDir, intent, tracker, n)

	if *calls != 1 {
		t.Fatalf("port-owner snapshot taken %d times, want EXACTLY 1 (one query for all %d daemons)", *calls, n)
	}
	if len(events) != 0 {
		t.Fatalf("sweep posted %d events for all-live daemons, want 0: %+v", len(events), events)
	}
}

// TestSupervisorLivenessSweepSnapshotErrorPostsNoRestart asserts a snapshot
// ERROR is treated EXACTLY like a per-port netstat failure: every daemon
// classifies port_owner_unverified, which the sweep observes (no event posted)
// rather than restarting. A probe error must NEVER drive a fleet restart loop.
// Still ONE snapshot for the whole sweep.
func TestSupervisorLivenessSweepSnapshotErrorPostsNoRestart(t *testing.T) {
	const n = 3
	stateDir, tracker, meta := seedRunningDaemons(t, n)
	installProductionPortOwnerProbeForSweep(t)

	calls := withFakePortOwnersSnapshot(t, func() (map[int]int, error) {
		return nil, errors.New("netstat policy-blocked")
	})

	intent := sweepIntentFromMeta(meta)
	events := runSweepCollectingEvents(t, stateDir, intent, tracker, n)

	if *calls != 1 {
		t.Fatalf("snapshot taken %d times on error path, want 1", *calls)
	}
	if len(events) != 0 {
		t.Fatalf("snapshot error posted %d events, want 0 (port_owner_unverified is observe-only, never a restart): %+v", len(events), events)
	}
}

// TestSupervisorLivenessSweepSnapshotOwnerMismatchObserveOnly is the P2a
// (decision D-A, MUST-FIX #10) rewrite of ...SnapshotOwnerMismatchRestarts: a
// daemon whose port is owned by a DIFFERENT live PID in the shared snapshot no
// longer posts an unconditional EvManualRestart. With the reap capability
// unwired (runSweepCollectingEvents passes a nil reaper), the foreign owner is
// handled observe-only → ZERO loop events. The single-snapshot perf invariant
// is unchanged (still exactly ONE snapshot for the whole sweep).
func TestSupervisorLivenessSweepSnapshotOwnerMismatchObserveOnly(t *testing.T) {
	const n = 3
	stateDir, tracker, meta := seedRunningDaemons(t, n)
	installProductionPortOwnerProbeForSweep(t)

	// Pin self-PID away from any tracked pid (500000+) and the foreign owner
	// (pid+999999) so the squatted port resolves port_owner_mismatch
	// deterministically, never port_owner_self.
	prevSelf := supervisorSelfPIDFn
	supervisorSelfPIDFn = func() int { return 1 }
	t.Cleanup(func() { supervisorSelfPIDFn = prevSelf })

	var squattedTask string
	snap := map[int]int{}
	for task, m := range meta {
		if squattedTask == "" {
			squattedTask = task
			snap[m.port] = m.pid + 999999 // foreign owner
			continue
		}
		snap[m.port] = m.pid
	}
	calls := withFakePortOwnersSnapshot(t, func() (map[int]int, error) { return snap, nil })

	intent := sweepIntentFromMeta(meta)
	events := runSweepCollectingEvents(t, stateDir, intent, tracker, n)

	if *calls != 1 {
		t.Fatalf("snapshot taken %d times, want 1", *calls)
	}
	if len(events) != 0 {
		t.Fatalf("sweep posted %d events, want 0 (a foreign port owner is observe-only under P2a, not an unconditional restart): %+v", len(events), events)
	}
}
