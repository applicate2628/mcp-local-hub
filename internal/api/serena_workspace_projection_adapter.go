package api

import (
	"context"
	"errors"
	"fmt"
)

// serenaWorkspaceProjectionEndpointFn is the narrow routing-observation seam.
// Production still uses the existing API routing owner; tests can provide one
// already-observed endpoint without opening a listener.
var serenaWorkspaceProjectionEndpointFn = func(a *API, ctx context.Context) (SerenaClientEndpoint, error) {
	return a.ResolveSerenaClientEndpoint(ctx)
}

// ProjectSerenaWorkspaceSnapshotCurrent acquires one command-scoped Serena
// authority snapshot, then delegates every comparison to the pure S1 join.
// It is intentionally the only presenter-facing acquisition adapter: callers
// receive a projection, never registry/intent/status/endpoint fragments.
func (a *API) ProjectSerenaWorkspaceSnapshotCurrent(ctx context.Context, rows []WorkspaceEntry, mode SerenaWorkspaceProjectionMode) (SerenaWorkspaceProjectionOutputV1, error) {
	statusRows, readiness, err := a.statusReadinessSnapshot(ctx)
	if err != nil {
		return SerenaWorkspaceProjectionOutputV1{}, err
	}
	return a.ProjectSerenaWorkspaceSnapshotHeld(ctx, rows, mode, statusRows, readiness)
}

// ProjectSerenaWorkspaceSnapshotHeld reuses the caller's already-observed
// supervisor status and readiness snapshot. In particular statusInternal calls
// this after its sole IPC dial, so rendering Serena rows cannot redial it.
func (a *API) ProjectSerenaWorkspaceSnapshotHeld(ctx context.Context, rows []WorkspaceEntry, mode SerenaWorkspaceProjectionMode, statusRows []DaemonStatus, readiness ReadinessSnapshotV1) (SerenaWorkspaceProjectionOutputV1, error) {
	if !hasSerenaWorkspaceRows(rows) {
		return SerenaWorkspaceProjectionOutputV1{StatusRows: cloneDaemonStatusRows(statusRows)}, nil
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return SerenaWorkspaceProjectionOutputV1{}, err
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		return SerenaWorkspaceProjectionOutputV1{}, fmt.Errorf("read Serena supervisor intent: %w", err)
	}
	endpoint, endpointErr := serenaWorkspaceProjectionEndpointFn(a, ctx)
	if endpointErr != nil {
		var unready *SerenaClientEndpointUnreadyError
		if !errors.As(endpointErr, &unready) {
			return SerenaWorkspaceProjectionOutputV1{}, endpointErr
		}
		endpoint = SerenaClientEndpoint{ReadinessStage: string(unready.Stage)}
	}
	return ProjectSerenaWorkspaceSnapshot(SerenaWorkspaceProjectionInputV1{
		Mode: mode, RegistryRows: rows, SupervisorIntent: *intent,
		StatusRows: statusRows, Readiness: readiness, Endpoint: endpoint,
	})
}

func hasSerenaWorkspaceRows(rows []WorkspaceEntry) bool {
	for _, row := range rows {
		if row.Language == SerenaLanguageSentinel && row.Backend == SerenaServerName {
			return true
		}
	}
	return false
}
