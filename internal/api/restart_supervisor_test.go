package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/scheduler"
)

func TestRestartUsesSupervisorRespawnForIntentDaemon(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-memory-default`,
			Server:   "memory",
			Daemon:   "default",
			Port:     9123,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var gotTask string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		gotTask = taskName
		if force {
			t.Fatal("Restart must not force quarantine override by default")
		}
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	// Restart now falls through to the legacy scheduler after the supervisor
	// pass (bot PR #268 r3), so mock an empty scheduler: this server has no
	// legacy task, so results stay the single supervisor row.
	fake := &restartAllFakeScheduler{}
	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	defer func() { restartSchedulerFactory = origFactory }()

	results, err := NewAPI().Restart("memory", "")
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if gotTask != `\mcp-local-hub-memory-default` {
		t.Fatalf("respawn task = %q, want memory default", gotTask)
	}
	if len(results) != 1 || results[0].TaskName != `\mcp-local-hub-memory-default` || results[0].Err != "" {
		t.Fatalf("results = %+v, want one supervisor success row", results)
	}
}

func TestRestartAllSkipsWatchdogMaintenanceRow(t *testing.T) {
	// deep-sec #268: the restart maintenance filter must skip *-watchdog rows
	// (not just *-weekly-refresh) so a legacy/hand-edited intent watchdog row
	// is never sent through supervisor respawn as if it were a daemon.
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-watchdog`,
			Server:   "watchdog",
			Daemon:   "default",
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var respawned []string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		respawned = append(respawned, taskName)
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	fake := &restartAllFakeScheduler{}
	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	defer func() { restartSchedulerFactory = origFactory }()

	if _, err := NewAPI().RestartAll(); err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	if len(respawned) != 0 {
		t.Fatalf("watchdog maintenance row respawned = %v, want none (must be skipped)", respawned)
	}
}

func TestRestartSupervisorOwnedDaemonsUsesDescriptorIdentityForWorkspaceLSP(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	const taskName = `\mcp-local-hub-lsp-deadbeef-go`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "mcp-language-server",
			Daemon:   "lsp-deadbeef-go",
			Port:     9123,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var gotTask string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		gotTask = taskName
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	results, handled, err := restartSupervisorOwnedDaemons(context.Background(), "mcp-language-server", "lsp-deadbeef-go")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false; workspace LSP descriptor should match by Server/Daemon fields")
	}
	if gotTask != taskName {
		t.Fatalf("respawn task = %q, want %q", gotTask, taskName)
	}
	if len(results) != 1 || results[0].TaskName != taskName || results[0].Err != "" {
		t.Fatalf("results = %+v, want one supervisor success row", results)
	}
}

func TestRestartAllFallsThroughToLegacySchedulerAndSkipsSupervisorHandledTasks(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	const supervisorTask = `\mcp-local-hub-lsp-deadbeef-go`
	const legacyTask = `\mcp-local-hub-memory-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: supervisorTask,
			Server:   "mcp-language-server",
			Daemon:   "lsp-deadbeef-go",
			Port:     9123,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var respawned []string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		respawned = append(respawned, taskName)
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	fake := &restartAllFakeScheduler{
		tasks: []scheduler.TaskStatus{
			{Name: supervisorTask},
			{Name: legacyTask},
		},
	}
	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	defer func() { restartSchedulerFactory = origFactory }()

	results, err := NewAPI().RestartAll()
	if err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	if len(respawned) != 1 || respawned[0] != supervisorTask {
		t.Fatalf("respawned = %v, want [%s]", respawned, supervisorTask)
	}
	if len(fake.runNames) != 1 || fake.runNames[0] != legacyTask {
		t.Fatalf("scheduler Run calls = %v, want only [%s]", fake.runNames, legacyTask)
	}
	if len(results) != 2 {
		t.Fatalf("results len = %d, want supervisor + legacy rows: %+v", len(results), results)
	}
	if results[0].TaskName != supervisorTask || results[1].TaskName != legacyTask {
		t.Fatalf("results = %+v, want supervisor then legacy", results)
	}
}

func TestRestartFallsBackToSchedulerWhenSupervisorRespawnFails(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	const taskName = `\mcp-local-hub-foo-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "foo",
			Daemon:   "default",
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		return RespawnResult{Success: false, Code: "SUPERVISOR_UNAVAILABLE", Message: "supervisor unavailable"}, nil
	})
	defer restoreRespawn()

	fake := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{{Name: taskName}}}
	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	defer func() { restartSchedulerFactory = origFactory }()

	results, err := NewAPI().Restart("foo", "")
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if len(fake.runNames) != 1 || fake.runNames[0] != taskName {
		t.Fatalf("scheduler fallback Run calls = %v, want [%s]", fake.runNames, taskName)
	}
	if len(results) != 2 || results[0].Err == "" || results[1].Err != "" {
		t.Fatalf("results = %+v, want supervisor failure row followed by scheduler success", results)
	}
}

