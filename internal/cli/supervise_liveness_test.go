package cli

import (
	"context"
	"path/filepath"
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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)
	select {
	case ev := <-events:
		t.Fatalf("liveness reposted stale cleaned daemon: %+v", ev)
	case <-time.After(100 * time.Millisecond):
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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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

	sweepSupervisorLivenessOnce(stateDir, intent, tracker, loop, nil)

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
