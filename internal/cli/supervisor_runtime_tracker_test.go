package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

func beginStopSettlementForTest(t *testing.T, tracker *DaemonRuntimeTracker, statePath, taskName string) api.StopSettlementReceiptV1 {
	t.Helper()
	receipts, err := tracker.BeginStopSettlementBatch(statePath, api.StopBatchCommandV1{
		ProtocolVersion: 1,
		BatchID:         "test-stop-batch",
		Targets:         []api.StopBatchTargetV1{{TaskName: taskName, ExpectedPort: 1}},
	}, map[string]int{taskName: 1})
	if err != nil {
		t.Fatalf("begin stop batch: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("stop batch receipts = %+v, want one", receipts)
	}
	return receipts[0]
}

func TestDaemonRuntimeTracker_LifecycleTransitions(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `mcp-local-hub-memory-default`
	startedAt := time.Date(2026, 5, 18, 10, 0, 0, 123456789, time.UTC)

	if _, ok := tracker.Get(taskName); ok {
		t.Fatal("new tracker unexpectedly has an entry")
	}

	tracker.MarkSpawned(taskName, 4321, startedAt)
	entry, ok := tracker.Get(`\mcp-local-hub-memory-default`)
	if !ok {
		t.Fatal("spawned entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 4321 || entry.PIDGeneration != 1 {
		t.Fatalf("spawned entry = %+v, want running pid=4321 pid_generation=1", entry)
	}
	if !entry.StartedAt.Equal(startedAt) {
		t.Fatalf("started_at = %s, want %s", entry.StartedAt, startedAt)
	}

	tracker.MarkExited(taskName)
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("exited entry missing")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || !entry.StartedAt.IsZero() || entry.PIDGeneration != 1 {
		t.Fatalf("exited entry = %+v, want idle pid=0 zero started_at generation preserved", entry)
	}

	spawnErr := errors.New("spawn failed")
	tracker.MarkSpawnFailed(taskName, spawnErr)
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("spawn-failed entry missing")
	}
	if entry.State != daemonRuntimeStateBackoff || entry.CurrentPID != 0 || entry.LastError != spawnErr.Error() || entry.RestartCount == 0 {
		t.Fatalf("spawn-failed entry = %+v, want backoff pid=0 last_error and restart_count", entry)
	}

	tracker.MarkSpawned(taskName, 9876, startedAt.Add(time.Minute))
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("respawned entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 9876 || entry.PIDGeneration != 2 || entry.LastError != "" {
		t.Fatalf("respawned entry = %+v, want running pid=9876 generation=2 and cleared last_error", entry)
	}

	tracker.MarkTerminated(taskName)
	entry, ok = tracker.Get(taskName)
	if !ok {
		t.Fatal("terminated entry missing")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || !entry.StartedAt.IsZero() {
		t.Fatalf("terminated entry = %+v, want idle pid=0 zero started_at", entry)
	}
}

func TestDaemonRuntimeTracker_FleetGenerationTracksPortOwnershipChanges(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `mcp-local-hub-memory-default`
	startedAt := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)

	if got := tracker.Generation(); got != 0 {
		t.Fatalf("new tracker Generation = %d, want 0", got)
	}

	gen1 := tracker.MarkSpawned(taskName, 4321, startedAt)
	if gen1 != 1 {
		t.Fatalf("MarkSpawned returned PIDGeneration %d, want 1", gen1)
	}
	if got := tracker.Generation(); got != 1 {
		t.Fatalf("Generation after spawn = %d, want 1", got)
	}

	tracker.MarkJobProtection(taskName, nil)
	if got := tracker.Generation(); got != 1 {
		t.Fatalf("Generation after non-ownership metadata update = %d, want 1", got)
	}

	if tracker.MarkExitedIfCurrent(taskName, gen1-1) {
		t.Fatalf("stale MarkExitedIfCurrent returned true; want false")
	}
	if got := tracker.Generation(); got != 1 {
		t.Fatalf("Generation after stale exit = %d, want 1", got)
	}

	if !tracker.MarkExitedIfCurrent(taskName, gen1) {
		t.Fatalf("current MarkExitedIfCurrent returned false; want true")
	}
	if got := tracker.Generation(); got != 2 {
		t.Fatalf("Generation after current exit = %d, want 2", got)
	}

	if !tracker.MarkExitedIfCurrent(taskName, gen1) {
		t.Fatalf("idempotent MarkExitedIfCurrent returned false; want true")
	}
	if got := tracker.Generation(); got != 2 {
		t.Fatalf("Generation after idempotent current exit = %d, want 2", got)
	}
}

func TestDaemonRuntimeTracker_ClearsJobProtectionWhenNoCurrentSpawn(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `mcp-local-hub-memory-default`
	unprotected := false
	startedAt := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	tracker.MarkJobProtection(taskName, &unprotected)
	tracker.MarkSpawned(taskName, 4321, startedAt)
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatal("spawned entry missing")
	}
	if entry.JobProtection == nil || *entry.JobProtection != false {
		t.Fatalf("running entry JobProtection = %v, want explicit false", entry.JobProtection)
	}

	for _, tt := range []struct {
		name string
		mark func()
	}{
		{"spawn-failed", func() { tracker.MarkSpawnFailed(taskName, errors.New("spawn failed")) }},
		{"spawn-failed-preserve-pid", func() { tracker.MarkSpawnFailedPreservePID(taskName, errors.New("orphan"), 9876) }},
		{"exited", func() { tracker.MarkExited(taskName) }},
		{"backoff", func() { tracker.MarkBackoff(taskName) }},
		{"quarantined", func() { tracker.MarkQuarantined(taskName) }},
		{"terminated", func() { tracker.MarkTerminated(taskName) }},
	} {
		tracker.MarkJobProtection(taskName, &unprotected)
		tracker.MarkSpawned(taskName, 4321, startedAt)
		tt.mark()
		entry, ok := tracker.Get(taskName)
		if !ok {
			t.Fatalf("%s: entry missing", tt.name)
		}
		if entry.JobProtection != nil {
			t.Fatalf("%s: JobProtection = %v, want nil when no current spawn is running", tt.name, *entry.JobProtection)
		}
	}
}