func TestRestartAllDoesNotFallBackToSchedulerWhenSupervisorRespawnIsRefused(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	const taskName = `\mcp-local-hub-foo-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "foo",
			Daemon:   "default",
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		return RespawnResult{Success: false, Code: "QUARANTINED", Message: "daemon is quarantined"}, nil
	})
	defer restoreRespawn()

	fake := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{{Name: taskName}}}
	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	defer func() { restartSchedulerFactory = origFactory }()

	results, err := NewAPI().RestartAll()
	if err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	if len(fake.runNames) != 0 {
		t.Fatalf("scheduler fallback Run calls = %v, want none for supervisor refusal", fake.runNames)
	}
	if len(results) != 1 || results[0].TaskName != taskName || results[0].Err == "" {
		t.Fatalf("results = %+v, want only supervisor refusal row", results)
	}
}

func TestRestartAllFallsBackToSchedulerWhenSupervisorUnavailable(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	const taskName = `\mcp-local-hub-foo-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "foo",
			Daemon:   "default",
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		return RespawnResult{Success: false, Code: "SUPERVISOR_UNAVAILABLE", Message: "supervisor unavailable"}, nil
	})
	defer restoreRespawn()

	fake := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{{Name: taskName}}}
	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	defer func() { restartSchedulerFactory = origFactory }()

	results, err := NewAPI().RestartAll()
	if err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	if len(fake.runNames) != 1 || fake.runNames[0] != taskName {
		t.Fatalf("scheduler fallback Run calls = %v, want [%s]", fake.runNames, taskName)
	}
	if len(results) != 2 || results[0].Err == "" || results[1].Err != "" {
		t.Fatalf("results = %+v, want supervisor unavailable row followed by scheduler success", results)
	}
}

