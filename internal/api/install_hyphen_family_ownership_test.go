package api

// Regression tests for the PR #288 hyphen-family ownership class (r36 lane A1).
//
// A server `demo` must NOT claim / kill / delete
// `\mcp-local-hub-demo-alpha-beta`, which belongs to sibling server
// `demo-alpha` (daemon `beta`). The landed disambiguator is
// blankServerRowOwnedByLongestInstalledPrefix: a `\mcp-local-hub-<server>-...`
// task is claimed for `server` ONLY when no LONGER installed server name is
// also a prefix of the task. These four tests pin the four code sites that the
// bot repeatedly flagged:
//
//	FIX 1 — uninstallSchedulerTasksForServer (legacy-scheduler-task DELETE)
//	FIX 2 — pruneObsoleteServerTasks         (full-reinstall DELETE)
//	FIX 3 — stopKillCore / Restart per-daemon gate (HasSuffix → exact identity)
//	FIX 4 — listTasksForServer                (stop/restart --server scope)
//
// Every test is FALSIFYING: each comment names the pre-fix behavior the
// assertion catches. They use fake schedulers + isolated state/manifest dirs +
// kill/port seams — NO real schtasks, NO real kills, NO real ports bound.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// seedManifestsWithDaemons writes a `<name>/manifest.yaml` for each entry under
// a fresh temp manifest dir and points MCPHUB_MANIFEST_DIR_OVERRIDE at it, so
// BOTH the installed-server catalog (listManifestNamesEmbedFirst, consulted by
// the longest-installed-prefix disambiguator) AND port resolution
// (manifestPortMap → portForTask) see EXACTLY these servers — no leakage from
// the binary's embedded shipped set. Each manifest declares the given
// daemon→port rows so a sibling task resolves to a REAL port (proving the
// danger the fix removes). Returns the manifest dir.
func seedManifestsWithDaemons(t *testing.T, specs map[string]map[string]int) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	for name, daemons := range specs {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("mkdir manifest %q: %v", name, err)
		}
		var b strings.Builder
		b.WriteString("name: " + name + "\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n")
		for dn, port := range daemons {
			b.WriteString("  - name: " + dn + "\n    port: " + strconv.Itoa(port) + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, name, "manifest.yaml"), []byte(b.String()), 0o600); err != nil {
			t.Fatalf("write manifest %q: %v", name, err)
		}
	}
	return dir
}

// hyphenFamilyTaskNames returns the bare names of every task in a slice, for
// readable assertion failure messages.
func hyphenFamilyTaskNames(tasks []scheduler.TaskStatus) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, strings.TrimPrefix(t.Name, "\\"))
	}
	return out
}

func sliceContainsBare(names []string, want string) bool {
	for _, n := range names {
		if strings.TrimPrefix(n, "\\") == strings.TrimPrefix(want, "\\") {
			return true
		}
	}
	return false
}

// FIX 1 — uninstallSchedulerTasksForServer must NOT return a sibling-owned task
// for deletion.
//
// Pre-fix: sch.List("mcp-local-hub-demo-") is a raw HasPrefix match that
// over-captures `\mcp-local-hub-demo-alpha-beta`; both DELETE callers
// (Uninstall, uninstallWithoutManifest) then sch.Delete it unconditionally,
// destroying sibling server demo-alpha's task. Post-fix: the function filters
// to longest-installed-prefix-owned tasks, so demo-alpha-beta is excluded
// (demo-alpha is a longer installed prefix that owns it).
func TestUninstallSchedulerTasksForServer_KeepsSiblingTask(t *testing.T) {
	daemonIntentTestHelper(t)
	// demo AND demo-alpha are both installed: demo-alpha is the longer prefix
	// owning \mcp-local-hub-demo-alpha-beta.
	seedInstalledServerManifests(t, "demo", "demo-alpha")

	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},      // demo's OWN daemon task
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // sibling demo-alpha / daemon beta
	}
	installFakeScheduler(t, f)

	_, tasks, err := uninstallSchedulerTasksForServer("demo")
	if err != nil {
		t.Fatalf("uninstallSchedulerTasksForServer(demo): %v", err)
	}
	got := hyphenFamilyTaskNames(tasks)

	// CORE: the sibling-owned task must NOT be in the to-delete set.
	if sliceContainsBare(got, "mcp-local-hub-demo-alpha-beta") {
		t.Fatalf("uninstall demo returned sibling task for deletion: %v (FIX 1 falsified)", got)
	}
	// demo's own task is still scheduled for deletion.
	if !sliceContainsBare(got, "mcp-local-hub-demo-alpha") {
		t.Fatalf("uninstall demo dropped its OWN task; to-delete=%v", got)
	}
}

