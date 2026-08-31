package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestSupervisorState_RoundTrip(t *testing.T) {
	// v0.5.0 Fix Group 5: WriteSupervisorState now flows through
	// the hardened secure-write pipeline. See the matching note in
	// supervisor_intent_test.go for why hardenedTempDir is required.
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")

	want := SupervisorStateFile{
		Version: 1,
		Daemons: map[string]SupervisorDaemonState{
			`\mcp-local-hub-memory-default`: {
				State:         "running",
				CurrentPID:    12345,
				PIDGeneration: 7,
				StartedAt:     "2026-05-16T18:00:00.000000000Z",
			},
		},
		TransientPIDs: []TransientPID{
			{PID: 23456, Kind: "workspace-weekly-refresh", StartedAt: "2026-05-16T03:00:00.000000000Z"},
		},
		MaintenanceFiredAt: map[string]string{
			"workspace-weekly-refresh": "2026-05-15T03:00:00.000000000Z",
		},
	}
	if err := WriteSupervisorState(path, &want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Daemons[`\mcp-local-hub-memory-default`].PIDGeneration != 7 {
		t.Fatalf("pid_generation lost")
	}
	if len(got.TransientPIDs) != 1 {
		t.Fatalf("transient_pids lost")
	}
}

// TestSupervisorState_StopSettlementReceiptRoundTrip pins the durable fence
// between a requested stop and any later re-registration of the same task.
// The receipt is intentionally additive: an older binary may ignore it, but a
// current supervisor must not silently lose it while rewriting runtime rows.
func TestSupervisorState_StopSettlementReceiptRoundTrip(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	const taskName = `\mcp-local-hub-time-default`
	want := &SupervisorStateFile{
		Version:             1,
		StopSettlementEpoch: 41,
		Daemons: map[string]SupervisorDaemonState{
			taskName: {State: "running", CurrentPID: 4812, PIDGeneration: 7, StartedAt: "2026-08-31T12:00:00Z"},
		},
		StopSettlements: map[string]StopSettlementReceiptV1{
			taskName: {
				Version:       1,
				BatchID:       "state-roundtrip",
				TaskName:      taskName,
				Epoch:         41,
				PID:           4812,
				StartedAt:     "2026-08-31T12:00:00Z",
				PIDGeneration: 7,
				Phase:         StopSettlementPhaseStopRequested,
			},
		},
	}
	if err := WriteSupervisorState(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.StopSettlementEpoch != want.StopSettlementEpoch {
		t.Fatalf("stop settlement epoch = %d, want %d", got.StopSettlementEpoch, want.StopSettlementEpoch)
	}
	receipt, ok := got.StopSettlements[taskName]
	if !ok || receipt != want.StopSettlements[taskName] {
		t.Fatalf("stop settlement receipt = %+v, want %+v", receipt, want.StopSettlements[taskName])
	}
}

func TestMutateSupervisorStateReadsUnderStateFileFlock(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}

	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock supervisor state file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- MutateSupervisorState(path, func(file *SupervisorStateFile) error {
			if file.MaintenanceFiredAt == nil {
				file.MaintenanceFiredAt = map[string]string{}
			}
			file.MaintenanceFiredAt["workspace-weekly-refresh"] = "2026-06-30T00:00:00Z"
			return nil
		})
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("MutateSupervisorState returned before the flock holder published a valid file; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	valid := []byte(`{"version":1,"daemons":{"task-a":{"state":"running","current_pid":42,"pid_generation":2}}}`)
	if err := WriteStateFileBytesLockHeld(path, valid); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish valid supervisor state under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock supervisor state file: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("MutateSupervisorState after flock release: %v", err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read supervisor state: %v", err)
	}
	if got.Daemons["task-a"].CurrentPID != 42 {
		t.Fatalf("daemon state lost after mutation: %+v", got.Daemons)
	}
	if got.MaintenanceFiredAt["workspace-weekly-refresh"] != "2026-06-30T00:00:00Z" {
		t.Fatalf("maintenance fired-at mutation missing: %+v", got.MaintenanceFiredAt)
	}
}

func TestSupervisorState_ReadRejectsSymlinkTarget(t *testing.T) {
	dir := hardenedTempDir(t)
	realPath := filepath.Join(dir, "real-supervisor-state.json")
	linkPath := filepath.Join(dir, "supervisor-state.json")

	if err := WriteStateFileAtomic(realPath, &SupervisorStateFile{Version: 1}); err != nil {
		t.Fatalf("seed real state: %v", err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	_, err := ReadSupervisorState(linkPath)
	if err == nil {
		t.Fatalf("ReadSupervisorState followed symlink target; want refusal")
	}
	if !errors.Is(err, ErrIrregularFile) {
		t.Fatalf("ReadSupervisorState err = %v, want ErrIrregularFile", err)
	}
}

// TestSupervisorState_IgnoresRemovedRestartPolicyFields proves a
// pre-audit-P3 supervisor-state.json carrying restart_history /
// backoff_until / quarantine_since / queued_action still reads cleanly
// after those fields were removed from the Go struct: they fall under the
// "ignore unknown fields" contract, so an in-place binary downgrade or a
// stale on-disk file never bricks supervisor startup. The known fields are
// preserved.
func TestSupervisorState_IgnoresRemovedRestartPolicyFields(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	body := `{"version":1,"daemons":{"\\mcp-local-hub-memory-default":{` +
		`"state":"backoff-waiting","current_pid":0,"pid_generation":4,` +
		`"restart_history":[{"at":"2026-06-09T09:50:00.000000000Z","exit_code":1}],` +
		`"backoff_until":"2026-06-09T10:05:00.000000000Z",` +
		`"quarantine_since":"2026-06-09T09:30:00.000000000Z",` +
		`"queued_action":{"kind":"respawn","reason":"manual-restart"}}}}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read state with removed restart-policy fields: %v", err)
	}
	daemon := got.Daemons[`\mcp-local-hub-memory-default`]
	if daemon.State != "backoff-waiting" || daemon.PIDGeneration != 4 {
		t.Fatalf("known daemon fields lost: %+v", daemon)
	}
}

func TestSupervisorState_IgnoresUnknownFields(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor-state.json")
	body := `{"version":1,"daemons":{"\\mcp-local-hub-memory-default":{"state":"running","current_pid":12345,"job_protection":false,"future_daemon_field":"x"}},"transient_pids":[],"future_top_level":"x"}`
	if err := WriteStateFileAtomic(path, json.RawMessage(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorState(path)
	if err != nil {
		t.Fatalf("read with unknown fields: %v", err)
	}
	daemon := got.Daemons[`\mcp-local-hub-memory-default`]
	if daemon.State != "running" || daemon.CurrentPID != 12345 {
		t.Fatalf("known daemon fields lost: %+v", daemon)
	}
	if daemon.JobProtection == nil || *daemon.JobProtection {
		t.Fatalf("known job_protection field lost: %+v", daemon.JobProtection)
	}
}
