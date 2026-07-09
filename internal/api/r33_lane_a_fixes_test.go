package api

// r33 Lane A — PR #288 bot round-32 + deep-predictive-sweep fixes.
//
// All four tests are hermetic: SetDaemonStateRootForTest redirects every state
// read/write to a fresh temp dir, the supervisor reconcile / probe / kill paths
// go through their package seams (supervisorReconcileApplyFn,
// installSupervisorRunningProbeFn, registerSupervisorReconcileFn, killByPortFn),
// and nothing touches the live host %LOCALAPPDATA%\mcp-local-hub\, the real
// scheduler, real IPC, or any real port.

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

// ---------------------------------------------------------------------------
// FIX 1 — removed-target / uninstall reconcile nudge probes the live owner on
// IPC-unavailable. A live-but-wedged supervisor (lock held, IPC unreachable)
// still tracks the child, so allowsRemovedTargetKill() must be FALSE; only a
// genuinely absent supervisor (no lock owner) leaves IPCUnavailable kill-OK.
// ---------------------------------------------------------------------------

// TestRemovedTargetNudge_OwnerAliveIPCUnavailable_DisallowsKill is the FIX 1
// core. With reconcile mapped to ErrSupervisorIPCUnavailable AND the
// flock-authoritative probe reporting a live lock owner, the removed-target
// nudge must demote to a status whose allowsRemovedTargetKill() is false so the
// force-kill seam is never entered.
//
// Negative-control: pre-fix nudgeSupervisorReconcileAfterRemovedTargets returned
// raw IPCUnavailable, which allowsRemovedTargetKill() treats as kill-OK — so the
// kill seam fired against a child the live supervisor still owns. Asserting the
// kill seam is NOT invoked fails against the pre-fix code.
func TestRemovedTargetNudge_OwnerAliveIPCUnavailable_DisallowsKill(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}))

	probeCalls := 0
	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(sd string) (bool, int, error) {
		probeCalls++
		if sd == "" {
			t.Fatal("removed-target owner probe received empty stateDir")
		}
		return true, 4242, nil // lock owner ALIVE, IPC unreachable
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	result := nudgeSupervisorReconcileAfterRemovedTargets(context.Background())
	if probeCalls != 1 {
		t.Fatalf("live-owner probe calls = %d, want 1 (the nudge must probe the lock owner on IPC-unavailable)", probeCalls)
	}
	if result.allowsRemovedTargetKill() {
		t.Fatalf("allowsRemovedTargetKill() = true for owner-alive + IPC-unavailable; a live wedged supervisor still tracks the child, so the kill must be skipped")
	}

	// End-to-end through the kill seam: assert the force-kill never runs.
	origKill := stopForceKillPIDFn
	t.Cleanup(func() { stopForceKillPIDFn = origKill })
	stopForceKillPIDFn = func(pid int) error {
		t.Fatalf("force-kill PID seam invoked (pid=%d) for owner-alive + IPC-unavailable removed target", pid)
		return nil
	}
	targets := []SupervisorDaemon{{TaskName: `\mcp-local-hub-demo-default`, Port: 19999}}
	var warns []string
	killRemovedSupervisorTargetsAfterNudge(targets, nil, result, "retry",
		func(format string, args ...any) { warns = append(warns, format) })
	if len(warns) == 0 {
		t.Fatal("expected a skip warning when the kill is disallowed; got none")
	}
}

// TestRemovedTargetNudge_NoOwnerIPCUnavailable_AllowsKill is the FIX 1 negative
// control: with reconcile mapped to ErrSupervisorIPCUnavailable AND the probe
// reporting NO live owner, the kill is safe (nothing will respawn the
// now-descriptorless child), so allowsRemovedTargetKill() stays true.
func TestRemovedTargetNudge_NoOwnerIPCUnavailable_AllowsKill(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}))

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) {
		return false, 0, nil // NO live owner — IPC really is unavailable
	}
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	result := nudgeSupervisorReconcileAfterRemovedTargets(context.Background())
	if result.status != supervisorReconcileNudgeIPCUnavailable {
		t.Fatalf("status = %v, want IPCUnavailable when no supervisor owns the lock", result.status)
	}
	if !result.allowsRemovedTargetKill() {
		t.Fatalf("allowsRemovedTargetKill() = false for no-owner + IPC-unavailable; the kill is safe because nothing respawns the child")
	}
}

