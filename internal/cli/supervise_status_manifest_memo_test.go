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

// TestSupervisorStatusPortResolvedForPartialArgvCompletedByField is commission PR
// #505 r6b P1: a well-formed row whose PARTIAL argv (`daemon --server memory`, no
// --daemon) is COMPLETED by its populated Daemon field must still resolve its
// manifest port. An earlier r6 revision rebuilt the status effDesc without the
// fields and refused to reattach them for any global argv, regressing this shape to
// port 0; the fix copies the whole row (like gui/daemon_env.go) and only gates the
// overwrite.
func TestSupervisorStatusPortResolvedForPartialArgvCompletedByField(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	task := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName: task,
		Server:   "memory",
		Daemon:   "default", // completes the missing --daemon
		Args:     []string{"daemon", "--server", "memory"},
		Port:     0,
	}}}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(task, 600003, time.Now().UTC().Add(-time.Hour))
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
		t.Fatalf("row port = %v, want 9123 (partial argv completed by the populated Daemon field must resolve — r6b P1 regression guard)", rows[0]["port"])
	}
}

// TestSupervisorStatusIdentityRecoveredViaOwnerForHyphenatedDaemon is the bot
// PR #505 guard: a legacy blank-field row whose args carry a hyphenated
// server+daemon (mcp-language-server / vscode-css) must recover identity via the
// OWNER (args), not the greedy ParseManagedTaskName split — which would derive
// server=mcp-language-server-vscode and, worse, disagree with the args so
// DescriptorServerDaemon fails closed and the port stays 0. F5 used to heal the
// blank fields; it is gone.
func TestSupervisorStatusIdentityRecoveredViaOwnerForHyphenatedDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	task := `\mcp-local-hub-mcp-language-server-vscode-css`
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName: task,
		// Blank Server/Daemon — identity lives only in the args.
		Args: []string{"daemon", "--server", "mcp-language-server", "--daemon", "vscode-css"},
		Port: 0,
	}}}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(task, 600002, time.Now().UTC().Add(-time.Hour))
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
	if rows[0]["server"] != "mcp-language-server" {
		t.Fatalf("server = %v, want mcp-language-server (owner args-recovery, not the greedy task-name split)", rows[0]["server"])
	}
	if rows[0]["daemon"] != "vscode-css" {
		t.Fatalf("daemon = %v, want vscode-css", rows[0]["daemon"])
	}
}
