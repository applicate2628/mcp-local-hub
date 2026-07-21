package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Pre-spawn existence-gate (P1.1) delivery coverage for the NON-IPC surfaces.
//
// The gate's diagnosis reaches bare `mcphub status` and the GUI Dashboard
// through the supervisor IPC path. It did NOT reach:
//
//   - the StatusWithOpts family (`mcphub status --health`,
//     `--workspace-scoped`, `--force-materialize`, StatusWithHealth, and the
//     unwired-host /api/status fallback), whose rows come from the scheduler,
//     the registry, and supervisor-INTENT.json — none of which read the
//     supervisor-STATE.json where the hold lives; and
//   - /api/health, whose DaemonRow had no field able to carry it.
//
// That is the worst possible asymmetry for this feature: `--health` is what an
// operator runs WHEN THEY SUSPECT SOMETHING IS WRONG, so the command most
// likely to be typed during the incident was the one that dropped the answer.

// seedSupervisorStateWithHold points the state-dir resolver at a temp dir and
// writes a supervisor-state.json carrying a hold for taskName.
func seedSupervisorStateWithHold(t *testing.T, taskName, reason, path string) {
	t.Helper()
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// Seed through the PRODUCTION writer, not os.WriteFile: ReadSupervisorState
	// goes through the hardened inode-anchored state-file read, which a plainly
	// written file does not satisfy. Using the real writer also means this test
	// exercises the same on-disk shape the supervisor actually produces.
	file := SupervisorStateFile{
		Version: 1,
		Daemons: map[string]SupervisorDaemonState{
			taskName: {State: "idle", SpawnHoldReason: reason, SpawnHoldPath: path},
		},
	}
	if err := WriteSupervisorState(filepath.Join(stateDir, supervisorStateFileLeaf), &file); err != nil {
		t.Fatalf("write supervisor-state.json: %v", err)
	}
}

func TestEnrichStatusWithSpawnHolds_JoinsStateFileOntoRows(t *testing.T) {
	const task = `\mcp-local-hub-memory-default`
	const held = `C:\Users\dev\.local\bin\mcphub.exe`
	seedSupervisorStateWithHold(t, task, "missing-binary", held)

	rows := []DaemonStatus{
		{TaskName: task, Server: "memory", Daemon: "default", State: "Stopped"},
		{TaskName: `\mcp-local-hub-time-default`, Server: "time", Daemon: "default", State: "Running"},
	}
	enrichStatusWithSpawnHolds(rows)

	if rows[0].SpawnHoldReason != "missing-binary" || rows[0].SpawnHoldPath != held {
		t.Fatalf("held row = {%q, %q}, want {missing-binary, %s} — `mcphub status --health` would show a stopped daemon with NO explanation, which is the surface an operator reaches for when something is wrong",
			rows[0].SpawnHoldReason, rows[0].SpawnHoldPath, held)
	}
	if rows[1].SpawnHoldReason != "" || rows[1].SpawnHoldPath != "" {
		t.Fatalf("healthy row picked up a phantom hold: {%q, %q}", rows[1].SpawnHoldReason, rows[1].SpawnHoldPath)
	}
}

// The join must match the daemon even when the row's task name lacks the
// canonical leading backslash the state file keys on.
func TestEnrichStatusWithSpawnHolds_MatchesBareTaskName(t *testing.T) {
	const canonical = `\mcp-local-hub-memory-default`
	seedSupervisorStateWithHold(t, canonical, "missing-binary", `C:\gone.exe`)

	rows := []DaemonStatus{{TaskName: `mcp-local-hub-memory-default`, Server: "memory"}}
	enrichStatusWithSpawnHolds(rows)
	if rows[0].SpawnHoldReason != "missing-binary" {
		t.Fatalf("bare task name did not join against the canonical state key; got %q", rows[0].SpawnHoldReason)
	}
}

// Best-effort contract: a missing/unreadable state file must leave rows intact,
// never fail the status command. A missing explanation must not break status.
func TestEnrichStatusWithSpawnHolds_BestEffortOnMissingStateFile(t *testing.T) {
	restore := SetDaemonStateRootForTest(t.TempDir())
	t.Cleanup(restore)

	rows := []DaemonStatus{{TaskName: `\mcp-local-hub-memory-default`, State: "Running"}}
	enrichStatusWithSpawnHolds(rows) // must not panic
	if rows[0].SpawnHoldReason != "" || rows[0].State != "Running" {
		t.Fatalf("rows were mutated despite an absent state file: %+v", rows[0])
	}
	enrichStatusWithSpawnHolds(nil) // must not panic
}

