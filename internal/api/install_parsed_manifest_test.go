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
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// fakeScheduler (register_test.go) implements the 5 install-relevant
// scheduler methods but not the full scheduler.Scheduler interface. The
// install pipeline routes through newScheduler() (the schedulerFactoryFn
// seam), which is typed scheduler.Scheduler, so the fake must satisfy the
// whole interface. Stop / Status / List are unused by executeInstallTo;
// model them as inert here so accidental use is loud rather than silently
// wrong.
func (f *fakeScheduler) Stop(string) error { return nil }
func (f *fakeScheduler) Status(name string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{Name: name}, nil
}

// List returns every seeded task whose Name carries `prefix` (the same
// HasPrefix shape the real scheduler.List uses). f.listSeed is empty by
// default, so existing callers that never seed it observe the prior
// nil-result behavior. The reconcile-prune path (pruneObsoleteServerTasks)
// drives this so FIX 2's prune-skip is observable: a seeded
// mcp-local-hub-serena-<ws> task must (or must NOT) appear in f.tasks after
// the install depending on whether the prune ran.
func (f *fakeScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	var out []scheduler.TaskStatus
	for _, t := range f.listSeed {
		if strings.HasPrefix(strings.TrimPrefix(t.Name, "\\"), strings.TrimPrefix(prefix, "\\")) {
			out = append(out, t)
		}
	}
	return out, nil
}

// installFakeScheduler routes newScheduler() (the executeInstallTo /
// installPlan factory seam) at the supplied fake for the duration of t.
func installFakeScheduler(t *testing.T, f *fakeScheduler) {
	t.Helper()
	orig := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return f, nil }
	t.Cleanup(func() { schedulerFactoryFn = orig })
}

func newInstallFakeScheduler() *fakeScheduler {
	return &fakeScheduler{tasks: map[string]bool{}, xml: map[string][]byte{}}
}

// globalTwoDaemonManifest is a global manifest with two logon-triggered
// daemons and one weekly-refresh task (weekly is NOT logon-triggered).
// Used to assert Pass A vs Pass B separation: Pass A creates 3 tasks,
// Pass B starts only the 2 logon-triggered ones.
func globalTwoDaemonManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:          "demo",
		Kind:          config.KindGlobal,
		Transport:     config.TransportNativeHTTP,
		Command:       "go", // on PATH whenever `go test` runs; InstallParsedManifest now runs Preflight (LookPath m.Command)
		Daemons:       []config.DaemonSpec{{Name: "alpha", Port: 9211}, {Name: "beta", Port: 9212}},
		WeeklyRefresh: true,
	}
}

type fakeInstallAutostartBackend struct {
	statusReturn autostart.State
	statusErr    error
	enableErr    error
	enableCalls  int
	enableOpts   []autostart.Options
}

func (f *fakeInstallAutostartBackend) Enable(opts autostart.Options) error {
	f.enableCalls++
	f.enableOpts = append(f.enableOpts, opts)
	return f.enableErr
}

func (f *fakeInstallAutostartBackend) Disable() error { return nil }

func (f *fakeInstallAutostartBackend) Status(opts autostart.Options) (autostart.State, error) {
	return f.statusReturn, f.statusErr
}

func installFakeAutostartBackend(t *testing.T, fb *fakeInstallAutostartBackend) {
	t.Helper()
	orig := installAutostartBackendFactoryFn
	installAutostartBackendFactoryFn = func() (autostart.Backend, error) { return fb, nil }
	t.Cleanup(func() { installAutostartBackendFactoryFn = orig })
}

// TestExecuteInstallTo_PassAPassB_Separation asserts the two-pass
// restructure: Pass A creates every scheduler task (no Run); Pass B is
// gated by startTasks. With startTasks=false, Run is never called.
func TestExecuteInstallTo_PassAPassB_Separation(t *testing.T) {
	t.Run("startTasks_false_skips_pass_b", func(t *testing.T) {
		preparePreflightBinaryChecks(t)
		f := newInstallFakeScheduler()
		installFakeScheduler(t, f)

		m := globalTwoDaemonManifest()
		plan, err := BuildPlan(m, "")
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		// 2 daemon tasks + 1 weekly = 3 created tasks.
		if got := len(plan.SchedulerTasks); got != 3 {
			t.Fatalf("plan SchedulerTasks = %d, want 3", got)
		}

		var buf bytes.Buffer
		if err := executeInstallTo(&buf, m, plan, 0, false, nil, false, false); err != nil {
			t.Fatalf("executeInstallTo(startTasks=false): %v", err)
		}
		if f.createCount != 3 {
			t.Errorf("Create calls = %d, want 3 (Pass A must create every task)", f.createCount)
		}
		if f.runCount != 0 {
			t.Errorf("Run calls = %d, want 0 (Pass B gated off by startTasks=false)", f.runCount)
		}
	})

	t.Run("startTasks_true_runs_only_logon_tasks", func(t *testing.T) {
		preparePreflightBinaryChecks(t)
		f := newInstallFakeScheduler()
		installFakeScheduler(t, f)

		m := globalTwoDaemonManifest()
		plan, err := BuildPlan(m, "")
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}

		var buf bytes.Buffer
		if err := executeInstallTo(&buf, m, plan, 0, true, nil, false, false); err != nil {
			t.Fatalf("executeInstallTo(startTasks=true): %v", err)
		}
		if f.createCount != 3 {
			t.Errorf("Create calls = %d, want 3", f.createCount)
		}
		// Only the 2 logon-triggered daemon tasks are Run; the weekly
		// refresh task (Trigger != "At logon") is skipped.
		if f.runCount != 2 {
			t.Errorf("Run calls = %d, want 2 (only logon-triggered tasks started)", f.runCount)
		}
		for _, n := range f.runNames {
			if strings.Contains(n, "weekly-refresh") {
				t.Errorf("weekly-refresh task %q must NOT be started by Pass B", n)
			}
		}
	})

	t.Run("pass_b_run_failure_does_not_roll_back", func(t *testing.T) {
		preparePreflightBinaryChecks(t)
		f := newInstallFakeScheduler()
		// Induce a Run failure on the first logon-triggered daemon task.
		f.failRunForTask = "mcp-local-hub-demo-alpha"
		installFakeScheduler(t, f)

		m := globalTwoDaemonManifest()
		plan, err := BuildPlan(m, "")
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}

		var buf bytes.Buffer
		// A Pass B Run failure must NOT abort the install — existing
		// warning-only contract. executeInstallTo returns nil.
		if err := executeInstallTo(&buf, m, plan, 0, true, nil, false, false); err != nil {
			t.Fatalf("executeInstallTo: Run failure must be warning-only, got error: %v", err)
		}
		// Pass A creates survive: all 3 tasks still present in the fake.
		if len(f.tasks) != 3 {
			t.Errorf("created tasks after Run failure = %d, want 3 (no rollback on Run failure)", len(f.tasks))
		}
		// The warning is printed to the writer.
		if !strings.Contains(buf.String(), "failed to start") {
			t.Errorf("expected a warning about the failed Run in output, got:\n%s", buf.String())
		}
	})
}

// serenaTemplateManifest is a workspace-scoped dynamic-pool manifest
// (non-nil DaemonTemplate, empty Daemons). BuildPlanWithOpts produces no
// scheduler tasks and no client updates for it — so InstallParsedManifest
// exercises the intent-write + pass gating without scheduler/client
// side-effects, which is exactly the install-foundation surface under test.
func serenaTemplateManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "go", // on PATH whenever `go test` runs; InstallParsedManifest now runs Preflight (LookPath m.Command)
		BaseArgs:  []string{"--from", "git+https://example/serena", "serena"},
		DaemonTemplate: &config.DaemonTemplate{
			Context:           "ide-assistant",
			PortPool:          &config.PortPool{Start: 9400, End: 9499},
			ExtraArgsTemplate: []string{"--project", "${workspace.path}"},
		},
	}
}

