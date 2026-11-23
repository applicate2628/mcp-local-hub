package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

func TestSupervisorStatusPendingStopRAMUsesReceiptIdentity(t *testing.T) {
	const ram = uint64(4096)
	alive := process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytes: ram, ResidentBytesKnown: true}
	for _, failed := range []bool{false, true} {
		state := "Stopping"
		if failed {
			state = "Failed"
		}
		t.Run(state, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				observation process.PIDStatusObservation
				observeErr  error
				change      func(*DaemonRuntimeEntry)
				wantCalls   int
				wantRAM     uint64
			}{
				{name: "matching", observation: alive, wantCalls: 1, wantRAM: ram},
				{name: "identity_mismatch", observation: alive, observeErr: process.ErrProcessIdentityMismatch, wantCalls: 1},
				{name: "probe_error", observation: alive, observeErr: errors.New("query denied"), wantCalls: 1},
				{name: "exited", observation: process.PIDStatusObservation{State: process.PIDStateDead}, wantCalls: 1},
				{name: "unknown_state", observation: process.PIDStatusObservation{State: process.PIDStateUnknown}, wantCalls: 1},
				{name: "memory_unknown", observation: process.PIDStatusObservation{State: process.PIDStateAlive}, wantCalls: 1},
				{name: "memory_zero", observation: process.PIDStatusObservation{State: process.PIDStateAlive, ResidentBytesKnown: true}, wantCalls: 1},
				{name: "different_pid", change: func(entry *DaemonRuntimeEntry) { entry.CurrentPID++ }},
				{name: "different_start", change: func(entry *DaemonRuntimeEntry) { entry.StartedAt = entry.StartedAt.Add(time.Second) }},
				{name: "different_generation", change: func(entry *DaemonRuntimeEntry) { entry.PIDGeneration++ }},
				{name: "missing_start", change: func(entry *DaemonRuntimeEntry) { entry.StartedAt = time.Time{} }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					stateDir, tracker, d, _ := seedStatusPIDObservation(t)
					statePath := filepath.Join(stateDir, "supervisor-state.json")
					receipt := beginStopSettlementForTest(t, tracker, statePath, d.TaskName)
					if failed {
						var err error
						receipt, err = tracker.AdvanceStopSettlement(statePath, receipt, api.StopSettlementPhaseFailed, api.StopSettlementFailureProcessAlive, "process still present")
						if err != nil {
							t.Fatal(err)
						}
					}
					if tc.change != nil {
						// Model independently read runtime/receipt snapshots that no
						// longer describe one generation, without altering the token.
						tracker.mu.Lock()
						entry := tracker.entries[d.TaskName]
						tc.change(&entry)
						tracker.entries[d.TaskName] = entry
						tracker.mu.Unlock()
					}
					before := tracker.Snapshot()
					beforeState, err := os.ReadFile(statePath)
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
						PIDAlive: func(int) bool { t.Error("pending receipt ran liveness"); return false },
						PIDIdentity: func(process.PIDIdentityProof) error {
							t.Error("pending receipt ran scalar identity")
							return errors.New("unexpected scalar identity")
						},
						PortLive: func(int) bool { t.Error("pending receipt ran port classification"); return false },
						PIDStatus: func(proof process.PIDIdentityProof) (process.PIDStatusObservation, error) {
							calls++
							want := process.PIDIdentityProof{PID: receipt.PID, ExecutablePath: daemonExpectedIdentityExe(d.Command), StartedAt: receipt.StartedAt}
							if proof != want {
								t.Errorf("RAM proof=%+v, want receipt-qualified %+v", proof, want)
							}
							return tc.observation, tc.observeErr
						},
					})
					t.Cleanup(restore)
					withFakeRSS(t, func(int) (uint64, bool) {
						t.Error("pending receipt fell back to scalar RAM")
						return 999 * ram, true
					})
					rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
					if err != nil || len(rows) != 1 {
						t.Fatalf("status rows=%+v error=%v", rows, err)
					}
					entry := before[d.TaskName]
					if rows[0]["state"] != state || rows[0]["current_pid"] != entry.CurrentPID || rows[0]["started_at"] != daemonRuntimeStartedAt(entry.StartedAt) {
						t.Fatalf("optional RAM changed stop projection: %+v", rows[0])
					}
					if calls != tc.wantCalls {
						t.Errorf("observations=%d, want %d", calls, tc.wantCalls)
					}
					if gotRAM, present := rows[0]["ram_bytes"]; (tc.wantRAM == 0 && present) || (tc.wantRAM != 0 && (!present || gotRAM != tc.wantRAM)) {
						t.Errorf("ram_bytes=%v present=%v, want %d", gotRAM, present, tc.wantRAM)
					}
					wantDiagnostic := map[string]any{
						"version": receipt.Version, "task_name": receipt.TaskName, "epoch": receipt.Epoch,
						"owned_pid": receipt.PID, "started_at": receipt.StartedAt, "pid_generation": receipt.PIDGeneration,
						"revision": receipt.Revision, "phase": string(receipt.Phase),
					}
					if failed {
						wantDiagnostic["failure_class"] = string(receipt.FailureClass)
						wantDiagnostic["failure_detail"] = receipt.FailureDetail
					}
					if !reflect.DeepEqual(rows[0]["stop_settlement"], wantDiagnostic) {
						t.Errorf("receipt diagnostic changed: got %+v, want %+v", rows[0]["stop_settlement"], wantDiagnostic)
					}
					if !reflect.DeepEqual(before, tracker.Snapshot()) {
						t.Error("status changed runtime state")
					}
					if after, present := tracker.StopSettlementReceipt(d.TaskName); !present || after != receipt {
						t.Errorf("status changed stop receipt: %+v", after)
					}
					afterState, err := os.ReadFile(statePath)
					if err != nil || !bytes.Equal(beforeState, afterState) {
						t.Errorf("status changed durable state: %v", err)
					}
				})
			}
		})
	}
}
