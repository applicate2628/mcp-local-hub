package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
		if err := executeInstallTo(&buf, m, plan, 0, false, nil, false); err != nil {
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
		if err := executeInstallTo(&buf, m, plan, 0, true, nil, false); err != nil {
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
		if err := executeInstallTo(&buf, m, plan, 0, true, nil, false); err != nil {
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
