package api

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// seedInstalledServerManifestsWithDaemons writes a `<name>/manifest.yaml` for
// each (server, daemons...) entry under a fresh temp manifest dir and points
// MCPHUB_MANIFEST_DIR_OVERRIDE at it, so listManifestNamesEmbedFirst() AND
// loadManifestForServer("", name) both resolve EXACTLY the seeded servers and
// their declared daemons — no leakage from the binary's embedded shipped set.
// Used by the FIX 5 exact-name resolver test, which needs each manifest to
// actually declare the daemon whose task name it must own.
func seedInstalledServerManifestsWithDaemons(t *testing.T, byServer map[string][]config.DaemonSpec) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	for name, daemons := range byServer {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("mkdir manifest %q: %v", name, err)
		}
		body := "name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: go\n"
		if len(daemons) > 0 {
			body += "daemons:\n"
			for _, d := range daemons {
				body += "  - name: " + d.Name + "\n    port: " + strconv.Itoa(d.Port) + "\n"
			}
		}
		body += "weekly_refresh: false\n"
		if err := os.WriteFile(filepath.Join(dir, name, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write manifest %q: %v", name, err)
		}
	}
	return dir
}

// ---------------------------------------------------------------------------
// FIX 1 (bot r35-2) — cleanupLegacySchedulerTasksForSupervisorInstall's
// full-server arm must NOT delete a hyphen-sibling's legacy scheduler task.
// ---------------------------------------------------------------------------

// TestCleanupLegacySchedulerTasksForSupervisorInstall_SparesHyphenSibling locks
// FIX 1. sch.List("mcp-local-hub-demo-") over-matches the sibling task
// \mcp-local-hub-demo-alpha-beta (server demo-alpha's daemon beta); the pre-fix
// loop deleted it unconditionally on every demo install. With the
// longest-installed-prefix gate, installing demo deletes only demo's own task
// and leaves the demo-alpha sibling's task intact.
//
// Negative-control (pre-fix): the unconditional delete loop removes
// \mcp-local-hub-demo-alpha-beta too → the survival assertion fails.
func TestCleanupLegacySchedulerTasksForSupervisorInstall_SparesHyphenSibling(t *testing.T) {
	daemonIntentTestHelper(t)
	// demo AND demo-alpha installed: demo-alpha is the longer prefix that owns
	// \mcp-local-hub-demo-alpha-beta.
	seedInstalledServerManifests(t, "demo", "demo-alpha")

	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},      // demo's own daemon task
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // sibling demo-alpha's daemon beta
	}
	installFakeScheduler(t, f)

	origLookup := lookupProcess
	lookupProcess = nil
	t.Cleanup(func() { lookupProcess = origLookup })

	origKill := forceKillByPortFn
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		return portKillNoListener, nil
	}
	t.Cleanup(func() { forceKillByPortFn = origKill })

	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 9313}},
	}
	var buf bytes.Buffer
	cleanupLegacySchedulerTasksForSupervisorInstall(m, "", &buf)

	deleted := make(map[string]struct{}, len(f.deleteNames))
	for _, n := range f.deleteNames {
		deleted[n] = struct{}{}
	}
	if _, ok := deleted["mcp-local-hub-demo-alpha-beta"]; ok {
		t.Fatalf("installing demo deleted sibling demo-alpha's task \\mcp-local-hub-demo-alpha-beta; deleteNames=%v", f.deleteNames)
	}
	if _, ok := deleted["mcp-local-hub-demo-alpha"]; !ok {
		t.Fatalf("installing demo did not delete its own obsolete task \\mcp-local-hub-demo-alpha; deleteNames=%v", f.deleteNames)
	}
}

// ---------------------------------------------------------------------------
// FIX 2 — legacySchedulerTasksForSupervisorInstallDryRun must MIRROR FIX 1: the
// dry-run preview must not claim it will delete a hyphen sibling FIX 1 spares.
// ---------------------------------------------------------------------------

// TestLegacySchedulerTasksDryRun_SparesHyphenSibling locks FIX 2. The dry-run
// preview of install demo (with sibling demo-alpha installed) must NOT list
// \mcp-local-hub-demo-alpha-beta for deletion — otherwise the dry-run LIES
// about what the real cleanup (FIX 1) does.
//
// Negative-control (pre-fix): the preview lists every prefix-matched task →
// the absence assertion fails.
func TestLegacySchedulerTasksDryRun_SparesHyphenSibling(t *testing.T) {
	daemonIntentTestHelper(t)
	seedInstalledServerManifests(t, "demo", "demo-alpha")

	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},
		{Name: `\mcp-local-hub-demo-alpha-beta`},
	}
	installFakeScheduler(t, f)

	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 9313}},
	}
	preview, err := legacySchedulerTasksForSupervisorInstallDryRun(m, "")
	if err != nil {
		t.Fatalf("legacySchedulerTasksForSupervisorInstallDryRun: %v", err)
	}
	listed := make(map[string]struct{}, len(preview))
	for _, n := range preview {
		listed[n] = struct{}{}
	}
	if _, ok := listed["mcp-local-hub-demo-alpha-beta"]; ok {
		t.Fatalf("dry-run preview claims it will delete sibling demo-alpha's task \\mcp-local-hub-demo-alpha-beta; preview=%v", preview)
	}
	if _, ok := listed["mcp-local-hub-demo-alpha"]; !ok {
		t.Fatalf("dry-run preview omitted demo's own task \\mcp-local-hub-demo-alpha; preview=%v", preview)
	}
}