// FIX 1 negative control: when NO longer-prefix sibling is installed, the
// blank/legacy prefix-matching task IS reclaimed (the documented safe
// full-cleanup fallback). Only `demo` installed → demo owns every
// `mcp-local-hub-demo-*` task, including the hyphenated one.
func TestUninstallSchedulerTasksForServer_ReclaimsWhenNoSibling(t *testing.T) {
	daemonIntentTestHelper(t)
	seedInstalledServerManifests(t, "demo") // no demo-alpha sibling

	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},
		{Name: `\mcp-local-hub-demo-alpha-beta`},
	}
	installFakeScheduler(t, f)

	_, tasks, err := uninstallSchedulerTasksForServer("demo")
	if err != nil {
		t.Fatalf("uninstallSchedulerTasksForServer(demo): %v", err)
	}
	got := hyphenFamilyTaskNames(tasks)
	if !sliceContainsBare(got, "mcp-local-hub-demo-alpha-beta") {
		t.Fatalf("no sibling installed but demo did not reclaim demo-alpha-beta; to-delete=%v", got)
	}
	if !sliceContainsBare(got, "mcp-local-hub-demo-alpha") {
		t.Fatalf("demo dropped its own task; to-delete=%v", got)
	}
}

// FIX 2 — pruneObsoleteServerTasks must NOT prune a sibling-owned task on a
// full reinstall of demo.
//
// Pre-fix: `planned` holds only demo's OWN tasks, so demo-alpha's task
// (absent from planned) was sch.Delete-d on every full reinstall of demo.
// Post-fix: the longest-installed-prefix guard skips it — demo-alpha owns it.
// demo's own obsolete task (not in planned, owned by demo) IS still pruned.
func TestPruneObsoleteServerTasks_KeepsSiblingPrunesOwnObsolete(t *testing.T) {
	daemonIntentTestHelper(t)
	seedInstalledServerManifests(t, "demo", "demo-alpha")

	f := newInstallFakeScheduler()
	f.listSeed = []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},      // demo's PLANNED task (kept by planned set)
		{Name: `\mcp-local-hub-demo-obsolete`},   // demo's OWN obsolete task → must be pruned
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // sibling demo-alpha / beta → must survive
	}

	// planned = demo's own current task set (only demo-alpha).
	planned := map[string]struct{}{
		"mcp-local-hub-demo-alpha": {},
	}

	rollbacks, err := pruneObsoleteServerTasks(f, "demo", planned, os.Stderr)
	if err != nil {
		t.Fatalf("pruneObsoleteServerTasks(demo): %v", err)
	}
	_ = rollbacks

	// CORE: sibling task survives.
	if sliceContainsBare(f.deleteNames, "mcp-local-hub-demo-alpha-beta") {
		t.Fatalf("full reinstall of demo pruned sibling task; deleted=%v (FIX 2 falsified)", f.deleteNames)
	}
	// demo's own obsolete task was pruned.
	if !sliceContainsBare(f.deleteNames, "mcp-local-hub-demo-obsolete") {
		t.Fatalf("demo's own obsolete task was not pruned; deleted=%v", f.deleteNames)
	}
	// The planned task is never pruned.
	if sliceContainsBare(f.deleteNames, "mcp-local-hub-demo-alpha") {
		t.Fatalf("planned task was pruned; deleted=%v", f.deleteNames)
	}
}

// FIX 4 — listTasksForServer must return ONLY demo-owned tasks for `--server
// demo`, never the sibling demo-alpha-beta.
//
// Pre-fix: sch.List("mcp-local-hub-demo-") over-captures
// \mcp-local-hub-demo-alpha-beta; portForTask→parseTaskName then resolves
// demo-alpha's REAL port and stop/restart kill its live daemon. Post-fix the
// primary list is filtered to demo-owned tasks. The workspace lsp- branch is
// untouched (covered by the workspace test below).
func TestListTasksForServer_FiltersSiblingTask(t *testing.T) {
	daemonIntentTestHelper(t)
	seedInstalledServerManifests(t, "demo", "demo-alpha")

	f := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // sibling
	}}

	tasks, err := listTasksForServer(f, "demo")
	if err != nil {
		t.Fatalf("listTasksForServer(demo): %v", err)
	}
	got := hyphenFamilyTaskNames(tasks)
	if sliceContainsBare(got, "mcp-local-hub-demo-alpha-beta") {
		t.Fatalf("listTasksForServer(demo) surfaced sibling task: %v (FIX 4 falsified)", got)
	}
	if !sliceContainsBare(got, "mcp-local-hub-demo-alpha") {
		t.Fatalf("listTasksForServer(demo) dropped demo's own task; got=%v", got)
	}
}

