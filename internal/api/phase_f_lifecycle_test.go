package api

// Cross-phase v0.6 Phase F lifecycle tests (bot PR #288 F1/F2/F3): Phase F
// routed GLOBAL daemons off per-daemon scheduler tasks and into
// supervisor-intent.json, but the uninstall / fresh-install / stop --force
// surfaces still assumed the scheduler-task model. These tests lock the
// supervisor-intent-aware behavior in.
//
// All tests are hermetic: apitest.HardenedTempDir + SetDaemonStateRootForTest
// redirect every state read/write to a fresh temp dir, LOCALAPPDATA /
// XDG_STATE_HOME are redirected so no resolver touches the real registry, and
// the supervisor reconcile / kill paths go through their package seams
// (supervisorReconcileApplyFn, killByPortFn, stopForceKillPIDFn,
// supervisorIPCStatusFn). Nothing touches the live host
// %LOCALAPPDATA%\mcp-local-hub\ or the real scheduler / supervisor.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// phaseFStateDir builds the hermetic state dir + env redirects shared by the
// Phase F lifecycle tests and returns the resolved state dir.
func phaseFStateDir(t *testing.T) string {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(stateDir, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(stateDir, "config"))
	t.Setenv("HOME", filepath.Join(stateDir, "home"))
	t.Setenv("USERPROFILE", filepath.Join(stateDir, "home"))
	t.Setenv("APPDATA", filepath.Join(stateDir, "AppData", "Roaming"))
	return stateDir
}

// ---------------------------------------------------------------------------
// F1 — uninstall removes the server's supervisor-intent descriptors,
// server-weekly-refresh timer, and stops, preserving every sibling, and
// nudges reconcile.
// ---------------------------------------------------------------------------

// TestRemoveServerFromSupervisorIntent_RemovesRowsTimerStops_PreservesSibling is
// the F1 core: a seeded intent with TWO servers (demo + other), demo carrying a
// descriptor row, a server-weekly-refresh timer, and a stop entry. Uninstalling
// demo must drop ALL of demo's artifacts while leaving every other-server
// artifact byte-identical.
//
// Negative-control: a pre-fix uninstall never touched supervisor-intent.json,
// so demo's row/timer/stop all survive (the supervisor respawns demo forever).
// Asserting they are GONE fails against the pre-fix (no-op) code.
func TestRemoveServerFromSupervisorIntent_RemovesRowsTimerStops_PreservesSibling(t *testing.T) {
	stateDir := phaseFStateDir(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	demoStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	demoBetaWatermark := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	otherStop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	enabledFalse := false
	seed := &SupervisorIntentFile{
		Version:    1,
		StrictMode: true,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "demo-cmd", Port: 9211},
			// A blank-Server legacy row keyed to demo must ALSO be dropped
			// (same ParseManagedTaskName ownership the F4 full-install path uses).
			{TaskName: `\mcp-local-hub-demo-beta`, Command: "demo-blank-cmd", Port: 9212},
			{TaskName: `\mcp-local-hub-other-d`, Server: "other", Daemon: "d", Command: "other-cmd", Port: 9991},
		},
		MaintenanceTimers: []MaintenanceTimer{
			{Name: `\mcp-local-hub-demo-weekly-refresh`, Kind: "server-weekly-refresh", Server: "demo", Command: "demo-cmd", Args: []string{"restart", "--server", "demo"}},
			{Name: `\mcp-local-hub-other-weekly-refresh`, Kind: "server-weekly-refresh", Server: "other", Command: "other-cmd", Args: []string{"restart", "--server", "other"}, Enabled: &enabledFalse},
		},
		Stops: map[string]DaemonIntent{
			`\mcp-local-hub-demo-alpha`: demoStop,
			`\mcp-local-hub-other-d`:    otherStop,
		},
		LegacyStopWatermarks: map[string]DaemonIntent{
			`\mcp-local-hub-demo-alpha`: demoStop,
			`\mcp-local-hub-demo-beta`:  demoBetaWatermark,
			`\mcp-local-hub-other-d`:    otherStop,
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	changed, err := NewAPI().removeServerFromSupervisorIntent("demo")
	if err != nil {
		t.Fatalf("removeServerFromSupervisorIntent: %v", err)
	}
	if !changed {
		t.Fatal("removeServerFromSupervisorIntent reported changed=false; want true (demo owned rows/timer/stops)")
	}

	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}

	// All demo descriptor rows gone (including the blank-Server one); the
	// sibling other/d survives verbatim.
	for _, d := range got.Daemons {
		if parsedServer, _ := ParseManagedTaskName(d.TaskName); parsedServer == "demo" || d.Server == "demo" {
			t.Errorf("demo descriptor row survived uninstall: %+v", d)
		}
	}
	if len(got.Daemons) != 1 || got.Daemons[0].TaskName != `\mcp-local-hub-other-d` {
		t.Fatalf("sibling daemon rows not preserved exactly; got %+v", got.Daemons)
	}
	if got.Daemons[0].Command != "other-cmd" || got.Daemons[0].Port != 9991 {
		t.Errorf("sibling daemon row mutated: %+v", got.Daemons[0])
	}

	// demo's weekly-refresh timer gone; other's preserved verbatim (incl. its
	// Enabled=&false off-switch).
	if len(got.MaintenanceTimers) != 1 || got.MaintenanceTimers[0].Server != "other" {
		t.Fatalf("maintenance timers not pruned to the sibling only; got %+v", got.MaintenanceTimers)
	}
	if got.MaintenanceTimers[0].Enabled == nil || *got.MaintenanceTimers[0].Enabled != false {
		t.Errorf("sibling timer off-switch not preserved verbatim: %+v", got.MaintenanceTimers[0])
	}

	// demo's stop gone; other's stop preserved.
	if _, ok := got.Stops[`\mcp-local-hub-demo-alpha`]; ok {
		t.Error("demo stop entry survived uninstall")
	}
	gotOther, ok := got.Stops[`\mcp-local-hub-other-d`]
	if !ok {
		t.Fatal("sibling stop entry was wiped by the uninstall cleanup")
	}
	if gotOther.Desired != otherStop.Desired || gotOther.Reason != otherStop.Reason || !gotOther.UpdatedAt.Equal(otherStop.UpdatedAt) {
		t.Errorf("sibling stop entry mutated: got %+v want %+v", gotOther, otherStop)
	}
	if _, ok := got.LegacyStopWatermarks[`\mcp-local-hub-demo-alpha`]; ok {
		t.Error("demo legacy-stop watermark survived uninstall")
	}
	if _, ok := got.LegacyStopWatermarks[`\mcp-local-hub-demo-beta`]; ok {
		t.Error("blank-Server demo legacy-stop watermark survived uninstall")
	}
	if _, ok := got.LegacyStopWatermarks[`\mcp-local-hub-other-d`]; ok {
		t.Fatalf("sibling stop retained redundant legacy-stop watermark: %+v", got.LegacyStopWatermarks)
	}

	// StrictMode is carried through untouched.
	if !got.StrictMode {
		t.Error("StrictMode flipped by the uninstall cleanup; want preserved")
	}
}