func TestDaemonRuntimeTracker_PersistAndHydrate(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{},
		TransientPIDs: []api.TransientPID{
			{PID: 2222, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-18T09:00:00Z"},
		},
		MaintenanceFiredAt: map[string]string{"workspace-weekly-refresh": "2026-05-18T09:00:00Z"},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	startedAt := time.Date(2026, 5, 18, 10, 0, 0, 123456789, time.UTC)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(`\mcp-local-hub-memory-default`, 4321, startedAt)
	tracker.MarkSpawnFailed(`\mcp-local-hub-serena-codex`, errors.New("boom"))

	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	mem := persisted.Daemons[`\mcp-local-hub-memory-default`]
	if mem.State != "running" || mem.CurrentPID != 4321 || mem.PIDGeneration != 1 || mem.StartedAt != startedAt.Format(time.RFC3339Nano) {
		t.Fatalf("persisted memory state = %+v, want running pid=4321 generation=1 started_at", mem)
	}
	serena := persisted.Daemons[`\mcp-local-hub-serena-codex`]
	if serena.State != "idle" || serena.CurrentPID != 0 {
		t.Fatalf("persisted serena state = %+v, want neutral idle pid=0", serena)
	}
	if len(persisted.TransientPIDs) != 1 || persisted.MaintenanceFiredAt["workspace-weekly-refresh"] == "" {
		t.Fatalf("persist lost non-daemon state: %+v", persisted)
	}

	hydrated := NewDaemonRuntimeTracker()
	hydrated.HydrateFromState(persisted)
	entry, ok := hydrated.Get(`\mcp-local-hub-memory-default`)
	if !ok {
		t.Fatal("hydrated memory entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 4321 || entry.PIDGeneration != 1 || !entry.StartedAt.Equal(startedAt) {
		t.Fatalf("hydrated memory entry = %+v, want running pid=4321 generation=1 started_at", entry)
	}
	entry, ok = hydrated.Get(`\mcp-local-hub-serena-codex`)
	if !ok {
		t.Fatal("hydrated serena entry missing")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
		t.Fatalf("hydrated serena entry = %+v, want idle pid=0", entry)
	}
}

func TestDaemonRuntimeTracker_BeginStopSettlementPersistsExactReceiptBeforeReturning(t *testing.T) {
	statePath := filepath.Join(apitest.HardenedTempDir(t), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	startedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, startedAt)

	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	if receipt.Version != 1 || receipt.BatchID != "test-stop-batch" || receipt.TaskName != taskName || receipt.Epoch != 1 || receipt.PID != 4812 || receipt.StartedAt != startedAt.Format(time.RFC3339Nano) || receipt.PIDGeneration != 1 || receipt.Revision != 1 || receipt.Phase != api.StopSettlementPhaseStopRequested {
		t.Fatalf("receipt = %+v, want exact stop-requested generation", receipt)
	}
	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted receipt: %v", err)
	}
	if persisted.StopSettlementEpoch != receipt.Epoch || persisted.StopSettlements[taskName] != receipt {
		t.Fatalf("persisted receipt = epoch %d rows %+v, want epoch %d receipt %+v", persisted.StopSettlementEpoch, persisted.StopSettlements, receipt.Epoch, receipt)
	}

	hydrated := NewDaemonRuntimeTracker()
	hydrated.HydrateFromState(persisted)
	got, ok := hydrated.StopSettlementReceipt(taskName)
	if !ok || got != receipt {
		t.Fatalf("hydrated receipt = %+v present=%v, want %+v", got, ok, receipt)
	}
}

func TestDaemonRuntimeTracker_InvalidHydratedReceiptFencesLifecycleAndSurvivesPersist(t *testing.T) {
	statePath := filepath.Join(apitest.HardenedTempDir(t), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	invalid := api.StopSettlementReceiptV1{
		Version: 1, BatchID: "interrupted", TaskName: taskName, Epoch: 7, PID: 4812,
		StartedAt: "2026-08-31T12:00:00Z", PIDGeneration: 3, Revision: 1,
		Phase: api.StopSettlementPhase("unknown_future_phase"),
	}
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version:         1,
		Daemons:         map[string]api.SupervisorDaemonState{taskName: {State: "idle"}},
		StopSettlements: map[string]api.StopSettlementReceiptV1{taskName: invalid},
	}); err != nil {
		t.Fatalf("seed invalid receipt: %v", err)
	}
	tracker, err := loadDaemonRuntimeTrackerFromStatePath(statePath)
	if err != nil {
		t.Fatalf("hydrate tracker: %v", err)
	}
	if err := tracker.StopSettlementIntegrityError(); err == nil {
		t.Fatal("invalid durable receipt did not fence lifecycle")
	}
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}
	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	if got, ok := persisted.StopSettlements[taskName]; !ok || got != invalid {
		t.Fatalf("disk-only invalid receipt = %+v present=%v, want preserved %+v", got, ok, invalid)
	}
}