// TestInstallParsedManifest_RejectsGlobalManifest is the FIX-A guard: the
// seam is workspace-scoped-only, so a kind: global manifest is rejected up
// front (it belongs to (*API).Install). The reject MUST happen before any
// mutation — no supervisor-intent.json written and no scheduler Create/Run.
func TestInstallParsedManifest_RejectsGlobalManifest(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest() // Kind: KindGlobal
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer: &buf,
	})
	if err == nil {
		t.Fatalf("InstallParsedManifest(global): want error, got nil (intentPath=%q)", intentPath)
	}
	if !strings.Contains(err.Error(), "workspace-scoped") || !strings.Contains(err.Error(), m.Name) {
		t.Errorf("error = %q, want it to name the workspace-scoped-only contract and the manifest %q", err.Error(), m.Name)
	}
	// No mutation: no committed intent, no scheduler create/run.
	committed := filepath.Join(stateDir, "supervisor-intent.json")
	if _, statErr := os.Stat(committed); !os.IsNotExist(statErr) {
		t.Errorf("supervisor-intent.json must not exist after a global-manifest reject; stat err = %v", statErr)
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("no scheduler mutation allowed on reject: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_RejectsNilManifest — the contract guard runs before
// Preflight (which would nil-deref m), so a nil manifest fails fast with no
// mutation.
func TestInstallParsedManifest_RejectsNilManifest(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	a := NewAPI()
	var buf bytes.Buffer
	if _, err := a.InstallParsedManifest(context.Background(), nil, InstallParsedManifestOpts{Writer: &buf}); err == nil {
		t.Fatal("InstallParsedManifest(nil): want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written on nil-manifest reject; stat err = %v", statErr)
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("no scheduler mutation on reject: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_RejectsWorkspaceScopedWithoutTemplate — a
// workspace-scoped manifest WITHOUT a daemon_template is a legacy (e.g. LSP)
// shape that belongs to (*API).Install, not this dynamic-pool seam. Rejected up
// front with no mutation.
func TestInstallParsedManifest_RejectsWorkspaceScopedWithoutTemplate(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	m.DaemonTemplate = nil // workspace-scoped but template-less

	a := NewAPI()
	var buf bytes.Buffer
	_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{Writer: &buf, Workspaces: []WorkspaceEntry{}})
	if err == nil {
		t.Fatal("InstallParsedManifest(workspace-scoped, no daemon_template): want error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon_template") {
		t.Errorf("error = %q, want it to name the missing daemon_template", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written on template-less reject; stat err = %v", statErr)
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("no scheduler mutation on reject: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_RejectsNilWorkspacesNonDry — a non-dry install with
// a nil Workspaces snapshot is a forgotten-snapshot caller bug: it would fall
// through to the plan-derived (nil) daemon set and SILENTLY DROP this server's
// existing supervisor-intent daemon rows. The seam rejects nil up front (an
// intentional zero-workspace install passes an empty non-nil slice instead).
func TestInstallParsedManifest_RejectsNilWorkspacesNonDry(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	// Workspaces omitted → nil.
	_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{Writer: &buf})
	if err == nil {
		t.Fatal("InstallParsedManifest(non-dry, nil Workspaces): want error, got nil")
	}
	if !strings.Contains(err.Error(), "Workspaces") {
		t.Errorf("error = %q, want it to name the required Workspaces snapshot", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written on nil-Workspaces reject; stat err = %v", statErr)
	}
}

// TestInstallParsedManifest_EmptyWorkspacesValid — an intentional zero-workspace
// install passes an empty NON-NIL slice. It is accepted (distinct from the nil
// reject), writes an intent, clears this server's daemon rows, and preserves
// siblings' rows.
func TestInstallParsedManifest_EmptyWorkspacesValid(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: "\\mcp-local-hub-serena-stale", Server: "serena", Command: "mcphub.exe"},
			{TaskName: "\\mcp-local-hub-othersrv-d", Server: "othersrv", Command: "mcphub.exe"},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	m := serenaTemplateManifest() // Name "serena"
	a := NewAPI()
	var buf bytes.Buffer
	intentOut, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: []WorkspaceEntry{}, // intentional zero-workspace install
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest(empty non-nil Workspaces): unexpected error: %v", err)
	}
	if intentOut == "" {
		t.Fatal("want a non-empty intent path on a successful zero-workspace install")
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	sawSibling := false
	for _, d := range written.Daemons {
		if d.Server == "serena" {
			t.Errorf("serena daemon row %q must be cleared on a zero-workspace install; rows: %+v", d.TaskName, written.Daemons)
		}
		if d.Server == "othersrv" {
			sawSibling = true
		}
	}
	if !sawSibling {
		t.Errorf("sibling daemon row must be preserved; rows: %+v", written.Daemons)
	}
}

// TestInstallParsedManifest_NoSchedulerOnNoWork — a workspace-scoped install
// has zero scheduler tasks, skips prune, and defers daemon starts, so it must
// NOT require a working scheduler. On Linux/macOS scheduler.New() returns "not
// implemented"; acquiring it unconditionally would break serena installs on
// those hosts before the supervisor-intent write. With an erroring scheduler
// factory (simulating the POSIX backend) the install must still succeed and
// write supervisor-intent.json.
func TestInstallParsedManifest_NoSchedulerOnNoWork(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	orig := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		return nil, errors.New("not implemented (simulated POSIX backend)")
	}
	t.Cleanup(func() { schedulerFactoryFn = orig })

	wsLive := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(wsLive),
		WorkspacePath: wsLive,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	})
	if err != nil {
		t.Fatalf("workspace-scoped install must not require a working scheduler, got: %v", err)
	}
	if _, statErr := os.Stat(intentPath); statErr != nil {
		t.Errorf("supervisor-intent.json not written at %s: %v", intentPath, statErr)
	}
}

// TestInstallParsedManifest_DefersDaemonSpawn asserts the structural
// deferred-start contract: a workspace-scoped install creates no scheduler
// Run (daemon spawn is deferred to the supervisor reconciler). The seam has
// no StartAfterWrite knob — it always passes StartTasks=false — so Pass B
// never runs regardless of manifest shape. Uses a fan-out manifest so there
// ARE daemon rows materialized while sch.Run stays at zero.
func TestInstallParsedManifest_DefersDaemonSpawn(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	wsLive := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(wsLive),
		WorkspacePath: wsLive,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}
	if f.runCount != 0 {
		t.Errorf("Run calls = %d, want 0 (daemon spawn deferred to the supervisor reconciler)", f.runCount)
	}
	if _, statErr := os.Stat(intentPath); statErr != nil {
		t.Errorf("supervisor-intent.json not written at %s: %v", intentPath, statErr)
	}
}

// TestInstallParsedManifest_WorkspaceScoped_NotRefused confirms the
// exported seam bypasses refuseWorkspaceScopedInstall (workspace-scoped is
// its intended use). A serena dynamic-pool manifest installs cleanly with
// no scheduler tasks and the intent file written.
func TestInstallParsedManifest_WorkspaceScoped_NotRefused(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: []WorkspaceEntry{}, // intentional zero-workspace install (empty non-nil; nil would be a forgotten-snapshot reject)
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest(workspace-scoped): want success, got %v", err)
	}
	if f.createCount != 0 {
		t.Errorf("Create calls = %d, want 0 (template manifest has no static daemons)", f.createCount)
	}
	if filepath.Base(intentPath) != "supervisor-intent.json" {
		t.Errorf("intent path basename = %q, want supervisor-intent.json", filepath.Base(intentPath))
	}
}

// TestInstallParsedManifest_FansOutPerWorkspaceDaemons is the D.3b-1 wiring
// guard: when the manifest is workspace-scoped (DaemonTemplate != nil) AND
// opts.Workspaces is non-empty, InstallParsedManifest must call the D.2
// BuildSupervisorDaemonsForSerena fan-out and write one supervisor-intent
// daemon per registered serena workspace, MERGED into the existing intent so
// a pre-existing sibling-server row survives.
func TestInstallParsedManifest_FansOutPerWorkspaceDaemons(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	// Seed a pre-existing supervisor-intent.json that owns a sibling-server
	// daemon (server "memory"). The serena install must NOT clobber it.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	siblingTask := `\mcp-local-hub-memory-default`
	seed := &SupervisorIntentFile{
		Version:   1,
		UpdatedAt: "2026-05-20T00:00:00Z",
		Daemons: []SupervisorDaemon{{
			TaskName: siblingTask,
			Server:   "memory",
			Daemon:   "default",
			Command:  "mcphub.exe",
			Args:     []string{"daemon", "--server", "memory", "--daemon", "default"},
			Port:     9128,
		}},
		StrictMode: false,
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	// Two registered serena workspaces, each with a distinct path + an
	// allocated serena port. Language MUST be the serena sentinel — the
	// fan-out helper filters on it. Paths must EXIST on disk: FIX 3 drops
	// workspace rows whose path is absent before the fan-out, so use real
	// temp dirs rather than literal placeholders.
	wsAlpha := t.TempDir()
	wsBeta := t.TempDir()
	workspaces := []WorkspaceEntry{
		{
			WorkspaceKey:  WorkspaceKey(wsAlpha),
			WorkspacePath: wsAlpha,
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9401,
		},
		{
			WorkspaceKey:  WorkspaceKey(wsBeta),
			WorkspacePath: wsBeta,
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9402,
		},
	}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	gotPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest(workspaces): %v", err)
	}
	if gotPath != intentPath {
		t.Fatalf("intent path = %q, want %q", gotPath, intentPath)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}

	// Partition the written daemons into serena rows and the sibling row.
	var serenaRows []SupervisorDaemon
	var siblingFound bool
	for _, d := range written.Daemons {
		switch d.Server {
		case "serena":
			serenaRows = append(serenaRows, d)
		case "memory":
			siblingFound = true
			if d.TaskName != siblingTask {
				t.Errorf("sibling task name = %q, want %q", d.TaskName, siblingTask)
			}
			if d.Port != 9128 {
				t.Errorf("sibling port = %d, want 9128 (must be preserved verbatim)", d.Port)
			}
		default:
			t.Errorf("unexpected server in written intent: %q", d.Server)
		}
	}

	// Sibling-server row PRESERVED.
	if !siblingFound {
		t.Fatalf("sibling-server daemon %q was clobbered; written daemons: %+v", siblingTask, written.Daemons)
	}

	// Exactly two serena daemon rows (one per workspace).
	if len(serenaRows) != 2 {
		t.Fatalf("serena daemon rows = %d, want 2; written daemons: %+v", len(serenaRows), written.Daemons)
	}

	// Each serena row carries the correct workspace path, allocated port,
	// and the task name SerenaTaskNameForWorkspace would produce.
	wantByTask := map[string]struct {
		path string
		port int
	}{
		SerenaTaskNameForWorkspace(wsAlpha): {path: wsAlpha, port: 9401},
		SerenaTaskNameForWorkspace(wsBeta):  {path: wsBeta, port: 9402},
	}
	seenTask := map[string]bool{}
	for _, d := range serenaRows {
		want, ok := wantByTask[d.TaskName]
		if !ok {
			t.Errorf("serena row has unexpected task name %q (want one of %v)", d.TaskName, wantByTask)
			continue
		}
		if seenTask[d.TaskName] {
			t.Errorf("duplicate serena task name %q", d.TaskName)
		}
		seenTask[d.TaskName] = true
		if d.Workspace != want.path {
			t.Errorf("task %q workspace = %q, want %q", d.TaskName, d.Workspace, want.path)
		}
		if d.Port != want.port {
			t.Errorf("task %q port = %d, want %d", d.TaskName, d.Port, want.port)
		}
		// End-to-end materializer-through-install check: each serena row
		// must carry a fully-materialized RuntimeSpec after a round-trip
		// through write + ReadSupervisorIntent (DisallowUnknownFields).
		// This proves the install fan-out writes spec-bearing descriptors
		// the proxy can consume (design claim #2 + #5).
		if d.RuntimeSpec == nil {
			t.Errorf("task %q must carry a materialized RuntimeSpec after install round-trip; got nil", d.TaskName)
			continue
		}
		if d.RuntimeSpec.ExternalPort != want.port {
			t.Errorf("task %q RuntimeSpec.ExternalPort = %d, want %d", d.TaskName, d.RuntimeSpec.ExternalPort, want.port)
		}
		if d.RuntimeSpec.UpstreamPort != want.port+config.NativeHTTPInternalPortOffset {
			t.Errorf("task %q RuntimeSpec.UpstreamPort = %d, want %d", d.TaskName, d.RuntimeSpec.UpstreamPort, want.port+config.NativeHTTPInternalPortOffset)
		}
		if d.RuntimeSpec.WorkspacePath != want.path {
			t.Errorf("task %q RuntimeSpec.WorkspacePath = %q, want %q", d.TaskName, d.RuntimeSpec.WorkspacePath, want.path)
		}
		// --context (appended) + --project (expanded) must be present in
		// the materialized child argv. serenaTemplateManifest uses
		// Context "ide-assistant".
		n := len(d.RuntimeSpec.ChildArgs)
		if n < 2 || d.RuntimeSpec.ChildArgs[n-2] != "--context" || d.RuntimeSpec.ChildArgs[n-1] != m.DaemonTemplate.Context {
			t.Errorf("task %q ChildArgs must END with --context %q; got %v", d.TaskName, m.DaemonTemplate.Context, d.RuntimeSpec.ChildArgs)
		}
	}
	if len(seenTask) != 2 {
		t.Errorf("matched serena task names = %d, want 2 (both workspaces)", len(seenTask))
	}

	// Deferred-start contract: no scheduler Run; the template manifest also
	// produces no scheduler Create tasks.
	if f.runCount != 0 {
		t.Errorf("Run calls = %d, want 0 (daemon spawn deferred to the supervisor reconciler)", f.runCount)
	}
}

// TestInstallParsedManifest_FanOut_AuditsWorkspaceTasks is the FIX-3 guard:
// for a workspace-scoped DaemonTemplate manifest, recordInstallAuditPreMutation
// derives its task list from m.Daemons, which is EMPTY — so the fan-out install
// would write per-workspace supervisor-intent daemon rows WITHOUT a fail-closed
// server-install audit entry. The fix threads the MATERIALIZED per-workspace
// task names into the pre-mutation audit. Two assertions:
//
//	(a) a fan-out install records a server-install audit entry per
//	    per-workspace task (the SerenaTaskNameForWorkspace names);
//	(b) injecting an audit-write failure ABORTS the install BEFORE any
//	    supervisor-intent mutation — no daemon rows committed.
func TestInstallParsedManifest_FanOut_AuditsWorkspaceTasks(t *testing.T) {
	t.Run("records_server_install_audit_per_workspace_task", func(t *testing.T) {
		daemonIntentTestHelper(t)
		preparePreflightBinaryChecks(t)
		f := newInstallFakeScheduler()
		installFakeScheduler(t, f)
		r := &recordingAuditWriter{}
		installRecordingAudit(t, r)

		wsAlpha := t.TempDir()
		wsBeta := t.TempDir()
		workspaces := []WorkspaceEntry{
			{WorkspaceKey: WorkspaceKey(wsAlpha), WorkspacePath: wsAlpha, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9401},
			{WorkspaceKey: WorkspaceKey(wsBeta), WorkspacePath: wsBeta, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9402},
		}

		m := serenaTemplateManifest()
		a := NewAPI()
		var buf bytes.Buffer
		if _, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
			Writer:     &buf,
			Workspaces: workspaces,
		}); err != nil {
			t.Fatalf("InstallParsedManifest(fan-out): %v", err)
		}

		// Every per-workspace task name must have a server-install audit entry.
		wantTasks := map[string]bool{
			SerenaTaskNameForWorkspace(wsAlpha): false,
			SerenaTaskNameForWorkspace(wsBeta):  false,
		}
		for _, e := range r.entries {
			if e.Action != AuditActionServerInstall {
				continue
			}
			if _, ok := wantTasks[e.Task]; ok {
				wantTasks[e.Task] = true
			}
		}
		for task, seen := range wantTasks {
			if !seen {
				t.Errorf("missing server-install audit entry for per-workspace task %q; recorded: %+v", task, r.entries)
			}
		}
	})

	t.Run("audit_failure_aborts_before_intent_write", func(t *testing.T) {
		stateDir := daemonIntentTestHelper(t)
		preparePreflightBinaryChecks(t)
		f := newInstallFakeScheduler()
		installFakeScheduler(t, f)
		// Fail every server-install audit append → the pre-mutation audit must
		// fail-close and abort the install before any intent mutation.
		r := &recordingAuditWriter{failActions: map[string]error{
			AuditActionServerInstall: errors.New("induced audit failure"),
		}}
		installRecordingAudit(t, r)

		wsAlpha := t.TempDir()
		workspaces := []WorkspaceEntry{
			{WorkspaceKey: WorkspaceKey(wsAlpha), WorkspacePath: wsAlpha, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9401},
		}

		m := serenaTemplateManifest()
		a := NewAPI()
		var buf bytes.Buffer
		_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
			Writer:     &buf,
			Workspaces: workspaces,
		})
		if err == nil {
			t.Fatalf("InstallParsedManifest must fail when the pre-mutation audit fails; got nil error")
		}
		// No supervisor-intent.json may exist — the abort happened before the
		// intent write (no daemon rows committed). The intent was never
		// pre-existing in this test, so absence proves no mutation occurred.
		committed := filepath.Join(stateDir, "supervisor-intent.json")
		if _, statErr := os.Stat(committed); !os.IsNotExist(statErr) {
			t.Errorf("supervisor-intent.json must not exist after an audit-failure abort; stat err = %v", statErr)
		}
	})
}

