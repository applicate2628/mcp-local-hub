package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

func TestSupervisorStatusDoesNotReportRunningForDeadRecordedPID(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC))

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return false },
		PortLive: func(port int) bool { return false },
	})
	defer restore()

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0]["state"] == "Running" {
		t.Fatalf("dead recorded pid surfaced as Running: %+v", rows[0])
	}
	if got := rows[0]["current_pid"]; got != 0 {
		t.Fatalf("dead recorded pid current_pid = %v, want 0", got)
	}
	if got := rows[0]["stale_pid"]; got != 22036 {
		t.Fatalf("stale_pid = %v, want 22036", got)
	}
}

func TestSupervisorStatusDaemonsUsesDescriptorIdentityForWorkspaceLSP(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const taskName = `\mcp-local-hub-lsp-deadbeef-go`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "mcp-language-server",
			Daemon:   "lsp-deadbeef-go",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	rows, err := supervisorStatusDaemons(stateDir, NewDaemonRuntimeTracker())
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0]["server"] != "mcp-language-server" {
		t.Fatalf("server = %q, want descriptor Server", rows[0]["server"])
	}
	if rows[0]["daemon"] != "lsp-deadbeef-go" {
		t.Fatalf("daemon = %q, want descriptor Daemon", rows[0]["daemon"])
	}
}

func TestHydrateControllerRunningStatesFromWarmStartSnapshot(t *testing.T) {
	taskName := `\mcp-local-hub-memory-default`
	ctrl := &supervisorController{}

	hydrateControllerRunningStates(ctrl, map[string]bool{taskName: true})

	st, ok := ctrl.GetSMState(taskName)
	if !ok {
		t.Fatalf("SM state not hydrated for %s", taskName)
	}
	if st != api.StRunning {
		t.Fatalf("SM state = %s, want %s", st, api.StRunning)
	}
}

