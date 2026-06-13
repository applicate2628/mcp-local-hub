package api

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

func TestUnregister_ReconcileFailureRestoresBareKeySupervisorStop(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	reconcileCalls := 0
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		reconcileCalls++
		return ReconcileResponse{}, errors.New("synthetic live supervisor reconcile failure")
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	ws := t.TempDir()
	canonical, err := CanonicalWorkspacePath(ws)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	wsKey := WorkspaceKey(canonical)
	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      "go",
		Backend:       "gopls-mcp",
		Port:          0,
		TaskName:      LSPTaskNameForWorkspaceLanguage(wsKey, "go"),
		Lifecycle:     LifecycleConfigured,
	}
	reg := NewRegistry(h.regPath)
	if err := reg.PutLSP(entry); err != nil {
		t.Fatalf("PutLSP: %v", err)
	}
	if err := reg.Save(); err != nil {
		t.Fatalf("Save registry: %v", err)
	}

	descriptor := BuildSupervisorDaemonForLSP(entry, "mcphub")
	bareTask := strings.TrimPrefix(descriptor.TaskName, "\\")
	operatorStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			descriptor,
		},
		Stops: map[string]DaemonIntent{
			bareTask: operatorStop,
		},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	_, err = mustNewAPI(t).unregisterWithManifest(nineLanguageManifest(), ws, []string{"go"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("unregister should fail when live-supervisor reconcile fails")
	}
	if !strings.Contains(err.Error(), "restored supervisor intent descriptor") {
		t.Fatalf("unregister error = %v, want restored-descriptor failure", err)
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconcileCalls)
	}

	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := got.FindSupervisorDaemonByTaskName(descriptor.TaskName); row == nil {
		t.Fatalf("rollback did not restore descriptor %q; rows=%+v", descriptor.TaskName, got.Daemons)
	}
	canonicalTask := descriptor.TaskName
	gotStop, ok := got.Stops[canonicalTask]
	if !ok {
		t.Fatalf("rollback did not restore bare-key operator stop under canonical key %q; stops=%+v", canonicalTask, got.Stops)
	}
	if gotStop.Desired != operatorStop.Desired || gotStop.Reason != operatorStop.Reason || !gotStop.UpdatedAt.Equal(operatorStop.UpdatedAt) {
		t.Fatalf("restored stop = %+v, want %+v", gotStop, operatorStop)
	}

	reg = NewRegistry(h.regPath)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load registry: %v", err)
	}
	if rows := reg.ListByWorkspaceLSP(wsKey); len(rows) != 1 || rows[0].Language != "go" {
		t.Fatalf("registry rows after failed unregister = %+v, want original go row preserved", rows)
	}
}

func TestRemoveSerenaSupervisorIntentForWorkspace_KillsLiveProxyBeforeDescriptorRemoval(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	workspace := t.TempDir()
	canonical, err := CanonicalWorkspacePath(workspace)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	descriptor := SupervisorDaemon{
		TaskName:  SerenaTaskNameForWorkspace(canonical),
		Server:    "serena",
		Daemon:    WorkspaceKey(canonical),
		Workspace: canonical,
		Port:      9123,
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{descriptor},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	origKill := forceKillByPortFn
	origReconcile := registerSupervisorReconcileFn
	var events []string
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		events = append(events, "kill")
		if port != descriptor.Port {
			t.Fatalf("forceKillByPortFn port=%d, want %d", port, descriptor.Port)
		}
		return portKillKilled, nil
	}
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		events = append(events, "reconcile")
		return ReconcileResponse{}, nil
	}
	defer func() {
		forceKillByPortFn = origKill
		registerSupervisorReconcileFn = origReconcile
	}()

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(canonical)
	if err != nil {
		t.Fatalf("RemoveSerenaSupervisorIntentForWorkspace: %v", err)
	}
	if !removed {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace removed=false")
	}
	if got, want := events, []string{"kill", "reconcile"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
}

