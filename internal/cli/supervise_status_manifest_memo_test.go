package cli

import (
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// TestSupervisorStatusPortEnrichedViaOwner is the P3d guard: a Port=0 legacy
// descriptor for a real manifest-backed server (memory) shows its manifest port
// (9123) in the status row, resolved through the port-resolution owner
// (api.NewDaemonPortResolver) — the same authority the liveness sweep, squatter,
// and recover paths use. The private newManifestPortResolver memo is gone; the
// once-per-server manifest-parse guarantee is covered api-side in
// TestDaemonPortResolver_ParsesManifestOncePerServer.
func TestSupervisorStatusPortEnrichedViaOwner(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	task := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName: task,
		Server:   "memory",
		Daemon:   "default",
		Port:     0, // legacy Port=0 — the owner resolves the manifest 9123 for display
	}}}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(task, 600001, time.Now().UTC().Add(-time.Hour))
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0]["port"] != 9123 {
		t.Fatalf("row port = %v, want 9123 (Port=0 resolved via the owner from the memory manifest)", rows[0]["port"])
	}
}
