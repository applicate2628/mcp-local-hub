package cli

// Falsifying regression tests for the PR #288 r37-1 hyphen-family instances the
// earlier exhaustive sweep missed: two cli-side DECISION sites parsed a
// blank-Server supervisor-intent / scheduler-task name with api.ServerFromTaskName
// (a greedy LastIndex('-') split), so a hyphenated-daemon task like
// \mcp-local-hub-demo-alpha-beta (real server "demo", daemon "alpha-beta") was
// mis-attributed to "demo-alpha".
//
//	r37-1a — supervisorIntentManagedServerSignals (supervisor_intent_signals.go)
//	r37-1b — shouldRemoveGlobalWatchdog scheduler-task gate (setup.go)
//
// Both now resolve the true owner via api.ServerOwningTaskByLongestInstalledPrefix
// (the single longest-installed-prefix owner). Each test seeds the installed
// catalog via MCPHUB_MANIFEST_DIR_OVERRIDE so the disambiguator sees EXACTLY the
// named servers, drives the gate through the recording fake scheduler, and routes
// every state file into a temp dir (api.SetDaemonStateRootForTest via
// setupWatchdogTestHelper). NO real schtasks, NO real kills, NO real ports bound.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

// seedSetupHyphenInstalledManifests seeds a hermetic installed-server catalog via
// MCPHUB_MANIFEST_DIR_OVERRIDE so ManifestList() (consulted by the
// longest-installed-prefix disambiguator in both r37-1 sites) sees EXACTLY the
// named servers — no leakage from the binary's embedded shipped set. Mirrors the
// api-side seedInstalledServerManifests + config-env-side
// seedConfigEnvInstalledManifests, which are not visible here.
func seedSetupHyphenInstalledManifests(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("mkdir manifest %q: %v", name, err)
		}
		body := "name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: go\n"
		if err := os.WriteFile(filepath.Join(dir, name, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("write manifest %q: %v", name, err)
		}
	}
}

// writeSetupHyphenIntent writes a supervisor-intent.json with the given daemons
// into the test state-dir (set by setupWatchdogTestHelper's
// api.SetDaemonStateRootForTest) so supervisorIntentManagedServerSignals reads
// it via api.DefaultSupervisorIntentPath.
func writeSetupHyphenIntent(t *testing.T, stateDir string, daemons ...api.SupervisorDaemon) {
	t.Helper()
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: daemons}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
}

// TestSupervisorIntentManagedServerSignals_BlankServerHyphenatedDaemon_KeysRealOwner
// is the falsifying regression for r37-1a. A blank-Server row
// \mcp-local-hub-demo-alpha-beta with ONLY "demo" installed must signal "demo",
// never "demo-alpha".
//
// Pre-fix: server=="" → server = api.ServerFromTaskName(taskName) → last-hyphen
// split → "demo-alpha", so the signal set wrongly contains "demo-alpha". This
// poisons install.go:351's reconcile-hub installed-server filter and setup.go's
// last-server maintenance gate. The assertion below FAILS pre-fix (the set
// contains "demo-alpha" and lacks "demo").
func TestSupervisorIntentManagedServerSignals_BlankServerHyphenatedDaemon_KeysRealOwner(t *testing.T) {
	stateDir, _ := setupWatchdogTestHelper(t)
	// ONLY "demo" installed — NOT "demo-alpha". So "demo" is the longest
	// installed prefix of \mcp-local-hub-demo-alpha-beta and owns the row.
	seedSetupHyphenInstalledManifests(t, "demo")
	writeSetupHyphenIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Port:     9123,
	})

	got, err := supervisorIntentManagedServerSignals()
	if err != nil {
		t.Fatalf("supervisorIntentManagedServerSignals: %v", err)
	}
	if _, ok := got["demo"]; !ok {
		t.Fatalf("signals must contain real owner \"demo\"; got %v (r37-1a falsified)", got)
	}
	if _, ok := got["demo-alpha"]; ok {
		t.Fatalf("signals must NOT contain mis-split \"demo-alpha\"; got %v (r37-1a falsified)", got)
	}
	if len(got) != 1 {
		t.Fatalf("signals = %v, want exactly {demo}", got)
	}
}