func TestRemoveServerFromSupervisorIntent_PrunesAmbiguousStopsByRemovedTaskName(t *testing.T) {
	now := time.Now().UTC()

	t.Run("demo alpha-beta stop removed with exact descriptor", func(t *testing.T) {
		stateDir := phaseFStateDir(t)
		intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
		seed := &SupervisorIntentFile{
			Version: 1,
			Daemons: []SupervisorDaemon{
				{TaskName: `\mcp-local-hub-demo-alpha-beta`, Server: "demo", Daemon: "alpha-beta", Command: "demo", Port: 33111},
				{TaskName: `\mcp-local-hub-other-default`, Server: "other", Daemon: "default", Command: "other", Port: 33112},
			},
			Stops: map[string]DaemonIntent{
				`\mcp-local-hub-demo-alpha-beta`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
				`\mcp-local-hub-other-default`:   {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			},
		}
		if err := WriteSupervisorIntent(intentPath, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		changed, err := NewAPI().removeServerFromSupervisorIntent("demo")
		if err != nil {
			t.Fatalf("removeServerFromSupervisorIntent: %v", err)
		}
		if !changed {
			t.Fatal("changed=false, want true")
		}
		got, err := ReadSupervisorIntent(intentPath)
		if err != nil {
			t.Fatalf("ReadSupervisorIntent: %v", err)
		}
		if _, ok := got.Stops[`\mcp-local-hub-demo-alpha-beta`]; ok {
			t.Fatalf("ambiguous stop for removed demo/alpha-beta descriptor survived: %+v", got.Stops)
		}
		if _, ok := got.Stops[`\mcp-local-hub-other-default`]; !ok {
			t.Fatalf("unrelated sibling stop was not preserved: %+v", got.Stops)
		}
	})

	t.Run("demo-alpha beta stop survives demo uninstall when its row survives", func(t *testing.T) {
		stateDir := phaseFStateDir(t)
		intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
		seed := &SupervisorIntentFile{
			Version: 1,
			Daemons: []SupervisorDaemon{
				{TaskName: `\mcp-local-hub-demo-default`, Server: "demo", Daemon: "default", Command: "demo", Port: 33113},
				{TaskName: `\mcp-local-hub-demo-alpha-beta`, Server: "demo-alpha", Daemon: "beta", Command: "demo-alpha", Port: 33114},
			},
			Stops: map[string]DaemonIntent{
				`\mcp-local-hub-demo-default`:    {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
				`\mcp-local-hub-demo-alpha-beta`: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			},
		}
		if err := WriteSupervisorIntent(intentPath, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		changed, err := NewAPI().removeServerFromSupervisorIntent("demo")
		if err != nil {
			t.Fatalf("removeServerFromSupervisorIntent: %v", err)
		}
		if !changed {
			t.Fatal("changed=false, want true")
		}
		got, err := ReadSupervisorIntent(intentPath)
		if err != nil {
			t.Fatalf("ReadSupervisorIntent: %v", err)
		}
		if _, ok := got.Stops[`\mcp-local-hub-demo-default`]; ok {
			t.Fatalf("demo/default stop survived uninstall: %+v", got.Stops)
		}
		if _, ok := got.Stops[`\mcp-local-hub-demo-alpha-beta`]; !ok {
			t.Fatalf("demo-alpha/beta sibling stop was pruned during demo uninstall: %+v", got.Stops)
		}
	})
}

// TestRemoveServerFromSupervisorIntent_MissingFile_NoOp asserts that a host
// with no supervisor-intent.json (e.g. a never-installed or remote-http-only
// host) makes the cleanup a no-op, not an error.
func TestRemoveServerFromSupervisorIntent_MissingFile_NoOp(t *testing.T) {
	phaseFStateDir(t)
	changed, err := NewAPI().removeServerFromSupervisorIntent("demo")
	if err != nil {
		t.Fatalf("removeServerFromSupervisorIntent(missing file): %v", err)
	}
	if changed {
		t.Error("changed=true for a missing intent file; want false")
	}
}

// TestRemoveServerFromSupervisorIntentBestEffort_NudgesReconcileOnlyWhenChanged
// proves the uninstall wrapper nudges the supervisor reconcile EXACTLY when it
// removed something, and skips the nudge for a no-op cleanup (nothing to
// terminate). The reconcile is driven through the supervisorReconcileApplyFn
// seam so no real IPC is touched.
func TestRemoveServerFromSupervisorIntentBestEffort_NudgesReconcileOnlyWhenChanged(t *testing.T) {
	stateDir := phaseFStateDir(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "demo-cmd", Port: 9211},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var reconcileCalls int32
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		if !apply {
			t.Errorf("uninstall reconcile nudge dialed apply=false, want apply=true")
		}
		return ReconcileResponse{}, nil
	}))

	// Removing demo (which exists) → changed → exactly one reconcile nudge.
	NewAPI().removeServerFromSupervisorIntentBestEffort("demo", &UninstallReport{})
	if got := atomic.LoadInt32(&reconcileCalls); got != 1 {
		t.Fatalf("reconcile nudge calls after removing-an-existing-server = %d, want 1", got)
	}

	// Removing demo AGAIN (now absent) → no change → no nudge.
	NewAPI().removeServerFromSupervisorIntentBestEffort("demo", &UninstallReport{})
	if got := atomic.LoadInt32(&reconcileCalls); got != 1 {
		t.Fatalf("reconcile nudge fired for a no-op cleanup; total calls = %d, want still 1", got)
	}
}

// TestUninstall_SchedulerUnavailableStillRemovesSupervisorIntent asserts the
// POSIX Phase-F lifecycle edge: a global daemon may exist only as a
// supervisor-intent descriptor, with no per-daemon scheduler task. If the
// scheduler backend is unavailable, uninstall must treat legacy tasks as an
// empty set and still remove this server's supervisor-intent artifacts.
//
// Negative-control: restore Uninstall/uninstallWithoutManifest to return on
// scheduler.ErrNotImplemented before removeServerFromSupervisorIntentBestEffort
// and both subtests fail with an uninstall error plus surviving intent rows.
func TestUninstall_SchedulerUnavailableStillRemovesSupervisorIntent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		server string
		call   func(*API, string) (*UninstallReport, error)
	}{
		{
			name:   "manifest-backed",
			server: "time",
			call: func(a *API, server string) (*UninstallReport, error) {
				return a.Uninstall(server)
			},
		},
		{
			name:   "retired-manifest",
			server: "gdb",
			call: func(a *API, server string) (*UninstallReport, error) {
				return a.uninstallWithoutManifest(server)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := phaseFStateDir(t)
			intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
			seed := &SupervisorIntentFile{
				Version: 1,
				Daemons: []SupervisorDaemon{
					{TaskName: `\mcp-local-hub-` + tc.server + `-default`, Server: tc.server, Daemon: "default", Command: "cmd", Port: 9128},
					{TaskName: `\mcp-local-hub-other-default`, Server: "other", Daemon: "default", Command: "other", Port: 9991},
				},
				MaintenanceTimers: []MaintenanceTimer{
					{Name: `\mcp-local-hub-` + tc.server + `-weekly-refresh`, Kind: "server-weekly-refresh", Server: tc.server},
					{Name: `\mcp-local-hub-other-weekly-refresh`, Kind: "server-weekly-refresh", Server: "other"},
				},
			}
			if err := WriteSupervisorIntent(intentPath, seed); err != nil {
				t.Fatalf("seed supervisor-intent: %v", err)
			}

			restoreScheduler := SetTestSchedulerFactoryFn(func() (scheduler.Scheduler, error) {
				return nil, fmt.Errorf("fake posix scheduler: %w", scheduler.ErrNotImplemented)
			})
			t.Cleanup(restoreScheduler)

			var reconcileCalls int32
			t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
				if !apply {
					t.Errorf("uninstall reconcile nudge apply = false, want true")
				}
				atomic.AddInt32(&reconcileCalls, 1)
				return ReconcileResponse{}, nil
			}))

			report, err := tc.call(NewAPI(), tc.server)
			if err != nil {
				t.Fatalf("uninstall with scheduler unavailable: %v", err)
			}
			if report == nil || report.Server != tc.server {
				t.Fatalf("report = %+v, want server %q", report, tc.server)
			}
			if len(report.TasksDeleted) != 0 {
				t.Fatalf("scheduler-unavailable uninstall deleted tasks = %v, want none", report.TasksDeleted)
			}

			got, err := ReadSupervisorIntent(intentPath)
			if err != nil {
				t.Fatalf("ReadSupervisorIntent: %v", err)
			}
			for _, d := range got.Daemons {
				if supervisorIntentRowOwnedBy(d, tc.server) {
					t.Fatalf("%s supervisor descriptor survived scheduler-unavailable uninstall: %+v", tc.server, d)
				}
			}
			if len(got.Daemons) != 1 || got.Daemons[0].Server != "other" {
				t.Fatalf("sibling daemon not preserved after uninstall; got %+v", got.Daemons)
			}
			for _, tm := range got.MaintenanceTimers {
				if maintenanceTimerOwnedBy(tm, tc.server) {
					t.Fatalf("%s maintenance timer survived scheduler-unavailable uninstall: %+v", tc.server, tm)
				}
			}
			if len(got.MaintenanceTimers) != 1 || got.MaintenanceTimers[0].Server != "other" {
				t.Fatalf("sibling timer not preserved after uninstall; got %+v", got.MaintenanceTimers)
			}
			if calls := atomic.LoadInt32(&reconcileCalls); calls != 1 {
				t.Fatalf("reconcile nudge calls = %d, want 1 after removing supervisor-intent rows", calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// F2 — a fresh GLOBAL install nudges the supervisor reconcile so the new
// descriptor spawns immediately (the IntentWatcher only feeds stops, not new
// descriptors, so without the nudge the daemon never starts until a manual
// reconcile / restart).
// ---------------------------------------------------------------------------

// TestInstallPlanCore_GlobalFreshInstall_NudgesSupervisorReconcile asserts the
// F2 fix: after a global install writes the descriptor rows, it dials exactly
// one reconcile --apply through the seam. Pre-fix nothing nudged, so the daemon
// would not start until the ≤60s IntentWatcher poll (which only feeds stops).
//
// Negative-control: remove the `if superviseGlobal { nudge... }` call in
// installPlanCore and reconcileCalls stays 0 → this test fails.
func TestInstallPlanCore_GlobalFreshInstall_NudgesSupervisorReconcile(t *testing.T) {
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	var reconcileCalls int32
	var gotApply atomic.Bool
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		gotApply.Store(apply)
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(global fresh install): %v", err)
	}

	if got := atomic.LoadInt32(&reconcileCalls); got != 1 {
		t.Fatalf("supervisor reconcile nudge calls = %d, want 1 (a fresh global install must nudge the supervisor so the new descriptor spawns)", got)
	}
	if !gotApply.Load() {
		t.Error("reconcile nudge dialed apply=false, want apply=true (a dry-run reconcile would not spawn the descriptor)")
	}
}

// TestInstallPlanCore_GlobalFreshInstall_EmitsDaemonInstalledEvents asserts the
// best-effort daemon-installed audit row: after a global install commits the
// supervisor-intent descriptor rows, installPlanCore emits one
// daemon-installed event per committed row to supervisor-events.log, keyed by
// task_name and carrying {server,daemon,command,port,workspace} in the body.
// This gives an operator (and a future GUI lifecycle consumer) a durable record
// that a daemon was installed, parallel to the lifecycle-side
// daemon-spawned/daemon-exited/daemon-quarantined rows.
//
// Negative-control: drop the emitDaemonInstalledEvents call in installPlanCore
// and supervisor-events.log carries no daemon-installed row → this test fails.
func TestInstallPlanCore_GlobalFreshInstall_EmitsDaemonInstalledEvents(t *testing.T) {
	stateDir := phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	// The reconcile nudge is irrelevant to this assertion; stub it so the
	// install completes without dialing a real supervisor.
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest() // server "demo", daemons alpha:9211 + beta:9212
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(global fresh install): %v", err)
	}

	logRaw, err := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	logStr := string(logRaw)

	if !strings.Contains(logStr, `"event":"daemon-installed"`) {
		t.Fatalf("no daemon-installed event in supervisor-events.log:\n%s", logStr)
	}
	// Both committed descriptor rows must be announced, keyed by canonical
	// leading-backslash task name (JSON-escaped backslashes).
	for _, taskName := range []string{
		`"task_name":"\\mcp-local-hub-demo-alpha"`,
		`"task_name":"\\mcp-local-hub-demo-beta"`,
	} {
		if !strings.Contains(logStr, taskName) {
			t.Errorf("daemon-installed event missing %s:\n%s", taskName, logStr)
		}
	}
	// Body fields: server, daemon, port, and the workspace key (empty for a
	// global daemon — the future GUI consumer keys teardown by it).
	for _, frag := range []string{
		`"server":"demo"`,
		`"daemon":"alpha"`,
		`"port":9211`,
		`"workspace":""`,
	} {
		if !strings.Contains(logStr, frag) {
			t.Errorf("daemon-installed body missing %s:\n%s", frag, logStr)
		}
	}
}

