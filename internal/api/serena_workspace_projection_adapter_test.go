package api

import (
	"context"
	"errors"
	"testing"
)

func TestProjectSerenaWorkspaceSnapshotHeld_ProjectsRegisteredRowAndUnreadyEndpoint(t *testing.T) {
	in := serenaAuthorityJoinHealthyInput()
	stateRoot := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateRoot))
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &in.SupervisorIntent); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	originalEndpoint := serenaWorkspaceProjectionEndpointFn
	endpointCalls := 0
	serenaWorkspaceProjectionEndpointFn = func(*API, context.Context) (SerenaClientEndpoint, error) {
		endpointCalls++
		return SerenaClientEndpoint{}, &SerenaClientEndpointUnreadyError{
			Stage: MCPFrontProbeStageInitializeResultMissing,
			Cause: errors.New("test endpoint is not ready"),
		}
	}
	t.Cleanup(func() { serenaWorkspaceProjectionEndpointFn = originalEndpoint })

	originalDial := statusInternalDialFn
	statusInternalDialFn = func(context.Context) ([]DaemonStatus, error) {
		t.Fatal("held projection must not open a second supervisor IPC dial")
		return nil, nil
	}
	t.Cleanup(func() { statusInternalDialFn = originalDial })

	out, err := NewAPI().ProjectSerenaWorkspaceSnapshotHeld(
		context.Background(), in.RegistryRows, SerenaWorkspaceProjectionModeSnapshot, in.StatusRows, in.Readiness,
	)
	if err != nil {
		t.Fatalf("held snapshot projection: %v", err)
	}
	if endpointCalls != 1 {
		t.Fatalf("endpoint observations = %d, want 1", endpointCalls)
	}
	if len(out.Workspaces) != 1 || len(out.StatusRows) != 1 {
		t.Fatalf("projection cardinality = workspaces:%d status:%d, want 1:1", len(out.Workspaces), len(out.StatusRows))
	}

	workspace := out.Workspaces[0]
	if workspace.WorkspaceKey != in.RegistryRows[0].WorkspaceKey || workspace.TaskName != in.RegistryRows[0].TaskName {
		t.Fatalf("registered workspace projection = %+v, want row %q/%q", workspace, in.RegistryRows[0].WorkspaceKey, in.RegistryRows[0].TaskName)
	}
	if workspace.ClientEndpoint != "" || workspace.EndpointMode != "" || workspace.ServiceState != ServiceStateDegraded {
		t.Fatalf("unready endpoint projection = %+v, want suppressed endpoint and degraded state", workspace)
	}
	if workspace.ReadinessStage != ReadinessStageComplete || !workspace.ReadinessSettled {
		t.Fatalf("held readiness join = %+v, want complete and settled", workspace)
	}
	if out.StatusRows[0].ServiceState != ServiceStateDegraded || out.StatusRows[0].ReadinessStage != ReadinessStageComplete || !out.StatusRows[0].ReadinessSettled {
		t.Fatalf("held status join = %+v, want degraded status with retained complete readiness", out.StatusRows[0])
	}
}
