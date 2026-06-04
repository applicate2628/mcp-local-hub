package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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

func TestRestartAllFallsBackToSchedulerWhenSupervisorRespawnFails(t *testing.T) {
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
	if len(fake.runNames) != 1 || fake.runNames[0] != taskName {
		t.Fatalf("scheduler fallback Run calls = %v, want [%s]", fake.runNames, taskName)
	}
	if len(results) != 2 || results[0].Err == "" || results[1].Err != "" {
		t.Fatalf("results = %+v, want supervisor failure row followed by scheduler success", results)
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