// TestUninstallNudge_OwnerAliveIPCUnavailable_DisallowsKill mirrors FIX 1 on the
// uninstall best-effort caller: nudgeSupervisorReconcileAfterUninstall must run
// the same live-owner probe so an uninstall against a wedged-but-live supervisor
// does not force-kill a still-tracked child.
func TestUninstallNudge_OwnerAliveIPCUnavailable_DisallowsKill(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}))

	origProbe := installSupervisorRunningProbeFn
	installSupervisorRunningProbeFn = func(string) (bool, int, error) { return true, 7777, nil }
	t.Cleanup(func() { installSupervisorRunningProbeFn = origProbe })

	result := nudgeSupervisorReconcileAfterUninstall(context.Background())
	if result.allowsRemovedTargetKill() {
		t.Fatalf("allowsRemovedTargetKill() = true for owner-alive + IPC-unavailable uninstall; the kill must be skipped")
	}
}

// ---------------------------------------------------------------------------
// FIX 2 — selectSupervisorOwnedTargets matches a blank-field row by EXACT
// canonical task name when both (server, daemonFilter) are known, instead of the
// last-hyphen ParseManagedTaskName split that misattributes hyphenated daemon
// names.
// ---------------------------------------------------------------------------

// TestSelectSupervisorOwnedTargets_HyphenatedDaemon_ExactNameMatch locks the FIX
// 2 inversion. A blank-Server/blank-Daemon row whose TaskName is
// \mcp-local-hub-demo-alpha-beta (server demo, daemon alpha-beta) must be
// RETURNED for the real query (server=demo, daemon=alpha-beta), which the
// last-hyphen ParseManagedTaskName split would mis-attribute as
// server=demo-alpha / daemon=beta and therefore SKIP.
//
// Pre-fix falsifying property: ParseManagedTaskName splits the name as
// demo-alpha/beta, so (demo, alpha-beta) yields rowServer="demo-alpha" !=
// server "demo" -> SKIPPED -> the real target is lost. Asserting len==1 fails
// against the pre-fix code.
//
// Note on the (demo-alpha, beta) decomposition: by exact-name matching it
// produces the SAME canonical task name (mcp-local-hub-demo-alpha-beta), so it
// is inherently indistinguishable from (demo, alpha-beta) for a BLANK-field row
// — exactly like the sibling supervisorIntentRowMatchesServerDaemon. The
// disambiguator in production is the populated Server/Daemon fields a modern
// writer always sets; the negative control below uses those.
func TestSelectSupervisorOwnedTargets_HyphenatedDaemon_ExactNameMatch(t *testing.T) {
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Blank Server/Daemon descriptor fields — identity must come from
			// the canonical task name, NOT a last-hyphen split.
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Port: 19001},
		},
	}

	got := selectSupervisorOwnedTargets(intent, "demo", "alpha-beta")
	if len(got) != 1 {
		t.Fatalf("selectSupervisorOwnedTargets(demo, alpha-beta) returned %d targets, want 1 (the exact-name match for the hyphenated daemon; pre-fix the last-hyphen mis-split skips it)", len(got))
	}
	if got[0].TaskName != `\mcp-local-hub-demo-alpha-beta` {
		t.Fatalf("returned target TaskName = %q, want \\mcp-local-hub-demo-alpha-beta", got[0].TaskName)
	}

	// Negative control: a non-colliding wrong query must NOT match the blank
	// row. mcp-local-hub-demo-alpha-gamma != mcp-local-hub-demo-alpha-beta, so
	// the exact-name match correctly rejects it (the pre-fix mis-split would
	// also reject this one — the point is exact-name discrimination is sound).
	wrong := selectSupervisorOwnedTargets(intent, "demo-alpha", "gamma")
	if len(wrong) != 0 {
		t.Fatalf("selectSupervisorOwnedTargets(demo-alpha, gamma) returned %d targets, want 0 (non-matching task name must be rejected)", len(wrong))
	}
}