// FIX 3 (Stop) — `stop --server demo --daemon beta` must kill ONLY demo's beta
// port, never sibling demo-alpha's beta (task \mcp-local-hub-demo-alpha-beta,
// whose port resolves to demo-alpha's REAL port).
//
// Pre-fix the per-daemon gate was bare HasSuffix(normalized, "-beta"), which
// matched \mcp-local-hub-demo-alpha-beta (parsedServer=demo-alpha) → its port
// 9402 was killed. Post-fix the exact-identity gate
// (parsedServer==demo && parsedDaemon==beta) rejects it; only demo's beta
// (9302) is killed.
func TestStop_ServerDaemon_DoesNotKillSiblingPort(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	// demo has its OWN beta (9302); demo-alpha also has a beta (9402). The
	// sibling task \mcp-local-hub-demo-alpha-beta resolves to 9402.
	seedManifestsWithDaemons(t, map[string]map[string]int{
		"demo":       {"beta": 9302},
		"demo-alpha": {"beta": 9402},
	})
	// Empty supervisor-intent → the supervisor stop pass finds no owned
	// targets and the legacy kill path (stopKillCore) runs, which is what FIX 3
	// patches.
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed empty supervisor-intent: %v", err)
	}

	f := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-beta`},
		{Name: `\mcp-local-hub-demo-alpha-beta`},
	}}
	origStop := stopSchedulerFactory
	stopSchedulerFactory = func() (scheduler.Scheduler, error) { return f, nil }
	t.Cleanup(func() { stopSchedulerFactory = origStop })

	var killedPorts []int
	origKill := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error {
		killedPorts = append(killedPorts, port)
		return nil
	}
	t.Cleanup(func() { killByPortFn = origKill })

	results, err := NewAPI().Stop("demo", "beta")
	if err != nil {
		t.Fatalf("Stop(demo, beta): %v", err)
	}

	for _, p := range killedPorts {
		if p == 9402 {
			t.Fatalf("Stop(demo,beta) killed sibling demo-alpha's beta port 9402; killed=%v (FIX 3 falsified)", killedPorts)
		}
	}
	if len(killedPorts) != 1 || killedPorts[0] != 9302 {
		t.Fatalf("Stop(demo,beta) killed ports %v, want only demo's beta [9302]", killedPorts)
	}
	// The sibling task name must never appear in a result row.
	for _, r := range results {
		if strings.TrimPrefix(r.TaskName, "\\") == "mcp-local-hub-demo-alpha-beta" {
			t.Fatalf("Stop(demo,beta) acted on sibling task: %+v", results)
		}
	}
}

// FIX 3 (Restart) — `restart --server demo --daemon beta` must kill ONLY demo's
// beta, never sibling demo-alpha-beta.
//
// The Restart legacy loop calls killDaemonByPort directly (not killByPortFn);
// killDaemonByPort calls lookupProcess(port). We record every port lookupProcess
// is asked about and assert 9402 is never queried, and that sch.Run/sch.Stop
// never touch the sibling task name.
func TestRestart_ServerDaemon_DoesNotKillSiblingPort(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	seedManifestsWithDaemons(t, map[string]map[string]int{
		"demo":       {"beta": 9302},
		"demo-alpha": {"beta": 9402},
	})
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed empty supervisor-intent: %v", err)
	}

	f := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-beta`},
		{Name: `\mcp-local-hub-demo-alpha-beta`},
	}}
	origRestart := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return f, nil }
	t.Cleanup(func() { restartSchedulerFactory = origRestart })

	var queriedPorts []int
	origLookup := lookupProcess
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		queriedPorts = append(queriedPorts, port)
		return 0, 0, 0, false // no listener → killDaemonByPort is a no-op
	}
	t.Cleanup(func() { lookupProcess = origLookup })

	if _, err := NewAPI().Restart("demo", "beta"); err != nil {
		t.Fatalf("Restart(demo, beta): %v", err)
	}

	for _, p := range queriedPorts {
		if p == 9402 {
			t.Fatalf("Restart(demo,beta) probed sibling demo-alpha's beta port 9402; probed=%v (FIX 3 falsified)", queriedPorts)
		}
	}
	if sliceContainsBare(f.runNames, "mcp-local-hub-demo-alpha-beta") || sliceContainsBare(f.stopNames, "mcp-local-hub-demo-alpha-beta") {
		t.Fatalf("Restart(demo,beta) ran/stopped sibling task; run=%v stop=%v", f.runNames, f.stopNames)
	}
	// demo's own beta was restarted.
	if !sliceContainsBare(f.runNames, "mcp-local-hub-demo-beta") {
		t.Fatalf("Restart(demo,beta) did not restart demo's own beta; run=%v", f.runNames)
	}
}

