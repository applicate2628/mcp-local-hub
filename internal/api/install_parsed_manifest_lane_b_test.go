package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/config"
)

func assertSupervisorIntentFlockAvailableDuringIPCStatus(t *testing.T, stateDir string) {
	t.Helper()
	probe := flock.New(filepath.Join(stateDir, supervisorIntentFileLeaf) + supervisorIntentLockSuffix)
	locked, err := probe.TryLock()
	if err != nil {
		t.Fatalf("probe supervisor-intent flock during IPC status: %v", err)
	}
	if !locked {
		t.Fatal("supervisor IPC status was called while the supervisor-intent flock was held")
	}
	if err := probe.Unlock(); err != nil {
		t.Fatalf("unlock supervisor-intent flock probe: %v", err)
	}
}

func TestRemoveServerFromSupervisorIntentBestEffort_KillsRemovedDaemonsAfterNudge(t *testing.T) {
	stateDir := phaseFStateDir(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "demo", Port: 0},
			{TaskName: `\mcp-local-hub-demo-beta`, Server: "demo", Daemon: "beta", Command: "demo", Port: 0},
			{TaskName: `\mcp-local-hub-other-default`, Server: "other", Daemon: "default", Command: "other", Port: 0},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	var order []string
	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		order = append(order, "ipc-status")
		assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
		return []DaemonStatus{
			{TaskName: `\mcp-local-hub-demo-alpha`, PID: 4101, State: "Running"},
			{TaskName: `\mcp-local-hub-demo-beta`, PID: 4102, State: "Running"},
			{TaskName: `\mcp-local-hub-other-default`, PID: 4999, State: "Running"},
		}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	var killedPIDs []int
	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error {
		order = append(order, "kill-pid")
		killedPIDs = append(killedPIDs, pid)
		return nil
	}
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		order = append(order, "nudge")
		return ReconcileResponse{}, nil
	}))

	report := &UninstallReport{}
	NewAPI().removeServerFromSupervisorIntentBestEffort("demo", report)

	if !reflect.DeepEqual(killedPIDs, []int{4101, 4102}) {
		t.Fatalf("killed PIDs = %v, want [4101 4102] for the removed demo rows only", killedPIDs)
	}
	if !reflect.DeepEqual(order, []string{"ipc-status", "nudge", "kill-pid", "kill-pid"}) {
		t.Fatalf("ipc/nudge/kill order = %v, want [ipc-status nudge kill-pid kill-pid]", order)
	}
	if len(report.TaskDeleteWarns) != 0 {
		t.Fatalf("unexpected uninstall warnings: %v", report.TaskDeleteWarns)
	}
}

func TestInstallParsedManifest_KillsDroppedWorkspaceRowAfterNudgeOnly(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	installFakeScheduler(t, newInstallFakeScheduler())

	wsDrop := t.TempDir()
	wsKeep := t.TempDir()
	dropEntry := WorkspaceEntry{WorkspaceKey: WorkspaceKey(wsDrop), WorkspacePath: wsDrop, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9401}
	keepEntry := WorkspaceEntry{WorkspaceKey: WorkspaceKey(wsKeep), WorkspacePath: wsKeep, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9402}
	m := serenaTemplateManifest()

	priorRows := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{dropEntry, keepEntry}, "seed", "mcphub")
	if len(priorRows) != 2 {
		t.Fatalf("prior serena rows = %d, want 2", len(priorRows))
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1, Daemons: priorRows}); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	var order []string
	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		order = append(order, "ipc-status")
		assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
		return []DaemonStatus{
			{TaskName: priorRows[0].TaskName, PID: 5101, State: "Running"},
			{TaskName: priorRows[1].TaskName, PID: 5102, State: "Running"},
		}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	var killedPorts []int
	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		order = append(order, "kill-port")
		killedPorts = append(killedPorts, port)
		return portKillKilled, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origForceKill })

	// Lane A inverted the kill preference: a captured supervisor-reported PID
	// is killed FIRST (identity-gated, best-effort), and the port kill is only
	// the no-PID-snapshot fallback. The IPC fake above reports PID 5101 for
	// the dropped row, so the kill must arrive via stopForceKillPIDFn.
	var killedPIDs []int
	origPIDKill := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error {
		order = append(order, "kill-pid")
		killedPIDs = append(killedPIDs, pid)
		return nil
	}
	t.Cleanup(func() { stopForceKillPIDFn = origPIDKill })

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		order = append(order, "nudge")
		return ReconcileResponse{}, nil
	}))

	if _, err := NewAPI().InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: []WorkspaceEntry{keepEntry},
	}); err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}

	if !reflect.DeepEqual(killedPIDs, []int{5101}) {
		t.Fatalf("killed PIDs = %v, want [5101] for the dropped workspace row only", killedPIDs)
	}
	if len(killedPorts) != 0 {
		t.Fatalf("killed ports = %v, want none (PID-first kill must not fall through to the port path)", killedPorts)
	}
	if !reflect.DeepEqual(order, []string{"ipc-status", "nudge", "kill-pid"}) {
		t.Fatalf("ipc/nudge/kill order = %v, want [ipc-status nudge kill-pid]", order)
	}
}