// TestRestartQuarantinedDoesNotWriteRunningIntent is the #279 fable N1 fix:
// a QUARANTINED respawn refusal is a DELIBERATE force-gate. The restart
// caller must NOT write Desired=running for a quarantined daemon — doing so
// (the daba5d0 pre-dial write) let the supervisor's IntentWatcher converge a
// spawn ≤60s later without force, answering the refusal yet delivering the
// spawn anyway (split-brain). The error row surfaces and daemon-intent.json
// stays at its prior stopped value (no running intent written).
func TestRestartQuarantinedDoesNotWriteRunningIntent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)

	const taskName = `\mcp-local-hub-time-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "time",
			Daemon:   "default",
			Port:     9128,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	// Seed daemon-intent.json at Desired=stopped — a prior stop. The fix
	// must NOT overwrite this for a quarantined daemon.
	if err := NewAPI().WriteDaemonIntent(taskName, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, auditWhoMcphubStop); err != nil {
		t.Fatalf("seed stopped daemon-intent: %v", err)
	}

	var dials int
	var intentDesiredAtDial string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		dials++
		res := NewAPI().ReadDaemonIntent()
		intentDesiredAtDial = res.File.Tasks[taskName].Desired
		return RespawnResult{Success: false, Code: "QUARANTINED", Message: "daemon is quarantined; pass force=true to override"}, nil
	})
	defer restoreRespawn()

	results, handled, err := restartSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false; supervisor-owned time daemon should be handled")
	}
	if dials != 1 {
		t.Fatalf("respawn dials = %d; want exactly 1 (no retry for QUARANTINED)", dials)
	}
	if intentDesiredAtDial != IntentDesiredStopped {
		t.Fatalf("daemon-intent at dial = %q, want %q (no running intent must be written BEFORE the quarantine dial)",
			intentDesiredAtDial, IntentDesiredStopped)
	}
	// Read-back after the call: the running intent must NEVER have landed
	// — the force-gate holds and the IntentWatcher must not converge a
	// quarantine bypass.
	after := NewAPI().ReadDaemonIntent().File.Tasks[taskName].Desired
	if after != IntentDesiredStopped {
		t.Fatalf("daemon-intent after refused respawn = %q, want %q (quarantined daemon must not receive Desired=running)",
			after, IntentDesiredStopped)
	}
	if len(results) != 1 || results[0].TaskName != taskName || results[0].Err == "" {
		t.Fatalf("results = %+v, want one supervisor refusal row", results)
	}
	if results[0].Code != "QUARANTINED" {
		t.Fatalf("refusal row code = %q, want QUARANTINED", results[0].Code)
	}
}

// TestRestartStoppedIntentWritesRunningThenReconciles is the #279 fable r3
// F-A fix: a RESPAWN_REFUSED_INTENT_STOPPED refusal (idle daemon, the SM
// only refused because daemon-intent.json still says Desired=stopped) is
// recoverable, but the OLD recovery (write running → respawn REDIAL) was
// dead against the real supervisor — the respawn verb never refreshes the
// controller's daemonIntent cache, so the redial read the stale "stopped"
// snapshot and was refused again. The fix writes Desired=running then
// dispatches ONE `reconcile --apply` (which re-reads BOTH intent files
// fresh from disk and refreshes the caches BEFORE posting EvIntentUpdate).
// This is the true mirror of TestStopUsesSupervisorReconcileAndSkipsKill.
//
// The respawn dial observes the still-stopped intent (the write happens
// AFTER the first refusal, never for a quarantined daemon); the reconcile
// stub asserts apply=true AND that Desired=running is already on disk
// before it returns. The supervisor half of this path (reconcile re-reading
// disk and applyReconcileDrift refreshing the cache BEFORE the
// spawn-direction post) is covered by internal/cli's
// TestReconcileIPC_SupervisorOwnedMissingTaskAppliesStart + the
// applyReconcileDrift cache-refresh ordering — not duplicated here.
//
// #279 opus gate: the reconcile stub returns REALISTIC per-target drift and
// the test exercises BOTH ownership shapes. Under the no-legacy ownership
// model (spec §0.2, aa1d089) EVERY supervisor-intent row is supervisor-owned,
// so the spawn-direction classifier posts EvIntentUpdate for a regular
// `time-default` global daemon EXACTLY as it does for a proxy-shaped
// `time-proxy` descriptor — both classify post_ev_intent_update and both are
// truly dispatched → plain success rows. One respawn dial + one reconcile
// call PER target. (The DeferredToIntentWatcherCode edge — a spawn target
// that posts NOTHING — is the no_op / missing-drift-entry case, pinned by
// TestStopAllRecordsIntentThenReconciles via a no_op drift entry, not by
// this test.)
func TestRestartStoppedIntentWritesRunningThenReconciles(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)

	const regularTask = `\mcp-local-hub-time-default`
	const proxyTask = `\mcp-local-hub-time-proxy`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: regularTask, Server: "time", Daemon: "default", Port: 9128},
			{TaskName: proxyTask, Server: "time", Daemon: "proxy", Port: 9129,
				Args: []string{"daemon", "serena-proxy"}},
		},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	for _, tn := range []string{regularTask, proxyTask} {
		if err := NewAPI().WriteDaemonIntent(tn, DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: time.Now().UTC(),
		}, auditWhoMcphubStop); err != nil {
			t.Fatalf("seed stopped daemon-intent for %s: %v", tn, err)
		}
	}

	var dials int
	dialDesired := map[string]string{}
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		dials++
		dialDesired[taskName] = NewAPI().ReadDaemonIntent().File.Tasks[taskName].Desired
		return RespawnResult{Success: false, Code: RespawnRefusedIntentStoppedCode, Message: "respawn refused: daemon-intent says Desired=stopped"}, nil
	})
	defer restoreRespawn()

	var reconcileCalls int32
	var gotApply bool
	restoreReconcile := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		gotApply = apply
		// The running intent for the target being dispatched must already
		// be on disk when the reconcile fires — the reconcile reads
		// daemon-intent.json fresh and would otherwise classify the task as
		// still-stopped and post nothing.
		if d := NewAPI().ReadDaemonIntent().File.Tasks[regularTask].Desired; d != IntentDesiredRunning {
			// The loop dispatches the regular task first; by the time ANY
			// reconcile fires, that task's running intent is on disk.
			t.Errorf("intent at reconcile time = %q, want %q (running must be on disk BEFORE the reconcile)", d, IntentDesiredRunning)
		}
		// No-legacy drift: every supervisor-intent row is dispatched through
		// the SM (post_ev_intent_update) — regular daemon included.
		return ReconcileResponse{
			AppliedCount: 2,
			Drift: []DriftEntry{
				{TaskName: regularTask, Action: ReconcileActionPostEvIntentUpdate},
				{TaskName: proxyTask, Action: ReconcileActionPostEvIntentUpdate},
			},
		}, nil
	})
	defer restoreReconcile()

	results, handled, err := restartSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false; supervisor-owned time daemons should be restarted")
	}
	if dials != 2 {
		t.Fatalf("respawn dials = %d; want exactly 2 (one per target: refuse → write running → reconcile, NO redial)", dials)
	}
	for _, tn := range []string{regularTask, proxyTask} {
		if dialDesired[tn] != IntentDesiredStopped {
			t.Fatalf("intent at the respawn dial for %s = %q, want %q (no intent write before the dial — only after a stopped-intent refusal)",
				tn, dialDesired[tn], IntentDesiredStopped)
		}
	}
	if got := atomic.LoadInt32(&reconcileCalls); got != 2 {
		t.Fatalf("reconcile calls = %d; want exactly 2 (one dispatch per target, replacing the redial)", got)
	}
	if !gotApply {
		t.Fatal("reconcile dialed with apply=false, want apply=true")
	}
	// Post-call the running intent stays on disk so the IntentWatcher
	// converges even if the supervisor missed the EvIntentUpdate.
	for _, tn := range []string{regularTask, proxyTask} {
		if after := NewAPI().ReadDaemonIntent().File.Tasks[tn].Desired; after != IntentDesiredRunning {
			t.Fatalf("daemon-intent after dispatch for %s = %q, want %q", tn, after, IntentDesiredRunning)
		}
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want two supervisor rows (regular + proxy)", results)
	}
	byTask := map[string]RestartResult{}
	for _, r := range results {
		byTask[r.TaskName] = r
	}
	// Regular daemon → truly dispatched under no-legacy ownership → plain
	// success row (empty Err + empty Code).
	regular, ok := byTask[regularTask]
	if !ok {
		t.Fatalf("missing regular-daemon row in %+v", results)
	}
	if regular.Err != "" || regular.Code != "" {
		t.Fatalf("regular row = %+v, want plain success (empty Err + empty Code; truly dispatched under no-legacy ownership)", regular)
	}
	// Proxy descriptor → truly dispatched → plain success row.
	proxy, ok := byTask[proxyTask]
	if !ok {
		t.Fatalf("missing proxy-daemon row in %+v", results)
	}
	if proxy.Err != "" || proxy.Code != "" {
		t.Fatalf("proxy row = %+v, want plain success (empty Err + empty Code; truly dispatched)", proxy)
	}
}

// TestRestartStoppedIntentReconcileFailureSurfacesErrorRow pins the
// reconcile-failure sub-case (#279 fable r3 F-A): the intent write lands
// (Desired=running on disk), but the `reconcile --apply` dispatch fails.
// The result is a plain error row — no respawn redial, no taskkill, no
// fallback — and the running intent stays on disk so the IntentWatcher
// still converges the spawn within ~60s. Exactly 1 respawn dial + 1
// reconcile call.
func TestRestartStoppedIntentReconcileFailureSurfacesErrorRow(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)

	const taskName = `\mcp-local-hub-time-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "time",
			Daemon:   "default",
			Port:     9128,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	if err := NewAPI().WriteDaemonIntent(taskName, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, auditWhoMcphubStop); err != nil {
		t.Fatalf("seed stopped daemon-intent: %v", err)
	}

	var dials int
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		dials++
		return RespawnResult{Success: false, Code: RespawnRefusedIntentStoppedCode, Message: "respawn refused: daemon-intent says Desired=stopped"}, nil
	})
	defer restoreRespawn()

	var reconcileCalls int32
	restoreReconcile := setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		atomic.AddInt32(&reconcileCalls, 1)
		return ReconcileResponse{}, errors.New("reconcile handler exploded")
	})
	defer restoreReconcile()

	results, handled, err := restartSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false; supervisor-owned time daemon should be handled")
	}
	if dials != 1 {
		t.Fatalf("respawn dials = %d; want exactly 1 (no redial after a reconcile failure)", dials)
	}
	if got := atomic.LoadInt32(&reconcileCalls); got != 1 {
		t.Fatalf("reconcile calls = %d; want exactly 1", got)
	}
	// The running intent must remain on disk so the IntentWatcher converges.
	if after := NewAPI().ReadDaemonIntent().File.Tasks[taskName].Desired; after != IntentDesiredRunning {
		t.Fatalf("daemon-intent after reconcile failure = %q, want %q (must stay running for IntentWatcher convergence)",
			after, IntentDesiredRunning)
	}
	if len(results) != 1 || results[0].TaskName != taskName || results[0].Err == "" {
		t.Fatalf("results = %+v, want one supervisor error row", results)
	}
	if !strings.Contains(results[0].Err, "reconcile handler exploded") {
		t.Fatalf("results[0].Err = %q, want the reconcile failure surfaced", results[0].Err)
	}
}