func TestLoadSupervisorCurrentRunningPersistsDeadRecordedPIDAsIdle(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	startedAt := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt,
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error {
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(pid int) bool {
		if pid != 22036 {
			t.Fatalf("liveness checked pid %d, want 22036", pid)
		}
		return false
	}
	defer func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	}()

	got, gotPIDs, err := loadSupervisorCurrentRunning(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if len(got) != 0 || len(gotPIDs) != 0 {
		t.Fatalf("dead recorded pid must not suppress startup spawn; currentRunning=%v pids=%v", got, gotPIDs)
	}
	after, err := api.ReadSupervisorState(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		t.Fatalf("read supervisor-state.json after stale cleanup: %v", err)
	}
	row := after.Daemons[taskName]
	if row.State != "idle" || row.CurrentPID != 0 || row.StartedAt != "" || row.JobProtection != nil {
		t.Fatalf("stale running row not persisted as idle: %+v", row)
	}
}

func TestSupervisorStartupRuntimeDoesNotHydrateStaleCleanedStoppedDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	startedAt := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt,
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error {
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(pid int) bool {
		if pid != 22036 {
			t.Fatalf("liveness checked pid %d, want 22036", pid)
		}
		return false
	}
	defer func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	}()

	tracker, currentRunning, runningPIDs, err := loadSupervisorStartupRuntime(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorStartupRuntime: %v", err)
	}
	if len(currentRunning) != 0 || len(runningPIDs) != 0 {
		t.Fatalf("stale cleaned daemon must not be current-running: currentRunning=%v pids=%v", currentRunning, runningPIDs)
	}
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatalf("tracker entry missing for %s", taskName)
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
		t.Fatalf("tracker hydrated stale running state: %+v", entry)
	}

	now := time.Date(2026, 6, 4, 10, 1, 0, 0, time.UTC)
	stoppedIntent := &api.DaemonIntentFile{Tasks: map[string]api.DaemonIntent{
		taskName: {
			Desired:   api.IntentDesiredStopped,
			Reason:    api.IntentReasonUserDisabled,
			UpdatedAt: now,
		},
	}}
	spawned := false
	r := NewReconciler(func(api.SupervisorDaemon) error {
		spawned = true
		return nil
	}, func(api.SupervisorDaemon) error { return nil })
	r.Reconcile(intent, stoppedIntent, currentRunning, now)
	if spawned {
		t.Fatal("stale cleaned daemon with stopped daemon-intent was respawned")
	}

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0]["state"] == "Running" || rows[0]["state"] == "Restarting" {
		t.Fatalf("stale cleaned daemon surfaced as active: %+v", rows[0])
	}
	if got := rows[0]["current_pid"]; got != 0 {
		t.Fatalf("current_pid = %v, want 0", got)
	}
	if _, ok := rows[0]["stale_pid"]; ok {
		t.Fatalf("stale cleaned daemon reported stale_pid from tracker: %+v", rows[0])
	}

	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restoreProbe := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(int) bool { return false },
		PortLive: func(int) bool { return false },
	})
	defer restoreProbe()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)
	select {
	case ev := <-events:
		t.Fatalf("liveness reposted stale cleaned daemon: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestLoadSupervisorStartupRuntimeTerminatesAliveExpiredUnboundPortBeforeRespawn
// guards the Codex bot #268 r10 P1 fix: a warm-restart entry whose recorded
// PID is still a live identity-verified mcphub process but whose port is
// unbound past the bind grace must be TERMINATED FIRST and respawned exactly
// once — not cleared-as-idle-then-duplicated. The pre-fix behavior cleared
// the live PID, omitted the task from currentRunning, and let the startup
// reconcile spawn a replacement alongside the still-running wrapper
// (duplicate daemon). Now loadSupervisorCurrentRunning KEEPS the live PID
// (state row untouched, task IN currentRunning so reconcile no-ops), and the
// immediate startup liveness sweep drives StRunning + EvManualRestart →
// terminate-first → EvChildExit → single respawn.
func TestLoadSupervisorStartupRuntimeTerminatesAliveExpiredUnboundPortBeforeRespawn(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	// Warm-restart wedged row: a genuinely-stale handoff daemon (alive PID, port
	// never bound) carries an OLD StartedAt — past the P1b startup deadline (60s
	// for this global daemon) — so the immediate first sweep still terminates it
	// at once (the preserved warm-restart behavior). Pre-P1b this was the flat 5s
	// grace + 1s; the deadline is longer now, so the back-date is longer too.
	startedAt := time.Now().UTC().Add(-(supervisorDefaultStartupBindDeadline + time.Second))
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt.Format(time.RFC3339Nano),
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error {
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(pid int) bool { return pid == 22036 }
	restoreProbe := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortLive: func(port int) bool {
			if port != 9123 {
				t.Fatalf("PortLive called with port %d, want 9123", port)
			}
			return false
		},
	})
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
		restoreProbe()
	})

	tracker, currentRunning, runningPIDs, err := loadSupervisorStartupRuntime(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorStartupRuntime: %v", err)
	}
	// The live-but-port-stale entry MUST stay current-running so reconcile
	// no-ops it (no duplicate spawn) and the tracker keeps the live PID for
	// the terminate-first handoff.
	if !currentRunning[taskName] {
		t.Fatalf("alive expired unbound port must stay current-running for terminate-first: %v", currentRunning)
	}
	if runningPIDs[taskName].PID != 22036 {
		t.Fatalf("running PID snapshot = %+v, want live pid 22036", runningPIDs[taskName])
	}
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatalf("tracker entry missing for %s", taskName)
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 22036 {
		t.Fatalf("alive expired unbound port cleared before terminate-first restart: %+v", entry)
	}
	after, err := api.ReadSupervisorState(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		t.Fatalf("read supervisor-state.json after stale scan: %v", err)
	}
	row := after.Daemons[taskName]
	if row.State != "running" || row.CurrentPID != 22036 {
		t.Fatalf("alive expired unbound port row was cleared instead of retained: %+v", row)
	}

	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan int, 2)
	terminated := make(chan int, 1)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("spawn task = %q, want %q", d.TaskName, taskName)
			}
			tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())
			spawned <- 33000
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("terminate task = %q, want %q", d.TaskName, taskName)
			}
			e, ok := tracker.Get(d.TaskName)
			if !ok {
				t.Fatalf("tracker entry missing for %s", d.TaskName)
			}
			terminated <- e.CurrentPID
			return nil
		},
	}
	// Startup hydrates the SM state to StRunning from currentRunning (the
	// same call runSupervise makes at supervise.go before reconcile).
	hydrateControllerRunningStates(ctrl, currentRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Startup reconcile MUST treat the running entry as steady-state — no
	// spawn, no terminate — so it never duplicates the wedged wrapper.
	reconciler := NewReconciler(func(api.SupervisorDaemon) error {
		t.Fatal("startup reconcile spawned a duplicate for the alive port-stale entry")
		return nil
	}, func(api.SupervisorDaemon) error {
		t.Fatal("startup reconcile terminated through the reconcile fan-out instead of the liveness sweep")
		return nil
	})
	reconciler.EventLoop = loop
	reconciler.Reconcile(intent, nil, currentRunning, time.Now().UTC())

	// The immediate startup liveness sweep (the same call
	// startSupervisorLivenessMonitor makes before its first ticker tick)
	// drives the terminate-first restart.
	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	// Terminate-first ordering: the live stale PID is terminated BEFORE any
	// respawn (no spawn-over-live duplicate).
	select {
	case pid := <-terminated:
		if pid != 22036 {
			t.Fatalf("terminated pid = %d, want live pid 22036", pid)
		}
	case pid := <-spawned:
		t.Fatalf("startup sweep spawned pid %d before terminating the live stale pid", pid)
	case <-time.After(2 * time.Second):
		t.Fatal("startup sweep did not terminate the alive expired unbound port")
	}

	// FOREIGN warm-start PID (hydrated via hydrateControllerRunningStates, never
	// own-spawned by this controller, terminate returns nil without marking the
	// tracker): the controller synthesizes the follow-up EvChildExit after the
	// successful terminate (#268 r11 P2), so the queued respawn completes
	// automatically — no manual EvChildExit post.
	select {
	case pid := <-spawned:
		if pid != 33000 {
			t.Fatalf("respawn pid = %d, want replacement 33000", pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminate-first startup restart wedged in StExiting: synthetic EvChildExit never drove the queued respawn")
	}

	// Exactly one respawn — the synthetic exit must not double-fire.
	select {
	case pid := <-spawned:
		t.Fatalf("startup restart spawned a duplicate replacement pid %d", pid)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestLoadSupervisorStartupRuntimeClearsPIDDeadAtPortStage is the
// regression guard for the #268 r10 P1 fix's NOT-alive branch: when the
// recorded PID passes the outer identity check but the inner liveness probe
// finds it DEAD at the port stage (a TOCTOU race — the process died between
// the two checks), the reason is pid_dead (NOT a live-PID reason), so the
// entry must be cleared-as-idle and omitted from currentRunning. There is no
// live process to terminate; the startup reconcile respawns it directly. The
// live-retain branch must NOT fire for a dead PID.
func TestLoadSupervisorStartupRuntimeClearsPIDDeadAtPortStage(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	startedAt := time.Now().UTC().Add(-(supervisorPortBindGrace + time.Second))
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt.Format(time.RFC3339Nano),
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	// Outer check: identity VERIFIED (nil) so the scan reaches the port
	// stage without clearing on identity.
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error { return nil }
	currentRunningIsPIDAliveFn = func(int) bool { return true }
	// Inner liveness probe: PID is now DEAD → supervisorDaemonEntryLive
	// returns reason pid_dead at the PIDAlive gate (before the port probe).
	restoreProbe := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(int) bool { return false },
		PortLive: func(int) bool { return false },
	})
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
		restoreProbe()
	})

	tracker, currentRunning, runningPIDs, err := loadSupervisorStartupRuntime(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorStartupRuntime: %v", err)
	}
	if len(currentRunning) != 0 || len(runningPIDs) != 0 {
		t.Fatalf("dead PID at port stage must not be retained as running: currentRunning=%v pids=%v", currentRunning, runningPIDs)
	}
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatalf("tracker entry missing for %s", taskName)
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
		t.Fatalf("dead PID at port stage not cleared before reconcile: %+v", entry)
	}
	after, err := api.ReadSupervisorState(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		t.Fatalf("read supervisor-state.json: %v", err)
	}
	row := after.Daemons[taskName]
	if row.State != "idle" || row.CurrentPID != 0 || row.StartedAt != "" {
		t.Fatalf("dead PID row not persisted as idle: %+v", row)
	}

	// Reconcile respawns the cleared entry directly (no terminate — there
	// is no live process).
	spawned := false
	reconciler := NewReconciler(func(d api.SupervisorDaemon) error {
		if d.TaskName != taskName {
			t.Fatalf("spawn task = %q, want %q", d.TaskName, taskName)
		}
		spawned = true
		return nil
	}, func(api.SupervisorDaemon) error {
		t.Fatal("dead PID at port stage must not terminate")
		return nil
	})
	reconciler.Reconcile(&api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}, nil, currentRunning, time.Now().UTC())
	if !spawned {
		t.Fatal("dead PID at port stage was not respawned by startup reconcile")
	}
}