// TestStatusWithOpts_CarriesSpawnHold drives the REAL StatusWithOpts end to end
// through the existing hermetic harness (fake scheduler, temp state dir, temp
// registry, nil'd process/port probes — nothing touches the live host).
//
// The direct unit tests above cannot catch a missing CALL: deleting
// `enrichStatusWithSpawnHolds(result)` from StatusWithOpts leaves them all
// green, because they invoke the function themselves. That is precisely the
// mutation this test exists to kill — the same call-site blind spot that let
// the producer and consumer wire mappings ship unpinned.
func TestStatusWithOpts_CarriesSpawnHold(t *testing.T) {
	const task = `\mcp-local-hub-fetch-default`
	const held = `C:\Users\dev\.local\bin\mcphub.exe`

	sch := &fakeScheduler{tasks: map[string]bool{}, xml: map[string][]byte{}}
	intentPath := statusSupervisorOnlyHermeticEnv(t, sch)

	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: task, Server: "fetch", Daemon: "default", Command: "mcphub.exe", Port: 9133},
		},
	}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// The hold lives in supervisor-STATE.json, a file none of the three
	// StatusWithOpts row builders read. The harness already redirected the
	// state root, so write the sibling state file into the same dir.
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir: %v", err)
	}
	if err := WriteSupervisorState(filepath.Join(stateDir, supervisorStateFileLeaf), &SupervisorStateFile{
		Version: 1,
		Daemons: map[string]SupervisorDaemonState{
			task: {State: "idle", SpawnHoldReason: "missing-binary", SpawnHoldPath: held},
		},
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{ProbeHealth: false})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	row, ok := statusRowByTask(rows)[task]
	if !ok {
		t.Fatalf("row %s missing from StatusWithOpts output: %+v", task, rows)
	}
	if row.SpawnHoldReason != "missing-binary" || row.SpawnHoldPath != held {
		t.Fatalf("StatusWithOpts row = {%q, %q}, want {missing-binary, %s} — `mcphub status --health` is what an operator runs when they suspect something is wrong, and it is the one path that dropped the answer",
			row.SpawnHoldReason, row.SpawnHoldPath, held)
	}
}

// TestHealthSnapshot_DaemonRowCarriesSpawnHold covers the /api/health surface.
// Without the DaemonRow fields the payload STRUCTURALLY cannot explain why a
// daemon is stopped — `state: "stopped"` with the cause nowhere on the wire.
func TestHealthSnapshot_DaemonRowCarriesSpawnHold(t *testing.T) {
	const held = `C:\Users\dev\.local\bin\mcphub.exe`
	a := NewAPI()
	original := a.HealthStatusFn
	defer func() { a.HealthStatusFn = original }()
	a.HealthStatusFn = func(StatusOpts) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{Server: "memory", Daemon: "default", State: "Stopped",
				SpawnHoldReason: "missing-binary", SpawnHoldPath: held},
			{Server: "time", Daemon: "default", State: "Running", PID: 42},
		}, nil
	}

	snap, err := a.HealthSnapshot(context.Background(), HealthOpts{})
	if err != nil {
		t.Fatalf("HealthSnapshot: %v", err)
	}
	if len(snap.Daemons.Items) != 2 {
		t.Fatalf("Daemons.Items = %d, want 2", len(snap.Daemons.Items))
	}
	if snap.Daemons.Items[0].SpawnHoldReason != "missing-binary" || snap.Daemons.Items[0].SpawnHoldPath != held {
		t.Fatalf("held DaemonRow = {%q, %q}, want the reason and path — /api/health is consulted exactly when something looks wrong",
			snap.Daemons.Items[0].SpawnHoldReason, snap.Daemons.Items[0].SpawnHoldPath)
	}
	if snap.Daemons.Items[1].SpawnHoldReason != "" {
		t.Fatalf("healthy DaemonRow carried a phantom hold: %q", snap.Daemons.Items[1].SpawnHoldReason)
	}

	// Additive-only: a healthy row must not gain the keys on the wire, so the
	// existing /api/health shape is unchanged for healthy fleets.
	raw, err := json.Marshal(snap.Daemons.Items[1])
	if err != nil {
		t.Fatalf("marshal healthy row: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal healthy row: %v", err)
	}
	if _, present := asMap["spawn_hold_reason"]; present {
		t.Fatalf("healthy row emitted spawn_hold_reason; the fields must be omitempty: %s", raw)
	}
}