// TestRestartSuccessFirstTryWritesRunningIntentOnlyPostSuccess pins the
// success path: a respawn that succeeds on the FIRST dial must observe the
// pre-existing (stopped) intent at dial time — proving no intent is written
// before the dial (the daba5d0 pre-dial write is removed). The running
// intent lands only AFTER the successful dial.
func TestRestartSuccessFirstTryWritesRunningIntentOnlyPostSuccess(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)

	const taskName = `\mcp-local-hub-time-default`
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "time",
			Daemon:   "default",
			Port:     9128,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	if err := NewAPI().WriteDaemonIntent(taskName, DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}, auditWhoMcphubStop); err != nil {
		t.Fatalf("seed stopped daemon-intent: %v", err)
	}

	var dials int
	var intentDesiredAtDial string
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		dials++
		intentDesiredAtDial = NewAPI().ReadDaemonIntent().File.Tasks[taskName].Desired
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	results, handled, err := restartSupervisorOwnedDaemons(context.Background(), "time", "")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if !handled {
		t.Fatal("handled=false; supervisor-owned time daemon should be restarted")
	}
	if dials != 1 {
		t.Fatalf("respawn dials = %d; want exactly 1 (success on first try, no retry)", dials)
	}
	if intentDesiredAtDial != IntentDesiredStopped {
		t.Fatalf("intent at dial = %q, want %q (no intent written before a first-try dial)",
			intentDesiredAtDial, IntentDesiredStopped)
	}
	// Post-success the running intent must now be on disk.
	after := NewAPI().ReadDaemonIntent().File.Tasks[taskName].Desired
	if after != IntentDesiredRunning {
		t.Fatalf("daemon-intent after success = %q, want %q (running intent recorded post-success)",
			after, IntentDesiredRunning)
	}
	if len(results) != 1 || results[0].TaskName != taskName || results[0].Err != "" {
		t.Fatalf("results = %+v, want one supervisor success row", results)
	}
}