func TestLoadSupervisorStartupRuntimeKeepsWithinGraceUnboundPortRunning(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	startedAt := time.Now().UTC()
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt.Format(time.RFC3339Nano),
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error {
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(pid int) bool { return pid == 22036 }
	restoreProbe := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortLive: func(port int) bool {
			if port != 9123 {
				t.Fatalf("PortLive called with port %d, want 9123", port)
			}
			return false
		},
	})
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
		restoreProbe()
	})

	_, currentRunning, runningPIDs, err := loadSupervisorStartupRuntime(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorStartupRuntime: %v", err)
	}
	if !currentRunning[taskName] {
		t.Fatalf("within-grace unbound port must stay current-running: %v", currentRunning)
	}
	if runningPIDs[taskName].PID != 22036 {
		t.Fatalf("running PID snapshot = %+v, want pid 22036", runningPIDs[taskName])
	}

	spawned := false
	reconciler := NewReconciler(func(api.SupervisorDaemon) error {
		spawned = true
		return nil
	}, func(api.SupervisorDaemon) error {
		t.Fatal("within-grace unbound port must not terminate")
		return nil
	})
	reconciler.Reconcile(&api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}, nil, currentRunning, time.Now().UTC())
	if spawned {
		t.Fatal("within-grace unbound port was respawned")
	}
}

func TestSupervisorLivenessSweepPostsChildExitForDeadRunningPID(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return false },
		PortLive: func(port int) bool { return false },
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvChildExit || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvChildExit for %s", ev, taskName)
		}
		if ev.Body["pid"] != 22036 {
			t.Fatalf("event pid = %v, want 22036", ev.Body["pid"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvChildExit for stale running PID")
	}
}

func TestSupervisorLivenessSweepPostsChildExitForDeadPIDWithForeignListener(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return false },
		PortOwnerPID: func(port int) (int, bool, error) {
			return 44000, true, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvChildExit || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvChildExit for %s", ev, taskName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvChildExit for dead PID with foreign listener")
	}
}

