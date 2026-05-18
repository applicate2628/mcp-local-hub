//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

func TestFinishProductionTerminate_EscalationAbortReturnsError(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	cmd := exec.Command(copyCurrentTestBinaryAsReconcileMcphub(t), "-test.run=TestProductionTerminateFn_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PRODUCTION_TERMINATE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper child started without Process")
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	pid := cmd.Process.Pid
	err = finishProductionTerminate(process.PIDIdentityProof{
		PID:            pid,
		ExecutablePath: canonicalMcphubPath(),
		StartedAt:      "2000-01-01T00:00:00Z",
	}, api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}, events)
	if err == nil {
		t.Fatal("finishProductionTerminate returned nil after PID identity mismatch at escalation")
	}
	if !strings.Contains(err.Error(), "terminate escalation aborted") {
		t.Fatalf("finishProductionTerminate error = %v, want escalation abort", err)
	}

	logRaw, readErr := os.ReadFile(eventsPath)
	if readErr != nil {
		t.Fatalf("read events log: %v", readErr)
	}
	if !strings.Contains(string(logRaw), `"event":"daemon-terminate-escalation-aborted-pid-reuse"`) {
		t.Fatalf("daemon-terminate-escalation-aborted-pid-reuse event missing from audit log:\n%s", string(logRaw))
	}
}