// TestInstallParsedManifest_DryRun_NoWriteNoPath is the FIX-1 regression
// guard: a dry run must (a) leave the state dir pristine — no committed
// supervisor-intent.json AND no leftover ".preflight" temp from a
// pre-flight probe that should be skipped on dry-run — and (b) return an
// empty intent path so the caller never dereferences a path for a file
// that was never written. Uses a workspace-scoped manifest (the only kind
// this seam accepts after FIX A); the fake scheduler must see zero Create /
// Run calls because installPlan short-circuits to the plan print before any
// mutation.
func TestInstallParsedManifest_DryRun_NoWriteNoPath(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer: &buf,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest(DryRun): %v", err)
	}
	// (b) empty path — nothing was written.
	if intentPath != "" {
		t.Errorf("DryRun intentPath = %q, want \"\" (nothing was committed)", intentPath)
	}
	// (a) no committed intent file in the state dir.
	committed := filepath.Join(stateDir, "supervisor-intent.json")
	if _, statErr := os.Stat(committed); !os.IsNotExist(statErr) {
		t.Errorf("supervisor-intent.json must not exist after a dry run; stat err = %v", statErr)
	}
	// (a) no leftover pre-flight temp probe — the pre-flight write is
	// skipped entirely on dry-run, so neither the temp file nor its flock
	// leaf may appear.
	preflight := filepath.Join(stateDir, "supervisor-intent.json.preflight")
	if _, statErr := os.Stat(preflight); !os.IsNotExist(statErr) {
		t.Errorf(".preflight temp must not exist after a dry run (pre-flight write is skipped); stat err = %v", statErr)
	}
	if _, statErr := os.Stat(preflight + ".lock"); !os.IsNotExist(statErr) {
		t.Errorf(".preflight.lock must not exist after a dry run; stat err = %v", statErr)
	}
	// (a) FIX 1 tightening: a dry run must NOT acquire (and therefore must
	// not create) the supervisor-intent flock leaf. Acquiring the flock is a
	// disk side effect that defeats the "dry run touches nothing" contract.
	lockLeaf := filepath.Join(stateDir, "supervisor-intent.json.lock")
	if _, statErr := os.Stat(lockLeaf); !os.IsNotExist(statErr) {
		t.Errorf("supervisor-intent.json.lock must not exist after a dry run (the intent flock must not be acquired); stat err = %v", statErr)
	}
	// The dry-run plan print still happens.
	if buf.Len() == 0 {
		t.Error("DryRun must still print the plan to the writer, got empty output")
	}
	// No scheduler mutation on dry-run.
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("DryRun must not mutate the scheduler: createCount=%d runCount=%d, want 0/0", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_DryRun_CorruptExistingIntentStillSucceeds is the
// other half of the FIX-1 guard: a dry run must build + print the plan WITHOUT
// reading or parsing the existing supervisor-intent.json. A deliberately
// corrupt (unparseable) intent file on disk must therefore NOT fail a dry run
// — dry-run only prints the plan and makes zero disk changes, so the intent
// read/merge must be gated behind !DryRun. Before the fix, buildMergedSupervisorIntent
// (which calls ReadSupervisorIntent → JSON parse) ran ahead of the dry-run
// short-circuit and a corrupt file aborted the dry run.
func TestInstallParsedManifest_DryRun_CorruptExistingIntentStillSucceeds(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	// Plant a corrupt supervisor-intent.json that ReadSupervisorIntent
	// cannot parse. A dry run must not even open it.
	committed := filepath.Join(stateDir, "supervisor-intent.json")
	if err := os.WriteFile(committed, []byte("{ this is not valid json"), 0o600); err != nil {
		t.Fatalf("plant corrupt intent: %v", err)
	}
	corruptBefore, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("read planted corrupt intent: %v", err)
	}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer: &buf,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest(DryRun) over a corrupt existing intent must succeed (intent must not be read on dry-run), got: %v", err)
	}
	if intentPath != "" {
		t.Errorf("DryRun intentPath = %q, want \"\"", intentPath)
	}
	// The corrupt file must be byte-for-byte untouched — dry-run neither
	// read it for a merge nor rewrote it.
	corruptAfter, err := os.ReadFile(committed)
	if err != nil {
		t.Fatalf("re-read corrupt intent after dry run: %v", err)
	}
	if !bytes.Equal(corruptBefore, corruptAfter) {
		t.Errorf("corrupt intent file was mutated by a dry run: before=%q after=%q", corruptBefore, corruptAfter)
	}
	// The plan is still printed.
	if buf.Len() == 0 {
		t.Error("DryRun must still print the plan even with a corrupt existing intent")
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("DryRun must not mutate the scheduler: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_PreservesPriorMaintenanceTimers is the FIX-B
// guard: the seam carries the prior intent's MaintenanceTimers through
// VERBATIM — this server's AND every sibling's — and never materializes,
// drops, or re-derives a per-server weekly timer of its own. Two prior
// timers are seeded:
//
//   - one for the TARGET server ("serena") with Enabled pointing at false
//     (an operator's deliberate off-switch). The pre-FIX-B merge dropped
//     every prior timer whose Server matched m.Name and re-derived from the
//     manifest, so this disabled timer would have been silently dropped (and,
//     because serena has weekly_refresh=false, NOT re-added). FIX B carries it
//     through untouched.
//   - one for a SIBLING server ("othersrv").
//
// After installing the target workspace-scoped manifest, BOTH timers must be
// present and byte-identical to what was seeded (including the false-valued
// *bool Enabled).
func TestInstallParsedManifest_PreservesPriorMaintenanceTimers(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	disabled := false
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	targetTimer := MaintenanceTimer{
		Name:    "\\mcp-local-hub-serena-weekly-refresh",
		Kind:    "server-weekly-refresh",
		Server:  "serena", // same server the install targets
		Command: "mcphub.exe",
		Args:    []string{"restart", "--server", "serena"},
		Enabled: &disabled, // operator-disabled; must survive verbatim
	}
	siblingTimer := MaintenanceTimer{
		Name:    "\\mcp-local-hub-othersrv-weekly-refresh",
		Kind:    "server-weekly-refresh",
		Server:  "othersrv",
		Command: "mcphub.exe",
		Args:    []string{"restart", "--server", "othersrv"},
	}
	seed := &SupervisorIntentFile{
		Version:           1,
		UpdatedAt:         "2026-05-20T00:00:00Z",
		MaintenanceTimers: []MaintenanceTimer{targetTimer, siblingTimer},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// Install the target workspace-scoped manifest WITH a live workspace so
	// the fan-out runs (and the daemon-row merge path replaces serena's daemon
	// rows) — the timers must still pass through untouched.
	wsLive := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(wsLive),
		WorkspacePath: wsLive,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}

	m := serenaTemplateManifest() // Name "serena"
	a := NewAPI()
	var buf bytes.Buffer
	if _, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	}); err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}

	byName := map[string]MaintenanceTimer{}
	for _, tm := range written.MaintenanceTimers {
		byName[tm.Name] = tm
	}
	if len(written.MaintenanceTimers) != 2 {
		t.Fatalf("written timers = %d, want 2 (both prior timers preserved verbatim); timers: %+v", len(written.MaintenanceTimers), written.MaintenanceTimers)
	}

	gotTarget, ok := byName[targetTimer.Name]
	if !ok {
		t.Fatalf("target server's prior timer %q was dropped; timers: %+v", targetTimer.Name, written.MaintenanceTimers)
	}
	// reflect.DeepEqual follows the *bool, so a flipped/nil Enabled is caught.
	if !reflect.DeepEqual(gotTarget, targetTimer) {
		t.Errorf("target timer not preserved verbatim:\n got  %+v (Enabled=%v)\n want %+v (Enabled=%v)", gotTarget, derefBool(gotTarget.Enabled), targetTimer, derefBool(targetTimer.Enabled))
	}
	if gotTarget.Enabled == nil || *gotTarget.Enabled != false {
		t.Errorf("target timer Enabled = %v, want a non-nil pointer to false (operator off-switch must survive)", gotTarget.Enabled)
	}

	gotSibling, ok := byName[siblingTimer.Name]
	if !ok {
		t.Fatalf("sibling server's prior timer %q was dropped; timers: %+v", siblingTimer.Name, written.MaintenanceTimers)
	}
	if !reflect.DeepEqual(gotSibling, siblingTimer) {
		t.Errorf("sibling timer not preserved verbatim:\n got  %+v\n want %+v", gotSibling, siblingTimer)
	}
}

// TestMergeServerWeeklyRefreshTimer_FullInstallWeeklyFalseDropsOnlyThisServer
// covers the Phase-F lifecycle edge where a full reinstall flips
// weekly_refresh from true to false. The old scheduler path pruned this
// server's mcp-local-hub-<server>-weekly-refresh task; the supervisor-intent
// merge must now drop only this server's server-weekly-refresh timer while
// preserving sibling timers. Filtered installs still preserve timers verbatim
// because a single-daemon install does not own the whole-server timer.
//
// Negative-control: restore mergeServerWeeklyRefreshTimer's !wantTimer branch
// to `return prior` and the full-install assertion fails because demo's timer
// survives.
func TestMergeServerWeeklyRefreshTimer_FullInstallWeeklyFalseDropsOnlyThisServer(t *testing.T) {
	disabled := false
	targetTimer := MaintenanceTimer{
		Name:    `\mcp-local-hub-demo-weekly-refresh`,
		Kind:    "server-weekly-refresh",
		Server:  "demo",
		Command: "mcphub",
		Args:    []string{"restart", "--server", "demo"},
		Enabled: &disabled,
	}
	siblingTimer := MaintenanceTimer{
		Name:    `\mcp-local-hub-other-weekly-refresh`,
		Kind:    "server-weekly-refresh",
		Server:  "other",
		Command: "mcphub",
		Args:    []string{"restart", "--server", "other"},
	}
	prior := []MaintenanceTimer{targetTimer, siblingTimer}
	m := &config.ServerManifest{
		Name:          "demo",
		Kind:          config.KindGlobal,
		WeeklyRefresh: false,
	}

	full := mergeServerWeeklyRefreshTimer(m, "", prior)
	if len(full) != 1 {
		t.Fatalf("full install timers = %d, want 1 sibling only; timers=%+v", len(full), full)
	}
	if !reflect.DeepEqual(full[0], siblingTimer) {
		t.Fatalf("full install did not preserve sibling timer verbatim:\n got  %+v\n want %+v", full[0], siblingTimer)
	}

	filtered := mergeServerWeeklyRefreshTimer(m, "alpha", prior)
	if !reflect.DeepEqual(filtered, prior) {
		t.Fatalf("filtered install mutated timers; got %+v want prior %+v", filtered, prior)
	}
}

// TestMergeServerWeeklyRefreshTimer_BlankServerNameFallbackReplacesWithoutDuplicate
// covers the legacy timer shape whose Server field is blank but whose canonical
// Name belongs to this server. Reinstalling weekly_refresh:true must recognize
// and replace that prior timer rather than appending a duplicate.
//
// Negative-control: restore the replace match to `tm.Server == m.Name` and the
// blank-Server prior timer survives alongside the fresh timer, so the owned
// timer count becomes 2.
func TestMergeServerWeeklyRefreshTimer_BlankServerNameFallbackReplacesWithoutDuplicate(t *testing.T) {
	disabled := false
	prior := []MaintenanceTimer{
		{
			Name:    `\mcp-local-hub-demo-weekly-refresh`,
			Kind:    "server-weekly-refresh",
			Server:  "",
			Command: "old",
			Args:    []string{"old"},
			Enabled: &disabled,
		},
		{
			Name:    `\mcp-local-hub-other-weekly-refresh`,
			Kind:    "server-weekly-refresh",
			Server:  "other",
			Command: "other",
			Args:    []string{"restart", "--server", "other"},
		},
	}
	m := &config.ServerManifest{
		Name:          "demo",
		Kind:          config.KindGlobal,
		WeeklyRefresh: true,
	}

	got := mergeServerWeeklyRefreshTimer(m, "", prior)
	var owned []MaintenanceTimer
	var sawSibling bool
	for _, tm := range got {
		if tm.Name == `\mcp-local-hub-other-weekly-refresh` && tm.Server == "other" {
			sawSibling = true
			continue
		}
		if maintenanceTimerOwnedBy(tm, "demo") {
			owned = append(owned, tm)
		}
	}
	if len(owned) != 1 {
		t.Fatalf("owned demo timers = %d, want exactly 1 replacement; timers=%+v", len(owned), got)
	}
	if owned[0].Server != "demo" || owned[0].Command == "old" {
		t.Fatalf("demo timer was not replaced with a fresh canonical timer: %+v", owned[0])
	}
	if owned[0].Enabled == nil || *owned[0].Enabled != false {
		t.Fatalf("operator off-switch was not preserved from blank-Server prior timer: %+v", owned[0])
	}
	if !sawSibling {
		t.Fatalf("sibling timer was not preserved; timers=%+v", got)
	}
}

// derefBool renders a *bool for error messages without panicking on nil.
func derefBool(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	if *b {
		return "true"
	}
	return "false"
}

// serenaTemplateManifestNamed is serenaTemplateManifest with a caller-chosen
// server name, used by the FIX-1 concurrency test so two installs contribute
// distinct (per-server-named) supervisor-intent daemon rows.
func serenaTemplateManifestNamed(name string) *config.ServerManifest {
	m := serenaTemplateManifest()
	m.Name = name
	return m
}

// TestInstallParsedManifest_ConcurrentInstalls_PreserveSiblingRows is the
// FIX-1 guard: two InstallParsedManifest calls for DIFFERENT servers running
// concurrently must not lose each other's daemon rows. Without the
// flock-guarded read-merge-write critical section, each goroutine reads a
// stale supervisor-intent snapshot, merges in only ITS server's rows, and the
// later writer clobbers the earlier writer's sibling row. The shared
// daemonStateRootOverride + schedulerFactoryFn are process-global, so both
// goroutines hit the SAME state dir and the SAME fake scheduler — exactly the
// contended surface the race exploits. Both manifests are workspace-scoped
// (the only kind this seam accepts after FIX A) and fan out to a distinct
// live workspace, so each contributes a daemon row keyed by its own server
// name + workspace.
func TestInstallParsedManifest_ConcurrentInstalls_PreserveSiblingRows(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	mAlpha := serenaTemplateManifestNamed("serena-alpha")
	mBeta := serenaTemplateManifestNamed("serena-beta")
	wsAlpha := t.TempDir()
	wsBeta := t.TempDir()
	wsByServer := map[string][]WorkspaceEntry{
		"serena-alpha": {{WorkspaceKey: WorkspaceKey(wsAlpha), WorkspacePath: wsAlpha, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9411}},
		"serena-beta":  {{WorkspaceKey: WorkspaceKey(wsBeta), WorkspacePath: wsBeta, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9412}},
	}

	a := NewAPI()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, m := range []*config.ServerManifest{mAlpha, mBeta} {
		wg.Add(1)
		go func(idx int, mm *config.ServerManifest) {
			defer wg.Done()
			var buf bytes.Buffer
			_, err := a.InstallParsedManifest(context.Background(), mm, InstallParsedManifestOpts{
				Writer:     &buf,
				Workspaces: wsByServer[mm.Name],
			})
			errs[idx] = err
		}(i, m)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent InstallParsedManifest[%d]: %v", i, err)
		}
	}

	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	servers := map[string]bool{}
	for _, d := range written.Daemons {
		servers[d.Server] = true
	}
	// BOTH servers' rows must survive the interleaved read-merge-write.
	if !servers["serena-alpha"] {
		t.Errorf("serena-alpha daemon row lost (concurrent write clobbered it); written daemons: %+v", written.Daemons)
	}
	if !servers["serena-beta"] {
		t.Errorf("serena-beta daemon row lost (concurrent write clobbered it); written daemons: %+v", written.Daemons)
	}
}