func TestSupervisorLivenessSweepRestartsAlivePIDWithForeignPortOwner(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortOwnerPID: func(port int) (int, bool, error) {
			if port != 9123 {
				t.Fatalf("PortOwnerPID called with port %d, want 9123", port)
			}
			return 44000, true, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvManualRestart || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvManualRestart for %s", ev, taskName)
		}
		if ev.Body["reason"] != "port_owner_mismatch" {
			t.Fatalf("event reason = %v, want port_owner_mismatch", ev.Body["reason"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvManualRestart for foreign port owner")
	}
}

func TestSupervisorLivenessSweepDoesNotRestartAlivePIDOwningPort(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortOwnerPID: func(port int) (int, bool, error) {
			return 22036, true, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		t.Fatalf("liveness sweep posted event for PID owning its port: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSupervisorLivenessSweepRejectsRecycledPID(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error {
			return process.ErrProcessIdentityMismatch
		},
		PortOwnerPID: func(port int) (int, bool, error) {
			return 22036, true, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvChildExit || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvChildExit for %s", ev, taskName)
		}
		if ev.Body["reason"] != "pid_identity_mismatch" {
			t.Fatalf("event reason = %v, want pid_identity_mismatch", ev.Body["reason"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvChildExit for recycled PID")
	}
}

func TestSupervisorLivenessSweepRejectsSelfOwnedPort(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	prevSelf := supervisorSelfPIDFn
	supervisorSelfPIDFn = func() int { return 22036 }
	defer func() { supervisorSelfPIDFn = prevSelf }()

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortOwnerPID: func(port int) (int, bool, error) {
			return 22036, true, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvManualRestart || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvManualRestart for %s", ev, taskName)
		}
		if ev.Body["reason"] != "port_owner_self" {
			t.Fatalf("event reason = %v, want port_owner_self", ev.Body["reason"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvManualRestart for self-owned port")
	}
}

func TestSupervisorLivenessSweepRestartsAlivePIDWithUnboundPort(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortLive: func(port int) bool {
			if port != 9123 {
				t.Fatalf("PortLive called with port %d, want 9123", port)
			}
			return false
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvManualRestart || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvManualRestart for %s", ev, taskName)
		}
		if ev.Body["reason"] != "port_unbound" {
			t.Fatalf("event reason = %v, want port_unbound", ev.Body["reason"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvManualRestart for unbound live port")
	}
}

func TestSupervisorLivenessSweepKeepsLivePIDBeforeRestartPost(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PortLive: func(port int) bool {
			if port != 9123 {
				t.Fatalf("PortLive called with port %d, want 9123", port)
			}
			return false
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvManualRestart || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvManualRestart for %s", ev, taskName)
		}
		entry, ok := tracker.Get(taskName)
		if !ok {
			t.Fatalf("tracker entry missing for %s", taskName)
		}
		if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 22036 {
			t.Fatalf("liveness cleared live wedged runtime before terminate-first restart: %+v", entry)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not post EvManualRestart for unbound live port")
	}
}

func TestSupervisorLivenessRestartForAliveUnboundPortTerminatesBeforeRespawn(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan int, 2)
	terminated := make(chan int, 1)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("spawn task = %q, want %q", d.TaskName, taskName)
			}
			tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())
			spawned <- 33000
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("terminate task = %q, want %q", d.TaskName, taskName)
			}
			entry, ok := tracker.Get(d.TaskName)
			if !ok {
				t.Fatalf("tracker entry missing for %s", d.TaskName)
			}
			terminated <- entry.CurrentPID
			return nil
		},
	}
	ctrl.smStates.Store(taskName, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(proof process.PIDIdentityProof) error {
			if proof.PID != 22036 {
				t.Fatalf("PIDIdentity pid = %d, want 22036", proof.PID)
			}
			return nil
		},
		PortOwnerPID: func(port int) (int, bool, error) {
			if port != 9123 {
				t.Fatalf("PortOwnerPID called with port %d, want 9123", port)
			}
			return 0, false, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	// Terminate-first ordering: the old live PID is terminated BEFORE any
	// respawn (no spawn-over-live).
	select {
	case pid := <-terminated:
		if pid != 22036 {
			t.Fatalf("terminated pid = %d, want old live pid 22036", pid)
		}
	case pid := <-spawned:
		t.Fatalf("liveness spawned pid %d before terminating old live pid", pid)
	case <-time.After(2 * time.Second):
		t.Fatal("liveness restart did not terminate old live pid")
	}

	// The terminate closure here returns nil WITHOUT marking the tracker
	// terminated and the daemon was never own-spawned by this controller, so
	// it is a FOREIGN warm-start PID with no cmd.Wait goroutine. The
	// controller SYNTHESIZES the follow-up EvChildExit after the successful
	// terminate (#268 r11 P2), so the queued respawn fires automatically — no
	// manual EvChildExit post is required.
	select {
	case pid := <-spawned:
		if pid != 33000 {
			t.Fatalf("spawned pid = %d, want replacement pid 33000", pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness terminate-first restart wedged in StExiting: synthetic EvChildExit never drove the queued respawn")
	}

	// Exactly one respawn — the synthetic exit must not double-fire.
	select {
	case pid := <-spawned:
		t.Fatalf("liveness spawned duplicate replacement pid %d", pid)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestSupervisorLivenessRestartWithClearedPIDSpawnsThroughControllerWithoutTerminate(t *testing.T) {
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	tracker.MarkExited(taskName)
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan struct{}, 1)
	terminated := make(chan struct{}, 1)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("spawn task = %q, want %q", d.TaskName, taskName)
			}
			tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())
			spawned <- struct{}{}
			return nil
		},
		terminate: func(api.SupervisorDaemon) error {
			terminated <- struct{}{}
			return nil
		},
	}
	ctrl.smStates.Store(taskName, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	loop.Post(api.LoopEvent{
		Kind:     api.EvManualRestart,
		TaskName: taskName,
		Body: map[string]any{
			"reason":                                supervisorLivenessReasonPortUnbound,
			supervisorLivenessRuntimeClearedBodyKey: true,
		},
	})

	select {
	case <-terminated:
		t.Fatal("cleared liveness restart attempted to terminate stale PID")
	case <-spawned:
	case <-time.After(2 * time.Second):
		t.Fatal("cleared liveness restart did not spawn through controller")
	}
}

// TestSupervisorLivenessForeignWarmStartPIDSynthesizesChildExitAndRespawns is
// the Codex bot #268 r11 P2 fix guard. A FOREIGN warm-start PID inherited from
// a previous supervisor (smState hydrated to StRunning by
// hydrateControllerRunningStates, the live PID still in the tracker) that is
// alive-but-port-stale must complete its restart: sweep -> EvManualRestart ->
// StExiting -> terminate (succeeds) -> the controller SYNTHESIZES the follow-up
// EvChildExit (no cmd.Wait goroutine exists for a foreign PID in this process)
// -> queued respawn fires exactly once. Pre-fix the SM wedged in StExiting with
// queued_action=respawn never consumed. Note the test posts NO manual
// EvChildExit — the synthetic one is the whole point.
func TestSupervisorLivenessForeignWarmStartPIDSynthesizesChildExitAndRespawns(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	// Hydrate the FOREIGN live PID exactly as loadSupervisorCurrentRunning
	// retains it for the warm-start handoff. The controller never spawned it,
	// so it is NOT in ownSpawned.
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan int, 4)
	terminated := make(chan int, 1)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("spawn task = %q, want %q", d.TaskName, taskName)
			}
			tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())
			spawned <- 33000
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			if d.TaskName != taskName {
				t.Fatalf("terminate task = %q, want %q", d.TaskName, taskName)
			}
			e, ok := tracker.Get(d.TaskName)
			if !ok {
				t.Fatalf("tracker entry missing for %s", d.TaskName)
			}
			// Mirror the production terminate fn: the targeted PID is gone
			// after a successful terminate.
			tracker.MarkTerminated(d.TaskName)
			terminated <- e.CurrentPID
			return nil
		},
	}
	// Warm-start hydration: SM state is StRunning for the foreign PID, the
	// same call runSupervise makes via hydrateControllerRunningStates.
	hydrateControllerRunningStates(ctrl, map[string]bool{taskName: true})
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Live PID, port owned by nobody (unbound) past the bind grace -> a
	// live-PID restart reason (port_unbound) -> EvManualRestart -> StExiting.
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(proof process.PIDIdentityProof) error {
			if proof.PID != 22036 {
				t.Fatalf("PIDIdentity pid = %d, want 22036", proof.PID)
			}
			return nil
		},
		PortOwnerPID: func(port int) (int, bool, error) {
			if port != 9123 {
				t.Fatalf("PortOwnerPID called with port %d, want 9123", port)
			}
			return 0, false, nil
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	// Terminate fires FIRST against the foreign live PID; no respawn yet.
	select {
	case pid := <-terminated:
		if pid != 22036 {
			t.Fatalf("terminated pid = %d, want foreign live pid 22036", pid)
		}
	case pid := <-spawned:
		t.Fatalf("respawned pid %d before terminating the foreign live pid", pid)
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not terminate the foreign warm-start PID")
	}

	// The SYNTHETIC EvChildExit (no manual post here) must drive the queued
	// respawn exactly once.
	select {
	case pid := <-spawned:
		if pid != 33000 {
			t.Fatalf("respawn pid = %d, want replacement 33000", pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreign warm-start restart wedged in StExiting: synthetic EvChildExit never drove the queued respawn")
	}

	// Exactly one respawn — the synthetic exit must not double-fire.
	select {
	case pid := <-spawned:
		t.Fatalf("foreign warm-start restart spawned a duplicate replacement pid %d", pid)
	case <-time.After(150 * time.Millisecond):
	}

	st, _ := ctrl.GetSMState(taskName)
	if st != api.StRunning {
		t.Fatalf("after synthetic respawn the SM state = %s, want %s", st, api.StRunning)
	}
}

// TestSupervisorLivenessOwnSpawnedRestartUsesRealChildExitNoSyntheticDoublePost
// is the negative guard for the #268 r11 P2 fix: an OWN-spawned child (the
// controller fired its spawn closure, so a real cmd.Wait posts EvChildExit on
// exit) must NOT get a synthetic EvChildExit on its terminate. It restarts only
// after the REAL exit event, and the spawn closure fires exactly twice total
// (initial + one respawn) — never a third from a stray synthetic post.
func TestSupervisorLivenessOwnSpawnedRestartUsesRealChildExitNoSyntheticDoublePost(t *testing.T) {
	taskName := `\mcp-local-hub-memory-default`
	descriptor := api.SupervisorDaemon{TaskName: taskName, Server: "memory", Daemon: "default", Port: 9123}
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{descriptor}}
	tracker := NewDaemonRuntimeTracker()
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan int, 4)
	terminated := make(chan int, 2)
	var spawnPID int32 = 40000
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			pid := int(atomic.AddInt32(&spawnPID, 1))
			tracker.MarkSpawned(d.TaskName, pid, time.Now().UTC())
			spawned <- pid
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			e, _ := tracker.Get(d.TaskName)
			tracker.MarkTerminated(d.TaskName)
			terminated <- e.CurrentPID
			return nil
		},
	}
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Own-spawn the daemon via EvStart -> spawn fires -> EvHealthOK -> StRunning,
	// AND the controller marks it own-spawned.
	loop.Post(api.LoopEvent{Kind: api.EvStart, TaskName: taskName})
	select {
	case <-spawned:
	case <-time.After(2 * time.Second):
		t.Fatal("EvStart did not spawn the own daemon")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := ctrl.GetSMState(taskName); st == api.StRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Manual restart of the OWN-spawned daemon -> StExiting -> terminate.
	loop.Post(api.LoopEvent{Kind: api.EvManualRestart, TaskName: taskName})
	select {
	case <-terminated:
	case <-time.After(2 * time.Second):
		t.Fatal("EvManualRestart did not terminate the own daemon")
	}

	// No synthetic EvChildExit for an own-spawned daemon: nothing respawns
	// until the REAL cmd.Wait exit event arrives.
	select {
	case pid := <-spawned:
		t.Fatalf("own-spawned restart synthesized a respawn (pid %d) before the real EvChildExit", pid)
	case <-time.After(200 * time.Millisecond):
	}

	// Simulate the real cmd.Wait EvChildExit; queued respawn fires once.
	loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: taskName})
	select {
	case <-spawned:
	case <-time.After(2 * time.Second):
		t.Fatal("own-spawned restart did not respawn after the real EvChildExit")
	}

	// Exactly two spawns total (initial + one respawn). A third would mean a
	// stray synthetic EvChildExit double-fired.
	select {
	case pid := <-spawned:
		t.Fatalf("own-spawned restart spawned a third time (pid %d) — synthetic double-post leaked", pid)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSupervisorLivenessForeignTerminateFailureNoSynthesizeNoRespawn guards the
// terminate-failure branch: when the foreign warm-start PID's terminate
// RETURNS AN ERROR (the PID may still be alive), the controller must NOT
// synthesize an EvChildExit and must NOT respawn over a possibly-live process.
// The wedged entry is left for the next liveness sweep to retry.
func TestSupervisorLivenessForeignTerminateFailureNoSynthesizeNoRespawn(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan int, 2)
	terminated := make(chan int, 1)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			spawned <- 33000
			return nil
		},
		terminate: func(d api.SupervisorDaemon) error {
			e, _ := tracker.Get(d.TaskName)
			terminated <- e.CurrentPID
			// Terminate FAILED — the live PID may still be running.
			return errors.New("terminate failed: access denied")
		},
	}
	hydrateControllerRunningStates(ctrl, map[string]bool{taskName: true})
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error { return nil },
		PortOwnerPID: func(port int) (int, bool, error) {
			return 0, false, nil // unbound -> port_unbound, live-PID restart reason
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	// Terminate is attempted against the foreign live PID and fails.
	select {
	case pid := <-terminated:
		if pid != 22036 {
			t.Fatalf("terminated pid = %d, want foreign live pid 22036", pid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("liveness sweep did not attempt to terminate the foreign warm-start PID")
	}

	// No synthesize on terminate failure -> NO respawn over the possibly-live
	// process. The SM stays in StExiting awaiting the next sweep.
	select {
	case pid := <-spawned:
		t.Fatalf("terminate failure still respawned (pid %d) over a possibly-live foreign process", pid)
	case <-time.After(300 * time.Millisecond):
	}

	st, _ := ctrl.GetSMState(taskName)
	if st != api.StExiting {
		t.Fatalf("after terminate failure the SM state = %s, want %s (left for retry)", st, api.StExiting)
	}
}

// TestSupervisorLivenessSweepClearedRestartPerformsClearOnLoop is the Codex
// deep-sec PR #268 Conc-F2 guard for the single-writer relocation: a dead-PID
// restart reason carries the clear instruction in the event body, and the
// actual tracker MarkExited + supervisor-state.json persist happen on the
// event-loop goroutine (inside handleLoopEvent), NOT in the sweep goroutine.
// The sweep itself performs zero tracker mutations. The handler clears the
// stale recorded PID, then the SM treats the verified-idle state as spawnable
// and spawns WITHOUT issuing a terminate against the dead PID. The on-disk
// row reflects the loop-side clear.
func TestSupervisorLivenessSweepClearedRestartPerformsClearOnLoop(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(16)
	spawned := make(chan struct{}, 1)
	terminated := make(chan struct{}, 1)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		statePath:           statePath,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())
			// Mirror the production spawn closure, which persists the new
			// running state to supervisor-state.json after MarkSpawned.
			_ = tracker.PersistTo(statePath)
			spawned <- struct{}{}
			return nil
		},
		terminate: func(api.SupervisorDaemon) error {
			terminated <- struct{}{}
			return nil
		},
	}
	ctrl.smStates.Store(taskName, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// Post the cleared-restart event the sweep would emit for a dead-PID
	// restart reason. The tracker is NOT pre-cleared here (unlike the older
	// test) — proving the loop-side clear is what invalidates the stale PID.
	loop.Post(api.LoopEvent{
		Kind:     api.EvManualRestart,
		TaskName: taskName,
		Body: map[string]any{
			"reason":                                supervisorLivenessReasonPortUnbound,
			supervisorLivenessRuntimeClearedBodyKey: true,
		},
	})

	select {
	case <-terminated:
		t.Fatal("cleared liveness restart issued a terminate against the dead PID")
	case <-spawned:
	case <-time.After(2 * time.Second):
		t.Fatal("cleared liveness restart did not spawn through the controller")
	}

	// The loop-side MarkExited+persist updated the on-disk row: the stale PID
	// 22036 was cleared before the respawn recorded 33000.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after, err := api.ReadSupervisorState(statePath)
		if err != nil {
			t.Fatalf("read supervisor-state.json: %v", err)
		}
		if after.Daemons[taskName].CurrentPID == 33000 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	after, _ := api.ReadSupervisorState(statePath)
	t.Fatalf("on-disk row never reflected loop-side clear+respawn: %+v", after.Daemons[taskName])
}