func TestStopSettlementAdmissionEntryAllowsOnlyExactProcessOrIdleBackoff(t *testing.T) {
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		entry   DaemonRuntimeEntry
		present bool
		want    bool
	}{
		{name: "absent", present: false},
		{name: "live without generation", entry: DaemonRuntimeEntry{State: daemonRuntimeStateRunning, CurrentPID: 4812}, present: true},
		{name: "live without start time", entry: DaemonRuntimeEntry{State: daemonRuntimeStateRunning, CurrentPID: 4812, PIDGeneration: 1}, present: true},
		{name: "exact live generation", entry: DaemonRuntimeEntry{State: daemonRuntimeStateRunning, CurrentPID: 4812, PIDGeneration: 1, StartedAt: started}, present: true, want: true},
		{name: "idle no process", entry: DaemonRuntimeEntry{State: daemonRuntimeStateIdle}, present: true, want: true},
		{name: "crash backoff no process", entry: DaemonRuntimeEntry{State: daemonRuntimeStateBackoff}, present: true, want: true},
		{name: "quarantined no process", entry: DaemonRuntimeEntry{State: daemonRuntimeStateQuarantine}, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopSettlementAdmissionEntry(tc.entry, tc.present); got != tc.want {
				t.Fatalf("stop settlement admission = %v, want %v for %+v present=%v", got, tc.want, tc.entry, tc.present)
			}
		})
	}
}

func TestDaemonRuntimeTracker_BeginStopSettlementBatchIsAtomic(t *testing.T) {
	statePath := filepath.Join(apitest.HardenedTempDir(t), "supervisor-state.json")
	const first = `\mcp-local-hub-time-default`
	const second = `\mcp-local-hub-fetch-default`
	started := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(first, 4812, started)
	tracker.MarkSpawned(second, 4813, started)
	command := api.StopBatchCommandV1{
		ProtocolVersion: 1,
		BatchID:         "batch-atomic",
		Targets: []api.StopBatchTargetV1{
			{TaskName: first, ExpectedPort: 9128},
			{TaskName: second, ExpectedPort: 9129},
		},
	}

	if _, err := tracker.BeginStopSettlementBatch(statePath, command, map[string]int{first: 9128}); err == nil {
		t.Fatal("partial descriptor snapshot admitted a batch")
	}
	if _, pending := tracker.StopSettlementReceipt(first); pending {
		t.Fatal("failed batch mutated first in-memory receipt")
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed batch state file = %v, want absent", err)
	}

	receipts, err := tracker.BeginStopSettlementBatch(statePath, command, map[string]int{first: 9128, second: 9129})
	if err != nil {
		t.Fatalf("begin atomic stop batch: %v", err)
	}
	if len(receipts) != 2 || receipts[0].TaskName != first || receipts[1].TaskName != second || receipts[0].Phase != api.StopSettlementPhaseStopRequested || receipts[1].Phase != api.StopSettlementPhaseStopRequested {
		t.Fatalf("batch receipts = %+v, want ordered stop_requested rows", receipts)
	}
	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read atomic state: %v", err)
	}
	if len(persisted.StopSettlements) != 2 || persisted.StopSettlements[first] != receipts[0] || persisted.StopSettlements[second] != receipts[1] {
		t.Fatalf("durable receipts = %+v, want exact batch snapshot %+v", persisted.StopSettlements, receipts)
	}
}

