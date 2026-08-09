package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

func TestDaemonRecoveryAuditHandoffWorkerRealCurrentBinaryPersistsCarrier(t *testing.T) {
	stateDir := t.TempDir()
	prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
		Event:  "daemon-recovery-real-bounded-handoff",
		Source: api.SupervisorEventSourceLifecycle,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := api.PreparedSupervisorEventBytes(prepared)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"version":   1,
		"state_dir": stateDir,
		"prepared":  raw,
		"replay":    false,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command:     exec.Command(os.Args[0], daemonrecovery.CommittedAuditHandoffWorkerCommand),
		Input:       payload,
		InputLimit:  64 << 10,
		StdoutLimit: 64 << 10,
		StderrLimit: 16 << 10,
	})
	if err != nil {
		t.Fatalf("worker: %v stderr=%q", err, string(run.Stderr.Prefix))
	}
	if !strings.Contains(string(run.Stdout.Prefix), `"outcome":"durable"`) {
		t.Fatalf("worker stdout=%q", string(run.Stdout.Prefix))
	}
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	if err := logger.TryReplayPending(); err != nil {
		t.Fatalf("replay carrier: %v", err)
	}
	logged, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil || !strings.Contains(string(logged), "daemon-recovery-real-bounded-handoff") {
		t.Fatalf("log=%q err=%v", string(logged), err)
	}
}