// FIX 4 (end-to-end) — `restart --server demo` (no daemon filter) must not kill
// any sibling demo-alpha-beta port.
//
// listTasksForServer now returns only demo-owned tasks, AND the empty-filter
// per-daemon gate requires parsedServer==demo, so the sibling is excluded twice
// over. Pre-fix, the over-captured sibling was processed with empty filter and
// killDaemonByPort hit its 9402 port.
func TestRestart_Server_DoesNotKillSiblingDaemon(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	seedManifestsWithDaemons(t, map[string]map[string]int{
		"demo":       {"alpha": 9301},
		"demo-alpha": {"beta": 9402},
	})
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed empty supervisor-intent: %v", err)
	}

	f := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: `\mcp-local-hub-demo-alpha`},
		{Name: `\mcp-local-hub-demo-alpha-beta`}, // sibling, real port 9402
	}}
	origRestart := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return f, nil }
	t.Cleanup(func() { restartSchedulerFactory = origRestart })

	var queriedPorts []int
	origLookup := lookupProcess
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		queriedPorts = append(queriedPorts, port)
		return 0, 0, 0, false
	}
	t.Cleanup(func() { lookupProcess = origLookup })

	if _, err := NewAPI().Restart("demo", ""); err != nil {
		t.Fatalf("Restart(demo): %v", err)
	}

	for _, p := range queriedPorts {
		if p == 9402 {
			t.Fatalf("Restart(demo) probed sibling demo-alpha's port 9402; probed=%v (FIX 4 falsified)", queriedPorts)
		}
	}
	if sliceContainsBare(f.runNames, "mcp-local-hub-demo-alpha-beta") {
		t.Fatalf("Restart(demo) ran sibling task; run=%v", f.runNames)
	}
	if !sliceContainsBare(f.runNames, "mcp-local-hub-demo-alpha") {
		t.Fatalf("Restart(demo) did not restart demo's own task; run=%v", f.runNames)
	}
}

// FIX 3/FIX 4 non-regression — workspace lsp- proxy tasks survive
// `stop --server mcp-language-server`. The exact-identity gate must NOT drop the
// workspace lazy-proxy tasks (their names carry no server slug); the
// IsLazyProxyTaskName branch keeps the original suffix semantics.
//
// This guards against an over-broad FIX 3/FIX 4 that would regress workspace
// stop/restart by parsing the lsp task as a foreign server.
func TestStop_WorkspaceServer_KeepsLazyProxyTasks(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	// mcp-language-server is workspace-scoped; declare it so
	// serverIsWorkspaceScoped(mcp-language-server)==true and the lsp- branch in
	// listTasksForServer activates.
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	if err := os.MkdirAll(filepath.Join(dir, "mcp-language-server"), 0o700); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	// A workspace-scoped manifest must carry a port_pool + languages to parse
	// (config.ParseManifest rejects workspace-scoped without port_pool), so
	// serverIsWorkspaceScoped activates the lsp- branch in listTasksForServer.
	wsBody := "name: mcp-language-server\n" +
		"kind: workspace-scoped\n" +
		"transport: stdio-bridge\n" +
		"command: mcp-language-server\n" +
		"port_pool:\n  start: 9200\n  end: 9299\n" +
		"languages:\n" +
		"  - name: go\n    backend: gopls-mcp\n    transport: stdio\n    lsp_command: gopls\n    extra_flags: [\"mcp\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "mcp-language-server", "manifest.yaml"), []byte(wsBody), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed empty supervisor-intent: %v", err)
	}

	const lspTask = `\mcp-local-hub-lsp-deadbeef-go`
	f := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: lspTask},
	}}

	// Direct listTasksForServer assertion: the workspace proxy is surfaced.
	tasks, err := listTasksForServer(f, "mcp-language-server")
	if err != nil {
		t.Fatalf("listTasksForServer(mcp-language-server): %v", err)
	}
	if !sliceContainsBare(hyphenFamilyTaskNames(tasks), "mcp-local-hub-lsp-deadbeef-go") {
		t.Fatalf("workspace lsp proxy dropped by listTasksForServer; got=%v", hyphenFamilyTaskNames(tasks))
	}

	// Per-daemon gate: an empty daemon filter keeps the proxy; a matching
	// language-keyed filter keeps it; a non-matching filter drops it. Workspace
	// proxies are owned via the registry/workspace-path handling, so the
	// installed-server set is irrelevant to them — pass nil.
	if !taskMatchesServerDaemonGate("mcp-local-hub-lsp-deadbeef-go", "mcp-language-server", "", nil) {
		t.Fatalf("empty-filter gate dropped the workspace lsp proxy")
	}
	if !taskMatchesServerDaemonGate("mcp-local-hub-lsp-deadbeef-go", "mcp-language-server", "lsp-deadbeef-go", nil) {
		t.Fatalf("matching daemon-filter gate dropped the workspace lsp proxy")
	}
	if taskMatchesServerDaemonGate("mcp-local-hub-lsp-deadbeef-go", "mcp-language-server", "lsp-cafebabe-rust", nil) {
		t.Fatalf("non-matching daemon-filter gate kept the wrong workspace lsp proxy")
	}
}

