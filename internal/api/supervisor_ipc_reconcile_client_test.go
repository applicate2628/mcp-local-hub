package api

import (
	"context"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func TestDialSupervisorIPCReconcile_RoundtripWithFakeListener(t *testing.T) {
	tests := []struct {
		name  string
		apply bool
		resp  ReconcileResponse
	}{
		{
			name:  "dry-run",
			apply: false,
			resp: ReconcileResponse{
				DryRun:       true,
				DriftCount:   2,
				AppliedCount: 0,
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
				DryRun:       false,
				DriftCount:   2,
				AppliedCount: 1,
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
