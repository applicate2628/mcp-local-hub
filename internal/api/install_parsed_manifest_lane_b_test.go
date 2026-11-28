package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/config"
)

// fakeMcphubIdentityForTest stubs processIdentityByPID to report every PID as
// an mcphub.exe image. The PR #290 identity gate fails CLOSED when a wired
// probe returns !ok, and on a Windows test host the production probe IS wired
// — a real lookup of a synthetic test PID would refuse the kill and turn these
// kill-path tests into probes of the host's process table.
//
// It ALSO stubs the SEC-F3 owner-SID arm (processOwnerSIDMatchesCurrentFn) to
// report every PID as an owned, current-user process. These kill-path tests use
// synthetic PIDs (4101/5101/6201/...) that do not refer to a live process; on a
// Windows test host the production owner-SID probe IS wired, and OpenProcess on
// a non-existent PID fails with ERROR_INVALID_PARAMETER, which the r4 classifier
// maps to the canonical process.ErrProcessAlreadyExited "already gone" sentinel.
// The r5 stop-force sentinel handling (Finding 2) then treats the synthetic PID
// as already-gone and SKIPS stopForceKillPIDFn — defeating the kill-assertion.
// Stubbing the seam to (true, nil) mirrors stop_force_supervisor_windows_test.go
// and force_kill_test.go so the owner-SID arm passes for a synthetic PID and the
// kill fires, which is exactly what the fixture's tests intend. The kill is still
// gated by ownership scope UPSTREAM of this arm, so a PID the scope logic declines
// to kill (e.g. the SparesHyphenSibling negative control) is still not killed.
func fakeMcphubIdentityForTest(t *testing.T) {
	t.Helper()
	orig := processIdentityByPID
	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, mcphubProcessImageName, true
	}
	t.Cleanup(func() { processIdentityByPID = orig })

	origSID := processOwnerSIDMatchesCurrentFn
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = origSID })
}

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
	fakeMcphubIdentityForTest(t)
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
	var completedPIDs []int
	var captures []*removedStopFixture
	installRemovedPrepareFixture(t, func(_ context.Context, task string, port int) (supervisorRemovedStop, error) {
		order = append(order, "capture")
		assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
		pid := map[string]int{seed.Daemons[0].TaskName: 4101, seed.Daemons[1].TaskName: 4102}[task]
		if pid == 0 {
			t.Fatalf("captured unrelated task %s", task)
		}
		stop := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
			order = append(order, "complete")
			assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
			current, err := ReadSupervisorIntent(intentPath)
			if err != nil || current.FindSupervisorDaemonByTaskName(task) != nil {
				t.Fatalf("cleanup preceded descriptor removal: %v", err)
			}
			completedPIDs = append(completedPIDs, pid)
			result := forceStopClosedFixture(task, port)
			result.Target.PID = pid
			return result, nil
		}}
		captures = append(captures, stop)
		return stop, nil
	})

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		order = append(order, "nudge")
		return ReconcileResponse{}, nil
	}))

	report := &UninstallReport{}
	NewAPI().removeServerFromSupervisorIntentBestEffort("demo", report)

	if !reflect.DeepEqual(completedPIDs, []int{4101, 4102}) {
		t.Fatalf("completed original fixture PIDs = %v, want [4101 4102] for the removed demo rows only", completedPIDs)
	}
	if !reflect.DeepEqual(order, []string{"capture", "capture", "nudge", "complete", "complete"}) {
		t.Fatalf("capture/nudge/complete order = %v, want [capture capture nudge complete complete]", order)
	}
	for _, capture := range captures {
		if capture.closed != 1 {
			t.Fatalf("capture close count=%d", capture.closed)
		}
	}
	if len(report.TaskDeleteWarns) != 0 {
		t.Fatalf("unexpected uninstall warnings: %v", report.TaskDeleteWarns)
	}
}

// Positive prepared add/drop coverage lives in CLI
// TestMigrateSerena_AlreadyMigrated_RegistryDrift_RefansOut. An ordinary API
// without an assembly must refuse before changing intent or capturing removals.
func TestInstallParsedManifest_MissingSerenaAssemblyPreservesWorkspaceRowsBeforeCleanup(t *testing.T) {
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

	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read prior supervisor intent: %v", err)
	}

	captureCalls, nudgeCalls := 0, 0
	installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) {
		captureCalls++
		return nil, errors.New("unexpected removal capture before Serena preparation")
	})
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		nudgeCalls++
		return ReconcileResponse{}, errors.New("unexpected reconcile before Serena preparation")
	}))

	path, err := NewAPI().InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		ManifestHash: testExactManifestHash,
		Writer:       io.Discard,
		Workspaces:   []WorkspaceEntry{keepEntry},
	})
	if path != "" || err == nil || !strings.Contains(err.Error(), "SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE") {
		t.Fatalf("unprepared Serena install returned path=%q error=%v", path, err)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("unprepared Serena install changed workspace intent: %v", err)
	}
	if captureCalls != 0 || nudgeCalls != 0 {
		t.Fatalf("unprepared Serena install reached cleanup: captures=%d nudges=%d", captureCalls, nudgeCalls)
	}
}

