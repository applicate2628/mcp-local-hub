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
	gotOtherWatermark, ok := got.LegacyStopWatermarks[`\mcp-local-hub-other-d`]
	if !ok {
		t.Fatal("sibling legacy-stop watermark was wiped by the uninstall cleanup")
	}
	if gotOtherWatermark.Desired != otherStop.Desired || gotOtherWatermark.Reason != otherStop.Reason || !gotOtherWatermark.UpdatedAt.Equal(otherStop.UpdatedAt) {
		t.Errorf("sibling legacy-stop watermark mutated: got %+v want %+v", gotOtherWatermark, otherStop)
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

	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
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

	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
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

	if err := NewAPI().installPlanCore(context.Background(), m, plan, "alpha", false, io.Discard); err != nil {
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
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(no supervisor): %v should be non-fatal", err)
	}
	if out := buf.String(); !strings.Contains(out, "no running supervisor") {
		t.Errorf("expected operator hint about no running supervisor; got output:\n%s", out)
	}
}

// TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_StartsAutostartOwnerNow
// locks the first-install contract: when the descriptor write succeeds but the
// post-install reconcile sees no running supervisor, install should start the
// already-enabled autostart owner immediately instead of leaving the daemon
// pending until a manual supervise or next logon.
//
// Negative-control: pre-fix nudgeSupervisorReconcileAfterGlobalInstall only
// prints the old next-supervise/logon note, so the positive started-owner line
// is absent and the stale pending-daemon note remains.
func TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_StartsAutostartOwnerNow(t *testing.T) {
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

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
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(no supervisor): %v should be non-fatal", err)
	}
	if probeCalls != 1 {
		t.Fatalf("supervisor running probe calls = %d, want 1", probeCalls)
	}
	if startCalls != 1 {
		t.Fatalf("autostart owner start calls = %d, want 1", startCalls)
	}
	out := buf.String()
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
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
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

// TestInstallPlanCore_GlobalInstallDeletesThisServersLegacyTasks
// asserts the Phase-F handoff cleanup: supervisor-path installs must remove
// stale pre-v0.6 per-daemon scheduler tasks for this server so those legacy
// tasks cannot spawn daemons outside the supervisor after logon.
//
// Negative-control: remove the legacy scheduler cleanup from the supervise
// global branch and deleteNames stays empty, so this test fails.
func TestInstallPlanCore_GlobalInstallDeletesThisServersLegacyTasks(t *testing.T) {
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},
		{Name: `\mcp-local-hub-demo-beta`},
		{Name: `\mcp-local-hub-demo-weekly-refresh`},
		{Name: `\mcp-local-hub-other-alpha`},
		{Name: `\mcp-local-hub-supervisor`},
	}
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest()
	m.WeeklyRefresh = false // full reinstall flips the per-server weekly timer off.
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(global install): %v", err)
	}

	deleted := map[string]bool{}
	for _, name := range f.deleteNames {
		deleted[strings.TrimPrefix(name, `\`)] = true
	}
	for _, want := range []string{"mcp-local-hub-demo-alpha", "mcp-local-hub-demo-beta", "mcp-local-hub-demo-weekly-refresh"} {
		if !deleted[want] {
			t.Fatalf("legacy scheduler task %q was not deleted; deleteNames=%v", want, f.deleteNames)
		}
	}
	for _, forbidden := range []string{"mcp-local-hub-other-alpha", "mcp-local-hub-supervisor"} {
		if deleted[forbidden] {
			t.Fatalf("deleted non-owned/control-plane task %q; deleteNames=%v", forbidden, f.deleteNames)
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

	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
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

// TestStopForce_TerminatesSupervisorOwnedDaemon asserts the F3 fix: a forced
// stop of a v0.6 GLOBAL server (descriptor in supervisor-intent.json, NO
// scheduler task) kills the daemon by its descriptor port. Pre-fix the force
// branch only called stopKillCore (scheduler-task targets) and killed NOTHING.
//
// Negative-control: revert StopWithOpts' Force branch to
// `return a.stopKillCore(opts.Server, opts.DaemonFilter, nil)` and the kill
// counter stays 0 → this test fails.
func TestStopForce_TerminatesSupervisorOwnedDaemon(t *testing.T) {
	stateDir := phaseFStateDir(t)
	installRecordingAudit(t, &recordingAuditWriter{})
	// Seed a supervisor-intent with a single GLOBAL descriptor for "time"
	// (the same shape the stop_supervisor tests use) and NO scheduler task.
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-time-default`, Server: "time", Daemon: "default", Port: 9128},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	// No scheduler tasks exist (Phase F global). stopKillCore must find nothing.
	fake := &restartAllFakeScheduler{tasks: nil}
	t.Cleanup(swapStopSchedulerFactory(fake))

	// Count force kills by port via the killByPortFn seam.
	var killedPorts []int
	t.Cleanup(swapKillByPort(func(port int, _ time.Duration) error {
		killedPorts = append(killedPorts, port)
		return nil
	}))
	// The reconcile seam must NOT be dialed by the force path (force records no
	// stop intent, so a reconcile would no-op). Fail loud if it is.
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		t.Error("stop --force dialed the supervisor reconcile; force must kill directly, not reconcile")
		return ReconcileResponse{}, nil
	}))

	results, err := NewAPI().StopWithOpts(StopOpts{Server: "time", Force: true})
	if err != nil {
		t.Fatalf("StopWithOpts(force): %v", err)
	}

	// The supervisor-owned daemon was killed by its descriptor port.
	if len(killedPorts) != 1 || killedPorts[0] != 9128 {
		t.Fatalf("force stop killed ports %v, want exactly [9128] (the supervisor-owned descriptor port)", killedPorts)
	}
	// A result row exists for the supervisor-owned task.
	var sawTask bool
	for _, r := range results {
		if r.TaskName == `\mcp-local-hub-time-default` {
			sawTask = true
			if r.Err != "" {
				t.Errorf("force stop of supervisor-owned daemon returned error row: %q", r.Err)
			}
		}
	}
	if !sawTask {
		t.Fatalf("no result row for the supervisor-owned task; results=%+v", results)
	}
}

