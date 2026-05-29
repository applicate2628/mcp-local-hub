package api

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
func (f *fakeScheduler) List(string) ([]scheduler.TaskStatus, error) { return nil, nil }

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
		if err := executeInstallTo(&buf, m, plan, 0, false, nil); err != nil {
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
		if err := executeInstallTo(&buf, m, plan, 0, true, nil); err != nil {
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
		if err := executeInstallTo(&buf, m, plan, 0, true, nil); err != nil {
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

// TestInstallParsedManifest_StartAfterWriteTrue_PreservesLegacyBehavior is
// the regression guard: an explicit StartAfterWrite=true must drive Pass B
// so logon-triggered tasks start in-process. Uses a global manifest
// (which has logon tasks) so the Pass-B Run is observable.
func TestInstallParsedManifest_StartAfterWriteTrue_PreservesLegacyBehavior(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:          &buf,
		StartAfterWrite: true,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}
	if intentPath == "" {
		t.Fatal("InstallParsedManifest returned empty intent path")
	}
	if f.createCount != 3 {
		t.Errorf("Create calls = %d, want 3", f.createCount)
	}
	if f.runCount != 2 {
		t.Errorf("Run calls = %d, want 2 (StartAfterWrite=true must drive Pass B)", f.runCount)
	}
	// supervisor-intent.json must exist at the returned path.
	if _, statErr := os.Stat(intentPath); statErr != nil {
		t.Errorf("supervisor-intent.json not written at %s: %v", intentPath, statErr)
	}
}

// TestInstallParsedManifest_StartAfterWriteFalse_DefersDaemonSpawn asserts
// the default deferred path: StartAfterWrite=false (the zero value) creates
// tasks + writes intent but never calls sch.Run (daemon spawn deferred to
// the supervisor reconciler).
func TestInstallParsedManifest_StartAfterWriteFalse_DefersDaemonSpawn(t *testing.T) {
	daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest()
	a := NewAPI()
	var buf bytes.Buffer
	intentPath, err := a.InstallParsedManifest(context.Background(), m, InstallParsedManifestOpts{
		Writer:          &buf,
		StartAfterWrite: false,
	})
	if err != nil {
		t.Fatalf("InstallParsedManifest: %v", err)
	}
	if f.createCount != 3 {
		t.Errorf("Create calls = %d, want 3 (tasks still created when spawn deferred)", f.createCount)
	}
	if f.runCount != 0 {
		t.Errorf("Run calls = %d, want 0 (StartAfterWrite=false defers daemon spawn)", f.runCount)
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
		Writer:          &buf,
		StartAfterWrite: false,
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
	// fan-out helper filters on it.
	wsAlpha := "C:/work/alpha"
	wsBeta := "C:/work/beta"
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
		Writer:          &buf,
		Workspaces:      workspaces,
		StartAfterWrite: false,
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
	}
	if len(seenTask) != 2 {
		t.Errorf("matched serena task names = %d, want 2 (both workspaces)", len(seenTask))
	}

	// StartAfterWrite=false: no scheduler Run; the template manifest also
	// produces no scheduler Create tasks.
	if f.runCount != 0 {
		t.Errorf("Run calls = %d, want 0 (StartAfterWrite=false)", f.runCount)
	}
}

// TestInstallParsedManifest_DryRun_NoWriteNoPath is the FIX-1 regression
// guard: a dry run must (a) leave the state dir pristine — no committed
// supervisor-intent.json AND no leftover ".preflight" temp from a
// pre-flight probe that should be skipped on dry-run — and (b) return an
// empty intent path so the caller never dereferences a path for a file
// that was never written. Uses a global manifest (which has logon tasks)
// so the dry-run short-circuit is exercised on a non-trivial plan; the
// fake scheduler must see zero Create / Run calls because installPlan
// short-circuits to the plan print before any mutation.
func TestInstallParsedManifest_DryRun_NoWriteNoPath(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	f := newInstallFakeScheduler()
	installFakeScheduler(t, f)

	m := globalTwoDaemonManifest()
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
	// The dry-run plan print still happens.
	if buf.Len() == 0 {
		t.Error("DryRun must still print the plan to the writer, got empty output")
	}
	// No scheduler mutation on dry-run.
	if f.createCount != 0 || f.runCount != 0 {
		t.Errorf("DryRun must not mutate the scheduler: createCount=%d runCount=%d, want 0/0", f.createCount, f.runCount)
	}
}
