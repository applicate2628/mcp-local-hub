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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
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

	// StrictMode is carried through untouched.
	if !got.StrictMode {
		t.Error("StrictMode flipped by the uninstall cleanup; want preserved")
	}
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

// TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_PrintsHint asserts that
// when no supervisor is running (ErrSupervisorIPCUnavailable), the install
// completes and prints the operator hint to start one — the descriptor is on
// disk and the next supervisor start spawns it.
func TestInstallPlanCore_GlobalFreshInstall_NoSupervisor_PrintsHint(t *testing.T) {
	phaseFStateDir(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

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

// TestStopForceKillSupervisorOwned_UnsupportedPortKillFallsBackToPID covers the
// POSIX supervisor-owned force path: descriptor ports exist, but the production
// port lookup hook is absent, so the port kill cannot target a process and must
// fall through to the IPC PID.
//
// Negative-control: keep killDaemonByPort returning nil when lookupProcess is
// nil and this test records no PID kill.
func TestStopForceKillSupervisorOwned_UnsupportedPortKillFallsBackToPID(t *testing.T) {
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
	if len(results) != 1 || results[0].Err != "" {
		t.Fatalf("unexpected results: %+v", results)
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