// TestSelectSupervisorOwnedTargets_CorruptGlobalArgvFailsClosed is bot PR #505 r6:
// a PARTIAL/corrupt global argv (`daemon --server demo`, no --daemon) is
// owner-rejected. selectSupervisorOwnedTargets must NOT fall back to the ambiguous
// canonical task-name match — `mcphub restart` / `stop --force` on the ambiguous
// name would otherwise respawn/kill a descriptor whose argv proves it is out of the
// requested scope. Fail closed for BOTH colliding sibling queries.
func TestSelectSupervisorOwnedTargets_CorruptGlobalArgvFailsClosed(t *testing.T) {
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Args: []string{"daemon", "--server", "demo"}, Port: 19010},
		},
	}
	if got := selectSupervisorOwnedTargets(intent, "demo", "alpha-beta"); len(got) != 0 {
		t.Fatalf("corrupt global argv selected %d targets for (demo, alpha-beta), want 0 (fail closed)", len(got))
	}
	if got := selectSupervisorOwnedTargets(intent, "demo-alpha", "beta"); len(got) != 0 {
		t.Fatalf("corrupt global argv selected %d targets for sibling (demo-alpha, beta), want 0", len(got))
	}

	// commission PR #505 r6 P1: a FULLY-POPULATED row whose fields contradict its
	// launch argv must not be selected by its stale field — restart/stop must not act
	// on a descriptor whose argv proves it belongs elsewhere.
	lyingIntent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default",
				Args: []string{"daemon", "--server", "time", "--daemon", "default"}, Port: 19011},
		},
	}
	if got := selectSupervisorOwnedTargets(lyingIntent, "memory", "default"); len(got) != 0 {
		t.Fatalf("populated field/argv-contradicting row selected %d for (memory,default), want 0 (fail closed)", len(got))
	}
	if got := selectSupervisorOwnedTargets(lyingIntent, "memory", ""); len(got) != 0 {
		t.Fatalf("server-only restart selected %d for the lying memory row, want 0 (DescriptorServerName='' on --server mismatch)", len(got))
	}
	// A well-formed populated row still selects (common-path neutral).
	okIntent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default",
				Args: []string{"daemon", "--server", "memory", "--daemon", "default"}, Port: 19012},
		},
	}
	if got := selectSupervisorOwnedTargets(okIntent, "memory", "default"); len(got) != 1 {
		t.Fatalf("well-formed populated row selected %d for (memory,default), want 1 (no common-path regression)", len(got))
	}
}

// TestSelectSupervisorOwnedTargets_PopulatedFieldsDisambiguateHyphenSplit is the
// FIX 2 negative control proper: a POPULATED-field row whose identity is
// server=demo-alpha / daemon=beta must match ONLY the (demo-alpha, beta) query
// and NOT the colliding (demo, alpha-beta) query. The populated fields are the
// authoritative disambiguator the exact-name path defers to (it only fires when
// the descriptor fields are blank), so the (server,daemon) filter below resolves
// the task-name concatenation collision correctly.
func TestSelectSupervisorOwnedTargets_PopulatedFieldsDisambiguateHyphenSplit(t *testing.T) {
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Populated identity: this IS server demo-alpha, daemon beta.
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Server: "demo-alpha", Daemon: "beta", Port: 19002},
		},
	}

	match := selectSupervisorOwnedTargets(intent, "demo-alpha", "beta")
	if len(match) != 1 {
		t.Fatalf("populated (demo-alpha, beta) query returned %d targets, want 1", len(match))
	}
	miss := selectSupervisorOwnedTargets(intent, "demo", "alpha-beta")
	if len(miss) != 0 {
		t.Fatalf("populated row identified as demo-alpha/beta must NOT match the (demo, alpha-beta) query; got %d targets", len(miss))
	}
}