// TestInstallParsedManifest_WorkspaceScoped_SkipsPrune is the FIX-2 guard: a
// workspace-scoped (DaemonTemplate) install produces ZERO SchedulerTasks, so
// the full-install reconcile would otherwise prune EVERY existing
// mcp-local-hub-serena-* scheduler task against an empty planned set. The
// prune must be skipped for DaemonTemplate manifests so a registered serena
// workspace task survives the intent write.
func TestInstallParsedManifest_WorkspaceScoped_SkipsPrune(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	// Seed an existing serena scheduler task for a registered workspace. If the
	// prune ran (the bug), executeInstallTo would Delete it because it is
	// absent from the empty planned set.
	wsExisting := t.TempDir()
	seededTaskBare := "mcp-local-hub-serena-" + WorkspaceKey(wsExisting)
	f.listSeed = []scheduler.TaskStatus{{Name: "\\" + seededTaskBare}}
	installFakeScheduler(t, f)

	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(wsExisting),
		WorkspacePath: wsExisting,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	if _, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	}); err != nil {
		t.Fatalf("InstallParsedManifest(workspace-scoped): %v", err)
	}

	// The seeded serena scheduler task must NOT have been deleted.
	for _, deleted := range f.deleteNames {
		if strings.TrimPrefix(deleted, "\\") == seededTaskBare {
			t.Errorf("seeded serena task %q was pruned; prune must be skipped for workspace-scoped installs (deleteNames=%v)", seededTaskBare, f.deleteNames)
		}
	}
	// And it still appears in a subsequent List (Delete removes from listSeed).
	remaining, _ := f.List("mcp-local-hub-serena-")
	var stillThere bool
	for _, t2 := range remaining {
		if strings.TrimPrefix(t2.Name, "\\") == seededTaskBare {
			stillThere = true
		}
	}
	if !stillThere {
		t.Errorf("seeded serena task %q no longer listed after install; it was destructively pruned", seededTaskBare)
	}
}

// TestInstallParsedManifest_FiltersStaleWorkspaceRows is the FIX-3 guard: a
// workspace row whose path no longer exists on disk must be dropped before the
// fan-out (so the supervisor never sets cmd.Dir to a dead path and spawn-loops)
// AND the drop must be operator-visible (a warn on the writer), never silent.
func TestInstallParsedManifest_FiltersStaleWorkspaceRows(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	wsLive := t.TempDir()                                   // exists
	wsStale := filepath.Join(t.TempDir(), "deleted-ws-dir") // absent (never created)

	workspaces := []WorkspaceEntry{
		{
			WorkspaceKey:  WorkspaceKey(wsLive),
			WorkspacePath: wsLive,
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9401,
		},
		{
			WorkspaceKey:  WorkspaceKey(wsStale),
			WorkspacePath: wsStale,
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9402,
		},
	}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	var serenaRows []SupervisorDaemon
	for _, d := range written.Daemons {
		if d.Server == "serena" {
			serenaRows = append(serenaRows, d)
		}
	}
	// Exactly one serena row — the live workspace; the stale one was dropped.
	if len(serenaRows) != 1 {
		t.Fatalf("serena daemon rows = %d, want 1 (stale-path row must be dropped); rows: %+v", len(serenaRows), serenaRows)
	}
	if serenaRows[0].Workspace != wsLive {
		t.Errorf("surviving serena row workspace = %q, want %q (the live path)", serenaRows[0].Workspace, wsLive)
	}
	// The stale-path task name must be absent entirely.
	staleTask := SerenaTaskNameForWorkspace(wsStale)
	for _, d := range serenaRows {
		if d.TaskName == staleTask {
			t.Errorf("stale workspace task %q must not appear in the written intent", staleTask)
		}
	}
	// The drop is operator-visible: a warn naming the stale path on the writer.
	// Match the warn keyword + the path leaf (filepath.Base has no separators,
	// so the assertion is robust to %q backslash-escaping in the message body).
	out := buf.String()
	if !strings.Contains(out, "stale workspace") || !strings.Contains(out, filepath.Base(wsStale)) {
		t.Errorf("expected an operator-visible warn naming the stale workspace %q, got output:\n%s", wsStale, out)
	}
}

// TestInstallParsedManifest_DryRunShowsWorkspaceFanOut is the FIX-C guard: a
// dry run of a DaemonTemplate workspace-scoped manifest with workspaces must
// preview the per-workspace supervisor-intent daemon rows the real path would
// write — naming each workspace's SerenaTaskNameForWorkspace task and marking
// rows whose path is stale (gone) — because the legacy plan print carries zero
// scheduler tasks for such a manifest and would otherwise report no planned
// changes. The preview must remain corrupt-safe: it touches NO disk (no
// supervisor-intent.json, no .lock, no supervisor-events.log).
func TestInstallParsedManifest_DryRunShowsWorkspaceFanOut(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	wsLive := t.TempDir()                                   // exists, serena sentinel → would write
	wsStale := filepath.Join(t.TempDir(), "deleted-ws-dir") // absent (never created) → stale skip
	wsNonSerena := t.TempDir()                              // exists but non-sentinel Language → skipped (mirrors BuildSupervisorDaemonsForSerena)
	workspaces := []WorkspaceEntry{
		{WorkspaceKey: WorkspaceKey(wsLive), WorkspacePath: wsLive, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9401},
		{WorkspaceKey: WorkspaceKey(wsStale), WorkspacePath: wsStale, Language: SerenaLanguageSentinel, Backend: "serena", Port: 9402},
		{WorkspaceKey: WorkspaceKey(wsNonSerena), WorkspacePath: wsNonSerena, Language: "python", Backend: "lsp", Port: 9403},
	}

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
		DryRun:     true,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest(DryRun, fan-out): %v", err)
	}
	if intentPath != "" {
		t.Errorf("DryRun intentPath = %q, want \"\" (nothing committed)", intentPath)
	}

	out := buf.String()
	// BOTH per-workspace task rows are named in the preview.
	liveTask := SerenaTaskNameForWorkspace(wsLive)
	staleTask := SerenaTaskNameForWorkspace(wsStale)
	if !strings.Contains(out, liveTask) {
		t.Errorf("dry-run preview missing live workspace task %q; output:\n%s", liveTask, out)
	}
	if !strings.Contains(out, staleTask) {
		t.Errorf("dry-run preview missing stale workspace task %q; output:\n%s", staleTask, out)
	}
	// The stale row is marked as stale.
	if !strings.Contains(out, "stale") {
		t.Errorf("dry-run preview must mark the stale workspace row as stale; output:\n%s", out)
	}
	// The non-sentinel row is named but labelled non-serena — the preview must
	// mirror BuildSupervisorDaemonsForSerena, which writes no daemon for it.
	nonSerenaTask := SerenaTaskNameForWorkspace(wsNonSerena)
	if !strings.Contains(out, nonSerenaTask) {
		t.Errorf("dry-run preview missing non-serena workspace task %q; output:\n%s", nonSerenaTask, out)
	}
	if !strings.Contains(out, "not a serena workspace") {
		t.Errorf("dry-run preview must mark the non-sentinel row as non-serena (real path skips it); output:\n%s", out)
	}

	// Corrupt-safe: the preview touches NO disk. The state dir must hold none
	// of the supervisor state artifacts after a dry run.
	for _, leaf := range []string{
		"supervisor-intent.json",
		"supervisor-intent.json.lock",
		"supervisor-intent.json.preflight",
		SupervisorEventLogFileLeaf,
	} {
		p := filepath.Join(stateDir, leaf)
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("dry run must not create %s; stat err = %v", leaf, statErr)
		}
	}
	// No scheduler mutation on dry-run.
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("DryRun must not mutate the scheduler: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_RejectsStdioBridgeDaemonTemplate is the
// install-time native-http gate (design §3.1). A stdio-bridge +
// daemon_template + kind:workspace-scoped manifest PASSES
// config.ServerManifest.Validate today (Validate rejects daemon_template
// only for kind!=workspace-scoped or transport=remote-http — see
// internal/config/manifest.go), so once the proxy's runtime transport check
// is removed, this contract gate is the ONLY enforcer. The reject MUST happen
// before any mutation: no supervisor-intent.json written, no scheduler
// Create/Run.
func TestInstallParsedManifest_RejectsStdioBridgeDaemonTemplate(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest()
	m.Transport = config.TransportStdioBridge // workspace-scoped + daemon_template, but NOT native-http

	a := NewAPI()
	var buf bytes.Buffer
	_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: []WorkspaceEntry{},
	})
	if err == nil {
		t.Fatal("InstallParsedManifest(stdio-bridge daemon_template): want error, got nil")
	}
	if !strings.Contains(err.Error(), "native-http") {
		t.Errorf("error = %q, want it to name the native-http transport gate", err.Error())
	}
	// No mutation: no committed intent, no scheduler create/run.
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
		t.Errorf("no intent must be written on stdio-bridge reject; stat err = %v", statErr)
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("no scheduler mutation on reject: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallParsedManifest_RejectsEmptyContextDaemonTemplate is the
// install-time empty-context gate (bot PR #246 P2). A workspace-scoped
// native-http daemon_template manifest whose DaemonTemplate.Context is empty
// PASSES config.ServerManifest.Validate today (Validate checks port_pool +
// extra_args_template, NOT Context), but the materializer would persist
// `--context ""` into every RuntimeSpec.ChildArgs → the supervisor respawns a
// serena child with an invalid empty context. The contract gate MUST reject it
// before any mutation: no supervisor-intent.json written, no scheduler
// Create/Run.
func TestInstallParsedManifest_RejectsEmptyContextDaemonTemplate(t *testing.T) {
	for _, ctx := range []string{"", "   "} {
		stateDir := daemonIntentTestHelper(t)
		preparePreflightBinaryChecks(t)
		f := newInstallFakeScheduler()
		installFakeScheduler(t, f)

		m := serenaTemplateManifest()
		m.DaemonTemplate.Context = ctx // workspace-scoped + native-http, but empty context

		a := NewAPI()
		var buf bytes.Buffer
		_, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
			Writer:     &buf,
			Workspaces: []WorkspaceEntry{},
		})
		if err == nil {
			t.Fatalf("InstallParsedManifest(empty context %q): want error, got nil", ctx)
		}
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("error = %q, want it to name the empty daemon_template.context", err.Error())
		}
		// No mutation: no committed intent, no scheduler create/run.
		if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); !os.IsNotExist(statErr) {
			t.Errorf("no intent must be written on empty-context reject; stat err = %v", statErr)
		}
		if f.createCount != 0 || f.runCount != 0 {
			t.Errorf("no scheduler mutation on reject: createCount=%d runCount=%d", f.createCount, f.runCount)
		}
	}
}

// TestInstallParsedManifest_AcceptsNativeHTTPDaemonTemplate is the
// companion positive guard: the native-http gate must NOT reject the valid
// shape. A kind:workspace-scoped + native-http + daemon_template manifest
// passes the contract gate and writes an intent (zero-workspace install).
func TestInstallParsedManifest_AcceptsNativeHTTPDaemonTemplate(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := serenaTemplateManifest() // native-http by construction
	a := NewAPI()
	var buf bytes.Buffer
	intentOut, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: []WorkspaceEntry{},
	})
	if err != nil {
		t.Fatalf("native-http daemon_template manifest must pass the gate; got error: %v", err)
	}
	if intentOut == "" {
		t.Fatal("want a non-empty intent path on a successful native-http install")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); statErr != nil {
		t.Errorf("native-http install must write supervisor-intent.json; stat err = %v", statErr)
	}
}

// TestInstallParsedManifest_RefusesSpecBearingWriteWhileSupervisorRunning is the
// bot PR #246 r2 P2 §7.1 gate. A running supervisor MAY be an OLD binary whose
// ReadSupervisorIntent (DisallowUnknownFields) would reject a newly written
// runtime_spec field and split-brain. While a supervisor holds its lock, a
// spec-bearing serena install must REFUSE and point at the cold-restart/upgrade
// flow rather than write the new field.
func TestInstallParsedManifest_RefusesSpecBearingWriteWhileSupervisorRunning(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	// Simulate a running supervisor by holding its singleton lock under stateDir.
	lk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire fake supervisor lock: %v", err)
	}
	defer lk.Release()

	ws := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}
	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	_, err = a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	})
	if err == nil {
		t.Fatal("expected §7.1 refuse error while a supervisor is running, got nil")
	}
	// Error names the gate, the running supervisor, the design ref, AND the
	// non-circular guidance (consultant r2 #2: STOP the supervisor — `install
	// --upgrade` alone leaves one running and would still be refused).
	for _, want := range []string{"refusing to write spec-bearing", "supervisor is running", "STOP the running supervisor", "§7.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refuse error %q missing %q", err.Error(), want)
		}
	}
	// The gate fires BEFORE the write — no spec-bearing intent must reach disk.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if written, rerr := ReadSupervisorIntent(intentPath); rerr == nil && written.HasRuntimeSpecRow() {
		t.Fatalf("gate must refuse before writing spec-bearing rows; found runtime_spec rows on disk")
	}
	// The refuse emits a durable audit row (consultant r2 (d)-observability),
	// not just the returned error string.
	logBytes, _ := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if !strings.Contains(string(logBytes), "spec-bearing-install-refused") {
		t.Errorf("expected durable spec-bearing-install-refused event in supervisor-events.log; got: %s", logBytes)
	}
}