// Unit-level exact-identity assertions for the shared gate + ownership helper,
// independent of scheduler wiring. These pin the precise semantics the bot's
// hyphen-family findings hinged on.
func TestTaskMatchesServerDaemonGate_ExactIdentity(t *testing.T) {
	// demo AND demo-alpha installed → the longest-installed-prefix rule used by
	// the empty-filter branch can defer demo-alpha-beta to demo-alpha.
	installed := map[string]struct{}{"demo": {}, "demo-alpha": {}}
	cases := []struct {
		name         string
		task         string
		server       string
		daemonFilter string
		want         bool
	}{
		{"demo/beta matches demo+beta", "mcp-local-hub-demo-beta", "demo", "beta", true},
		{"sibling demo-alpha/beta rejected for demo+beta", "mcp-local-hub-demo-alpha-beta", "demo", "beta", false},
		{"sibling demo-alpha/beta accepted for demo-alpha+beta", "mcp-local-hub-demo-alpha-beta", "demo-alpha", "beta", true},
		{"empty filter rejects sibling (longest-prefix owns it)", "mcp-local-hub-demo-alpha-beta", "demo", "", false},
		{"empty filter keeps own task", "mcp-local-hub-demo-beta", "demo", "", true},
		// Hyphenated daemon: exact name reconstruction makes this unambiguous
		// despite parseTaskName's greedy LastIndex('-') split.
		{"hyphenated daemon vscode-css under demo", "mcp-local-hub-demo-vscode-css", "demo", "vscode-css", true},
		{"same task also matches the demo-vscode/css decomposition", "mcp-local-hub-demo-vscode-css", "demo-vscode", "css", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskMatchesServerDaemonGate(tc.task, tc.server, tc.daemonFilter, installed); got != tc.want {
				t.Fatalf("taskMatchesServerDaemonGate(%q,%q,%q)=%v, want %v", tc.task, tc.server, tc.daemonFilter, got, tc.want)
			}
		})
	}
}

func TestTaskOwnedByServerExactOrLongestPrefix(t *testing.T) {
	installed := map[string]struct{}{"demo": {}, "demo-alpha": {}}
	cases := []struct {
		name   string
		task   string
		server string
		want   bool
	}{
		{"exact demo/alpha owned by demo", `\mcp-local-hub-demo-alpha`, "demo", true},
		{"sibling demo-alpha-beta not owned by demo", `\mcp-local-hub-demo-alpha-beta`, "demo", false},
		{"sibling demo-alpha-beta owned by demo-alpha (exact parse)", `\mcp-local-hub-demo-alpha-beta`, "demo-alpha", true},
		{"lsp proxy owned by no global server", `\mcp-local-hub-lsp-deadbeef-go`, "demo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskOwnedByServerExactOrLongestPrefix(tc.task, tc.server, installed); got != tc.want {
				t.Fatalf("taskOwnedByServerExactOrLongestPrefix(%q,%q)=%v, want %v", tc.task, tc.server, got, tc.want)
			}
		})
	}

	// Empty installed set → safe full-cleanup fallback: a blank/legacy
	// prefix-matching task IS claimed (no sibling proof to defer to).
	empty := map[string]struct{}{}
	if !taskOwnedByServerExactOrLongestPrefix(`\mcp-local-hub-demo-alpha-beta`, "demo", empty) {
		t.Fatalf("empty installed set should claim demo-alpha-beta for demo (full-cleanup fallback)")
	}
}
