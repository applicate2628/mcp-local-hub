package api

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestRestartUsesSupervisorRespawnForIntentDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-memory-default`,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var gotTask string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		gotTask = taskName
		if force {
			t.Fatal("Restart must not force quarantine override by default")
		}
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	results, err := NewAPI().Restart("memory", "")
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if gotTask != `\mcp-local-hub-memory-default` {
		t.Fatalf("respawn task = %q, want memory default", gotTask)
	}
	if len(results) != 1 || results[0].TaskName != `\mcp-local-hub-memory-default` || results[0].Err != "" {
		t.Fatalf("results = %+v, want one supervisor success row", results)
	}
}

func TestRestartFallsBackWhenNoSupervisorIntentMatches(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	called := false
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		called = true
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	_, handled, err := restartSupervisorOwnedDaemons(context.Background(), "memory", "")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if handled {
		t.Fatal("handled=true with no supervisor intent; want scheduler fallback")
	}
	if called {
		t.Fatal("supervisor respawn called even though supervisor-intent.json was absent")
	}
}