// TestInstallParsedManifest_NonSpecInstallNotGatedByRunningSupervisor proves the
// §7.1 gate is scoped to runtime_spec-bearing writes. An empty-workspaces serena
// install produces NO runtime_spec rows, so it must proceed even while a
// supervisor is running (an old supervisor reads a no-runtime_spec intent fine).
func TestInstallParsedManifest_NonSpecInstallNotGatedByRunningSupervisor(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	lk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire fake supervisor lock: %v", err)
	}
	defer lk.Release()

	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentOut, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: []WorkspaceEntry{}, // no workspaces => no runtime_spec rows
	})
	if err != nil {
		t.Fatalf("non-spec install must NOT be gated by a running supervisor, got: %v", err)
	}
	if intentOut == "" {
		t.Fatal("want a non-empty intent path on a successful zero-workspace install")
	}
}

// TestInstallParsedManifest_SpecBearingWrite_BypassedWhenCallerHoldsMatchingLock
// is the Phase-1 typed-token bypass guard (plan-serena-lock-interlock-2026-06-09
// "Revision 1" + "Phase 1"). The §7.1 gate normally refuses a spec-bearing
// runtime_spec write while a supervisor holds its singleton lock. But the
// migrate / auto-register flows (Phase 2) acquire that VERY lock around their
// reap+rewrite, so to them the held lock is THEIR OWN handle, not a foreign
// supervisor. A caller that proves it holds the gate's exact lock — by passing
// the opaque token minted from (*SupervisorLock).AllowSpecBearingWriteBypass()
// — bypasses the probe and the spec-bearing write SUCCEEDS.
//
// The test engineers the held lock deterministically (no timing): it acquires
// the real supervisor.lock on the gate's exact path, then asserts
// SupervisorRunningUnderStateDir reports running (the per-handle flock quirk
// that makes the gate refuse WITHOUT the token), proving the bypass is what
// flips the outcome rather than an absent supervisor.
func TestInstallParsedManifest_SpecBearingWrite_BypassedWhenCallerHoldsMatchingLock(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	// Acquire the gate's exact lock — the same path the §7.1 probe inspects.
	lk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock on gate path: %v", err)
	}
	defer lk.Release()

	// Sanity: with the lock held, the gate's own liveness probe reports running.
	// This is the per-handle flock quirk that drives the refuse path — the token
	// is the ONLY thing that flips it to a successful write.
	running, _, probeErr := SupervisorRunningUnderStateDir(stateDir)
	if probeErr != nil {
		t.Fatalf("liveness probe errored: %v", probeErr)
	}
	if !running {
		t.Fatal("expected SupervisorRunningUnderStateDir to report running while the lock is held (per-handle quirk); the bypass test would be vacuous otherwise")
	}

	ws := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}
	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:               &buf,
		Workspaces:           workspaces,
		SupervisorLockBypass: lk.AllowSpecBearingWriteBypass(),
	})
	if err != nil {
		t.Fatalf("spec-bearing install with a matching held-lock token must SUCCEED, got: %v", err)
	}
	if intentPath == "" {
		t.Fatal("want a non-empty intent path on a successful bypassed install")
	}
	// The write actually committed the spec-bearing rows.
	written, rerr := ReadSupervisorIntent(intentPath)
	if rerr != nil {
		t.Fatalf("ReadSupervisorIntent: %v", rerr)
	}
	if !written.HasRuntimeSpecRow() {
		t.Fatal("bypassed install must commit runtime_spec rows; none found on disk")
	}
	// The bypass emits the info audit event (mirrors the refuse-emit).
	logBytes, _ := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if !strings.Contains(string(logBytes), "spec-bearing-write-allowed-under-caller-lock") {
		t.Errorf("expected spec-bearing-write-allowed-under-caller-lock event in supervisor-events.log; got: %s", logBytes)
	}
}

// TestInstallParsedManifest_SpecBearingWrite_StillRefusesWithoutToken proves the
// zero-value bypass (the default for the 14 existing call sites) preserves the
// §7.1 fail-closed behavior: a spec-bearing write while the lock is held still
// REFUSES and emits spec-bearing-install-refused. This is the control case for
// the bypass test above — same held lock, no token → refuse.
func TestInstallParsedManifest_SpecBearingWrite_StillRefusesWithoutToken(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	lk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	defer lk.Release()

	ws := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}
	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	// Zero-value SupervisorLockBypass (omitted) — exactly the 14 existing sites.
	_, err = a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:     &buf,
		Workspaces: workspaces,
	})
	if err == nil {
		t.Fatal("expected §7.1 refuse with a zero-value bypass token while a supervisor is running, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to write spec-bearing") {
		t.Errorf("refuse error %q missing the gate signature", err.Error())
	}
	logBytes, _ := os.ReadFile(filepath.Join(stateDir, SupervisorEventLogFileLeaf))
	if !strings.Contains(string(logBytes), "spec-bearing-install-refused") {
		t.Errorf("expected spec-bearing-install-refused event in supervisor-events.log; got: %s", logBytes)
	}
}

// TestInstallParsedManifest_SpecBearingWrite_RefusesWhenBypassLockPathMismatches
// is the in-gate Crux-3 guard: a token minted from a lock whose leaf does NOT
// match the gate's own stateDir/supervisor.lock is REJECTED — the gate treats it
// as no-bypass, the probe runs, and the spec-bearing write REFUSES. This folds
// the path-mismatch check INTO the gate so a misconfigured Phase-2 call site
// (wrong resolver) cannot silently re-open the split-brain.
func TestInstallParsedManifest_SpecBearingWrite_RefusesWhenBypassLockPathMismatches(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	// Hold the gate's REAL lock so the probe reports running (the gate must
	// refuse because the token does not match — not because no supervisor runs).
	gateLk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire gate supervisor lock: %v", err)
	}
	defer gateLk.Release()

	// Mint a token from a lock on a DIFFERENT leaf (a sibling temp dir). Its
	// .path does not equal the gate's stateDir/supervisor.lock, so the identity
	// check must reject it.
	otherDir := t.TempDir()
	otherLk, err := AcquireSupervisorLock(filepath.Join(otherDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire mismatched supervisor lock: %v", err)
	}
	defer otherLk.Release()

	ws := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}
	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	_, err = a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:               &buf,
		Workspaces:           workspaces,
		SupervisorLockBypass: otherLk.AllowSpecBearingWriteBypass(),
	})
	if err == nil {
		t.Fatal("expected §7.1 refuse when the bypass token's lock leaf mismatches the gate path, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to write spec-bearing") {
		t.Errorf("refuse error %q missing the gate signature (path-mismatch token must NOT bypass)", err.Error())
	}
}

// TestInstallParsedManifest_SpecBearingWrite_RefusesWhenBypassLockAlreadyReleased
// proves the identity check verifies the lock is STILL HELD, not merely that the
// handle once existed. A token minted from a matching lock that is then released
// (Release() nils .fl) is stale and must be REJECTED — the gate falls through to
// the probe and refuses.
func TestInstallParsedManifest_SpecBearingWrite_RefusesWhenBypassLockAlreadyReleased(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	lk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	// Mint the token while the lock is held, then release it — the token is now
	// stale (its .fl is nil).
	token := lk.AllowSpecBearingWriteBypass()
	lk.Release()

	// Re-hold the gate's lock from a fresh acquire so the probe still reports
	// running (otherwise the refuse could be attributed to "no supervisor"
	// rather than to the stale token being rejected).
	gateLk, err := AcquireSupervisorLock(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		t.Fatalf("re-acquire supervisor lock after release: %v", err)
	}
	defer gateLk.Release()

	ws := t.TempDir()
	workspaces := []WorkspaceEntry{{
		WorkspaceKey:  WorkspaceKey(ws),
		WorkspacePath: ws,
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
	}}
	m := serenaTemplateManifest()
	a := NewAPI()
	var buf bytes.Buffer
	_, err = a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:               &buf,
		Workspaces:           workspaces,
		SupervisorLockBypass: token, // stale: minted then released
	})
	if err == nil {
		t.Fatal("expected §7.1 refuse when the bypass token's lock was already released, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to write spec-bearing") {
		t.Errorf("refuse error %q missing the gate signature (released token must NOT bypass)", err.Error())
	}
}

// TestInstallPlanCore_GlobalFreshInstall_WritesSupervisorIntent_NoSchedulerTask
// is the v0.6 Phase F DONE-GATE (spec §5 "Phase F", line 638): a fresh install
// of a GLOBAL manifest's daemons must spawn from supervisor-intent.json via the
// supervisor reconcile seam, NOT from per-daemon `\mcp-local-hub-<server>-<daemon>`
// Task Scheduler tasks.
//
// installPlanCore is the shared owner of the Phase F "global daemons →
// supervisor-intent (not scheduler)" decision; (*API).Install (install.go:229)
// delegates here after manifest-load + preflight + plan-build, so driving
// installPlanCore directly with a BuildPlan-produced plan exercises the exact
// branch Install reaches — mirroring how the existing executeInstallTo tests in
// this file drive the lower-level primitive directly.
//
// State safety: daemonIntentTestHelper redirects daemonStateRootOverride to a
// fresh t.TempDir, so the supervisor-intent.json write lands in the temp state
// dir, never the live host's %LOCALAPPDATA%\mcp-local-hub\.
func TestInstallPlanCore_GlobalFreshInstall_WritesSupervisorIntent_NoSchedulerTask(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest() // KindGlobal, daemons alpha+beta, weekly-refresh
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// Sanity: the plan must carry SupervisorIntent rows so installPlanCore takes
	// the superviseGlobal branch (2 daemon + 1 weekly = 3).
	if len(plan.SupervisorIntent) != 3 {
		t.Fatalf("plan SupervisorIntent = %d, want 3 (precondition for the supervisor-intent path)", len(plan.SupervisorIntent))
	}

	a := NewAPI()
	var buf bytes.Buffer
	if err := a.installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(global fresh install): %v", err)
	}

	// DONE-GATE assertion 1: supervisor-intent.json was written with a
	// descriptor row for each global daemon (alpha + beta).
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent(%s): %v — Phase F requires global daemons spawn from this file", intentPath, err)
	}
	byServerDaemon := map[string]bool{}
	for _, d := range written.Daemons {
		byServerDaemon[d.Server+"/"+d.Daemon] = true
	}
	for _, want := range []string{"demo/alpha", "demo/beta"} {
		if !byServerDaemon[want] {
			t.Errorf("supervisor-intent.json missing daemon descriptor %q; rows=%+v", want, written.Daemons)
		}
	}

	// DONE-GATE assertion 2: the daemons spawn via the supervisor seam, NOT via
	// scheduler tasks — no Create (Pass A) and no Run (Pass B) was issued.
	if f.createCount != 0 {
		t.Errorf("scheduler Create calls = %d, want 0; Phase F global installs create NO per-daemon scheduler task (created: %v)", f.createCount, f.tasks)
	}
	if f.runCount != 0 {
		t.Errorf("scheduler Run calls = %d, want 0; Phase F defers every daemon spawn to the supervisor reconcile loop (runNames: %v)", f.runCount, f.runNames)
	}
}