func TestInstallPlanCore_RemovedTargetKillDependsOnNudgeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nudgeErr  error
		wantKill  []int
		wantOrder []string
		wantWarn  bool
	}{
		{name: "success kills removed target", wantKill: []int{6201}, wantOrder: []string{"capture", "capture", "nudge", "complete"}},
		{name: "hard nudge failure skips kill", nudgeErr: errors.New("synthetic reconcile scheduler-list timeout"), wantOrder: []string{"capture", "capture", "nudge"}, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeMcphubIdentityForTest(t)
			stateDir := daemonIntentTestHelper(t)
			preparePreflightBinaryChecks(t)
			installFakeScheduler(t, newInstallFakeScheduler())
			installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

			const (
				removedTask  = "\\mcp-local-hub-demo-beta"
				retainedTask = "\\mcp-local-hub-demo-alpha"
			)
			seed := &SupervisorIntentFile{
				Version: 1,
				Daemons: []SupervisorDaemon{
					{TaskName: removedTask, Server: "demo", Daemon: "beta", Command: "old", Port: 0},
					{TaskName: retainedTask, Server: "demo", Daemon: "alpha", Command: "old-alpha", Port: 9321},
				},
			}
			intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
			if err := WriteSupervisorIntent(intentPath, seed); err != nil {
				t.Fatalf("seed supervisor-intent: %v", err)
			}

			var order []string
			var completedPIDs []int
			var captures []*removedStopFixture
			installRemovedPrepareFixture(t, func(_ context.Context, task string, port int) (supervisorRemovedStop, error) {
				order = append(order, "capture")
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
				current, err := ReadSupervisorIntent(intentPath)
				if err != nil || current.FindSupervisorDaemonByTaskName(task) == nil {
					t.Fatalf("capture followed descriptor removal: %v", err)
				}
				pid := map[string]int{removedTask: 6201, retainedTask: 6202}[task]
				if pid == 0 {
					t.Fatalf("unexpected capture %s", task)
				}
				capture := &removedStopFixture{complete: func(context.Context) (ForceStopResultV1, error) {
					order = append(order, "complete")
					assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
					current, err := ReadSupervisorIntent(intentPath)
					if err != nil || current.FindSupervisorDaemonByTaskName(task) != nil {
						t.Fatalf("cleanup preceded descriptor removal: %v", err)
					}
					completedPIDs = append(completedPIDs, pid)
					result := forceStopClosedFixture(task, port)
					result.Target.PID = pid
					return result, nil
				}}
				captures = append(captures, capture)
				return capture, nil
			})

			t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
				order = append(order, "nudge")
				assertSupervisorIntentFlockAvailableDuringIPCStatus(t, stateDir)
				current, err := ReadSupervisorIntent(intentPath)
				if err != nil || current.FindSupervisorDaemonByTaskName(removedTask) != nil || current.FindSupervisorDaemonByTaskName(retainedTask) == nil {
					t.Fatalf("nudge preceded committed removal or lost retained row: %v", err)
				}
				return ReconcileResponse{}, tc.nudgeErr
			}))

			m := &config.ServerManifest{
				Name:      "demo",
				Kind:      config.KindGlobal,
				Transport: config.TransportNativeHTTP,
				Command:   "go",
				Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 9321}},
			}
			plan, err := BuildPlan(m, "")
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}

			var buf bytes.Buffer
			if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, &buf); err != nil {
				t.Fatalf("installPlanCore: %v", err)
			}

			if !reflect.DeepEqual(completedPIDs, tc.wantKill) {
				t.Fatalf("completed original fixture PIDs = %v, want %v", completedPIDs, tc.wantKill)
			}
			if !reflect.DeepEqual(order, tc.wantOrder) {
				t.Fatalf("capture/nudge/complete order = %v, want %v", order, tc.wantOrder)
			}
			if len(captures) != 2 {
				t.Fatalf("captured %d original rows, want 2", len(captures))
			}
			for _, capture := range captures {
				if capture.closed != 1 {
					t.Fatalf("capture close count=%d, want 1", capture.closed)
				}
			}
			out := buf.String()
			hasWarn := strings.Contains(out, "warning:") &&
				strings.Contains(out, "reconcile") &&
				strings.Contains(out, "original creator custody required") &&
				strings.Contains(out, "cleanup incomplete")
			if strings.Contains(out, "mcphub stop --force") {
				t.Fatalf("cleanup warning recommends an unavailable CLI fallback: %s", out)
			}
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

func TestInstallParsedManifest_MissingSerenaAssemblyPreservesStopsBeforeCleanup(t *testing.T) {
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
		LegacyStopWatermarks: map[string]DaemonIntent{
			priorRows[0].TaskName: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			priorRows[1].TaskName: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			otherTask:             {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		},
	}
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	// Keep redundant legacy watermarks in the prior data until an admitted write.
	if err := WriteStateFileAtomic(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read prior supervisor intent: %v", err)
	}

	captureCalls, nudgeCalls := 0, 0
	installRemovedPrepareFixture(t, func(context.Context, string, int) (supervisorRemovedStop, error) {
		captureCalls++
		return nil, errors.New("unexpected removal capture before Serena preparation")
	})
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		nudgeCalls++
		return ReconcileResponse{}, errors.New("unexpected reconcile before Serena preparation")
	}))

	path, err := NewAPI().InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		ManifestHash: testExactManifestHash,
		Writer:       io.Discard,
		Workspaces:   []WorkspaceEntry{keepEntry},
	})
	if path != "" || err == nil || !strings.Contains(err.Error(), "SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE") {
		t.Fatalf("unprepared Serena install returned path=%q error=%v", path, err)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("unprepared Serena install changed stop or watermark data: %v", err)
	}
	if captureCalls != 0 || nudgeCalls != 0 {
		t.Fatalf("unprepared Serena install reached cleanup: captures=%d nudges=%d", captureCalls, nudgeCalls)
	}
}
