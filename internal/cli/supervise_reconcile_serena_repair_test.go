// Package cli — composition test for the P1 fix tracked under
// work-items/active (mcphub-register-intent): `mcphub workspace register`
// used to commit a workspaces.yaml row and print an unqualified success
// WITHOUT ever touching supervisor-intent.json — no daemon row, no
// reconcile, no spawn. The fix wires the running supervisor's apply-mode
// `reconcile` IPC handler to self-heal that registry/intent split
// (api.RepairSerenaIntentFromRegistry) BEFORE computing drift, so a
// just-registered workspace's daemon row is appended and reconciled in the
// SAME round trip `mcphub workspace register` triggers via
// DialSupervisorIPCReconcile(apply=true).
//
// This file tests the SERVER side of that wiring (handleReconcile itself,
// supervise_reconcile_ipc.go). The CLIENT side (runWorkspaceRegister's
// gated success message) is tested separately in workspace_cmd_test.go —
// each of the pre-existing "Registry allocation", "full auto-register
// transaction", "auto-register idempotency", "append-only intent repair",
// and "router-miss-only auto-register" tests covers ONE seam in isolation;
// none of them composed "explicit register" with "live-supervisor
// convergence", which is exactly the gap this fixed defect lived in.
package cli

import (
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// seedOrphanSerenaRegistryRow writes one serena sentinel row (no matching
// supervisor-intent daemon — an "orphan", the exact shape a bare `mcphub
// workspace register` used to leave behind pre-fix) into the registry at
// regPath. Returns the derived workspace key.
func seedOrphanSerenaRegistryRow(t *testing.T, regPath, workspacePath string, port int) string {
	t.Helper()
	key := api.WorkspaceKey(workspacePath)
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		t.Fatalf("lock registry: %v", err)
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if err := reg.PutSerena(api.WorkspaceEntry{
		WorkspaceKey:  key,
		WorkspacePath: workspacePath,
		Language:      api.SerenaLanguageSentinel,
		Backend:       api.SerenaServerName,
		Port:          port,
		TaskName:      "mcp-local-hub-serena-" + key,
		Languages:     []string{"python"},
	}); err != nil {
		t.Fatalf("put serena row: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
	return key
}

// TestReconcileIPC_ApplyRepairsSerenaIntentFromRegistryBeforeDrift seeds an
// orphan serena registry row (registered, but with NO matching
// supervisor-intent daemon) alongside an EXISTING spec-bearing serena daemon
// for a DIFFERENT workspace — satisfying the §7.1 "dynamic pool already
// introduced" precondition so RepairSerenaIntentFromRegistry may APPEND
// rather than defer to `mcphub migrate serena legacy-to-dynamic-pool`. It
// then asserts that ONE apply-mode `reconcile` IPC call:
//
//  1. appends the missing daemon row to supervisor-intent.json (observable
//     both in the handler's own response body's drift AND, durably, on disk
//     via api.ReadSupervisorIntent + HasSpecBearingSerenaDaemonForWorkspaceKey), and
//  2. computes and APPLIES drift against that now-complete intent in the
//     SAME round trip — an EvIntentUpdate is posted for the newly-appended
//     row (the "observable reconcile/start request" this composition proves,
//     i.e. the fix is not merely a file write with no live effect).
func TestReconcileIPC_ApplyRepairsSerenaIntentFromRegistryBeforeDrift(t *testing.T) {
	manifestDir := t.TempDir()
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	healthyWS := t.TempDir()
	healthyKey := api.WorkspaceKey(healthyWS)
	healthyTaskName := `\mcp-local-hub-serena-` + healthyKey
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  healthyTaskName,
				Server:    api.SerenaServerName,
				Daemon:    "serena-" + healthyKey,
				Command:   "mcphub",
				Args:      []string{"daemon", "serena-proxy", "--task-name", healthyTaskName},
				Workspace: healthyWS,
				Port:      9150,
				// Non-nil RuntimeSpec is the ONLY thing HasRuntimeSpecRow cares
				// about; its contents are otherwise unused by this test.
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   1,
					ChildCommand:  "uvx",
					UpstreamPort:  19150,
					ExternalPort:  9150,
					WorkspacePath: healthyWS,
				},
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	// Seed the orphan AFTER constructing the fixture — newReconcileTestFixture
	// redirects LOCALAPPDATA/XDG_STATE_HOME to its OWN temp root (see its doc
	// comment), so api.DefaultRegistryPath() must be resolved (and the row
	// written) after that redirect is in effect.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	orphanWS := t.TempDir()
	orphanKey := seedOrphanSerenaRegistryRow(t, regPath, orphanWS, 9151)
	orphanTaskName := `\mcp-local-hub-serena-` + orphanKey

	// Neither daemon has a scheduler row — both read as "missing" from the
	// scheduler's perspective; only the orphan's post-repair presence in
	// intent is under test here.
	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   1001,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": true},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)

	// The orphan must show up in THIS SAME reconcile pass's drift — proving
	// the repair ran BEFORE the intent read/drift computation, not after.
	var orphanDrift *api.DriftEntry
	for i := range body.Drift {
		if body.Drift[i].TaskName == orphanTaskName {
			orphanDrift = &body.Drift[i]
		}
	}
	if orphanDrift == nil {
		t.Fatalf("no drift entry for the repaired orphan task %s; drift=%+v", orphanTaskName, body.Drift)
	}
	if orphanDrift.IntentDesired != api.ReconcileIntentDesiredRunning {
		t.Errorf("orphan IntentDesired = %q, want %q (the repair-appended row wants running)",
			orphanDrift.IntentDesired, api.ReconcileIntentDesiredRunning)
	}
	if orphanDrift.Action != api.ReconcileActionPostEvIntentUpdate {
		t.Errorf("orphan Action = %q, want %q (missing scheduler + intent-wants-running must spawn)",
			orphanDrift.Action, api.ReconcileActionPostEvIntentUpdate)
	}
	if body.AppliedCount < 1 {
		t.Errorf("AppliedCount = %d, want >= 1 (the orphan's spawn must be applied)", body.AppliedCount)
	}

	// An EvIntentUpdate for the orphan's task name must actually be posted —
	// proving this is a live reconcile effect, not just a durable file write.
	deadline := time.After(2 * time.Second)
	found := false
	for !found {
		select {
		case ev := <-fx.postedCh:
			if ev.Kind == api.EvIntentUpdate && ev.TaskName == orphanTaskName {
				found = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for EvIntentUpdate for the repaired orphan task")
		}
	}

	// Durable proof: re-read supervisor-intent.json from disk (independent of
	// the in-memory response body) and confirm the spec-bearing row for the
	// orphan workspace key is now actually there.
	intentPath := filepath.Join(fx.deps.stateDir, "supervisor-intent.json")
	onDisk, rerr := api.ReadSupervisorIntent(intentPath)
	if rerr != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", rerr)
	}
	if !onDisk.HasSpecBearingSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Fatalf("supervisor-intent.json has no spec-bearing serena daemon for the repaired workspace key %s after apply-mode reconcile", orphanKey)
	}
	// The pre-existing healthy row must be UNTOUCHED (append-only, never
	// replace-all — the core invariant RepairSerenaIntentFromRegistry exists
	// to preserve).
	if !onDisk.HasSpecBearingSerenaDaemonForWorkspaceKey(healthyKey) {
		t.Fatalf("pre-existing healthy serena daemon for workspace key %s was lost — repair must be append-only", healthyKey)
	}
}

// TestReconcileIPC_DryRunDoesNotRepairSerenaIntentFromRegistry pins the
// apply-mode scoping: a dry-run reconcile (apply=false) must NOT mutate
// state, so the self-heal must not run at all when args.Apply is false.
func TestReconcileIPC_DryRunDoesNotRepairSerenaIntentFromRegistry(t *testing.T) {
	manifestDir := t.TempDir()
	seedSerenaManifest(t, manifestDir, alreadyMigratedManifestYAML)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestDir)

	healthyWS := t.TempDir()
	healthyKey := api.WorkspaceKey(healthyWS)
	healthyTaskName := `\mcp-local-hub-serena-` + healthyKey
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  healthyTaskName,
				Server:    api.SerenaServerName,
				Daemon:    "serena-" + healthyKey,
				Command:   "mcphub",
				Workspace: healthyWS,
				Port:      9150,
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   1,
					ChildCommand:  "uvx",
					UpstreamPort:  19150,
					ExternalPort:  9150,
					WorkspacePath: healthyWS,
				},
			},
		},
	}
	fx := newReconcileTestFixture(t, intent)

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatalf("registry path: %v", err)
	}
	orphanWS := t.TempDir()
	orphanKey := seedOrphanSerenaRegistryRow(t, regPath, orphanWS, 9151)

	installSchedulerListFake(t, []scheduler.TaskStatus{})

	req := api.IPCRequest{
		ID:   1002,
		Cmd:  "reconcile",
		Args: map[string]any{"apply": false},
	}
	conn := newFakeIPCConn()
	if err := handleReconcile(conn, req, fx.deps); err != nil {
		t.Fatalf("handleReconcile: %v", err)
	}
	_, body := decodeReconcileResponse(t, conn)
	if !body.DryRun {
		t.Errorf("DryRun=false in dry-run mode response: %+v", body)
	}
	for _, d := range body.Drift {
		if d.TaskName == `\mcp-local-hub-serena-`+orphanKey {
			t.Fatalf("dry-run must not surface the orphan at all (no repair ran): %+v", d)
		}
	}

	intentPath := filepath.Join(fx.deps.stateDir, "supervisor-intent.json")
	onDisk, rerr := api.ReadSupervisorIntent(intentPath)
	if rerr != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", rerr)
	}
	if onDisk.HasSpecBearingSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Fatalf("dry-run reconcile must NEVER mutate supervisor-intent.json, but the orphan key %s is now present", orphanKey)
	}
}