func TestInstallPlanCore_GlobalDryRunPrintsSupervisorMutationPlan(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", true, &buf); err != nil {
		t.Fatalf("installPlanCore(global dry-run): %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Supervisor intent rows to write",
		"mcp-local-hub-demo-alpha",
		"mcp-local-hub-demo-beta",
		"Maintenance timers to ensure",
		"mcp-local-hub-demo-weekly-refresh",
		"Autostart owner to ensure",
		"Legacy scheduler tasks to clean",
		"Removed supervisor targets to kill",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("global dry-run output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Scheduler tasks to create") {
		t.Errorf("global supervisor dry-run must not claim scheduler tasks will be created:\n%s", out)
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Fatalf("dry-run mutated scheduler: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

func TestInstallPlanCore_GlobalDryRunDoesNotCreateMissingStateDir(t *testing.T) {
	statePathsHelper(t)
	stateDir := filepath.Join(t.TempDir(), "missing-state")
	daemonStateRootOverride = stateDir
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", true, &buf); err != nil {
		t.Fatalf("installPlanCore(global dry-run): %v", err)
	}
	if _, statErr := os.Stat(stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("global dry-run must not create state dir %s; stat err = %v", stateDir, statErr)
	}
	out := buf.String()
	for _, want := range []string{
		"Supervisor intent rows to write",
		"mcp-local-hub-demo-alpha",
		"Maintenance timers to ensure",
		"Autostart owner to ensure",
		"Prior supervisor-intent diff unavailable",
		"not found",
		"No changes made.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("global dry-run output missing %q:\n%s", want, out)
		}
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Fatalf("dry-run mutated scheduler: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

func TestInstallPlanCore_GlobalDryRunCorruptIntentStillPrintsPlan(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	before := []byte("{ this is not valid json")
	if err := os.WriteFile(intentPath, before, 0o600); err != nil {
		t.Fatalf("seed corrupt supervisor intent: %v", err)
	}

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	var buf bytes.Buffer
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", true, &buf); err != nil {
		t.Fatalf("installPlanCore(global dry-run) over corrupt intent must still succeed: %v", err)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("re-read corrupt supervisor intent: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("dry-run mutated corrupt supervisor intent: before=%q after=%q", before, after)
	}
	out := buf.String()
	for _, want := range []string{
		"Supervisor intent rows to write",
		"mcp-local-hub-demo-alpha",
		"mcp-local-hub-demo-beta",
		"Maintenance timers to ensure",
		"mcp-local-hub-demo-weekly-refresh",
		"Prior supervisor-intent diff unavailable",
		"decode",
		"No changes made.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("global dry-run output missing %q:\n%s", want, out)
		}
	}
	if f.createCount != 0 || f.runCount != 0 {
		t.Fatalf("dry-run mutated scheduler: createCount=%d runCount=%d", f.createCount, f.runCount)
	}
}

// TestInstallPlanCore_GlobalInstall_EnablesAbsentAutostartOwner pins the logon
// owner handoff for the supervisor-intent path. A global install suppresses
// per-daemon scheduler tasks, so an absent autostart shim must be enabled or a
// reboot leaves no owner that can start the supervisor.
//
// Negative-control: remove the post-install autostart owner ensure call and the
// fake Enable count stays 0, so this test fails.
func TestInstallPlanCore_GlobalInstall_EnablesAbsentAutostartOwner(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	// Seed only strict_mode so the install's merged intent must preserve it and
	// thread the same value into autostart.Enable.
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{
		Version:    1,
		StrictMode: true,
	}); err != nil {
		t.Fatalf("seed supervisor-intent strict_mode: %v", err)
	}
	fb := &fakeInstallAutostartBackend{statusReturn: autostart.StateAbsent}
	installFakeAutostartBackend(t, fb)
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(global install): %v", err)
	}

	if fb.enableCalls != 1 {
		t.Fatalf("autostart Enable calls = %d, want 1 for absent owner", fb.enableCalls)
	}
	if len(fb.enableOpts) != 1 || !fb.enableOpts[0].StrictMode {
		t.Fatalf("autostart Enable opts = %+v, want StrictMode=true from supervisor-intent", fb.enableOpts)
	}
}

// TestInstallPlanCore_GlobalInstall_AutostartEnableUsesCanonicalMcphubPath
// proves install-created autostart shims point at the canonical installed
// binary, not os.Executable from the current build/download directory.
//
// Negative-control: pre-fix ensureGlobalInstallAutostartOwner builds
// autostart.Options with an empty MCPHubPath, so the fake records "".
func TestInstallPlanCore_GlobalInstall_AutostartEnableUsesCanonicalMcphubPath(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	canonical := filepath.Join(t.TempDir(), MCPHubBinaryName())
	if err := os.WriteFile(canonical, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write canonical stub: %v", err)
	}
	t.Cleanup(SetTestCanonicalMcphubPath(canonical))

	fb := &fakeInstallAutostartBackend{statusReturn: autostart.StateAbsent}
	installFakeAutostartBackend(t, fb)
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(global install): %v", err)
	}

	if fb.enableCalls != 1 {
		t.Fatalf("autostart Enable calls = %d, want 1", fb.enableCalls)
	}
	if got := fb.enableOpts[0].MCPHubPath; got != canonical {
		t.Fatalf("autostart Enable MCPHubPath = %q, want canonical %q", got, canonical)
	}
}

// TestInstallPlanCore_GlobalInstall_AutostartAlreadyEnabledNoops proves install
// does not rewrite an existing logon owner merely because daemon scheduler tasks
// are suppressed.
//
// Negative-control: unconditionally call Enable after the supervisor-path
// install and the fake Enable count becomes 1, so this test fails.
func TestInstallPlanCore_GlobalInstall_AutostartAlreadyEnabledNoops(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	fb := &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped}
	installFakeAutostartBackend(t, fb)
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nil
	}))

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(global install): %v", err)
	}

	if fb.enableCalls != 0 {
		t.Fatalf("autostart Enable calls = %d, want 0 when owner is already enabled", fb.enableCalls)
	}
}

// TestEnsureGlobalInstallAutostartOwner_RecreatesDriftedShimWithCanonicalPath
// pins the install-time owner handoff for a drifted existing shim. StateDrifted
// means the stored command line or binary path disagrees with the canonical
// Enable(opts) body, so install must rewrite it before relying on that owner.
//
// Negative-control: pre-fix ensureGlobalInstallAutostartOwner only warned for
// StateDrifted, so the fake Enable count stayed 0 and the canonical path was
// never applied.
func TestEnsureGlobalInstallAutostartOwner_RecreatesDriftedShimWithCanonicalPath(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), MCPHubBinaryName())
	t.Cleanup(SetTestCanonicalMcphubPath(canonical))
	fb := &fakeInstallAutostartBackend{statusReturn: autostart.StateDrifted}
	installFakeAutostartBackend(t, fb)

	ensureGlobalInstallAutostartOwner(io.Discard, true)

	if fb.enableCalls != 1 {
		t.Fatalf("autostart Enable calls = %d, want 1 for drifted owner", fb.enableCalls)
	}
	if got := fb.enableOpts[0].MCPHubPath; got != canonical {
		t.Fatalf("autostart Enable MCPHubPath = %q, want canonical %q", got, canonical)
	}
	if !fb.enableOpts[0].StrictMode {
		t.Fatalf("autostart Enable opts = %+v, want StrictMode=true", fb.enableOpts[0])
	}
}

func TestEnsureGlobalInstallAutostartOwner_EnabledStatesDoNotRewriteShim(t *testing.T) {
	for _, state := range []autostart.State{autostart.StateEnabledRunning, autostart.StateEnabledStopped} {
		t.Run(state.String(), func(t *testing.T) {
			fb := &fakeInstallAutostartBackend{statusReturn: state}
			installFakeAutostartBackend(t, fb)

			ensureGlobalInstallAutostartOwner(io.Discard, false)

			if fb.enableCalls != 0 {
				t.Fatalf("autostart Enable calls = %d, want 0 for %s", fb.enableCalls, state)
			}
		})
	}
}

func TestKillLegacySchedulerTaskDaemonByPortBestEffort_ForeignOwnerNotKilled(t *testing.T) {
	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	t.Cleanup(func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
	})
	lookupProcess = nil

	legacyKillCalled := false
	killByPortFn = func(port int, timeout time.Duration) error {
		legacyKillCalled = true
		return nil
	}
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		if port != 33031 {
			t.Fatalf("forceKillByPortFn port = %d, want 33031", port)
		}
		return portKillIdentityMismatch, errors.New("port owned by foreign process")
	}

	var buf bytes.Buffer
	killLegacySchedulerTaskDaemonByPortBestEffort("mcp-local-hub-demo-alpha", 33031, &buf)
	if legacyKillCalled {
		t.Fatal("legacy handoff used killByPortFn on a foreign owner; want identity-mismatch warning without kill")
	}
	out := buf.String()
	if !strings.Contains(out, "port owned by foreign process") || !strings.Contains(out, "not killing") {
		t.Fatalf("warning = %q, want foreign-owner not-killing warning", out)
	}
}

func TestKillLegacySchedulerTaskDaemonByPortBestEffort_McphubOwnerKilled(t *testing.T) {
	origKill := killByPortFn
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	t.Cleanup(func() {
		killByPortFn = origKill
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
	})
	lookupProcess = nil

	killByPortFn = func(port int, timeout time.Duration) error {
		t.Fatalf("legacy handoff must use outcome-aware kill path, got killByPortFn(%d)", port)
		return nil
	}
	var killedPorts []int
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		killedPorts = append(killedPorts, port)
		return portKillKilled, nil
	}

	killLegacySchedulerTaskDaemonByPortBestEffort("mcp-local-hub-demo-alpha", 33032, io.Discard)
	if !reflect.DeepEqual(killedPorts, []int{33032}) {
		t.Fatalf("killed ports = %v, want [33032]", killedPorts)
	}
}

func TestInstallPlanCore_GlobalFilteredInstall_ReplacesOnlySelectedDaemon(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: "\\mcp-local-hub-demo-alpha", Server: "demo", Daemon: "alpha", Command: "stale-alpha", Port: 9991},
			{TaskName: "\\mcp-local-hub-demo-beta", Server: "demo", Daemon: "beta", Command: "preserve-beta", Port: 9992},
			{TaskName: "\\mcp-local-hub-other-d", Server: "other", Daemon: "d", Command: "preserve-other", Port: 9993},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "alpha")
	if err != nil {
		t.Fatalf("BuildPlan(filtered): %v", err)
	}
	if got := len(plan.SupervisorIntent); got != 1 {
		t.Fatalf("filtered plan SupervisorIntent rows = %d, want 1", got)
	}

	a := NewAPI()
	var buf bytes.Buffer
	if err := a.installPlanCore(context.Background(), m, plan, "alpha", false, &buf); err != nil {
		t.Fatalf("installPlanCore(filtered global install): %v", err)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent(%s): %v", intentPath, err)
	}
	byKey := map[string]SupervisorDaemon{}
	for _, d := range written.Daemons {
		byKey[d.Server+"/"+d.Daemon] = d
	}
	alpha, ok := byKey["demo/alpha"]
	if !ok {
		t.Fatalf("filtered install did not write selected daemon demo/alpha; rows=%+v", written.Daemons)
	}
	if alpha.Port != 9211 || alpha.Command == "stale-alpha" {
		t.Errorf("selected daemon was not refreshed from the manifest: %+v", alpha)
	}
	beta, ok := byKey["demo/beta"]
	if !ok {
		t.Fatalf("filtered install dropped unselected existing daemon demo/beta; rows=%+v", written.Daemons)
	}
	if beta.Command != "preserve-beta" || beta.Port != 9992 {
		t.Errorf("unselected existing daemon was not preserved verbatim: %+v", beta)
	}
	other, ok := byKey["other/d"]
	if !ok {
		t.Fatalf("filtered install dropped sibling server daemon other/d; rows=%+v", written.Daemons)
	}
	if other.Command != "preserve-other" || other.Port != 9993 {
		t.Errorf("sibling server daemon was not preserved verbatim: %+v", other)
	}
	if _, ok := byKey["demo/"]; ok {
		t.Fatalf("unexpected empty daemon key present: rows=%+v", written.Daemons)
	}
	for _, d := range written.Daemons {
		if d.Server == "demo" && d.Daemon != "alpha" && d.Daemon != "beta" {
			t.Fatalf("filtered install wrote unrequested demo daemon %q; rows=%+v", d.Daemon, written.Daemons)
		}
	}
}

func TestInstallPlanCore_GlobalFullReinstall_KillsRemovedSupervisorDaemon(t *testing.T) {
	fakeMcphubIdentityForTest(t)
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{statusReturn: autostart.StateEnabledStopped})

	const (
		alphaTask = "\\mcp-local-hub-demo-alpha"
		betaTask  = "\\mcp-local-hub-demo-beta"
		otherTask = "\\mcp-local-hub-other-d"
	)
	now := time.Unix(1700000000, 0).UTC()
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: alphaTask, Server: "demo", Daemon: "alpha", Command: "stale-alpha", Port: 9321},
			{TaskName: betaTask, Server: "demo", Daemon: "beta", Command: "stale-beta", Port: 9322},
			{TaskName: otherTask, Server: "other", Daemon: "d", Command: "preserve-other", Port: 9323},
		},
		Stops: map[string]DaemonIntent{
			alphaTask: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			betaTask:  {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
			otherTask: {Desired: IntentDesiredStopped, Reason: IntentReasonUserStop, UpdatedAt: now},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var order []string
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(ctx context.Context, apply bool) (ReconcileResponse, error) {
		order = append(order, "nudge")
		return ReconcileResponse{}, nil
	}))

	var killPorts []int
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		order = append(order, "kill-port")
		killPorts = append(killPorts, port)
		t.Fatalf("forceKillByPortFn called for port %d after successful PID kill", port)
		return portKillNoListener, nil
	}
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if port != 9322 {
			t.Fatalf("lookupProcess port = %d, want 9322", port)
		}
		return 0, 0, 0, false
	}
	t.Cleanup(func() {
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
	})

	var killedPIDs []int
	origPID := stopForceKillPIDFn
	stopForceKillPIDFn = func(pid int) error {
		order = append(order, "kill-pid")
		killedPIDs = append(killedPIDs, pid)
		return nil
	}
	t.Cleanup(func() { stopForceKillPIDFn = origPID })

	origStatus := supervisorIPCStatusFn
	supervisorIPCStatusFn = func(ctx context.Context) ([]DaemonStatus, error) {
		order = append(order, "ipc-status")
		return []DaemonStatus{{TaskName: betaTask, PID: 4244, State: "Running"}}, nil
	}
	t.Cleanup(func() { supervisorIPCStatusFn = origStatus })

	m := &config.ServerManifest{
		Name:          "demo",
		Kind:          config.KindGlobal,
		Transport:     config.TransportNativeHTTP,
		Command:       "go",
		Daemons:       []config.DaemonSpec{{Name: "alpha", Port: 9321}},
		WeeklyRefresh: true,
	}
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan(full reinstall): %v", err)
	}
	if got := len(plan.SupervisorIntent); got != 2 {
		t.Fatalf("plan SupervisorIntent rows = %d, want 2 (alpha daemon + weekly refresh)", got)
	}

	if err := NewAPI().installPlanCore(context.Background(), m, plan, "", false, io.Discard); err != nil {
		t.Fatalf("installPlanCore(full reinstall): %v", err)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent(%s): %v", intentPath, err)
	}
	for _, d := range written.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == canonicalIntentTaskKey(betaTask) {
			t.Fatalf("removed daemon %s survived merged intent: %+v", betaTask, written.Daemons)
		}
	}
	// The captured supervisor-reported PID (4244) is killed first. The
	// descriptor port is only polled for release; it must not fall through to a
	// blind port kill because the removed row's port may already be reused.
	if len(killPorts) != 0 {
		t.Fatalf("forceKillByPortFn ports = %v, want none after the PID kill", killPorts)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != 4244 {
		t.Fatalf("PID kills = %v, want exactly [4244]", killedPIDs)
	}
	if len(order) != 3 || order[0] != "ipc-status" || order[1] != "nudge" || order[2] != "kill-pid" {
		t.Fatalf("ipc/reconcile/kill order = %v, want [ipc-status nudge kill-pid]", order)
	}
	if _, ok := written.Stops[betaTask]; ok {
		t.Fatalf("removed daemon %s retained a dangling stop entry: %+v", betaTask, written.Stops)
	}
	if _, ok := written.Stops[otherTask]; !ok {
		t.Fatalf("sibling stop %s was not preserved: %+v", otherTask, written.Stops)
	}
}