// ---------------------------------------------------------------------------
// FIX 3 — the no-manifest (retired-manifest) uninstall path must build a
// sibling-safe scope, not pass nil scope (which routes ownership to the RAW
// HasPrefix residual that force-prunes + kills a hyphen sibling's row).
// ---------------------------------------------------------------------------

// TestRemoveServerFromSupervisorIntentBestEffort_NoManifest_SparesHyphenSibling
// locks FIX 3. A retired-manifest uninstall of "gdb" must leave the
// blank-Server row \mcp-local-hub-gdb-remote-default (installed sibling
// "gdb-remote") intact — neither pruned from supervisor-intent.json nor
// force-killed.
//
// Negative-control (pre-fix nil scope): supervisorIntentRowOwnedByScope(d,
// "gdb", nil) falls to the raw prefix match, prunes the gdb-remote row and
// kills its PID → the survival assertions fail.
func TestRemoveServerFromSupervisorIntentBestEffort_NoManifest_SparesHyphenSibling(t *testing.T) {
	fakeMcphubIdentityForTest(t)
	stateDir := phaseFStateDir(t)
	// gdb-remote installed: it owns \mcp-local-hub-gdb-remote-default. gdb is
	// the retired (no-manifest) server being uninstalled.
	seedInstalledServerManifests(t, "gdb-remote", "other")
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Blank-Server sibling row that the pre-fix raw prefix would mis-claim.
			{TaskName: `\mcp-local-hub-gdb-remote-default`, Command: "preserve-gdb-remote", Port: 9401},
			{TaskName: `\mcp-local-hub-other-default`, Server: "other", Daemon: "default", Command: "preserve-other", Port: 9991},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent: %v", err)
	}

	// If a reconcile nudge or kill fires, record it — the sibling-only state
	// means NOTHING should be removed, so changed=false and no nudge/kill.
	var nudged int
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) {
		nudged++
		return ReconcileResponse{}, nil
	}))
	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{
			{TaskName: `\mcp-local-hub-gdb-remote-default`, PID: 7401, State: "Running"},
		}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })
	var killedPIDs []int
	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error {
		killedPIDs = append(killedPIDs, pid)
		return nil
	}
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	report := &UninstallReport{}
	NewAPI().removeServerFromSupervisorIntentBestEffort("gdb", report)

	if len(killedPIDs) != 0 {
		t.Fatalf("retired-manifest uninstall of gdb killed PIDs %v, want none (gdb-remote sibling must not be touched)", killedPIDs)
	}

	got, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	survived := make(map[string]SupervisorDaemon, len(got.Daemons))
	for _, d := range got.Daemons {
		survived[canonicalIntentTaskKey(d.TaskName)] = d
	}
	if d, ok := survived[`\mcp-local-hub-gdb-remote-default`]; !ok || d.Command != "preserve-gdb-remote" {
		t.Fatalf("sibling gdb-remote row was pruned by retired-manifest uninstall of gdb; survived=%+v", survived)
	}
	if _, ok := survived[`\mcp-local-hub-other-default`]; !ok {
		t.Fatalf("unrelated sibling other was pruned; survived=%+v", survived)
	}
}

// ---------------------------------------------------------------------------
// FIX 4 (bot r35-3) — selectSupervisorOwnedTargets server-only (daemonFilter=="")
// blank-row case must claim the real target via the longest-installed-prefix
// disambiguator instead of the lossy ParseManagedTaskName last-hyphen split.
// ---------------------------------------------------------------------------