// TestSelectSupervisorOwnedTargets_PopulatedAndUnfilteredUnchanged guards the
// non-regression contract: populated-field rows keep their exact (Server,Daemon)
// match, and the single-arg (server-only, no daemonFilter) caller still returns
// every owned row including the legacy rowDaemon=="" -> "default" path.
func TestSelectSupervisorOwnedTargets_PopulatedAndUnfilteredUnchanged(t *testing.T) {
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 19010},
			{TaskName: `\mcp-local-hub-memory-extra`, Server: "memory", Daemon: "extra", Port: 19011},
			{TaskName: `\mcp-local-hub-other-default`, Server: "other", Daemon: "default", Port: 19012},
		},
	}

	// Populated-field exact match still works.
	got := selectSupervisorOwnedTargets(intent, "memory", "extra")
	if len(got) != 1 || got[0].TaskName != `\mcp-local-hub-memory-extra` {
		t.Fatalf("populated-field filter (memory, extra) = %+v, want exactly the memory-extra row", got)
	}

	// Single-arg (server-only) caller returns every memory row, no other server.
	all := selectSupervisorOwnedTargets(intent, "memory", "")
	if len(all) != 2 {
		t.Fatalf("server-only filter (memory) returned %d rows, want 2", len(all))
	}
	for _, d := range all {
		if d.Server != "memory" {
			t.Fatalf("server-only filter leaked a non-memory row: %+v", d)
		}
	}
}

// ---------------------------------------------------------------------------
// FIX 3 — upsertLSPSupervisorIntent captures a prior stop tombstone after the
// read boundary canonicalizes raw bare keys, so a bare-keyed stop survives the
// rollback closure instead of being silently dropped.
// ---------------------------------------------------------------------------

// TestUpsertLSPSupervisorIntent_BareKeyStop_SurvivesRollback locks FIX 3. A
// bare-keyed operator stop for the LSP task is captured on the replace path; when
// the returned rollback closure runs against a post-spawn intent (descriptor
// present, stop cleared) it restores the descriptor WITH the stop tombstone.
//
// Negative-control: without read-boundary canonicalization, the canonical-only
// lookup misses the bare key, so hadPriorStop=false and the rollback restores the
// descriptor WITHOUT the stop — reviving a deliberately-stopped daemon. Asserting
// the stop is present after rollback fails without that canonicalization.
func TestUpsertLSPSupervisorIntent_BareKeyStop_SurvivesRollback(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(stateDir))
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	wsKey := "wskey1234"
	lang := "go"
	canonicalTask := LSPIntentTaskNameForWorkspaceLanguage(wsKey, lang) // leading-backslash canonical
	bareTask := canonicalTask[1:]                                       // bare (no leading backslash)

	entry := WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: filepath.Join(stateDir, "ws"),
		Language:      lang,
		Port:          19200,
	}
	priorDescriptor := BuildSupervisorDaemonForLSP(entry, "mcphub")

	// Seed: the descriptor already exists (forces the replace path) AND a stop is
	// keyed under the BARE form (legacy/external/migrated).
	stop := DaemonIntent{Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: time.Now().UTC()}
	writeRawSupervisorIntentFileForTest(t, intentPath, SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{priorDescriptor},
		Stops:   map[string]DaemonIntent{bareTask: stop},
	})
	rawSeed := readRawSupervisorIntentFileForTest(t, stateDir)
	if _, ok := rawSeed.Stops[bareTask]; !ok {
		t.Fatalf("raw seed lost bare stop key %q before exercising rollback path: %+v", bareTask, rawSeed.Stops)
	}
	if _, ok := rawSeed.Stops[canonicalTask]; ok {
		t.Fatalf("raw seed is canonicalized; test would be vacuous for bare-key regression: %+v", rawSeed.Stops)
	}

	rollback, err := NewAPI().upsertLSPSupervisorIntent(entry, "mcphub")
	if err != nil {
		t.Fatalf("upsertLSPSupervisorIntent: %v", err)
	}
	if rollback == nil {
		t.Fatal("upsertLSPSupervisorIntent returned a nil rollback closure")
	}

	// Simulate the post-spawn state the failed-reconcile rollback runs against:
	// the descriptor is present but the stop was cleared on disk.
	postSpawn := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{priorDescriptor},
		Stops:   map[string]DaemonIntent{},
	}
	if err := WriteSupervisorIntent(intentPath, postSpawn); err != nil {
		t.Fatalf("post-spawn WriteSupervisorIntent: %v", err)
	}

	rollback()

	rolledBack, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent after rollback: %v", err)
	}
	restored, ok := supervisorStopForTask(rolledBack.Stops, canonicalTask)
	if !ok {
		t.Fatalf("stop tombstone missing after rollback; FIX 3 must capture the bare-keyed prior stop. stops=%+v", rolledBack.Stops)
	}
	if restored.Desired != IntentDesiredStopped {
		t.Fatalf("restored stop Desired = %q, want %q", restored.Desired, IntentDesiredStopped)
	}
}