// TestStopForceKillSupervisorOwned_PortlessFallsBackToPID asserts the
// descriptor-port-less fallback: when a supervisor-owned descriptor has no
// port, the force path resolves a live PID from the IPC status and kills it.
func TestStopForceKillSupervisorOwned_PortlessFallsBackToPID(t *testing.T) {
	pinOwnerSIDMatch(t) // SEC-F3: same-user owner; this test asserts the portless PID fallback.
	stateDir := phaseFStateDir(t)
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Port 0 → no port-kill target; the IPC status supplies the PID.
			{TaskName: `\mcp-local-hub-time-default`, Server: "time", Daemon: "default", Port: 0},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// killByPortFn must NOT fire (no port); stopForceKillPIDFn must.
	t.Cleanup(swapKillByPort(func(port int, _ time.Duration) error {
		t.Errorf("killByPortFn fired for a portless descriptor (port=%d); want PID fallback", port)
		return nil
	}))
	var killedPIDs []int
	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error { killedPIDs = append(killedPIDs, pid); return nil }
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	origIdentity := processIdentityByPID
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid == 4242 {
			return "mcphub.exe", "svchost.exe", true
		}
		return "", "", false
	}
	t.Cleanup(func() { processIdentityByPID = origIdentity })

	// IPC status reports the live PID for the task.
	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(ctx context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: `\mcp-local-hub-time-default`, PID: 4242, State: "Running"}}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true (a supervisor-owned target was in scope)")
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != 4242 {
		t.Fatalf("PID-fallback kills = %v, want exactly [4242]", killedPIDs)
	}
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestStopForceKillSupervisorOwned_PortlessNoPIDReturnsError(t *testing.T) {
	stateDir := phaseFStateDir(t)
	// A GENUINELY portless supervisor-owned daemon: a server with NO manifest, so
	// the owner (EffectiveDaemonPort) resolves nothing and d.Port stays 0. NOTE a
	// `time` Port=0 row is no longer portless — the owner resolves its manifest
	// port 9128 (bot PR #505) — so it can no longer exercise the no-kill-surface
	// path; use an unknown server to keep testing the genuinely-portless case.
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-phantomsrv-default`, Server: "phantomsrv", Daemon: "default", Port: 0,
				Args: []string{"daemon", "--server", "phantomsrv", "--daemon", "default"}},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed: %v", err)
	}

	origStatus := supervisorIPCStatusFn
	origPID := stopForceKillPIDFn
	origForceKill := forceKillByPortFn
	t.Cleanup(func() {
		supervisorIPCStatusFn = origStatus
		stopForceKillPIDFn = origPID
		forceKillByPortFn = origForceKill
	})
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: `\mcp-local-hub-phantomsrv-default`, PID: 0, State: "Running"}}, nil
	}
	stopForceKillPIDFn = func(pid int) error {
		t.Fatalf("stopForceKillPIDFn called with pid %d; no live PID was available", pid)
		return nil
	}
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for portless descriptor port=%d", port)
		return portKillNoListener, nil
	}

	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "phantomsrv", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true (a supervisor-owned target was in scope)")
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1; results=%+v", len(results), results)
	}
	if results[0].Err == "" {
		t.Fatalf("portless descriptor with no live PID returned success; want no-kill-surface error row: %+v", results[0])
	}
	if !strings.Contains(results[0].Err, "no kill surface") || !strings.Contains(results[0].Err, "portless descriptor") {
		t.Fatalf("result error = %q, want no kill surface / portless descriptor wording", results[0].Err)
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

func TestStopForceKillSupervisorOwned_PIDSuccessWithPortWaitsForRelease(t *testing.T) {
	pinOwnerSIDMatch(t) // SEC-F3: same-user owner; this test asserts PID-kill-then-port-release.
	stateDir := phaseFStateDir(t)
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-time-default`, Server: "time", Daemon: "default", Port: 33105},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		supervisorPID = 43105
	)
	origIdentity := processIdentityByPID
	origStatus := supervisorIPCStatusFn
	origPID := stopForceKillPIDFn
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		processIdentityByPID = origIdentity
		supervisorIPCStatusFn = origStatus
		stopForceKillPIDFn = origPID
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	processIdentityByPID = func(pid int) (string, string, bool) {
		switch pid {
		case supervisorPID:
			return mcphubProcessImageName, mcphubProcessImageName, true
		default:
			t.Fatalf("processIdentityByPID pid = %d, want %d", pid, supervisorPID)
			return "", "", false
		}
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: `\mcp-local-hub-time-default`, PID: supervisorPID, State: "Running"}}, nil
	}
	var killedPIDs []int
	stopForceKillPIDFn = func(pid int) error {
		killedPIDs = append(killedPIDs, pid)
		return nil
	}
	var portLookups int
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		portLookups++
		if port != 33105 {
			t.Fatalf("lookupProcess port = %d, want 33105", port)
		}
		if portLookups <= 2 {
			return supervisorPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	var taskkillPIDs []int
	taskkillProcessTreeByPIDFn = func(pid int) error {
		taskkillPIDs = append(taskkillPIDs, pid)
		return nil
	}
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for port %d after successful PID kill", port)
		return portKillNoListener, nil
	}

	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true")
	}
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("unexpected results: %+v", results)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != supervisorPID {
		t.Fatalf("PID kills = %v, want [%d]", killedPIDs, supervisorPID)
	}
	if portLookups < 3 {
		t.Fatalf("port release wait consulted lookupProcess %d times, want initial lookup plus release polling", portLookups)
	}
	if len(taskkillPIDs) != 0 {
		t.Fatalf("taskkill PIDs = %v, want none for wait-only release", taskkillPIDs)
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
	origLookup := lookupProcess
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		processIdentityByPID = origIdentity
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
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
	lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
		if gotPort != port {
			t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
		}
		return foreignPID, 0, 0, true
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

// TestStopForceKillSupervisorOwned_UnsupportedPortKillReportsSuccessWithUnverifiedPortWarning
// covers the POSIX supervisor-owned force path: descriptor ports exist, but the
// Windows-only port lookup hook is structurally absent (lookupProcess == nil on
// non-Windows). A SUCCESSFUL trusted PID/tree kill IS the proof the daemon is
// gone, so the force-stop goal is achieved: the per-target row must be SUCCESS
// (empty Err) carrying a warning that notes the port-release proof was
// unavailable on this platform but the trusted kill succeeded (bot PR #288
// r35-1, site 1). The pre-fix code demanded a port-release proof it could not
// supply and turned the success into a FAILED stop — this test inverts that
// entrenching assertion.
//
// Negative-control (now matched by the assertions): keep returning success after
// the PID kill and this test observes an empty per-target Err despite no
// port-release proof.
func TestStopForceKillSupervisorOwned_UnsupportedPortKillReportsSuccessWithUnverifiedPortWarning(t *testing.T) {
	pinOwnerSIDMatch(t) // SEC-F3: same-user owner; this test asserts the unsupported-port-kill warning path.
	stateDir := phaseFStateDir(t)
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-time-default`, Server: "time", Daemon: "default", Port: 9315},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed: %v", err)
	}

	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	killByPortFn = killDaemonByPort
	forceKillByPortFn = killDaemonByPortOutcome
	lookupProcess = nil
	t.Cleanup(func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
	})

	var killedPIDs []int
	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error { killedPIDs = append(killedPIDs, pid); return nil }
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	origIdentity := processIdentityByPID
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid == 4243 {
			return "mcphub.exe", "svchost.exe", true
		}
		return "", "", false
	}
	t.Cleanup(func() { processIdentityByPID = origIdentity })

	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(ctx context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: `\mcp-local-hub-time-default`, PID: 4243, State: "Running"}}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	results, handled, err := stopForceKillSupervisorOwned(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("stopForceKillSupervisorOwned: %v", err)
	}
	if !handled {
		t.Fatal("handled=false, want true (a supervisor-owned target was in scope)")
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != 4243 {
		t.Fatalf("PID-fallback kills = %v, want exactly [4243]", killedPIDs)
	}
	if len(results) != 1 {
		t.Fatalf("unexpected results: %+v", results)
	}
	if results[0].Err != "" {
		t.Fatalf("results[0].Err = %q, want empty — a successful trusted PID kill IS the proof on a host with no port-owner lookup", results[0].Err)
	}
	if results[0].Warning == "" {
		t.Fatalf("results[0].Warning = %q, want a non-empty warning noting the trusted kill succeeded without a port-release proof", results[0].Warning)
	}
	if !strings.Contains(results[0].Warning, "trusted PID/tree kill succeeded") {
		t.Fatalf("results[0].Warning = %q, want it to note the trusted PID/tree kill succeeded", results[0].Warning)
	}
}

// TestStopForceKillSupervisorOwned_NoTargets_NoOp asserts the no-op path: a
// server with no supervisor-owned descriptor returns (nil,false,nil) so the
// caller falls through to the legacy stopKillCore.
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