func TestDaemonRuntimeTracker_StopSettlementRejectsStaleTransitionAndRemovesCommitLast(t *testing.T) {
	statePath := filepath.Join(apitest.HardenedTempDir(t), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	prepared := beginStopSettlementForTest(t, tracker, statePath, taskName)
	if _, err := tracker.AdvanceStopSettlement(statePath, prepared, api.StopSettlementPhaseExitObserved, "", ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := tracker.AdvanceStopSettlement(statePath, prepared, api.StopSettlementPhaseFailed, api.StopSettlementFailurePersistence, "stale completion"); err == nil {
		t.Fatal("stale receipt transition succeeded")
	}
	if err := tracker.RemoveStopSettlement(statePath, prepared); err == nil {
		t.Fatal("prepared receipt removed before port-release commit")
	}
	exited, ok := tracker.StopSettlementReceipt(taskName)
	if !ok || exited.Revision != prepared.Revision+1 || exited.Phase != api.StopSettlementPhaseExitObserved {
		t.Fatalf("receipt after stale transition = %+v present=%v", exited, ok)
	}
	released, err := tracker.AdvanceStopSettlement(statePath, exited, api.StopSettlementPhasePortReleased, "", "")
	if err != nil {
		t.Fatalf("record port released: %v", err)
	}
	if err := tracker.RemoveStopSettlement(statePath, released); err != nil {
		t.Fatalf("remove after port release: %v", err)
	}
	if _, ok := tracker.StopSettlementReceipt(taskName); ok {
		t.Fatal("receipt remained in tracker after durable removal")
	}
	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted removal: %v", err)
	}
	if _, ok := persisted.StopSettlements[taskName]; ok {
		t.Fatalf("receipt remained durable after removal: %+v", persisted.StopSettlements[taskName])
	}
	if persisted.StopSettlementEpoch != prepared.Epoch {
		t.Fatalf("epoch regressed on removal: %d want %d", persisted.StopSettlementEpoch, prepared.Epoch)
	}
}

func TestDaemonRuntimeTracker_StopSettlementTransitionTableAndDigestFence(t *testing.T) {
	statePath := filepath.Join(apitest.HardenedTempDir(t), "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4812, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	receipt := beginStopSettlementForTest(t, tracker, statePath, taskName)
	if receipt.Mode != "stop" || receipt.Port != 1 || receipt.Attempt != 1 || receipt.OperationID != receipt.BatchID {
		t.Fatalf("receipt identity = %+v, want mode/port/attempt/operation", receipt)
	}
	if _, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhasePortReleased, "", ""); err == nil {
		t.Fatal("illegal stop_requested -> port_released transition succeeded")
	}
	failed, err := tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseFailed, api.StopSettlementFailureListenerAlive, "listener remains bound")
	if err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if failed.ResumePhase != api.StopSettlementPhaseStopRequested || failed.FailureClass != api.StopSettlementFailureListenerAlive {
		t.Fatalf("failed receipt = %+v, want resume at stop_requested", failed)
	}
	resumed, err := tracker.AdvanceStopSettlement(statePath, failed, api.StopSettlementPhaseStopRequested, "", "")
	if err != nil {
		t.Fatalf("resume at recorded phase: %v", err)
	}
	if resumed.Attempt != 2 || resumed.Revision != failed.Revision+1 || resumed.FailureClass != "" || resumed.ResumePhase != "" {
		t.Fatalf("resumed receipt = %+v, want attempt+1 and cleared failure", resumed)
	}
	if _, err := tracker.AdvanceStopSettlement(statePath, failed, api.StopSettlementPhaseStopRequested, "", ""); err == nil {
		t.Fatal("stale failed receipt resumed after newer revision")
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state.StopSettlementMapGeneration == 0 || state.StopSettlementDigest == "" {
		t.Fatalf("state lacks receipt map integrity metadata: %+v", state)
	}
	if want, err := api.StopSettlementMapDigest(state.StopSettlementEpoch, state.StopSettlementMapGeneration, state.StopSettlements); err != nil || want != state.StopSettlementDigest {
		t.Fatalf("receipt map digest got=%q want=%q err=%v", state.StopSettlementDigest, want, err)
	}
	state.StopSettlementDigest = "bad"
	tracker.HydrateFromState(state)
	if err := tracker.StopSettlementIntegrityError(); err == nil {
		t.Fatal("digest mismatch did not fence lifecycle")
	}
}

// TestDaemonRuntimeTracker_HydrateFromStateAcceptsOmittedEstablishedEmptyStopSettlements
// catches recovery fencing after a successful terminal stop: the writer's
// empty map is omitted on JSON reload, but its established digest remains
// authoritative.
func TestDaemonRuntimeTracker_HydrateFromStateAcceptsOmittedEstablishedEmptyStopSettlements(t *testing.T) {
	digest, err := api.StopSettlementMapDigest(36, 292, map[string]api.StopSettlementReceiptV1{})
	if err != nil {
		t.Fatalf("digest empty map: %v", err)
	}
	state := &api.SupervisorStateFile{
		Version:                     1,
		StopSettlementEpoch:         36,
		StopSettlementMapGeneration: 292,
		StopSettlementDigest:        digest,
		// StopSettlements is deliberately nil: this is JSON's omitted-map shape.
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.HydrateFromState(state)
	if err := tracker.StopSettlementIntegrityError(); err != nil {
		t.Fatalf("omitted established empty map fenced hydration: %v", err)
	}
	if pending := tracker.PendingStopSettlements(); len(pending) != 0 {
		t.Fatalf("pending settlements = %+v, want none", pending)
	}

	state.StopSettlementDigest = "wrong"
	tracker.HydrateFromState(state)
	if err := tracker.StopSettlementIntegrityError(); err == nil {
		t.Fatal("wrong digest did not fence hydration")
	}
}

func TestStopSettlementRevisionComparisonIncludesAllSemanticFields(t *testing.T) {
	base := api.StopSettlementReceiptV1{
		Version:       1,
		BatchID:       "batch-a",
		TaskName:      `\mcp-local-hub-time-default`,
		Epoch:         1,
		PID:           4812,
		StartedAt:     "2026-08-31T12:00:00Z",
		PIDGeneration: 2,
		BatchIndex:    0,
		Mode:          "stop",
		Port:          9128,
		Revision:      3,
		Attempt:       2,
		Phase:         api.StopSettlementPhaseFailed,
		FailureDetail: "listener remains bound",
		FailureClass:  api.StopSettlementFailureListenerAlive,
		ResumePhase:   api.StopSettlementPhaseExitObserved,
		OperationID:   "op-a",
	}
	if !sameStopSettlementRevision(base, base) {
		t.Fatal("identical receipt did not match")
	}
	mutations := []struct {
		name string
		edit func(*api.StopSettlementReceiptV1)
	}{
		{"attempt", func(v *api.StopSettlementReceiptV1) { v.Attempt++ }},
		{"failure_class", func(v *api.StopSettlementReceiptV1) { v.FailureClass = api.StopSettlementFailureProcessAlive }},
		{"resume_phase", func(v *api.StopSettlementReceiptV1) { v.ResumePhase = api.StopSettlementPhaseStopRequested }},
		{"failure_detail", func(v *api.StopSettlementReceiptV1) { v.FailureDetail = "another detail" }},
		{"operation_id", func(v *api.StopSettlementReceiptV1) { v.OperationID = "op-b" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			tc.edit(&got)
			if sameStopSettlementRevision(base, got) {
				t.Fatalf("CAS comparison accepted changed %s: base=%+v changed=%+v", tc.name, base, got)
			}
		})
	}
}

func TestStopSettlementFailureClassIsClosedAndFailureDetailIsNonSemanticText(t *testing.T) {
	receipt := api.StopSettlementReceiptV1{
		Version:       1,
		BatchID:       "batch-a",
		TaskName:      `\mcp-local-hub-time-default`,
		Epoch:         1,
		PID:           4812,
		StartedAt:     "2026-08-31T12:00:00Z",
		PIDGeneration: 2,
		BatchIndex:    0,
		Mode:          "stop",
		Port:          9128,
		Revision:      3,
		Attempt:       2,
		Phase:         api.StopSettlementPhaseFailed,
		FailureClass:  api.StopSettlementFailureListenerAlive,
		FailureDetail: "port 9128 remains bound by pid 4812",
		ResumePhase:   api.StopSettlementPhaseExitObserved,
		OperationID:   "op-a",
	}
	if !validStopSettlementReceipt(receipt) {
		t.Fatalf("known typed failure receipt rejected: %+v", receipt)
	}
	receipt.FailureClass = api.StopSettlementFailureClass("text-derived")
	if validStopSettlementReceipt(receipt) {
		t.Fatalf("unknown failure class accepted: %+v", receipt)
	}
}

func TestPortFenceReceiptValidatorMatrix(t *testing.T) {
	base := api.StopSettlementReceiptV1{Version: 1, BatchID: "batch", TaskName: `\mcp-local-hub-time-default`, Epoch: 1, PID: 0, StartedAt: "", PIDGeneration: 0, BatchIndex: 0, Mode: "port_fence", Port: 9128, Revision: 1, Attempt: 1, Phase: api.StopSettlementPhaseExitObserved, OperationID: "batch"}
	if !validStopSettlementReceipt(base) {
		t.Fatalf("valid port fence rejected: %+v", base)
	}
	for _, tc := range []struct {
		name string
		edit func(*api.StopSettlementReceiptV1)
	}{
		{"stop_requested", func(v *api.StopSettlementReceiptV1) { v.Phase = api.StopSettlementPhaseStopRequested }},
		{"negative_generation", func(v *api.StopSettlementReceiptV1) { v.PIDGeneration = -1 }},
		{"pid", func(v *api.StopSettlementReceiptV1) { v.PID = 1 }},
		{"started", func(v *api.StopSettlementReceiptV1) { v.StartedAt = "2026-08-31T12:00:00Z" }},
		{"bad_failed_resume", func(v *api.StopSettlementReceiptV1) {
			v.Phase = api.StopSettlementPhaseFailed
			v.FailureClass = api.StopSettlementFailureListenerAlive
			v.FailureDetail = "bound"
			v.ResumePhase = api.StopSettlementPhaseStopRequested
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			tc.edit(&got)
			if validStopSettlementReceipt(got) {
				t.Fatalf("invalid port fence accepted: %+v", got)
			}
		})
	}
	failed := base
	failed.Phase = api.StopSettlementPhaseFailed
	failed.FailureClass = api.StopSettlementFailureListenerAlive
	failed.FailureDetail = "bound"
	failed.ResumePhase = api.StopSettlementPhaseExitObserved
	if !validStopSettlementReceipt(failed) {
		t.Fatalf("failed port fence rejected: %+v", failed)
	}
}

// TestDaemonRuntimeTracker_PersistDoesNotEmitVestigialRestartFields pins
// the 2026-06-20 supervisor audit P3 decision (Option 2: DELETE the
// vestigial restart-policy fields, document in-memory-only). The
// restart-policy runtime state (30-min crash sliding window, backoff
// deadline, quarantine timestamp, queued post-exit action) is
// in-memory-only in DaemonRuntimeTracker and the SM's SMContext; it is
// NOT persisted and resets on cold restart by design. The persisted
// supervisor-state.json must therefore NEVER carry restart_history /
// backoff_until / quarantine_since / queued_action keys — the previous
// schema claimed they were persisted but no production path ever wrote a
// non-empty value (this superseded the Codex deep-sec PR #268 Conc-F1
// forward-copy, which only ever preserved empties).
//
// The test asserts on the RAW JSON bytes because the Go struct no longer
// has those fields at all — a struct-field assertion can't catch a
// regression that re-adds them, but a raw-key scan can.
func TestDaemonRuntimeTracker_PersistDoesNotEmitVestigialRestartFields(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`

	// Drive a crash + backoff + quarantine + spawn sequence so any path
	// that WOULD persist restart-policy state has fired before the persist.
	tracker := NewDaemonRuntimeTracker()
	startedAt := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(taskName, 5555, startedAt)
	tracker.RecordCrashAndCountInWindow(taskName, startedAt.Add(time.Minute), 30*time.Minute)
	tracker.MarkBackoff(taskName)
	tracker.MarkQuarantined(taskName)
	tracker.MarkSpawned(taskName, 6666, startedAt.Add(2*time.Minute))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	// The tracker fields ARE persisted (state/pid/generation/started_at).
	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	row := persisted.Daemons[taskName]
	if row.State != "running" || row.CurrentPID != 6666 || row.PIDGeneration != 2 {
		t.Fatalf("persisted row tracker fields = %+v, want running pid=6666 generation=2", row)
	}

	// The vestigial restart-policy keys MUST be absent from the on-disk JSON.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read raw supervisor-state.json: %v", err)
	}
	for _, key := range []string{"restart_history", "backoff_until", "quarantine_since", "queued_action"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("persisted supervisor-state.json carries vestigial key %q (audit P3 regression):\n%s", key, raw)
		}
	}
}

func TestDaemonRuntimeTracker_PersistCollapsesRestartPolicyRuntimeStates(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	runningTask := `\mcp-local-hub-memory-default`
	backoffTask := `\mcp-local-hub-serena-default`
	quarantinedTask := `\mcp-local-hub-lsp-default`
	startedAt := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)

	hot := NewDaemonRuntimeTracker()
	hot.MarkSpawned(runningTask, 7777, startedAt)
	hot.MarkSpawned(backoffTask, 8888, startedAt.Add(time.Minute))
	hot.MarkBackoff(backoffTask)
	hot.MarkSpawned(quarantinedTask, 9999, startedAt.Add(2*time.Minute))
	hot.MarkQuarantined(quarantinedTask)
	if err := hot.PersistTo(statePath); err != nil {
		t.Fatalf("persist hot tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	if row := persisted.Daemons[runningTask]; row.State != "running" || row.CurrentPID != 7777 || row.PIDGeneration != 1 {
		t.Fatalf("running row = %+v, want running pid=7777 generation=1", row)
	}
	for _, taskName := range []string{backoffTask, quarantinedTask} {
		row := persisted.Daemons[taskName]
		if row.State != "idle" || row.CurrentPID != 0 || row.StartedAt != "" {
			t.Fatalf("restart-policy runtime row %s = %+v, want neutral idle pid=0 no started_at", taskName, row)
		}
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read raw supervisor-state.json: %v", err)
	}
	for _, transient := range []string{"backoff-waiting", "quarantined"} {
		if strings.Contains(string(raw), transient) {
			t.Fatalf("persisted supervisor-state.json carries transient state %q:\n%s", transient, raw)
		}
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		if proof.PID != 7777 {
			t.Fatalf("verified PID = %d, want only running PID 7777", proof.PID)
		}
		return nil
	}
	currentRunningIsPIDAliveFn = func(pid int) bool {
		t.Fatalf("verified PID path should not need alive fallback, got pid %d", pid)
		return false
	}
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	cold, currentRunning, runningPIDs, err := loadSupervisorStartupRuntime(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorStartupRuntime: %v", err)
	}
	if !currentRunning[runningTask] || runningPIDs[runningTask].PID != 7777 {
		t.Fatalf("verified running daemon not seeded: currentRunning=%v runningPIDs=%v", currentRunning, runningPIDs)
	}
	for _, taskName := range []string{backoffTask, quarantinedTask} {
		if currentRunning[taskName] {
			t.Fatalf("non-running restart-policy row %s reached currentRunning: %v", taskName, currentRunning)
		}
		entry, ok := cold.Get(taskName)
		if !ok {
			t.Fatalf("cold tracker missing %s", taskName)
		}
		if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
			t.Fatalf("cold tracker %s = %+v, want idle pid=0", taskName, entry)
		}
	}
}

// TestDaemonRuntimeTracker_RestartPolicyStateResetsOnColdRestart pins the
// in-memory-only contract: a crash window + quarantine that exists in one
// supervisor's tracker does NOT survive a cold restart (a fresh tracker
// hydrated from the persisted file). This is the behavior the audit P3
// decision documents as intentional (pre-restart crashes are irrelevant
// to runtime respawn decisions; a cold restart is an operator reset).
func TestDaemonRuntimeTracker_RestartPolicyStateResetsOnColdRestart(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	taskName := `\mcp-local-hub-memory-default`

	now := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	hot := NewDaemonRuntimeTracker()
	hot.MarkSpawned(taskName, 5555, now)
	// Record several crashes so the sliding window is non-empty in-memory.
	for i := 0; i < 3; i++ {
		hot.RecordCrashAndCountInWindow(taskName, now.Add(time.Duration(i)*time.Minute), 30*time.Minute)
	}
	if got := hot.CrashCountInWindow(taskName, now.Add(5*time.Minute), 30*time.Minute); got != 3 {
		t.Fatalf("hot tracker crash window = %d, want 3", got)
	}
	if err := hot.PersistTo(statePath); err != nil {
		t.Fatalf("persist hot tracker: %v", err)
	}

	// Cold restart: a fresh tracker hydrated from the persisted state has
	// NO crash history (the window is in-memory-only and was not persisted).
	cold, err := loadDaemonRuntimeTrackerFromStatePath(statePath)
	if err != nil {
		t.Fatalf("load cold tracker: %v", err)
	}
	if got := cold.CrashCountInWindow(taskName, now.Add(5*time.Minute), 30*time.Minute); got != 0 {
		t.Fatalf("cold tracker crash window = %d, want 0 (reset on restart)", got)
	}
	entry, ok := cold.Get(taskName)
	if !ok {
		t.Fatal("cold tracker missing hydrated entry")
	}
	// RestartCount is in-memory-only and also resets to 0 on cold restart.
	if entry.RestartCount != 0 {
		t.Fatalf("cold tracker RestartCount = %d, want 0 (reset on restart)", entry.RestartCount)
	}
}

// TestDaemonRuntimeTracker_PersistDropsRowForRemovedDaemon proves
// PersistTo rebuilds file.Daemons wholesale from the tracker snapshot:
// a task absent from the tracker (e.g. removed via clearRemovedTaskRuntime
// → tracker.Remove) is dropped from the file entirely rather than stranded
// as a stale row. (The wholesale rebuild is also why no stale vestigial
// restart-policy fields can survive — there is no per-row merge anymore.)
func TestDaemonRuntimeTracker_PersistDropsRowForRemovedDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	removed := `\mcp-local-hub-removed-default`
	kept := `\mcp-local-hub-memory-default`

	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			removed: {
				State:         "quarantined",
				PIDGeneration: 9,
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	// The tracker only knows about `kept` — `removed` was dropped (e.g. its
	// intent was removed and clearRemovedTaskRuntime called tracker.Remove).
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(kept, 4321, time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC))
	if err := tracker.PersistTo(statePath); err != nil {
		t.Fatalf("persist tracker: %v", err)
	}

	persisted, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor-state.json: %v", err)
	}
	if _, ok := persisted.Daemons[removed]; ok {
		t.Fatalf("removed daemon resurrected by persist preserve logic: %+v", persisted.Daemons[removed])
	}
	if _, ok := persisted.Daemons[kept]; !ok {
		t.Fatalf("kept daemon missing after persist: %+v", persisted.Daemons)
	}
}

func TestSupervisorCleanStartHydratesFromExistingState(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	startedAt := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			`\mcp-local-hub-memory-default`: {
				State:         "running",
				CurrentPID:    4321,
				PIDGeneration: 7,
				StartedAt:     startedAt.Format(time.RFC3339Nano),
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	tracker, err := loadDaemonRuntimeTrackerFromStatePath(statePath)
	if err != nil {
		t.Fatalf("load runtime tracker: %v", err)
	}
	entry, ok := tracker.Get(`\mcp-local-hub-memory-default`)
	if !ok {
		t.Fatal("hydrated runtime entry missing")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 4321 || entry.PIDGeneration != 7 || !entry.StartedAt.Equal(startedAt) {
		t.Fatalf("hydrated runtime entry = %+v, want running pid=4321 generation=7 started_at", entry)
	}
}

// TestMarkExitedIfCurrent_StaleGenerationIsNoop is the P1a source-side guard:
// a late exit stamped with an OLDER generation must leave the CURRENT child's
// tracking untouched and return false (the drop signal the wait goroutine acts
// on). This is the mechanism that stops the lost-child factory — a superseded
// child's cmd.Wait must never clear the live current child's CurrentPID.
func TestMarkExitedIfCurrent_StaleGenerationIsNoop(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `\mcp-local-hub-serena-default`
	now := time.Now().UTC()

	gen1 := tracker.MarkSpawned(taskName, 100, now)
	if gen1 != 1 {
		t.Fatalf("first MarkSpawned returned generation %d, want 1", gen1)
	}
	gen2 := tracker.MarkSpawned(taskName, 200, now.Add(time.Second))
	if gen2 != 2 {
		t.Fatalf("second MarkSpawned returned generation %d, want 2", gen2)
	}

	// A late exit for the SUPERSEDED gen1 child must be a no-op returning false.
	if tracker.MarkExitedIfCurrent(taskName, gen1) {
		t.Fatalf("MarkExitedIfCurrent(gen1=1) returned true; a stale exit must be dropped")
	}
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatal("entry missing after stale-exit drop")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID != 200 || entry.PIDGeneration != 2 {
		t.Fatalf("stale exit mutated current tracking: %+v, want running pid=200 generation=2", entry)
	}

	// The CURRENT gen2 child's exit clears normally and returns true.
	if !tracker.MarkExitedIfCurrent(taskName, gen2) {
		t.Fatalf("MarkExitedIfCurrent(gen2=2) returned false; the current generation must clear")
	}
	entry, _ = tracker.Get(taskName)
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 {
		t.Fatalf("current exit did not clear: %+v, want idle pid=0", entry)
	}
}

// TestMarkExitedIfCurrent_IdempotentForCurrentGeneration verifies the
// current-generation clear is idempotent: clearing twice at the same generation
// both return true (an already-cleared current-gen entry is still "current").
func TestMarkExitedIfCurrent_IdempotentForCurrentGeneration(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `\mcp-local-hub-memory-default`
	gen := tracker.MarkSpawned(taskName, 4321, time.Now().UTC())

	if !tracker.MarkExitedIfCurrent(taskName, gen) {
		t.Fatalf("first MarkExitedIfCurrent(gen=%d) returned false", gen)
	}
	// The generation is preserved across MarkExited-style clears, so a second
	// clear at the same generation is still current → true.
	if !tracker.MarkExitedIfCurrent(taskName, gen) {
		t.Fatalf("second MarkExitedIfCurrent(gen=%d) returned false; current-gen clear must be idempotent", gen)
	}
	entry, ok := tracker.Get(taskName)
	if !ok {
		t.Fatal("entry missing after idempotent clears")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || entry.PIDGeneration != gen {
		t.Fatalf("entry after idempotent clears = %+v, want idle pid=0 generation=%d preserved", entry, gen)
	}
}