// TestSupervisorIntentManagedServerSignals_LongerInstalledSiblingOwns pins the
// other direction: with BOTH "demo" and "demo-alpha" installed, "demo-alpha" is
// the longer prefix and is the REAL owner of \mcp-local-hub-demo-alpha-beta, so
// the signal set keys "demo-alpha".
func TestSupervisorIntentManagedServerSignals_LongerInstalledSiblingOwns(t *testing.T) {
	stateDir, _ := setupWatchdogTestHelper(t)
	seedSetupHyphenInstalledManifests(t, "demo", "demo-alpha")
	writeSetupHyphenIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Port:     9124,
	})

	got, err := supervisorIntentManagedServerSignals()
	if err != nil {
		t.Fatalf("supervisorIntentManagedServerSignals: %v", err)
	}
	if _, ok := got["demo-alpha"]; !ok {
		t.Fatalf("signals must contain real owner \"demo-alpha\"; got %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("signals = %v, want exactly {demo-alpha}", got)
	}
}

// TestSupervisorIntentManagedServerSignals_OrphanRowFallsBackToTaskName guards
// the preserved fallback: a blank-Server row whose <server>-... portion matches
// NO installed server (an orphan/foreign row) keeps the existing taskName key —
// the fix only changes the resolution when an installed prefix exists.
func TestSupervisorIntentManagedServerSignals_OrphanRowFallsBackToTaskName(t *testing.T) {
	stateDir, _ := setupWatchdogTestHelper(t)
	// "demo" installed, but the orphan row's portion ("zzz-x") is owned by no
	// installed server → ok=false → taskName fallback.
	seedSetupHyphenInstalledManifests(t, "demo")
	const orphan = `\mcp-local-hub-zzz-x`
	writeSetupHyphenIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: orphan,
		Port:     9125,
	})

	got, err := supervisorIntentManagedServerSignals()
	if err != nil {
		t.Fatalf("supervisorIntentManagedServerSignals: %v", err)
	}
	if _, ok := got[orphan]; !ok {
		t.Fatalf("orphan row must fall back to taskName key %q; got %v", orphan, got)
	}
	if len(got) != 1 {
		t.Fatalf("signals = %v, want exactly {%q}", got, orphan)
	}
}

// TestSupervisorIntentManagedServerSignals_PopulatedServerPathUnchanged guards
// the unchanged populated-Server path: a row carrying a real Server field is
// keyed on that field verbatim, regardless of any catalog or hyphenation.
func TestSupervisorIntentManagedServerSignals_PopulatedServerPathUnchanged(t *testing.T) {
	stateDir, _ := setupWatchdogTestHelper(t)
	// No manifest override needed — the populated path never consults the
	// catalog. (A hyphenated daemon name in a populated row must NOT be
	// re-split.)
	writeSetupHyphenIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Server:   "demo",
		Daemon:   "alpha-beta",
		Port:     9126,
	})

	got, err := supervisorIntentManagedServerSignals()
	if err != nil {
		t.Fatalf("supervisorIntentManagedServerSignals: %v", err)
	}
	if _, ok := got["demo"]; !ok {
		t.Fatalf("populated Server=demo row must key \"demo\"; got %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("signals = %v, want exactly {demo}", got)
	}
}

