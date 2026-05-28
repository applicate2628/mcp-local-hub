package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestRunSupervise_FiresMaintenanceTimersFromIntent(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	intentPath := filepath.Join(tmpHome, "supervisor-intent.json")
	intent := &api.SupervisorIntentFile{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		MaintenanceTimers: []api.MaintenanceTimer{
			{
				Name:    `\mcp-local-hub-workspace-weekly-refresh`,
				Kind:    "workspace-weekly-refresh",
				Command: os.Args[0],
				Args:    []string{"-test.run=TestMaintenanceWiring_HelperExitZero"},
			},
		},
	}
	if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		MaintenanceFiredAt: map[string]string{
			"workspace-weekly-refresh": "2020-01-01T00:00:00Z",
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)
	defer cleanupExit()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	if !waitForCondition(5*time.Second, func() bool {
		raw, err := os.ReadFile(eventsPath)
		return err == nil && strings.Contains(string(raw), `"event":"maintenance-fired"`)
	}) {
		raw, _ := os.ReadFile(eventsPath)
		t.Fatalf("maintenance-fired event did not appear:\n%s", raw)
	}

	exitCh <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervise did not exit on test-exit signal within 5s")
	}

	got, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read supervisor-state.json: %v", err)
	}
	if got.MaintenanceFiredAt["workspace-weekly-refresh"] == "" {
		t.Fatalf("maintenance_fired_at missing after fire: %+v", got.MaintenanceFiredAt)
	}
	if !waitForCondition(2*time.Second, func() bool {
		got, err := api.ReadSupervisorState(statePath)
		return err == nil && len(got.TransientPIDs) == 0
	}) {
		got, _ := api.ReadSupervisorState(statePath)
		t.Fatalf("transient PID was not drained after helper exit: %+v", got.TransientPIDs)
	}
}

func TestMaintenanceWiring_HelperExitZero(t *testing.T) {}