// ---------------------------------------------------------------------------
// FIX 4 — registerOneLanguageSupervised's rollback kills the port once the
// register stop has been cleared and the reconcile has been attempted, not on a
// late post-readiness flag. A reconcile that spawns-then-errors must still leave
// the rollback owning the port kill so restoreIntent() never orphans a daemon.
// ---------------------------------------------------------------------------

// TestRegisterSupervised_ReconcileErrorAfterStopCleared_RollbackKillsPort locks
// FIX 4. A fresh-workspace supervised register clears the stop, then the
// reconcile errors (spawn-then-error). proxy readiness is never reached, so the
// pre-fix supervisorSpawnRequested flag stays false and rollback skips the port
// kill — orphaning a daemon. Post-fix reconcileAttempted is set before the
// reconcile, so the rollback kills the port.
//
// Negative-control: pre-fix the rollback gates on the late flag and SKIPS the
// kill; asserting killByPortFn fired exactly once fails against the pre-fix code.
func TestRegisterSupervised_ReconcileErrorAfterStopCleared_RollbackKillsPort(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()
	restoreState := SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
	defer restoreState()

	// Reconcile spawns-then-errors: returns an error AFTER the register stop was
	// cleared, before proxy readiness is reached.
	origReconcile := registerSupervisorReconcileFn
	reconcileCalls := 0
	registerSupervisorReconcileFn = func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		reconcileCalls++
		return ReconcileResponse{}, context.DeadlineExceeded
	}
	defer func() { registerSupervisorReconcileFn = origReconcile }()

	// Capture the rollback port kill. A FRESH workspace has no legacy task, so
	// the only killByPortFn call possible is the rollback closure's.
	origKill := killByPortFn
	killCalls := 0
	var killedPort int
	killByPortFn = func(port int, _ time.Duration) error {
		killCalls++
		killedPort = port
		return nil
	}
	defer func() { killByPortFn = origKill }()

	ws := t.TempDir() // fresh workspace: no seeded legacy scheduler task / proxy
	_, err := mustNewAPI(t).registerWithManifest(nineLanguageManifest(), ws, []string{"go"}, RegisterOpts{
		Writer:          &bytes.Buffer{},
		SupervisedProxy: true,
	})
	if err == nil {
		t.Fatal("expected supervised register to fail when the reconcile errors after the stop was cleared")
	}
	if reconcileCalls != 1 {
		t.Fatalf("reconcile calls = %d, want 1", reconcileCalls)
	}
	if killCalls != 1 {
		t.Fatalf("killByPortFn calls = %d, want 1 (the rollback must kill the port after a spawn-then-error reconcile, or restoreIntent orphans the daemon)", killCalls)
	}
	if killedPort <= 0 {
		t.Fatalf("rollback killed port %d, want a positive registered port", killedPort)
	}
}