// TestSupervisorLivenessSweepConcurrentWithHandlerNoRace is the Conc-F2
// -race guard: the 5s liveness sweep runs in its OWN goroutine while the
// event-loop handler concurrently mutates the same task's tracker entry and
// persists supervisor-state.json. With the fix the sweep performs zero
// tracker mutations and no off-loop persist, so there is no read/modify/
// persist sequence racing the handler's MarkSpawned/MarkExited+persist for
// the same task. Run under `go test -race` over this + the handler/spawn set
// to detect any reintroduced off-loop tracker write. The assertion is simply
// that the run completes without a detected data race and the final on-disk
// state is internally consistent (parseable, version set).
func TestSupervisorLivenessSweepConcurrentWithHandlerNoRace(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("seed persist: %v", err)
	}
	intentCache := newIntentCache()
	intentCache.Refresh(intent)
	loop := api.NewEventLoop(64)
	ctrl := &supervisorController{
		intentCache:         intentCache,
		eventLoop:           loop,
		tracker:             tracker,
		statePath:           statePath,
		daemonIntent:        newDaemonIntentCache(),
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
		spawn: func(d api.SupervisorDaemon) error {
			tracker.MarkSpawned(d.TaskName, 33000, time.Now().UTC())
			return nil
		},
		terminate: func(api.SupervisorDaemon) error { return nil },
	}
	ctrl.smStates.Store(taskName, api.StRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	// The sweep observes a dead PID (PIDAlive false) -> EvChildExit path
	// (non-restart reason) -> the handler drives the SM. The probe is shared
	// process-global state, so set it once for the whole concurrent run.
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(int) bool { return false },
		PortLive: func(int) bool { return false },
	})
	defer restore()

	var wg sync.WaitGroup
	wg.Add(2)
	// Sweep goroutine: hammers sweepSupervisorLivenessOnce (the same call the
	// 5s monitor makes) on its own goroutine, exactly as production does.
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)
		}
	}()
	// Handler-feeder goroutine: concurrently drives spawn/exit transitions for
	// the SAME task so the loop goroutine mutates the tracker + persists while
	// the sweep runs.
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			loop.Post(api.LoopEvent{Kind: api.EvManualRestart, TaskName: taskName})
			loop.Post(api.LoopEvent{Kind: api.EvChildExit, TaskName: taskName})
		}
	}()
	wg.Wait()

	// Deterministically drain the loop instead of a fixed sleep (the "natural
	// window" anti-pattern flagged by the Race-window assertion discipline):
	// post the TEST-ONLY evReapBarrier at the tail of the main channel AFTER
	// both producers finished, then block until the loop signals it. The loop
	// priority-drains self-posts before each main-channel read, so when the
	// barrier fires every prior event — and every cascaded self-post, including
	// the last on-loop supervisor-state.json persist — has completed and
	// happens-before this read. The post-barrier read is then race-free, so the
	// on-disk state is internally consistent (no torn/partial write surfaced as
	// a parse error or lost version) without a probabilistic settle delay.
	barrierDone := make(chan struct{})
	loop.Post(api.LoopEvent{
		Kind: evReapBarrier,
		Body: map[string]any{reapBarrierResultBodyKey: barrierDone},
	})
	select {
	case <-barrierDone:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not drain within 5s (wedged?)")
	}
	final, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("final supervisor-state.json unreadable (torn write / race corruption): %v", err)
	}
	if final.Version == 0 {
		t.Fatalf("final supervisor-state.json lost version field: %+v", final)
	}
}