// TestSelectSupervisorOwnedTargets_ServerOnly_BlankRow_ClaimsViaLongestPrefix
// locks FIX 4. selectSupervisorOwnedTargets(intent, "demo", "") on a blank-field
// \mcp-local-hub-demo-alpha-beta row:
//   - when only demo is installed → the row is demo's, so it is RETURNED.
//   - when demo-alpha is ALSO installed → demo-alpha owns it, so demo does NOT
//     claim it.
//
// Negative-control (pre-fix): the last-hyphen split derives server=demo-alpha,
// the rowServer != "demo" filter skips the row → the only-demo subtest gets 0
// targets and fails.
func TestSelectSupervisorOwnedTargets_ServerOnly_BlankRow_ClaimsViaLongestPrefix(t *testing.T) {
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Blank Server/Daemon descriptor fields → identity must come from the
			// longest-installed-prefix disambiguator, NOT a last-hyphen split.
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Port: 19101},
		},
	}

	t.Run("only demo installed → demo claims the blank row", func(t *testing.T) {
		seedInstalledServerManifests(t, "demo")
		got := selectSupervisorOwnedTargets(intent, "demo", "")
		if len(got) != 1 {
			t.Fatalf("selectSupervisorOwnedTargets(demo, \"\") returned %d targets, want 1 (the blank row is demo's when no sibling is installed; pre-fix the last-hyphen mis-split skips it)", len(got))
		}
		if got[0].TaskName != `\mcp-local-hub-demo-alpha-beta` {
			t.Fatalf("returned target TaskName = %q, want \\mcp-local-hub-demo-alpha-beta", got[0].TaskName)
		}
	})

	t.Run("demo-alpha also installed → demo does NOT claim the sibling row", func(t *testing.T) {
		seedInstalledServerManifests(t, "demo", "demo-alpha")
		got := selectSupervisorOwnedTargets(intent, "demo", "")
		if len(got) != 0 {
			t.Fatalf("selectSupervisorOwnedTargets(demo, \"\") returned %d targets, want 0 (demo-alpha is the longer installed prefix and owns the row)", len(got))
		}
		// The rightful owner DOES claim it.
		owner := selectSupervisorOwnedTargets(intent, "demo-alpha", "")
		if len(owner) != 1 || owner[0].TaskName != `\mcp-local-hub-demo-alpha-beta` {
			t.Fatalf("selectSupervisorOwnedTargets(demo-alpha, \"\") = %+v, want exactly the demo-alpha-beta row", owner)
		}
	})
}

// ---------------------------------------------------------------------------
// FIX 5 — SchedulerUpgrade must recreate a hyphenated-daemon task with the
// CORRECT (server, daemon) Args resolved from the task name (exact-name match
// against installed manifests), not parseTaskName's lossy last-hyphen split.
// ---------------------------------------------------------------------------

// TestSchedulerUpgrade_HyphenatedDaemon_RecreatesWithCorrectArgs locks FIX 5.
// Task \mcp-local-hub-demo-alpha-beta belongs to installed server demo (daemon
// alpha-beta); sibling server demo-alpha is also installed (daemon gamma, which
// does NOT match the task name). The upgrade must Delete+Create the task with
// Args [daemon --server demo --daemon alpha-beta].
//
// Negative-control (pre-fix): parseTaskName splits to demo-alpha/beta and the
// recreated task carries Args [daemon --server demo-alpha --daemon beta] →
// spawns the wrong server/daemon. Asserting --server demo --daemon alpha-beta
// fails pre-fix.
func TestSchedulerUpgrade_HyphenatedDaemon_RecreatesWithCorrectArgs(t *testing.T) {
	daemonIntentTestHelper(t)
	// demo declares daemon alpha-beta (which produces the exact task name);
	// demo-alpha declares an unrelated daemon gamma (so the exact-name match is
	// unambiguous and resolves to demo/alpha-beta).
	seedInstalledServerManifestsWithDaemons(t, map[string][]config.DaemonSpec{
		"demo":       {{Name: "alpha-beta", Port: 9313}},
		"demo-alpha": {{Name: "gamma", Port: 9314}},
	})

	// ensureCanonicalMcphubPresent() os.Stat's this path; point it at a real
	// stub file so the preflight passes without touching the host binary.
	stubDir := t.TempDir()
	stubPath := filepath.Join(stubDir, mcphubShortName)
	if err := os.WriteFile(stubPath, []byte("stub\n"), 0o755); err != nil {
		t.Fatalf("write stub mcphub: %v", err)
	}
	origCanonical := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = stubPath
	t.Cleanup(func() { testCanonicalMcphubPathOverride = origCanonical })

	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{{Name: `\mcp-local-hub-demo-alpha-beta`}}
	// Seed the task as existing so ExportXML/Delete behave like a real upgrade.
	f.tasks[`\mcp-local-hub-demo-alpha-beta`] = true
	f.xml = map[string][]byte{`\mcp-local-hub-demo-alpha-beta`: []byte("<Task/>")}
	installFakeScheduler(t, f)

	results, err := NewAPI().SchedulerUpgrade()
	if err != nil {
		t.Fatalf("SchedulerUpgrade: %v", err)
	}

	// The task must have been recreated (one createdSpec) with the right Args.
	var recreated *scheduler.TaskSpec
	for i := range f.createdSpecs {
		if f.createdSpecs[i].Name == `\mcp-local-hub-demo-alpha-beta` {
			recreated = &f.createdSpecs[i]
			break
		}
	}
	if recreated == nil {
		t.Fatalf("task \\mcp-local-hub-demo-alpha-beta was not recreated; createdSpecs=%+v results=%+v", f.createdSpecs, results)
	}
	wantArgs := []string{"daemon", "--server", "demo", "--daemon", "alpha-beta"}
	if len(recreated.Args) != len(wantArgs) {
		t.Fatalf("recreated Args = %v, want %v", recreated.Args, wantArgs)
	}
	for i := range wantArgs {
		if recreated.Args[i] != wantArgs[i] {
			t.Fatalf("recreated Args = %v, want %v (the task name is the authority — not the lossy demo-alpha/beta split)", recreated.Args, wantArgs)
		}
	}
}
