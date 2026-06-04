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