// --- Codex bot #268 P2 (supervise_liveness.go:261): a port-owner PROBE ERROR
// must NOT trigger a restart. ----------------------------------------------

// TestSupervisorDaemonEntryLive_PortOwnerProbeError_TCPUp_ReturnsLive is the
// unit guard for the degrade path: when the OS-level port-owner probe ERRORS
// (netstat could not run) but the TCP loopback probe confirms the port is
// bound+answering, supervisorDaemonEntryLive returns LIVE — a probe error is
// not proof of a dead daemon, and the TCP answer positively confirms the
// identity-verified PID is serving.
func TestSupervisorDaemonEntryLive_PortOwnerProbeError_TCPUp_PastGrace_StillUnverified(t *testing.T) {
	// deep-sec #268 round-6: an owner-probe error past grace must NOT report a
	// clean live just because SOME listener answers on the port — it could be a
	// DIFFERENT process while the tracked PID is merely still alive. It must
	// surface port_owner_unverified (warn, no restart), not suppress the
	// ambiguity. round-2 still holds: port_owner_unverified is excluded from
	// supervisorLivenessReasonNeedsRestart, so there is no fleet restart loop.
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(int) bool { return true },
		PIDIdentity: func(process.PIDIdentityProof) error { return process.ErrProcessIdentityUnsupported },
		PortLive:    func(int) bool { return true }, // a (possibly FOREIGN) listener answers
		PortOwnerPID: func(int) (int, bool, error) {
			return 0, false, errors.New("netstat -ano failed: access denied")
		},
	})
	defer restore()

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Port: 9123}
	entry := DaemonRuntimeEntry{
		State:      daemonRuntimeStateRunning,
		CurrentPID: 22036,
		StartedAt:  time.Now().UTC().Add(-time.Minute), // well past bind grace
	}
	live, reason := supervisorDaemonEntryLive(d, entry, time.Now().UTC())
	if live {
		t.Fatalf("owner-probe error past grace must NOT report live just because a (possibly foreign) listener answers")
	}
	if reason != supervisorLivenessReasonPortOwnerUnverified {
		t.Fatalf("reason = %q, want port_owner_unverified", reason)
	}
}

