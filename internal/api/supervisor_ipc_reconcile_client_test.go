package api

import (
	"context"
	"encoding/json"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

// Keep the test identity short: Go folds it into t.TempDir(), which in turn
// becomes the Unix-domain-socket path. The production path-length contract is
// covered separately; this roundtrip fixture must stay below sun_path.
func TestIPCReconcileRoundtrip(t *testing.T) {
	tests := []struct {
		name  string
		apply bool
		resp  ReconcileResponse
	}{
		{
			name:  "dry-run",
			apply: false,
			resp: ReconcileResponse{
				DryRun:              true,
				DriftCount:          2,
				AppliedCount:        0,
				SerenaRepairOutcome: SerenaIntentRepairOutcomeCompleted,
				Drift: []DriftEntry{
					{
						TaskName:       `\mcp-local-hub-memory-default`,
						SchedulerState: ReconcileSchedulerStateStopped,
						IntentDesired:  ReconcileIntentDesiredRunning,
						SMState:        StIdle,
						Action:         ReconcileActionPostEvIntentUpdate,
					},
					{
						TaskName:       `\mcp-local-hub-orphan-default`,
						SchedulerState: ReconcileSchedulerStateRunning,
						IntentDesired:  ReconcileIntentDesiredUnknown,
						SMState:        StIdle,
						Action:         ReconcileActionNeedsManualReview,
					},
				},
			},
		},
		{
			name:  "apply",
			apply: true,
			resp: ReconcileResponse{
				DryRun:                 false,
				DriftCount:             2,
				AppliedCount:           1,
				SerenaRepairOutcome:    SerenaIntentRepairOutcomeSkippedRegistryLock,
				SerenaRepairIncomplete: []SerenaIntentRepairIncomplete{{WorkspaceKey: "abcd1234", Reason: SerenaIntentRepairIncompleteGenerationMismatch}},
				SerenaRepairRecovered:  []SerenaIntentRepairRecovery{{WorkspaceKey: "efgh5678", Reason: SerenaIntentRepairRecoveryLegacyLeaseExpired}},
				Drift: []DriftEntry{
					{
						TaskName:       `\mcp-local-hub-memory-default`,
						SchedulerState: ReconcileSchedulerStateStopped,
						IntentDesired:  ReconcileIntentDesiredRunning,
						SMState:        StIdle,
						Action:         ReconcileActionPostEvIntentUpdate,
					},
					{
						TaskName:       `\mcp-local-hub-orphan-default`,
						SchedulerState: ReconcileSchedulerStateRunning,
						IntentDesired:  ReconcileIntentDesiredUnknown,
						SMState:        StIdle,
						Action:         ReconcileActionNeedsManualReview,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := apitest.HardenedTempDir(t)
			withDaemonStateRootOverride(t, stateDir)
			owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-21T10:00:00Z"}
			writeSupervisorOwnerForTest(t, stateDir, owner)

			stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
				if req.Cmd != "reconcile" {
					t.Fatalf("cmd = %q, want reconcile", req.Cmd)
				}
				gotApply, ok := req.Args["apply"].(bool)
				if !ok {
					t.Fatalf("apply arg = %#v, want bool", req.Args["apply"])
				}
				if gotApply != tt.apply {
					t.Fatalf("apply arg = %v, want %v", gotApply, tt.apply)
				}
				if _, present := req.Args["settle_target"]; present {
					t.Fatalf("legacy reconcile request unexpectedly carried settle_target: %#v", req.Args)
				}
				return IPCResponse{
					ID:     req.ID,
					OK:     true,
					Result: tt.resp,
					Final:  true,
				}
			})
			defer stop()

			got, err := DialSupervisorIPCReconcile(context.Background(), tt.apply)
			if err != nil {
				t.Fatalf("DialSupervisorIPCReconcile: %v", err)
			}
			if got.DryRun != tt.resp.DryRun {
				t.Fatalf("DryRun = %v, want %v", got.DryRun, tt.resp.DryRun)
			}
			if got.DriftCount != tt.resp.DriftCount {
				t.Fatalf("DriftCount = %d, want %d", got.DriftCount, tt.resp.DriftCount)
			}
			if got.AppliedCount != tt.resp.AppliedCount {
				t.Fatalf("AppliedCount = %d, want %d", got.AppliedCount, tt.resp.AppliedCount)
			}
			if got.SerenaRepairOutcome != tt.resp.SerenaRepairOutcome {
				t.Fatalf("SerenaRepairOutcome = %q, want %q", got.SerenaRepairOutcome, tt.resp.SerenaRepairOutcome)
			}
			if len(got.SerenaRepairRecovered) != len(tt.resp.SerenaRepairRecovered) {
				t.Fatalf("SerenaRepairRecovered = %+v, want %+v", got.SerenaRepairRecovered, tt.resp.SerenaRepairRecovered)
			}
			for i := range tt.resp.SerenaRepairRecovered {
				if got.SerenaRepairRecovered[i] != tt.resp.SerenaRepairRecovered[i] {
					t.Fatalf("SerenaRepairRecovered[%d] = %+v, want %+v", i, got.SerenaRepairRecovered[i], tt.resp.SerenaRepairRecovered[i])
				}
			}
			if len(got.Drift) != len(tt.resp.Drift) {
				t.Fatalf("Drift len = %d, want %d: %+v", len(got.Drift), len(tt.resp.Drift), got.Drift)
			}
			for i := range tt.resp.Drift {
				if got.Drift[i] != tt.resp.Drift[i] {
					t.Fatalf("Drift[%d] = %+v, want %+v", i, got.Drift[i], tt.resp.Drift[i])
				}
			}
		})
	}
}

func TestDialSupervisorIPCReconcileTarget_Roundtrip(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-08-02T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)
	target := ReconcileTarget{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "testdata/workspace",
		TaskName:      "mcp-local-hub-serena-abcd1234",
		RegisteredAt:  "2026-08-02T10:00:00.123456789Z",
		ExpectedPort:  9311,
	}
	wantSettlement := &ReconcileTargetSettlement{
		State:         ReconcileTargetSettlementReady,
		Reason:        ReconcileTargetReasonReady,
		Target:        target,
		CurrentPID:    4812,
		PIDGeneration: 3,
	}
	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		encoded, err := json.Marshal(req.Args["settle_target"])
		if err != nil {
			t.Fatalf("marshal settle_target: %v", err)
		}
		var got ReconcileTarget
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("decode settle_target: %v", err)
		}
		if got != target {
			t.Fatalf("settle_target = %+v, want %+v", got, target)
		}
		return IPCResponse{
			ID: req.ID,
			OK: true,
			Result: ReconcileResponse{
				TargetSettlement: wantSettlement,
			},
			Final: true,
		}
	})
	defer stop()

	got, err := DialSupervisorIPCReconcileTarget(context.Background(), true, target)
	if err != nil {
		t.Fatalf("DialSupervisorIPCReconcileTarget: %v", err)
	}
	if got.TargetSettlement == nil || *got.TargetSettlement != *wantSettlement {
		t.Fatalf("TargetSettlement = %+v, want %+v", got.TargetSettlement, wantSettlement)
	}
}