func TestRemoveSerenaSupervisorIntentForWorkspace_KillFailurePreservesDescriptor(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	workspace := t.TempDir()
	canonical, err := CanonicalWorkspacePath(workspace)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	descriptor := SupervisorDaemon{
		TaskName:  SerenaTaskNameForWorkspace(canonical),
		Server:    "serena",
		Daemon:    WorkspaceKey(canonical),
		Workspace: canonical,
		Port:      9124,
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{descriptor}}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	origKill := forceKillByPortFn
	origReconcile := registerSupervisorReconcileFn
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) {
		return portKillLookupUnavailable, errors.New("synthetic kill failure")
	}
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		t.Fatal("reconcile must not run when live serena kill fails")
		return ReconcileResponse{}, nil
	}
	defer func() {
		forceKillByPortFn = origKill
		registerSupervisorReconcileFn = origReconcile
	}()

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(canonical)
	if err == nil {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace returned nil error for kill failure")
	}
	if removed {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace removed=true after kill failure")
	}
	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := got.FindSupervisorDaemonByTaskName(descriptor.TaskName); row == nil {
		t.Fatalf("descriptor %q was removed despite kill failure", descriptor.TaskName)
	}
}

func TestRemoveSerenaSupervisorIntentForWorkspace_RemovesStopAndNudges(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	reconcileCalls := 0
	registerSupervisorReconcileFn = func(_ context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		if !apply {
			t.Fatal("serena supervisor teardown must request apply=true reconcile")
		}
		return ReconcileResponse{}, nil
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	workspace := t.TempDir()
	canonical, err := CanonicalWorkspacePath(workspace)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	taskName := SerenaTaskNameForWorkspace(canonical)
	stop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName:  taskName,
			Server:    "serena",
			Daemon:    WorkspaceKey(canonical),
			Workspace: canonical,
		}},
		Stops: map[string]DaemonIntent{taskName: stop},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(canonical)
	if err != nil {
		t.Fatalf("RemoveSerenaSupervisorIntentForWorkspace: %v", err)
	}
	if !removed {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace removed=false")
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconcileCalls)
	}
	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := got.FindSupervisorDaemonByTaskName(taskName); row != nil {
		t.Fatalf("serena descriptor %q survived removal: %+v", taskName, row)
	}
	if _, ok := got.Stops[taskName]; ok {
		t.Fatalf("serena stop %q survived removal: %+v", taskName, got.Stops)
	}
}

func TestRemoveSerenaSupervisorIntentForWorkspace_ReconcileFailureRestoresDescriptorAndStop(t *testing.T) {
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	origReconcile := registerSupervisorReconcileFn
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, errors.New("synthetic live supervisor reconcile failure")
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	workspace := t.TempDir()
	canonical, err := CanonicalWorkspacePath(workspace)
	if err != nil {
		t.Fatalf("CanonicalWorkspacePath: %v", err)
	}
	descriptor := SupervisorDaemon{
		TaskName:  SerenaTaskNameForWorkspace(canonical),
		Server:    "serena",
		Daemon:    WorkspaceKey(canonical),
		Workspace: canonical,
	}
	stop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
	}
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			descriptor,
		},
		Stops: map[string]DaemonIntent{
			descriptor.TaskName: stop,
		},
	}); err != nil {
		t.Fatalf("WriteSupervisorIntent: %v", err)
	}

	removed, err := NewAPI().RemoveSerenaSupervisorIntentForWorkspace(canonical)
	if err == nil {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace should fail on live-supervisor reconcile error")
	}
	if !removed {
		t.Fatal("RemoveSerenaSupervisorIntentForWorkspace removed=false after descriptor removal")
	}
	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if row := got.FindSupervisorDaemonByTaskName(descriptor.TaskName); row == nil {
		t.Fatalf("rollback did not restore serena descriptor %q; rows=%+v", descriptor.TaskName, got.Daemons)
	}
	gotStop, ok := got.Stops[descriptor.TaskName]
	if !ok {
		t.Fatalf("rollback did not restore serena stop %q; stops=%+v", descriptor.TaskName, got.Stops)
	}
	if gotStop.Desired != stop.Desired || gotStop.Reason != stop.Reason || !gotStop.UpdatedAt.Equal(stop.UpdatedAt) {
		t.Fatalf("restored serena stop = %+v, want %+v", gotStop, stop)
	}
}