// TestInstallPlanCore_GlobalFreshInstall_NoPerDaemonSchedulerTaskCreated is the
// v0.6 Phase F FALSIFICATION test (spec §5 "Phase F", line 639): a fresh global
// install must create ZERO `\mcp-local-hub-<server>-<daemon>` per-daemon Task
// Scheduler entries. If any such task is created, Phase F is incomplete (the
// pre-Phase-F scheduler model survived).
//
// Distinct from the done-gate above: that test asserts the POSITIVE (intent
// written + no Create/Run counts); this asserts the NEGATIVE by inspecting the
// exact task-name shape the legacy model used (`mcp-local-hub-demo-alpha`,
// `-beta`, `-weekly-refresh`) never reached the scheduler.
func TestInstallPlanCore_GlobalFreshInstall_NoPerDaemonSchedulerTaskCreated(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	a := NewAPI()
	var buf bytes.Buffer
	if err := a.installPlanCore(context.Background(), m, plan, "", false, &buf); err != nil {
		t.Fatalf("installPlanCore(global fresh install): %v", err)
	}

	// No per-daemon `\mcp-local-hub-<server>-<daemon>` task may exist in the
	// fake's created-task set. The legacy model would have created exactly these
	// three names; under Phase F the set must be empty.
	for _, legacyTask := range []string{
		"mcp-local-hub-demo-alpha",
		"mcp-local-hub-demo-beta",
		"mcp-local-hub-demo-weekly-refresh",
	} {
		for created := range f.tasks {
			if strings.TrimPrefix(created, "\\") == legacyTask {
				t.Errorf("Phase F regression: per-daemon scheduler task %q was created; global daemons must live in supervisor-intent.json, not scheduler tasks", legacyTask)
			}
		}
	}
	// And the created-specs slice (the ordered Pass A record) must be empty.
	if len(f.createdSpecs) != 0 {
		t.Errorf("Pass A created %d scheduler task spec(s); Phase F global install must create none: %+v", len(f.createdSpecs), f.createdSpecs)
	}

	// P2-A regression guard: the global manifest's `weekly_refresh: true` must
	// NOT be silently dropped. Pre-Phase-F it materialized a
	// `mcp-local-hub-demo-weekly-refresh` SCHEDULER task (the legacy assertion
	// above proves that path is gone); Phase F's successor is a supervisor
	// server-weekly-refresh MaintenanceTimer in supervisor-intent.json (the
	// cadence supervise_maintenance.go:333 already dispatches on). Without the
	// fix, mergeServerWeeklyRefreshTimer's predecessor carried only the prior
	// (here: empty) timer set verbatim, so no timer existed and the weekly
	// restart never fired. This block asserts the timer IS materialized — it
	// FAILS pre-fix (written.MaintenanceTimers is empty) and passes post-fix.
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent(%s): %v", intentPath, err)
	}
	wantName := canonicalIntentTaskKey("mcp-local-hub-demo-weekly-refresh")
	var got *MaintenanceTimer
	for i := range written.MaintenanceTimers {
		if written.MaintenanceTimers[i].Name == wantName {
			got = &written.MaintenanceTimers[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("global manifest weekly_refresh=true was SILENTLY DROPPED: no server-weekly-refresh MaintenanceTimer %q in supervisor-intent.json; timers=%+v", wantName, written.MaintenanceTimers)
	}
	if got.Kind != "server-weekly-refresh" {
		t.Errorf("materialized weekly-refresh timer Kind = %q, want %q (the cadence supervise_maintenance.go dispatches on)", got.Kind, "server-weekly-refresh")
	}
	if got.Server != "demo" {
		t.Errorf("materialized weekly-refresh timer Server = %q, want %q", got.Server, "demo")
	}
	if !reflect.DeepEqual(got.Args, []string{"restart", "--server", "demo"}) {
		t.Errorf("materialized weekly-refresh timer Args = %v, want [restart --server demo]", got.Args)
	}
	if got.Command == "" {
		t.Errorf("materialized weekly-refresh timer Command is empty; want the resolved mcphub binary path")
	}
	// Enabled is nil on a fresh install (no prior off-switch to preserve), so the
	// scheduler's nil==default-on contract honors the timer.
	if got.Enabled != nil {
		t.Errorf("materialized weekly-refresh timer Enabled = %v, want nil (default-on on a fresh install)", *got.Enabled)
	}
}

func TestCleanupLegacySchedulerTasksForSupervisorInstall_DeletesTaskAndKillsPort(t *testing.T) {
	t.Run("legacy daemon task is deleted before its manifest port is killed", func(t *testing.T) {
		daemonIntentTestHelper(t)
		f := newInstallFakeScheduler()
		f.listSeed = []scheduler.TaskStatus{{Name: `\mcp-local-hub-demo-alpha`}}
		installFakeScheduler(t, f)

		origLookup := lookupProcess
		lookupProcess = nil
		t.Cleanup(func() { lookupProcess = origLookup })

		var killed []int
		origKill := forceKillByPortFn
		forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
			if len(f.deleteNames) == 0 {
				t.Errorf("forceKillByPortFn called before deleting the legacy scheduler task")
			}
			if timeout != 5*time.Second {
				t.Errorf("kill timeout = %s, want 5s", timeout)
			}
			killed = append(killed, port)
			return portKillKilled, nil
		}
		t.Cleanup(func() { forceKillByPortFn = origKill })

		m := &config.ServerManifest{
			Name:      "demo",
			Kind:      config.KindGlobal,
			Transport: config.TransportStdioBridge,
			Command:   "go",
			Daemons: []config.DaemonSpec{
				{Name: "alpha", Port: 9313},
				{Name: "beta", Port: 33013},
			},
		}
		var buf bytes.Buffer
		cleanupLegacySchedulerTasksForSupervisorInstall(m, "", &buf)

		if len(f.deleteNames) != 1 || f.deleteNames[0] != "mcp-local-hub-demo-alpha" {
			t.Fatalf("deleted legacy tasks = %v, want [mcp-local-hub-demo-alpha]", f.deleteNames)
		}
		if len(killed) != 1 || killed[0] != 9313 {
			t.Fatalf("forceKillByPortFn ports = %v, want only the listed legacy task port [9313]", killed)
		}
	})

	t.Run("hyphenated daemon name uses exact manifest match for port kill", func(t *testing.T) {
		daemonIntentTestHelper(t)
		f := newInstallFakeScheduler()
		f.listSeed = []scheduler.TaskStatus{{Name: `\mcp-local-hub-demo-vscode-css`}}
		installFakeScheduler(t, f)

		origLookup := lookupProcess
		lookupProcess = nil
		t.Cleanup(func() { lookupProcess = origLookup })

		var killed []int
		origKill := forceKillByPortFn
		forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
			if len(f.deleteNames) == 0 {
				t.Errorf("forceKillByPortFn called before deleting the legacy scheduler task")
			}
			killed = append(killed, port)
			return portKillKilled, nil
		}
		t.Cleanup(func() { forceKillByPortFn = origKill })

		m := &config.ServerManifest{
			Name:      "demo",
			Kind:      config.KindGlobal,
			Transport: config.TransportStdioBridge,
			Command:   "go",
			Daemons: []config.DaemonSpec{
				{Name: "vscode-css", Port: 33017},
			},
		}
		var buf bytes.Buffer
		cleanupLegacySchedulerTasksForSupervisorInstall(m, "", &buf)

		if len(f.deleteNames) != 1 || f.deleteNames[0] != "mcp-local-hub-demo-vscode-css" {
			t.Fatalf("deleted legacy tasks = %v, want [mcp-local-hub-demo-vscode-css]", f.deleteNames)
		}
		if len(killed) != 1 || killed[0] != 33017 {
			t.Fatalf("forceKillByPortFn ports = %v, want hyphenated daemon port [33017]", killed)
		}
	})

	t.Run("zero port deletes task but skips kill with warning", func(t *testing.T) {
		daemonIntentTestHelper(t)
		f := newInstallFakeScheduler()
		f.listSeed = []scheduler.TaskStatus{{Name: `\mcp-local-hub-demo-alpha`}}
		installFakeScheduler(t, f)

		origLookup := lookupProcess
		lookupProcess = nil
		t.Cleanup(func() { lookupProcess = origLookup })

		var killed []int
		origKill := forceKillByPortFn
		forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
			killed = append(killed, port)
			return portKillKilled, nil
		}
		t.Cleanup(func() { forceKillByPortFn = origKill })

		m := &config.ServerManifest{
			Name:      "demo",
			Kind:      config.KindGlobal,
			Transport: config.TransportStdioBridge,
			Command:   "go",
			Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 0}},
		}
		var buf bytes.Buffer
		cleanupLegacySchedulerTasksForSupervisorInstall(m, "", &buf)

		if len(f.deleteNames) != 1 || f.deleteNames[0] != "mcp-local-hub-demo-alpha" {
			t.Fatalf("deleted legacy tasks = %v, want [mcp-local-hub-demo-alpha]", f.deleteNames)
		}
		if len(killed) != 0 {
			t.Fatalf("forceKillByPortFn ports = %v, want none for port 0", killed)
		}
		if !strings.Contains(buf.String(), "daemon port unknown") {
			t.Fatalf("cleanup warning = %q, want daemon port unknown warning", buf.String())
		}
	})

	t.Run("filtered install skips absent legacy task without delete or kill", func(t *testing.T) {
		daemonIntentTestHelper(t)
		f := newInstallFakeScheduler()
		installFakeScheduler(t, f)

		origLookup := lookupProcess
		lookupProcess = nil
		t.Cleanup(func() { lookupProcess = origLookup })

		var killed []int
		origKill := forceKillByPortFn
		forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
			killed = append(killed, port)
			return portKillKilled, nil
		}
		t.Cleanup(func() { forceKillByPortFn = origKill })

		m := &config.ServerManifest{
			Name:      "demo",
			Kind:      config.KindGlobal,
			Transport: config.TransportStdioBridge,
			Command:   "go",
			Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 33031}},
		}
		var buf bytes.Buffer
		cleanupLegacySchedulerTasksForSupervisorInstall(m, "alpha", &buf)

		if len(f.deleteNames) != 0 {
			t.Fatalf("deleted legacy tasks = %v, want none when filtered task is absent", f.deleteNames)
		}
		if len(killed) != 0 {
			t.Fatalf("forceKillByPortFn ports = %v, want none when filtered task is absent", killed)
		}
	})

	t.Run("filtered install deletes and kills when legacy task exists", func(t *testing.T) {
		daemonIntentTestHelper(t)
		f := newInstallFakeScheduler()
		f.listSeed = []scheduler.TaskStatus{{Name: `\mcp-local-hub-demo-alpha`}}
		installFakeScheduler(t, f)

		origLookup := lookupProcess
		lookupProcess = nil
		t.Cleanup(func() { lookupProcess = origLookup })

		var killed []int
		origKill := forceKillByPortFn
		forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
			if len(f.deleteNames) == 0 {
				t.Errorf("forceKillByPortFn called before deleting the filtered legacy scheduler task")
			}
			killed = append(killed, port)
			return portKillKilled, nil
		}
		t.Cleanup(func() { forceKillByPortFn = origKill })

		m := &config.ServerManifest{
			Name:      "demo",
			Kind:      config.KindGlobal,
			Transport: config.TransportStdioBridge,
			Command:   "go",
			Daemons:   []config.DaemonSpec{{Name: "alpha", Port: 33032}},
		}
		var buf bytes.Buffer
		cleanupLegacySchedulerTasksForSupervisorInstall(m, "alpha", &buf)

		if len(f.deleteNames) != 1 || f.deleteNames[0] != "mcp-local-hub-demo-alpha" {
			t.Fatalf("deleted legacy tasks = %v, want [mcp-local-hub-demo-alpha]", f.deleteNames)
		}
		if len(killed) != 1 || killed[0] != 33032 {
			t.Fatalf("forceKillByPortFn ports = %v, want [33032]", killed)
		}
	})
}