// TestSupervisorDaemonEntryLive_PortOwnerProbeError_TCPDownPastGrace_Unverified
// guards the genuinely-ambiguous tail: owner probe ERRORS and the TCP fallback
// ALSO fails to confirm the port, past the bind grace. supervisorDaemonEntryLive
// returns (false, port_owner_unverified) — NOT a confirmed mismatch — and that
// reason is deliberately excluded from supervisorLivenessReasonNeedsRestart so
// the sweep never restarts on it.
func TestSupervisorDaemonEntryLive_PortOwnerProbeError_TCPDownPastGrace_Unverified(t *testing.T) {
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(int) bool { return true },
		PIDIdentity: func(process.PIDIdentityProof) error { return process.ErrProcessIdentityUnsupported },
		PortLive:    func(int) bool { return false }, // TCP cannot confirm either
		PortOwnerPID: func(int) (int, bool, error) {
			return 0, false, errors.New("netstat -ano failed: access denied")
		},
	})
	defer restore()

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Port: 9123}
	entry := DaemonRuntimeEntry{
		State:      daemonRuntimeStateRunning,
		CurrentPID: 22036,
		// Past the P1b default startup deadline (60s) for this global daemon so
		// the probe-error-past-grace path fires (P1b lengthened the pre-first-bind
		// window from the flat 5s).
		StartedAt: time.Now().UTC().Add(-(supervisorDefaultStartupBindDeadline + time.Second)),
	}
	live, reason := supervisorDaemonEntryLive(d, entry, time.Now().UTC())
	if live {
		t.Fatalf("owner-probe error + TCP down past grace = live; want not-live with the unverified reason")
	}
	if reason != supervisorLivenessReasonPortOwnerUnverified {
		t.Fatalf("reason = %q; want %q", reason, supervisorLivenessReasonPortOwnerUnverified)
	}
	if supervisorLivenessReasonNeedsRestart(reason) {
		t.Fatalf("%q is a needs-restart reason; want excluded (a probe error must not restart)", reason)
	}
}

// TestSupervisorDaemonEntryLive_PortOwnerProbeError_WithinGrace_ReturnsLive: a
// probe error while the daemon is still inside the port-bind grace (a fresh
// spawn that may not have bound yet) is live regardless of TCP, mirroring the
// unbound-within-grace rule.
func TestSupervisorDaemonEntryLive_PortOwnerProbeError_WithinGrace_ReturnsLive(t *testing.T) {
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(int) bool { return true },
		PIDIdentity: func(process.PIDIdentityProof) error { return process.ErrProcessIdentityUnsupported },
		PortLive:    func(int) bool { return false },
		PortOwnerPID: func(int) (int, bool, error) {
			return 0, false, errors.New("netstat -ano failed: access denied")
		},
	})
	defer restore()

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Port: 9123}
	entry := DaemonRuntimeEntry{
		State:      daemonRuntimeStateRunning,
		CurrentPID: 22036,
		StartedAt:  time.Now().UTC(), // just spawned, within grace
	}
	live, reason := supervisorDaemonEntryLive(d, entry, time.Now().UTC())
	if !live {
		t.Fatalf("owner-probe error within bind grace = not live (reason %q); want live", reason)
	}
}

// TestSupervisorLivenessReasonNeedsRestart_ProbeErrorExcluded pins the
// invariant table: confirmed problems restart; the probe-error reason does not.
func TestSupervisorLivenessReasonNeedsRestart_ProbeErrorExcluded(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{supervisorLivenessReasonPortUnbound, true},
		{supervisorLivenessReasonPortOwnerMismatch, true},
		{supervisorLivenessReasonPortOwnerSelf, true},
		{supervisorLivenessReasonPortOwnerUnverified, false}, // PROBE ERROR — no restart
		{supervisorLivenessReasonPIDDead, false},
		{supervisorLivenessReasonMissingPID, false},
	}
	for _, tc := range cases {
		if got := supervisorLivenessReasonNeedsRestart(tc.reason); got != tc.want {
			t.Fatalf("NeedsRestart(%q) = %v; want %v", tc.reason, got, tc.want)
		}
	}
}