func TestReconcileResponseSerenaRepairOutcomeWireCompatibility(t *testing.T) {
	newProducerPayload := []byte(`{"dry_run":false,"drift_count":0,"applied_count":0,"serena_repair_outcome":"completed","serena_repair_recovered":[{"workspace_key":"abcd1234","reason":"legacy_lease_expired"}]}`)
	var legacyConsumer struct {
		DryRun       bool `json:"dry_run"`
		DriftCount   int  `json:"drift_count"`
		AppliedCount int  `json:"applied_count"`
	}
	if err := json.Unmarshal(newProducerPayload, &legacyConsumer); err != nil {
		t.Fatalf("legacy consumer decode new payload: %v", err)
	}
	if legacyConsumer.DryRun || legacyConsumer.DriftCount != 0 || legacyConsumer.AppliedCount != 0 {
		t.Fatalf("legacy consumer decoded changed existing fields: %+v", legacyConsumer)
	}

	oldProducerPayload := []byte(`{"dry_run":false,"drift_count":0,"applied_count":0}`)
	var currentConsumer ReconcileResponse
	if err := json.Unmarshal(oldProducerPayload, &currentConsumer); err != nil {
		t.Fatalf("current consumer decode old payload: %v", err)
	}
	if currentConsumer.SerenaRepairOutcome != "" {
		t.Fatalf("old producer outcome = %q, want empty so the CLI treats it as incomplete", currentConsumer.SerenaRepairOutcome)
	}
	if len(currentConsumer.SerenaRepairRecovered) != 0 {
		t.Fatalf("old producer recoveries = %+v, want empty additive field", currentConsumer.SerenaRepairRecovered)
	}
}