// TestBuildMergedSupervisorIntent_PreservesStopsSubBlock locks in the
// cross-phase E2xF contract (bot PR #284 P2): the install merge must carry
// the prior stops sub-block VERBATIM — it owns descriptors + this server's
// weekly timer, never the operator stops. Pre-fix the merged struct omitted
// Stops entirely, so ANY install wiped every operator stop across ALL
// servers (the stopped daemons respawned on the next reconcile).
func TestBuildMergedSupervisorIntent_PreservesStopsSubBlock(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	stop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-other-d`, Server: "other", Daemon: "d", Command: "keep", Port: 9991},
		},
		Stops: map[string]DaemonIntent{
			`\mcp-local-hub-other-d`: stop, // sibling server's operator stop
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &config.ServerManifest{
		Name: "demo",
		Daemons: []config.DaemonSpec{
			{Name: "alpha", Port: 9992},
		},
	}
	merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "", io.Discard)
	if err != nil {
		t.Fatalf("buildMergedSupervisorIntent: %v", err)
	}
	got, ok := merged.Stops[`\mcp-local-hub-other-d`]
	if !ok {
		t.Fatalf("install merge DROPPED the prior stops sub-block — every operator stop would be wiped on any install")
	}
	if got.Desired != stop.Desired || got.Reason != stop.Reason || !got.UpdatedAt.Equal(stop.UpdatedAt) {
		t.Fatalf("stop entry mutated by the install merge: got %+v want %+v", got, stop)
	}
}

// TestInstallPlanCore_GlobalFilteredInstall_BlankDaemonRowNotDuplicated is the
// bot PR #284 P2 (finding 1) regression: a legacy / older-writer
// supervisor-intent row whose Daemon field is BLANK but whose task_name IS the
// selected daemon's canonical task (`\mcp-local-hub-demo-alpha`) must be
// REPLACED by the filtered install, not preserved alongside the freshly
// materialized row. The field-only filter (`d.Server == m.Name && d.Daemon ==
// daemonFilter`) misses the blank-Daemon row, so the stale row survives AND the
// fresh row appends -> DUPLICATE task_name entries (duplicate status rows,
// ambiguous stop/restart selection). The task-name fallback added to
// buildMergedSupervisorIntent's kept-loop closes this.
//
// Negative-control: without the canonical-task-name fallback this test fails
// because the blank-Daemon stale row survives, producing TWO rows keyed
// `\mcp-local-hub-demo-alpha`.
//
// State safety: daemonIntentTestHelper redirects the state dir to a fresh
// t.TempDir; nothing touches the live host %LOCALAPPDATA%\mcp-local-hub\.
func TestInstallPlanCore_GlobalFilteredInstall_BlankDaemonRowNotDuplicated(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Legacy/older-writer row: task_name IS demo/alpha but the Server
			// AND Daemon fields are blank (other intent readers re-derive them
			// from the task name via ParseManagedTaskName). The field-only
			// filter cannot see this is the selected daemon's row.
			{TaskName: `\mcp-local-hub-demo-alpha`, Command: "stale-blank-alpha", Port: 9991},
			{TaskName: `\mcp-local-hub-demo-beta`, Server: "demo", Daemon: "beta", Command: "preserve-beta", Port: 9992},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	m := globalTwoDaemonManifest()
	plan, err := BuildPlan(m, "alpha")
	if err != nil {
		t.Fatalf("BuildPlan(filtered alpha): %v", err)
	}

	a := NewAPI()
	var buf bytes.Buffer
	if err := a.installPlanCore(context.Background(), m, plan, "alpha", false, &buf); err != nil {
		t.Fatalf("installPlanCore(filtered global install): %v", err)
	}

	written, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatalf("ReadSupervisorIntent(%s): %v", intentPath, err)
	}

	// CORE assertion: exactly ONE row keyed to the selected daemon's task name.
	// Pre-fix the blank-Daemon stale row + the fresh row both carry this key.
	const alphaTask = `\mcp-local-hub-demo-alpha`
	var alphaRows []SupervisorDaemon
	for _, d := range written.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == alphaTask {
			alphaRows = append(alphaRows, d)
		}
	}
	if len(alphaRows) != 1 {
		t.Fatalf("filtered install produced %d rows for task %q, want exactly 1 (no duplicate); rows=%+v", len(alphaRows), alphaTask, written.Daemons)
	}
	// The surviving alpha row must be the freshly materialized one (populated
	// Server/Daemon from the manifest, refreshed port/command), not the stale
	// blank row.
	got := alphaRows[0]
	if got.Server != "demo" || got.Daemon != "alpha" {
		t.Errorf("surviving alpha row has blank/stale identity fields %+v; want Server=demo Daemon=alpha (fresh row)", got)
	}
	if got.Command == "stale-blank-alpha" || got.Port != 9211 {
		t.Errorf("surviving alpha row was not refreshed from the manifest: %+v", got)
	}

	// The unselected sibling beta is preserved verbatim.
	var betaRows []SupervisorDaemon
	for _, d := range written.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == `\mcp-local-hub-demo-beta` {
			betaRows = append(betaRows, d)
		}
	}
	if len(betaRows) != 1 {
		t.Fatalf("unselected sibling beta = %d rows, want exactly 1 (preserved verbatim); rows=%+v", len(betaRows), written.Daemons)
	}
	if betaRows[0].Command != "preserve-beta" || betaRows[0].Port != 9992 {
		t.Errorf("unselected sibling beta not preserved verbatim: %+v", betaRows[0])
	}
}

// TestBuildMergedSupervisorIntent_FilteredInstall_PreservesSiblingStop is the
// bot PR #284 P2 (finding 2) regression for the FILTERED path: master's
// 4ed263d carries prior.Stops verbatim into the merged file. This locks that
// the Stops preservation COMPOSES with the daemonFilter kept-loop — a
// user-stopped SIBLING daemon (beta stopped, filtered install of alpha) keeps
// BOTH its descriptor row AND its stop tombstone. If the fix had been written
// only for the full-install branch, the filtered branch would silently respawn
// the stopped sibling on the next reconcile.
//
// Driven directly against buildMergedSupervisorIntent (like the
// PreservesStopsSubBlock test above) so the assertion isolates the merge
// contract from scheduler/preflight noise.
func TestBuildMergedSupervisorIntent_FilteredInstall_PreservesSiblingStop(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	betaStop := DaemonIntent{
		Desired:   IntentDesiredStopped,
		Reason:    IntentReasonUserStop,
		UpdatedAt: time.Now().UTC(),
	}
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Command: "stale-alpha", Port: 9991},
			{TaskName: `\mcp-local-hub-demo-beta`, Server: "demo", Daemon: "beta", Command: "preserve-beta", Port: 9992},
		},
		Stops: map[string]DaemonIntent{
			// Operator stopped the SIBLING beta. The filtered install of alpha
			// must not disturb beta's descriptor OR its stop tombstone.
			`\mcp-local-hub-demo-beta`: betaStop,
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := globalTwoDaemonManifest() // alpha (9211) + beta (9212)
	merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "alpha", io.Discard)
	if err != nil {
		t.Fatalf("buildMergedSupervisorIntent(filtered alpha): %v", err)
	}

	// Sibling beta's stop tombstone survives the filtered install.
	got, ok := merged.Stops[`\mcp-local-hub-demo-beta`]
	if !ok {
		t.Fatalf("filtered install DROPPED the sibling beta stop tombstone — beta would respawn on the next reconcile despite the operator stop")
	}
	if got.Desired != betaStop.Desired || got.Reason != betaStop.Reason || !got.UpdatedAt.Equal(betaStop.UpdatedAt) {
		t.Fatalf("sibling stop tombstone mutated by the filtered merge: got %+v want %+v", got, betaStop)
	}

	// Sibling beta's DESCRIPTOR is also preserved verbatim (not just the stop).
	byKey := map[string]SupervisorDaemon{}
	for _, d := range merged.Daemons {
		byKey[canonicalIntentTaskKey(d.TaskName)] = d
	}
	beta, ok := byKey[`\mcp-local-hub-demo-beta`]
	if !ok {
		t.Fatalf("filtered install dropped the sibling beta descriptor; rows=%+v", merged.Daemons)
	}
	if beta.Command != "preserve-beta" || beta.Port != 9992 {
		t.Errorf("sibling beta descriptor not preserved verbatim: %+v", beta)
	}

	// The selected alpha row was refreshed from the manifest.
	alpha, ok := byKey[`\mcp-local-hub-demo-alpha`]
	if !ok {
		t.Fatalf("filtered install dropped the selected alpha descriptor; rows=%+v", merged.Daemons)
	}
	if alpha.Command == "stale-alpha" || alpha.Port != 9211 {
		t.Errorf("selected alpha row not refreshed from manifest: %+v", alpha)
	}
}

// TestBuildMergedSupervisorIntent_FullInstall_BlankServerRowNotDuplicated is the
// bot PR #288 F4 regression: on a FULL install (daemonFilter=="") the kept-loop
// must drop a legacy / older-writer row whose Server field is BLANK but whose
// canonical TaskName belongs to this server (`\mcp-local-hub-demo-alpha`). The
// #284 fix added the TaskName fallback ONLY for the filtered path; the full
// path matched only `d.Server == m.Name`, so a blank-Server row survived AND
// the fresh row appended -> DUPLICATE task_name entries. supervisorIntentRowOwnedBy's
// ParseManagedTaskName fallback closes this.
//
// Negative-control: revert the full-install branch to the field-only
// `d.Server == m.Name` match and this test fails — TWO rows keyed
// `\mcp-local-hub-demo-alpha` survive (verified by running the test against the
// pre-fix code).
//
// Driven directly against buildMergedSupervisorIntent (like the filtered
// blank-row precedent) so the assertion isolates the merge contract.
func TestBuildMergedSupervisorIntent_FullInstall_BlankServerRowNotDuplicated(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Legacy/older-writer row: task_name IS demo/alpha but BOTH Server
			// AND Daemon fields are blank. The full-install field-only filter
			// (d.Server == m.Name) cannot see this is owned by demo.
			{TaskName: `\mcp-local-hub-demo-alpha`, Command: "stale-blank-alpha", Port: 9991},
			// Sibling server's row must survive untouched.
			{TaskName: `\mcp-local-hub-other-d`, Server: "other", Daemon: "d", Command: "preserve-other", Port: 9993},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := globalTwoDaemonManifest() // demo: alpha (9211) + beta (9212)
	merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "", io.Discard)
	if err != nil {
		t.Fatalf("buildMergedSupervisorIntent(full install): %v", err)
	}

	// CORE assertion: exactly ONE row keyed to demo/alpha. Pre-fix the
	// blank-Server stale row + the fresh row both carry this key.
	var alphaRows []SupervisorDaemon
	for _, d := range merged.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == `\mcp-local-hub-demo-alpha` {
			alphaRows = append(alphaRows, d)
		}
	}
	if len(alphaRows) != 1 {
		t.Fatalf("full install produced %d rows for task \\mcp-local-hub-demo-alpha, want exactly 1 (no duplicate from the blank-Server legacy row); rows=%+v", len(alphaRows), merged.Daemons)
	}
	// The surviving alpha row must be the freshly materialized one.
	if got := alphaRows[0]; got.Server != "demo" || got.Daemon != "alpha" || got.Command == "stale-blank-alpha" || got.Port != 9211 {
		t.Errorf("surviving alpha row is stale/blank, not the fresh manifest row: %+v", got)
	}

	// Sibling server's row preserved verbatim.
	var otherRows []SupervisorDaemon
	for _, d := range merged.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == `\mcp-local-hub-other-d` {
			otherRows = append(otherRows, d)
		}
	}
	if len(otherRows) != 1 {
		t.Fatalf("sibling other/d = %d rows, want exactly 1 (preserved verbatim); rows=%+v", len(otherRows), merged.Daemons)
	}
	if otherRows[0].Command != "preserve-other" || otherRows[0].Port != 9993 {
		t.Errorf("sibling other/d not preserved verbatim: %+v", otherRows[0])
	}
}

// TestBuildMergedSupervisorIntent_FullInstall_BlankServerHyphenatedDaemonRowNotDuplicated
// is the bot PR #288 r19 F1 regression: legacy blank-Server rows must be
// matched by the manifest server prefix, not by ParseManagedTaskName's
// last-hyphen split. A row for server demo / daemon alpha-beta used to parse as
// server demo-alpha / daemon beta, so a full reinstall preserved the stale row
// and appended a duplicate fresh descriptor.
//
// Negative-control: with the ParseManagedTaskName fallback this test fails with
// two rows keyed \mcp-local-hub-demo-alpha-beta.
func TestBuildMergedSupervisorIntent_FullInstall_BlankServerHyphenatedDaemonRowNotDuplicated(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-demo-alpha-beta`, Command: "stale-blank-alpha-beta", Port: 9991},
			{TaskName: `\mcp-local-hub-other-alpha-beta`, Server: "other", Daemon: "alpha-beta", Command: "preserve-other", Port: 9993},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons: []config.DaemonSpec{
			{Name: "alpha-beta", Port: 33013},
		},
	}
	merged, _, _, err := NewAPI().buildMergedSupervisorIntent(m, intentPath, nil, "", io.Discard)
	if err != nil {
		t.Fatalf("buildMergedSupervisorIntent(full install): %v", err)
	}

	var alphaBetaRows []SupervisorDaemon
	for _, d := range merged.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == `\mcp-local-hub-demo-alpha-beta` {
			alphaBetaRows = append(alphaBetaRows, d)
		}
	}
	if len(alphaBetaRows) != 1 {
		t.Fatalf("full install produced %d rows for task \\mcp-local-hub-demo-alpha-beta, want exactly 1; rows=%+v", len(alphaBetaRows), merged.Daemons)
	}
	if got := alphaBetaRows[0]; got.Server != "demo" || got.Daemon != "alpha-beta" || got.Command == "stale-blank-alpha-beta" || got.Port != 33013 {
		t.Errorf("surviving alpha-beta row is stale/blank, not the fresh manifest row: %+v", got)
	}

	var otherRows []SupervisorDaemon
	for _, d := range merged.Daemons {
		if canonicalIntentTaskKey(d.TaskName) == `\mcp-local-hub-other-alpha-beta` {
			otherRows = append(otherRows, d)
		}
	}
	if len(otherRows) != 1 {
		t.Fatalf("sibling other/alpha-beta = %d rows, want exactly 1 (preserved verbatim); rows=%+v", len(otherRows), merged.Daemons)
	}
	if otherRows[0].Command != "preserve-other" || otherRows[0].Port != 9993 {
		t.Errorf("sibling other/alpha-beta not preserved verbatim: %+v", otherRows[0])
	}
}