func TestInstallPlanCore_RemovedTargetKillDependsOnNudgeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nudgeErr error
		wantKill []int
		wantWarn bool
	}{
		{name: "success kills removed target", wantKill: []int{6201}},
		{name: "hard nudge failure skips kill", nudgeErr: errors.New("synthetic reconcile scheduler-list timeout"), wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := daemonIntentTestHelper(t)
			preparePreflightBinaryChecks(t)
			installFakeScheduler(t, newInstallFakeScheduler())
			installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

			const removedTask = `\mcp-local-hub-demo-beta`
			seed := &SupervisorIntentFile{
				Version: 1,
				Daemons: []SupervisorDaemon{
					{TaskName: removedTask, Server: "demo", Daemon: "beta", Command: "old", Port: 0},
				},
			}
			if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), seed); err != nil {
				t.Fatalf("seed supervisor-intent: %v", err)
			}

			origStatus := supervisorIPCStatusFn
			supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
				return []DaemonStatus{{TaskName: removedTask, PID: 6201, State: "Running"}}, nil
			}
			t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

			var killedPIDs []int
			origPID := stopForceKillPIDFn
			stopForceKillPIDFn = func(pid int) error {
				killedPIDs = append(killedPIDs, pid)
				return nil
			}
			t.Cleanup(func() { stopForceKillPIDFn = origPID })

			t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
				return ReconcileResponse{}, tc.nudgeErr
			}))

			m := &config.ServerManifest{
				Name:      "demo",
				Kind:      config.KindGlobal,
				Transport: config.TransportRemoteHTTP,
				URL:       "https://example.invalid/mcp",
			}
			plan, err := BuildPlan(m, "")
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}

			var buf bytes.Buffer
			if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
				t.Fatalf("installPlanCore: %v", err)
			}

			if !reflect.DeepEqual(killedPIDs, tc.wantKill) {
				t.Fatalf("killed PIDs = %v, want %v", killedPIDs, tc.wantKill)
			}
			out := buf.String()
			hasWarn := strings.Contains(out, "warning:") &&
				strings.Contains(out, "reconcile") &&
				strings.Contains(out, "re-run install") &&
				strings.Contains(out, "mcphub stop --force demo")
			if hasWarn != tc.wantWarn {
				t.Fatalf("warning presence = %v, want %v; output:\n%s", hasWarn, tc.wantWarn, out)
			}
		})
	}
}

func TestInstallPlanCore_GlobalInstall_HeldSupervisorLockAfterIPCFailureSkipsRemovedTargetKill(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	installFakeScheduler(t, newInstallFakeScheduler())
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	const removedTask = `\mcp-local-hub-demo-beta`
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: removedTask, Server: "demo", Daemon: "beta", Command: "old", Port: 0},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
		return []DaemonStatus{{TaskName: removedTask, PID: 6201, State: "Running"}}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	var killedPIDs []int
	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error {
		killedPIDs = append(killedPIDs, pid)
		return nil
	}
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) { return true, 4242, nil }
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	origStart := installAutostartOwnerStartFn
	installAutostartOwnerStartFn = func() error {
		t.Fatal("autostart owner start must not run when the supervisor lock is already held")
		return nil
	}
	t.Cleanup(func() { installAutostartOwnerStartFn = origStart })

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, errSupervisorUnavailableForTest()
	}))

	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://example.invalid/mcp",
	}
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
		t.Fatalf("installPlanCore: %v", err)
	}

	if len(killedPIDs) != 0 {
		t.Fatalf("held supervisor lock after IPC failure killed removed PID(s) %v, want none", killedPIDs)
	}
	out := buf.String()
	for _, want := range []string{"supervisor lock is held", "IPC is unreachable", "mcphub restart", "force kill removed supervisor targets skipped"} {
		if !strings.Contains(out, want) {
			t.Fatalf("install output missing %q; output:\n%s", want, out)
		}
	}
}

func TestInstallParsedManifest_PrunesStopsForDroppedWorkspaceRows(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	installFakeScheduler(t, newInstallFakeScheduler())

	wsDrop := t.TempDir()
	wsKeep := t.TempDir()
	dropEntry := WorkspaceEntry{WorkspaceKey: WorkspaceKey(wsDrop), WorkspacePath: wsDrop, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9403}
	keepEntry := WorkspaceEntry{WorkspaceKey: WorkspaceKey(wsKeep), WorkspacePath: wsKeep, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9404}
	m := serenaTemplateManifest()

	priorRows := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{dropEntry, keepEntry}, "seed", "mcphub")
	if len(priorRows) != 2 {
		t.Fatalf("prior serena rows = %d, want 2", len(priorRows))
	}
	otherTask := `\mcp-local-hub-other-default`
	now := time.Unix(1700000000, 0).UTC()
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: append(priorRows, SupervisorDaemon{
			TaskName: otherTask,
			Server:   "other",
			Daemon:   "default",
			Command:  "other",
			Port:     0,
		}),
		Stops: map[string]DaemonIntent{
			priorRows[0].TaskName: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			priorRows[1].TaskName: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			otherTask:             {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		},
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) { return nil, nil }
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })
	origForceKill := forceKillByPortFn
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) { return portKillKilled, nil }
	t.Cleanup(func() { forceKillByPortFn = origForceKill })
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	if _, err := NewAPI().InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     io.Discard,
		Workspaces: []WorkspaceEntry{keepEntry},
	}); err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if _, ok := written.Stops[priorRows[0].TaskName]; ok {
		t.Fatalf("dropped workspace stop %q survived: %+v", priorRows[0].TaskName, written.Stops)
	}
	if _, ok := written.Stops[priorRows[1].TaskName]; !ok {
		t.Fatalf("surviving workspace stop %q was pruned: %+v", priorRows[1].TaskName, written.Stops)
	}
	if _, ok := written.Stops[otherTask]; !ok {
		t.Fatalf("sibling server stop %q was pruned: %+v", otherTask, written.Stops)
	}
}