// TestInstallPlanCore_FilteredGlobalInstall_EmitsOnlySelectedDaemonInstalled
// asserts that daemon-installed audit rows describe only the descriptors changed
// by the current install. A daemon-filtered install intentionally preserves
// sibling rows from supervisor-intent.json; those preserved rows must not be
// re-announced as freshly installed.
func TestInstallPlanCore_FilteredGlobalInstall_EmitsOnlySelectedDaemonInstalled(t *testing.T) {
	stateDir := phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "preserve-alpha", Port: 9991},
			{TaskName: `\mcp-local-hub-demo-beta`, Server: "demo", Daemon: "beta", Command: "preserve-beta", Port: 9992},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "alpha")
	if err != nil {
		t.Fatalf("BuildPlan(filtered alpha): %v", err)
	}
	if got := len(plan.SupervisorIntent); got != 1 {
		t.Fatalf("filtered plan SupervisorIntent rows = %d, want 1", got)
	}

	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "alpha", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(filtered alpha): %v", err)
	}

	logRaw, err := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"task_name":"\\mcp-local-hub-demo-alpha"`) {
		t.Fatalf("daemon-installed event missing selected alpha row:\n%s", logStr)
	}
	if strings.Contains(logStr, `"task_name":"\\mcp-local-hub-demo-beta"`) || strings.Contains(logStr, `"command":"preserve-beta"`) {
		t.Fatalf("daemon-installed event included untouched sibling beta row:\n%s", logStr)
	}
}

// TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_StartFailurePrintsHint
// asserts that when no supervisor is running and the immediate autostart owner
// start fails, the install still completes and keeps the operator hint — the
// descriptor is on disk and the next supervisor start spawns it.
func TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_StartFailurePrintsHint(t *testing.T) {
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) { return false, 0, nil }
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	origStart := installAutostartOwnerStartFn
	installAutostartOwnerStartFn = func() error { return fmt.Errorf("synthetic autostart start failure") }
	t.Cleanup(func() { installAutostartOwnerStartFn = origStart })

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, errSupervisorUnavailableForTest()
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(no supervisor): %v should be non-fatal", err)
	}
	if out := buf.String(); !strings.Contains(out, "no running supervisor") {
		t.Errorf("expected operator hint about no running supervisor; got output:\n%s", out)
	}
}

func TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_AutostartDispatch(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		testInstallColdAutostartDispatch(t, false, nil)
	})
	if runtime.GOOS == "windows" {
		t.Run("refused", func(t *testing.T) {
			prior := []byte("<Task>available observation</Task>")
			refusal := autostart.NewObservedTaskReplacementRefusal(autostart.WindowsTaskName, prior, nil)
			testInstallColdAutostartDispatch(t, false, refusal)
		})
		t.Run("saved_off_projection_failed", func(t *testing.T) {
			testInstallColdAutostartDispatch(t, true, nil)
		})
	}
}

func testInstallColdAutostartDispatch(t *testing.T, autorunOff bool, enableErr error) {
	t.Helper()
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	fb := &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped, enableErr: enableErr}
	installFakeAutostartBackend(t, fb)
	if autorunOff {
		path := SettingsPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("gui.autorun_enabled: \"false\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fb.disableErr = scheduler.ErrPermissionDenied
	}

	probeCalls := 0
	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(stateDir string) (bool, int, error) {
		probeCalls++
		if stateDir == "" {
			t.Fatal("supervisor running probe received empty stateDir")
		}
		return false, 0, nil
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	startCalls := 0
	origStart := installAutostartOwnerStartFn
	installAutostartOwnerStartFn = func() error {
		startCalls++
		return nil
	}
	t.Cleanup(func() { installAutostartOwnerStartFn = origStart })

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, errSupervisorUnavailableForTest()
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(no supervisor): %v should be non-fatal", err)
	}
	if probeCalls != 1 {
		t.Fatalf("supervisor running probe calls = %d, want 1", probeCalls)
	}
	out := buf.String()
	if runtime.GOOS == "windows" {
		if startCalls != 0 || !strings.Contains(out, "skips starting the Windows autostart task by name") ||
			strings.Contains(out, "supervisor autostart owner started") {
			t.Fatalf("Windows install dispatched a task by name: starts=%d output=%q", startCalls, out)
		}
		if autorunOff && (fb.disableCalls != 1 || fb.enableCalls != 0 || !strings.Contains(out, fb.disableErr.Error())) {
			t.Fatalf("OFF projection not exercised: enable=%d disable=%d output=%q", fb.enableCalls, fb.disableCalls, out)
		}
		if autorunOff && (strings.Contains(out, "mcphub autostart enable") || !strings.Contains(out, "mcphub autostart status --details")) {
			t.Fatalf("failed OFF projection must recommend inspecting the saved policy without enabling autorun; output=%q", out)
		}
		if !autorunOff && (fb.enableCalls != 1 || fb.disableCalls != 0) {
			t.Fatalf("ON projection not exercised: enable=%d disable=%d output=%q", fb.enableCalls, fb.disableCalls, out)
		}
		if enableErr != nil && !strings.Contains(out, ErrSchedulerReplacementUnowned.Error()) {
			t.Fatalf("refusal not exercised: enable=%d output=%q", fb.enableCalls, out)
		}
		return
	}
	if startCalls != 1 {
		t.Fatalf("autostart owner start calls = %d, want 1", startCalls)
	}
	if !strings.Contains(out, "supervisor autostart owner started") {
		t.Fatalf("install output missing started-owner confirmation; got:\n%s", out)
	}
	if strings.Contains(out, "will start on the next `mcphub supervise`") {
		t.Fatalf("install output kept the stale pending-daemon note after a successful owner start; got:\n%s", out)
	}
}

// TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_RunningProbeSkipsStart
// is the double-start guard: if the IPC nudge saw an unavailable supervisor but
// the flock-authoritative probe now reports one running, install must not fire
// the autostart owner a second time. A held lock is not convergence, though:
// the operator output must say the supervisor is wedged rather than claiming a
// successful nudge.
func TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_RunningProbeSkipsStart(t *testing.T) {
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) { return true, 1234, nil }
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	origStart := installAutostartOwnerStartFn
	installAutostartOwnerStartFn = func() error {
		t.Fatal("autostart owner start must not run when supervisor probe reports running")
		return nil
	}
	t.Cleanup(func() { installAutostartOwnerStartFn = origStart })

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, errSupervisorUnavailableForTest()
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(no supervisor): %v should be non-fatal", err)
	}
	out := buf.String()
	if !strings.Contains(out, "supervisor lock is held") || !strings.Contains(out, "IPC is unreachable") {
		t.Fatalf("install output missing wedged-supervisor warning; got:\n%s", out)
	}
	if strings.Contains(out, "supervisor already running") {
		t.Fatalf("install output claimed convergence for a lock-held/IPC-unreachable supervisor; got:\n%s", out)
	}
	if strings.Contains(out, "will start on the next `mcphub supervise`") {
		t.Fatalf("install output kept the stale pending-daemon note even though supervisor is running; got:\n%s", out)
	}
}

func TestInstallPlanCore_GlobalInstallRetainsObservedLegacyTasks(t *testing.T) {
	stateDir := phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	seedInstalledServerManifests(t, "demo", "demo-alpha")
	ownNames := []string{"mcp-local-hub-demo-alpha", "mcp-local-hub-demo-beta", "mcp-local-hub-demo-weekly-refresh"}
	listedNames := append(append([]string{}, ownNames...), "mcp-local-hub-demo-alpha-beta", "mcp-local-hub-other-alpha", "mcp-local-hub-supervisor")
	definitions := make(map[string][]byte, len(listedNames))
	f := newInstallFakeScheduler()
	for _, name := range listedNames {
		definitions[name] = []byte(`<Task name="` + name + `"/>`)
		f.listSeed = append(f.listSeed, scheduler.TaskStatus{Name: `\` + name})
		f.tasks[name], f.xml[name] = true, bytes.Clone(definitions[name])
	}
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	origStatus, origLookup := supervisorIPCStatusFn, lookupProcess
	origPIDKill, origStart := stopForceKillPIDFn, installAutostartOwnerStartFn
	t.Cleanup(func() {
		supervisorIPCStatusFn, lookupProcess = origStatus, origLookup
		stopForceKillPIDFn, installAutostartOwnerStartFn = origPIDKill, origStart
	})
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) { return nil, nil }
	lookupProcess = nil
	stopForceKillPIDFn = func(pid int) error {
		t.Errorf("automatic install attempted to kill PID %d", pid)
		return nil
	}
	t.Cleanup(swapKillByPort(func(port int, _ time.Duration) error {
		t.Errorf("automatic install attempted to kill the listener on port %d", port)
		return nil
	}))
	installAutostartOwnerStartFn = func() error {
		t.Error("automatic install attempted to start the autostart owner after reconcile")
		return nil
	}
	var nudges int
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(_ context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Error("install did not request reconcile apply")
		}
		nudges++
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest()
	m.WeeklyRefresh = false // full reinstall flips the per-server weekly timer off.
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var out bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, &out); err != nil {
		t.Fatalf("installPlanCore(global install): %v", err)
	}
	if len(f.deleteNames) != 0 || len(f.importNames) != 0 || f.createCount != 0 || f.runCount != 0 {
		t.Fatalf("install mutated legacy scheduler: deletes=%v imports=%v creates=%d runs=%d", f.deleteNames, f.importNames, f.createCount, f.runCount)
	}
	for _, name := range listedNames {
		if !f.tasks[name] || !bytes.Equal(f.xml[name], definitions[name]) {
			t.Errorf("legacy scheduler task %q changed: exists=%v XML=%q", name, f.tasks[name], f.xml[name])
		}
	}
	if nudges != 1 {
		t.Errorf("reconcile nudge calls = %d, want 1", nudges)
	}
	snapshots, err := filepath.Glob(filepath.Join(stateDir, "scheduler-recovery-*.json"))
	if err != nil || len(snapshots) != len(ownNames) {
		t.Fatalf("recovery snapshots = %v, want %d, err=%v", snapshots, len(ownNames), err)
	}
	const warningPrefix = "Legacy scheduler task "
	if count := strings.Count(out.String(), warningPrefix); count != len(ownNames) {
		t.Fatalf("retention warnings = %d, want %d; output:\n%s", count, len(ownNames), out.String())
	}
	for _, name := range ownNames {
		assertUpgradeRecoverySnapshot(t, name, definitions[name])
		if !strings.Contains(out.String(), warningPrefix+name+" retained; daemon stop skipped") {
			t.Errorf("missing retention warning for %q; output:\n%s", name, out.String())
		}
	}
}

// TestInstallPlanCore_GlobalDaemonlessFullInstallDropsStaleSupervisorRows
// asserts the daemonless reinstall edge: switching a global server to
// transport=remote-http contributes zero supervisor rows, but a full install
// still owns this server's prior rows and must remove them, preserving
// siblings and nudging reconcile so the old daemon terminates.
//
// Negative-control: route zero-row global installs through the old
// client-config-only branch and the demo row survives while reconcileCalls
// stays 0.
func TestInstallPlanCore_GlobalDaemonlessFullInstallDropsStaleSupervisorRows(t *testing.T) {
	stateDir := phaseFStateDir(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "old", Port: 9211},
			{TaskName: `\mcp-local-hub-other-default`, Server: "other", Daemon: "default", Command: "other", Port: 9991},
		},
		MaintenanceTimers: []MaintenanceTimer{
			{Name: `\mcp-local-hub-demo-weekly-refresh`, Kind: "server-weekly-refresh", Server: "demo"},
			{Name: `\mcp-local-hub-other-weekly-refresh`, Kind: "server-weekly-refresh", Server: "other"},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	var reconcileCalls int32
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		if !apply {
			t.Errorf("daemonless reinstall reconcile nudge apply = false, want true")
		}
		atomic.AddInt32(&reconcileCalls, 1)
		return ReconcileResponse{}, nil
	}))

	m := &config.ServerManifest{
		Name:           "demo",
		Kind:           config.KindGlobal,
		Transport:      config.TransportRemoteHTTP,
		URL:            "https://example.invalid/mcp",
		ClientBindings: nil,
		WeeklyRefresh:  false,
	}
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan(remote-http): %v", err)
	}
	if len(plan.SupervisorIntent) != 0 {
		t.Fatalf("remote-http plan SupervisorIntent = %+v, want none", plan.SupervisorIntent)
	}

	if err := NewAPI().installPlanCore(context.Background(), m, bindTestPlanManifestHash(plan), "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(daemonless full install): %v", err)
	}

	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	for _, d := range got.Daemons {
		if supervisorIntentRowOwnedBy(d, "demo") {
			t.Fatalf("daemonless full install left stale demo descriptor row: %+v", d)
		}
	}
	if len(got.Daemons) != 1 || got.Daemons[0].Server != "other" {
		t.Fatalf("sibling daemon row not preserved; got %+v", got.Daemons)
	}
	for _, tm := range got.MaintenanceTimers {
		if maintenanceTimerOwnedBy(tm, "demo") {
			t.Fatalf("daemonless full install left stale demo weekly timer: %+v", tm)
		}
	}
	if len(got.MaintenanceTimers) != 1 || got.MaintenanceTimers[0].Server != "other" {
		t.Fatalf("sibling maintenance timer not preserved; got %+v", got.MaintenanceTimers)
	}
	if calls := atomic.LoadInt32(&reconcileCalls); calls != 1 {
		t.Fatalf("reconcile nudge calls = %d, want 1 after dropping stale daemon rows", calls)
	}
}

// ---------------------------------------------------------------------------
// F3 — `mcphub stop --force` terminates supervisor-owned daemons (Phase F:
// no scheduler task) instead of recording the audit and killing nothing.
// ---------------------------------------------------------------------------

// Force-stop uses one original-owner receipt and preserves running intent.
// Reverting to the old descriptor/PID kill path trips the sealed fallbacks.
func TestStopForce_TerminatesSupervisorOwnedDaemon(t *testing.T) {
	stateDir, task := seedForceStopDescriptor(t, "time", 9128)
	installRecordingAudit(t, &recordingAuditWriter{})
	t.Cleanup(swapStopSchedulerFactory(&restartAllFakeScheduler{tasks: nil}))
	sealForceStopNumericalFallbacks(t)
	var calls int
	installForceStopReceipt(t, func(ctx context.Context, got string, port int) (ForceStopResultV1, error) {
		calls++
		if got != task || port != 9128 {
			t.Fatalf("force_stop selected %q/%d, want %q/9128", got, port, task)
		}
		return forceStopClosedFixture(got, port), nil
	})
	results, err := NewAPI().StopWithOpts(StopOpts{Server: "time", Force: true})
	if err != nil || calls != 1 || len(results) != 1 || results[0].Err != "" {
		t.Fatalf("force stop: calls=%d rows=%+v err=%v", calls, results, err)
	}
	intent, err := ReadSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatal(err)
	}
	if stop, present := intent.Stops[task]; present && stop.Desired == IntentDesiredStopped {
		t.Fatal("force-stop changed the desired state to stopped")
	}
}

// A portless descriptor needs the same retained creator; a PID is not authority.
func TestStopForceKillSupervisorOwned_PortlessUsesOriginalOwner(t *testing.T) {
	_, task := seedForceStopDescriptor(t, "phantomsrv", 0)
	sealForceStopNumericalFallbacks(t)
	var calls int
	installForceStopReceipt(t, func(ctx context.Context, got string, port int) (ForceStopResultV1, error) {
		calls++
		if got != task || port != 0 {
			t.Fatalf("portless request=%q/%d", got, port)
		}
		return forceStopClosedFixture(got, port), nil
	})
	rows, handled, err := stopForceKillSupervisorOwned(context.Background(), "phantomsrv", "")
	if err != nil || !handled || calls != 1 || len(rows) != 1 || rows[0].Err != "" {
		t.Fatalf("portless receipt: calls=%d handled=%t rows=%+v err=%v", calls, handled, rows, err)
	}
}

// No PID or listener does not prove original tree closure.
func TestStopForceKillSupervisorOwned_PortlessMissingOwnerReturnsError(t *testing.T) {
	seedForceStopDescriptor(t, "phantomsrv", 0)
	sealForceStopNumericalFallbacks(t)
	installForceStopReceipt(t, func(context.Context, string, int) (ForceStopResultV1, error) {
		return ForceStopResultV1{}, &IPCErr{Code: ForceStopOwnerUnavailable, Message: "original owner absent"}
	})
	rows, handled, err := stopForceKillSupervisorOwned(context.Background(), "phantomsrv", "")
	if err != nil || !handled || len(rows) != 1 || rows[0].Code != ForceStopOwnerUnavailable || rows[0].Err == "" {
		t.Fatalf("missing owner: handled=%t rows=%+v err=%v", handled, rows, err)
	}
}

// TestForceKillOneSupervisorTarget_LegacyPortZeroEngagesResolvedPortKill is the
// bot PR #505 positive guard: a legacy Port=0 row resolves its manifest port
// (memory → 9123) through the owner, so the by-port kill fallback engages on the
// RESOLVED port instead of skipping the descriptor as portless — catching a
// surviving child that still holds the manifest port after F5's deletion.
func TestForceKillOneSupervisorTarget_LegacyPortZeroEngagesResolvedPortKill(t *testing.T) {
	orig := forceKillByPortFn
	t.Cleanup(func() { forceKillByPortFn = orig })
	var killedPort int
	forceKillByPortFn = func(port int, _ time.Duration) (portKillOutcome, error) {
		killedPort = port
		return portKillKilled, nil
	}
	d := SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0,
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"}}
	// No PID in the map → skip the PID arm → the resolved-port kill fallback runs.
	result := forceKillOneSupervisorTarget(d, map[string]int{})
	if killedPort != 9123 {
		t.Fatalf("forceKillByPortFn port = %d, want 9123 (resolved from the memory manifest, not the raw Port=0)", killedPort)
	}
	if result.Err != "" {
		t.Fatalf("portKillKilled outcome should be a success row; got Err=%q", result.Err)
	}
}

// A successor or unrelated listener never becomes a second termination target.
func TestStopForceKillSupervisorOwned_ClosedTreePreservesOccupiedPortObservation(t *testing.T) {
	seedForceStopDescriptor(t, "time", 33105)
	sealForceStopNumericalFallbacks(t)
	installForceStopReceipt(t, func(ctx context.Context, task string, port int) (ForceStopResultV1, error) {
		receipt := forceStopClosedFixture(task, port)
		receipt.PortState, receipt.PortOwnerPID = "occupied", 53106
		return receipt, nil
	})
	rows, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil || !handled || len(rows) != 1 || rows[0].Err != "" || !strings.Contains(rows[0].Warning, "occupied") || !strings.Contains(rows[0].Warning, "53106") {
		t.Fatalf("separate closure/port facts: handled=%t rows=%+v err=%v", handled, rows, err)
	}
}

func TestWaitPortReleasedAfterPIDKill_ForeignPortReuseSucceedsWithoutKill(t *testing.T) {
	const (
		killedPID  = 43106
		foreignPID = 53106
		port       = 33106
	)

	origIdentity := processIdentityByPID
	origForceKill := forceKillByPortFn
	origLookup := lookupPortPID
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		processIdentityByPID = origIdentity
		forceKillByPortFn = origForceKill
		lookupPortPID = origLookup
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	processIdentityByPID = func(pid int) (string, string, bool) {
		switch pid {
		case killedPID:
			return mcphubProcessImageName, mcphubProcessImageName, true
		case foreignPID:
			return "node.exe", "explorer.exe", true
		default:
			t.Fatalf("processIdentityByPID pid = %d, want %d or %d", pid, killedPID, foreignPID)
			return "", "", false
		}
	}
	lookupPortPID = func(gotPort int) (int, bool, error) {
		if gotPort != port {
			t.Fatalf("lookupPortPID port = %d, want %d", gotPort, port)
		}
		return foreignPID, true, nil
	}
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for reused foreign port %d", port)
		return portKillNoListener, nil
	}
	taskkillProcessTreeByPIDFn = func(pid int) error {
		t.Fatalf("taskkillProcessTreeByPIDFn called for foreign pid %d", pid)
		return nil
	}

	warnContext, err := waitPortReleasedAfterPIDKill(port, killedPID, 0)
	if err != nil {
		t.Fatalf("waitPortReleasedAfterPIDKill error = %v, want success for foreign port reuse", err)
	}
	if warnContext == "" || !strings.Contains(warnContext, "foreign process") || !strings.Contains(warnContext, "node.exe") {
		t.Fatalf("warning context = %q, want foreign process node.exe context", warnContext)
	}
}

// Native closure and failed port observation remain separate facts.
func TestStopForceKillSupervisorOwned_ClosedTreeRetainsPortProbeFailure(t *testing.T) {
	seedForceStopDescriptor(t, "time", 9128)
	sealForceStopNumericalFallbacks(t)
	installForceStopReceipt(t, func(ctx context.Context, task string, port int) (ForceStopResultV1, error) {
		receipt := forceStopClosedFixture(task, port)
		receipt.PortState, receipt.PortError = "unknown", "port observation failed"
		return receipt, nil
	})
	rows, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil || !handled || len(rows) != 1 || rows[0].Err != "" || !strings.Contains(rows[0].Warning, "port observation failed") {
		t.Fatalf("port error was lost: handled=%t rows=%+v err=%v", handled, rows, err)
	}
}

// TestStopForceKillSupervisorOwned_NoTargets_NoOp asserts the no-op path: a
// server with no supervisor-owned descriptor returns (nil,false,nil) so the
// caller reports any manifest-only task as lacking original custody.
func TestStopForceKillSupervisorOwned_NoTargets_NoOp(t *testing.T) {
	phaseFStateDir(t) // no intent file seeded
	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned(no intent): %v", err)
	}
	if handled || results != nil {
		t.Fatalf("want (nil,false,nil) for a host with no supervisor-owned target; got results=%+v handled=%v", results, handled)
	}
}

// --- small local test shims (kept here so the file is self-contained) -------

func swapStopSchedulerFactory(f scheduler.Scheduler) func() {
	orig := stopSchedulerFactory
	stopSchedulerFactory = func() (scheduler.Scheduler, error) { return f, nil }
	return func() { stopSchedulerFactory = orig }
}

func swapKillByPort(fn func(port int, timeout time.Duration) error) func() {
	orig := killByPortFn
	origForce := forceKillByPortFn
	killByPortFn = fn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		if err := fn(port, timeout); err != nil {
			return portKillNoListener, err
		}
		if port == 0 {
			return portKillNoPort, nil
		}
		return portKillKilled, nil
	}
	return func() {
		killByPortFn = orig
		forceKillByPortFn = origForce
	}
}

// errSupervisorUnavailableForTest wraps ErrSupervisorIPCUnavailable so the
// install hint branch (errors.Is(err, ErrSupervisorIPCUnavailable)) matches.
func errSupervisorUnavailableForTest() error {
	return fmt.Errorf("seam: supervisor unavailable: %w", ErrSupervisorIPCUnavailable)
}