// TestSupervisorLivenessSweepNoRestartOnPortOwnerProbeErrorTCPUp is the core
// anti-loop guard: a healthy port-bearing daemon whose OS-level owner probe
// FAILS (e.g. netstat policy-blocked) but whose port answers over TCP must NOT
// be restarted — the sweep posts NO event (no EvManualRestart, no EvChildExit),
// so the 5s cadence cannot loop the fleet (Codex bot #268 P2).
func TestSupervisorLivenessSweepNoRestartOnPortOwnerProbeErrorTCPUp(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error { return nil },
		PortLive:    func(int) bool { return true }, // TCP confirms healthy
		PortOwnerPID: func(int) (int, bool, error) {
			return 0, false, errors.New("netstat -ano failed: access denied")
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		t.Fatalf("port-owner probe error on a TCP-healthy daemon posted %+v; want NO event (no restart loop)", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSupervisorLivenessSweepObservesProbeErrorTCPDownWithoutRestart guards the
// ambiguous tail in the SWEEP: owner probe error + TCP down + past grace must
// emit an observable warn (daemon-port-owner-unverified) but post NEITHER an
// EvManualRestart NOR an EvChildExit — a probe failure is not proof the daemon
// is dead OR foreign-owned, so it is observed, not acted on.
func TestSupervisorLivenessSweepObservesProbeErrorTCPDownWithoutRestart(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	auditLog, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer auditLog.Close()
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	// Back-date past the P1b default startup deadline (60s) so the daemon is
	// unambiguously past the first-bind grace for a global (non-serena) daemon —
	// the probe-error-past-grace path this test exercises. (Pre-P1b this used the
	// flat 5s grace; P1b lengthened the pre-first-bind window, so the back-date
	// must clear the longer deadline.)
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-(supervisorDefaultStartupBindDeadline + time.Second)))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error { return nil },
		PortLive:    func(int) bool { return false }, // TCP cannot confirm
		PortOwnerPID: func(int) (int, bool, error) {
			return 0, false, errors.New("netstat -ano failed: access denied")
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, auditLog, nil)

	// No event posted — neither restart nor teardown.
	select {
	case ev := <-events:
		t.Fatalf("probe error (TCP down, past grace) posted %+v; want NO event (probe error must not restart or tear down)", ev)
	case <-time.After(300 * time.Millisecond):
	}

	// The observable warn fired.
	body, err := readSupervisorEventsLog(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	if !strings.Contains(body, "daemon-port-owner-unverified") {
		t.Fatalf("observable warn daemon-port-owner-unverified not emitted; log:\n%s", body)
	}
	// And it did NOT mislabel the daemon as a generic stale/restart candidate.
	if strings.Contains(body, "daemon-running-state-stale") {
		t.Fatalf("probe error mislabeled as daemon-running-state-stale (a restart/teardown reason); log:\n%s", body)
	}
}

// TestSupervisorLivenessSweepConfirmedOwnerMismatchStillRestarts is the
// negative control: a CONFIRMED owner mismatch (a DIFFERENT live PID owns the
// port, no probe error) must STILL route to EvManualRestart with reason
// port_owner_mismatch. The #268 P2 fix narrows ONLY the probe-error path; a
// real foreign owner is unchanged.
func TestSupervisorLivenessSweepConfirmedOwnerMismatchStillRestarts(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	taskName := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 22036, time.Now().UTC().Add(-time.Minute))
	loop := api.NewEventLoop(16)
	events := make(chan api.LoopEvent, 1)
	loop.RegisterHandler(func(e api.LoopEvent) { events <- e })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error { return nil },
		PortLive:    func(int) bool { return true },
		PortOwnerPID: func(int) (int, bool, error) {
			return 44000, true, nil // a DIFFERENT live PID owns the port (no err)
		},
	})
	defer restore()

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil, nil)

	select {
	case ev := <-events:
		if ev.Kind != api.EvManualRestart || ev.TaskName != taskName {
			t.Fatalf("event = %+v, want EvManualRestart for %s", ev, taskName)
		}
		if ev.Body["reason"] != supervisorLivenessReasonPortOwnerMismatch {
			t.Fatalf("event reason = %v, want %s", ev.Body["reason"], supervisorLivenessReasonPortOwnerMismatch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirmed owner mismatch did not post EvManualRestart")
	}
}

func TestDefaultSupervisorLivenessProbeUsesPortOwnerVerification(t *testing.T) {
	probe := defaultSupervisorLivenessProbe()
	switch runtime.GOOS {
	case "windows", "linux":
		// These platforms have a real OS-level socket-owner proof (Windows
		// netstat, Linux /proc), so liveness MUST use it — TCP-only liveness
		// trusts a foreign listener squatting the daemon port.
		if probe.PortOwnerPID == nil {
			t.Fatalf("on %s the default liveness probe must verify socket owner; TCP-only liveness trusts foreign listeners", runtime.GOOS)
		}
	default:
		// macOS and other POSIX targets fail closed at api.LoopbackPortOwnerPID;
		// installing it would block the PortLive TCP fallback and classify every
		// live daemon port_owner_unverified forever (Codex bot #271 P2), so
		// PortOwnerPID MUST stay nil there to preserve TCP liveness.
		if probe.PortOwnerPID != nil {
			t.Fatalf("on %s PortOwnerPID must be nil so the PortLive TCP fallback runs", runtime.GOOS)
		}
	}
}

func TestSupervisorDaemonEntryLiveWithoutOwnerProbeOnlyUsedForTestSeams(t *testing.T) {
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive:    func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(process.PIDIdentityProof) error { return nil },
		PortLive:    func(int) bool { return true },
		// Deliberately nil PortOwnerPID to document the legacy vulnerable seam.
	})
	defer restore()

	live, reason := supervisorDaemonEntryLive(api.SupervisorDaemon{Port: 9123}, DaemonRuntimeEntry{
		CurrentPID: 22036,
		StartedAt:  time.Now().UTC().Add(-time.Minute),
	}, time.Now().UTC())
	if !live || reason != "" {
		t.Fatalf("nil PortOwnerPID test seam live=%v reason=%q, want legacy TCP-only live", live, reason)
	}
}

// TestLivenessSweepIntentDetectsSameSizeContentChange is the regression guard
// for PR #475 bot P2: livenessSweepIntent MUST re-read + re-parse
// supervisor-intent.json on every sweep, NOT gate the read behind a
// stat/mtime/size cache. A migration that flips a daemon port 9123→9124 is a
// SAME-BYTE-LENGTH rewrite (both 4 ASCII digits → identical serialized file
// size) that can land within the filesystem's mtime resolution, producing an
// identical (mtime, size) tuple. A stat-only change-detector would then serve
// the stale parse and the liveness sweep — which DRIVES RESTART DECISIONS —
// would act on the old port. This test seeds 9123, sweeps, rewrites to 9124
// (same byte length), sweeps again, and asserts the SECOND sweep observes
// 9124. A stat-gated cache that returned the cached 9123 here would fail.
func TestLivenessSweepIntentDetectsSameSizeContentChange(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	taskName := `\mcp-local-hub-memory-default`

	seed := func(port int) {
		t.Helper()
		intent := &api.SupervisorIntentFile{
			Version: 1,
			Daemons: []api.SupervisorDaemon{{
				TaskName: taskName,
				Server:   "memory",
				Daemon:   "default",
				Port:     port,
			}},
		}
		if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
			t.Fatalf("write supervisor-intent.json port=%d: %v", port, err)
		}
	}

	// A distinct fallback so a "returned the fallback instead of disk" bug is
	// not mistaken for a successful disk read.
	fallback := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{TaskName: taskName, Port: 1}},
	}

	portOf := func(got *api.SupervisorIntentFile) int {
		t.Helper()
		if got == nil || len(got.Daemons) != 1 {
			t.Fatalf("sweep intent = %+v, want exactly one daemon", got)
		}
		return got.Daemons[0].Port
	}

	// First sweep: disk says 9123.
	seed(9123)
	if got := portOf(livenessSweepIntent(stateDir, fallback)); got != 9123 {
		t.Fatalf("first sweep port = %d, want 9123 (disk read)", got)
	}

	// Same-byte-length rewrite: 9123 → 9124. Capture the file size before and
	// after to PROVE this is the same-size case the bug hinges on — if the
	// sizes ever diverge, the test would no longer exercise the stat-gate hole
	// and must be reworked.
	sizeBefore := fileSizeForTest(t, intentPath)
	seed(9124)
	sizeAfter := fileSizeForTest(t, intentPath)
	if sizeBefore != sizeAfter {
		t.Fatalf("intent file size changed across the 9123→9124 rewrite (%d → %d): this test requires a SAME-SIZE change to exercise the stat-gate hole", sizeBefore, sizeAfter)
	}

	// Second sweep: MUST observe the new port. A stat/mtime/size cache would
	// (wrongly) return the cached 9123 here.
	if got := portOf(livenessSweepIntent(stateDir, fallback)); got != 9124 {
		t.Fatalf("second sweep port = %d, want 9124 (same-size content change MUST be detected; a stat-gated cache would return the stale 9123)", got)
	}
}

func fileSizeForTest(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}