// TestSupervisorIntentManagedServerSignals_PopulatedLyingFieldFailsClosed is
// commission PR #505 r6b: a fully-populated row whose Server field CONTRADICTS its
// launch argv must NOT publish its stale field as a managed-server signal (the
// signal gates hub client-config reconcile writes — a publish decision).
func TestSupervisorIntentManagedServerSignals_PopulatedLyingFieldFailsClosed(t *testing.T) {
	stateDir, _ := setupWatchdogTestHelper(t)
	writeSetupHyphenIntent(t, stateDir, api.SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "time", "--daemon", "default"},
	})
	got, err := supervisorIntentManagedServerSignals()
	if err != nil {
		t.Fatalf("supervisorIntentManagedServerSignals: %v", err)
	}
	if _, ok := got["memory"]; ok {
		t.Fatalf("lying row (field memory, argv --server time) must NOT publish 'memory' signal; got %v (r6b)", got)
	}
	if _, ok := got["time"]; ok {
		t.Fatalf("lying row must not publish the argv 'time' as an installed-server signal either; got %v", got)
	}
}

// TestShouldRemoveGlobalWatchdog_BlankServerHyphenatedTask_GateDecidesByRealOwner
// is the falsifying regression for r37-1b. Uninstalling "demo" when the only
// remaining real scheduler task is \mcp-local-hub-demo-alpha-beta (real server
// "demo", daemon "alpha-beta") and ONLY "demo" is installed must authorize the
// last-server teardown (return true) — that task IS demo's, so nothing remains
// after demo is uninstalled.
//
// Pre-fix: srv := api.ServerFromTaskName(\mcp-local-hub-demo-alpha-beta) →
// last-hyphen split → "demo-alpha" != serverBeingUninstalled("demo") → the task
// is counted as a remaining peer "demo-alpha" → the gate returns FALSE (watchdog
// wrongly kept installed). Post-fix the longest-installed-prefix owner is "demo"
// == serverBeingUninstalled → skipped → remaining empty → gate returns TRUE.
//
// This asserts the gate DECISION flips, which is exactly the r37-1b defect.
func TestShouldRemoveGlobalWatchdog_BlankServerHyphenatedTask_GateDecidesByRealOwner(t *testing.T) {
	stateDir, fakeSch := setupWatchdogTestHelper(t)
	// ONLY "demo" installed → demo owns \mcp-local-hub-demo-alpha-beta.
	seedSetupHyphenInstalledManifests(t, "demo")
	// Empty supervisor-intent so the scheduler-task side is the deciding factor.
	writeSetupHyphenIntent(t, stateDir)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // really demo's daemon alpha-beta
		{Name: api.LegacyWatchdogTaskName},       // hub-wide maintenance, filtered out
		{Name: api.LivenessTaskName},             // hub-wide maintenance, filtered out
	}

	out := &bytes.Buffer{}
	if !shouldRemoveGlobalWatchdog(out, "demo") {
		t.Fatalf("gate must authorize teardown: the only real task is demo's own "+
			"hyphenated daemon, so nothing remains after uninstalling demo; output=%q "+
			"(r37-1b falsified — last-hyphen split mis-counted it as peer demo-alpha)", out.String())
	}
}

// TestShouldRemoveGlobalWatchdog_LongerInstalledSiblingKeepsWatchdog pins the
// other direction: with BOTH "demo" and "demo-alpha" installed, the task
// \mcp-local-hub-demo-alpha-beta is REALLY demo-alpha's. Uninstalling "demo"
// must keep the watchdog because demo-alpha (a peer) still owns a task.
func TestShouldRemoveGlobalWatchdog_LongerInstalledSiblingKeepsWatchdog(t *testing.T) {
	stateDir, fakeSch := setupWatchdogTestHelper(t)
	seedSetupHyphenInstalledManifests(t, "demo", "demo-alpha")
	writeSetupHyphenIntent(t, stateDir)

	fakeSch.listResult = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // really demo-alpha's daemon beta
	}

	out := &bytes.Buffer{}
	if shouldRemoveGlobalWatchdog(out, "demo") {
		t.Fatalf("gate must KEEP watchdog: demo-alpha (peer) still owns a task; output=%q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("demo-alpha")) {
		t.Fatalf("gate output should name the retaining peer demo-alpha; got %q", out.String())
	}
}