func TestRestartFallsBackWhenNoSupervisorIntentMatches(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	defer restoreState()

	called := false
	restoreRespawn := setSupervisorRestartHooksForTest(func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error) {
		called = true
		return RespawnResult{Success: true, Code: "OK"}, nil
	})
	defer restoreRespawn()

	_, handled, err := restartSupervisorOwnedDaemons(context.Background(), "memory", "")
	if err != nil {
		t.Fatalf("restartSupervisorOwnedDaemons: %v", err)
	}
	if handled {
		t.Fatal("handled=true with no supervisor intent; want scheduler fallback")
	}
	if called {
		t.Fatal("supervisor respawn called even though supervisor-intent.json was absent")
	}
}

type restartAllFakeScheduler struct {
	tasks     []scheduler.TaskStatus
	runNames  []string
	stopNames []string
}

func (f *restartAllFakeScheduler) Create(scheduler.TaskSpec) error { return nil }
func (f *restartAllFakeScheduler) Delete(string) error             { return nil }
func (f *restartAllFakeScheduler) Run(name string) error {
	f.runNames = append(f.runNames, name)
	return nil
}
func (f *restartAllFakeScheduler) Stop(name string) error {
	f.stopNames = append(f.stopNames, name)
	return nil
}
func (f *restartAllFakeScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, nil
}
func (f *restartAllFakeScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	var out []scheduler.TaskStatus
	for _, task := range f.tasks {
		if strings.HasPrefix(strings.TrimPrefix(task.Name, `\`), prefix) {
			out = append(out, task)
		}
	}
	return out, nil
}
func (f *restartAllFakeScheduler) ExportXML(string) ([]byte, error) {
	return nil, scheduler.ErrTaskNotFound
}
func (f *restartAllFakeScheduler) ImportXML(string, []byte) error { return nil }
