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

// TestSupervisorStatusLyingServerFieldFailsClosed is the PR #505 r6b residual
// close-out: a fully-POPULATED status row whose Server field CONTRADICTS its launch
// argv ({Server:memory, args --server time}) must NOT display/route as its stale
// field. The status server feeds secret-rotation restart bucketing
// (internal/api/secrets.go runningByServer), so a lying label would misroute the
// post-rotation restart. DescriptorServerName fails closed to "" on the mismatch.
func TestSupervisorStatusLyingServerFieldFailsClosed(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	task := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName: task,
		Server:   "memory", Daemon: "default", // stale/lying fields
		Args: []string{"daemon", "--server", "time", "--daemon", "default"}, // argv truth
		Port: 0,
	}}}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(task, 600004, time.Now().UTC().Add(-time.Hour))
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
	if rows[0]["server"] == "memory" {
		t.Fatalf("server = %q, must NOT be the lying stale field 'memory' (field/argv mismatch → fail closed, r6b residual)", rows[0]["server"])
	}
	// The whole fail-closed contract: a corrupt row also resolves an HONEST port 0
	// (the owner refuses the mismatched identity — no fabricated manifest port).
	if rows[0]["port"] != 0 {
		t.Fatalf("corrupt-row port = %v, want honest 0 (owner fails closed, no fabricated manifest port)", rows[0]["port"])
	}
}

// TestSupervisorStatusPositionalAndPartialLegacyShapes is the bot #506 regression
// guard: the mismatch-fail-closed server logic must NOT break the legacy shapes.
// (a) positional argv ["daemon","<server>"] (blank fields, server only in the task
// name) must recover server+daemon from the task name — not blank out. (b) a
// blank-Server + populated-Daemon row must PRESERVE the daemon field during server
// recovery (bot #506 P2), not re-parse it away.
func TestSupervisorStatusPositionalAndPartialLegacyShapes(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{
		// (a) positional server, blank fields, no --server/--daemon flags.
		{TaskName: `\mcp-local-hub-memory-default`, Args: []string{"daemon", "memory"}},
		// positional server + --daemon flag.
		{TaskName: `\mcp-local-hub-serena-codex`, Args: []string{"daemon", "serena", "--daemon", "codex"}},
		// (b) blank Server, populated Daemon (must be preserved).
		{TaskName: `\mcp-local-hub-filesystem-worker`, Daemon: "worker", Args: []string{"daemon", "filesystem"}},
	}}
	tracker := NewDaemonRuntimeTracker()
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	byTask := map[string]map[string]any{}
	for _, r := range rows {
		byTask[r["task_name"].(string)] = r
	}
	if r := byTask[`\mcp-local-hub-memory-default`]; r["server"] != "memory" || r["daemon"] != "default" {
		t.Fatalf("positional [daemon memory] = %v/%v, want memory/default (task-name recovery, not blank)", r["server"], r["daemon"])
	}
	if r := byTask[`\mcp-local-hub-serena-codex`]; r["server"] != "serena" || r["daemon"] != "codex" {
		t.Fatalf("positional [daemon serena --daemon codex] = %v/%v, want serena/codex", r["server"], r["daemon"])
	}
	if r := byTask[`\mcp-local-hub-filesystem-worker`]; r["daemon"] != "worker" {
		t.Fatalf("blank-server + populated-daemon: daemon = %v, want preserved 'worker' (bot #506 P2)", r["daemon"])
	}
}

// TestSupervisorStatusWellFormedPopulatedRowNeutral guards common-path neutrality:
// a well-formed populated row (fields agree with argv) still shows its identity.
func TestSupervisorStatusWellFormedPopulatedRowNeutral(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	task := `\mcp-local-hub-memory-default`
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{
		TaskName: task,
		Server:   "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"},
		Port: 0,
	}}}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(task, 600005, time.Now().UTC().Add(-time.Hour))
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	rows, err := supervisorStatusDaemons(stateDir, tracker)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if rows[0]["server"] != "memory" || rows[0]["daemon"] != "default" {
		t.Fatalf("well-formed row = %v/%v, want memory/default (common-path neutral)", rows[0]["server"], rows[0]["daemon"])
	}
	if rows[0]["port"] != 9123 {
		t.Fatalf("well-formed row port = %v, want 9123", rows[0]["port"])
	}
}
