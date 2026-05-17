// Package cli — production reconcile wiring tests.
//
// Spec §"Reconcile loop" + plan Task 7.1 production wiring.
//
// These tests pin the runSupervise → NewReconciler wiring contract:
// when supervisor-intent.json contains daemon descriptors, runSupervise
// MUST construct a Reconciler with production spawn/terminate closures
// and invoke Reconcile against the parsed intent BEFORE flipping
// reconcileReady.Store(true).
//
// Why the wiring needs a test: the v0.5.0 Phase 6 / Task 6.2 stub
// loaded supervisor-intent.json and discarded the parsed result (only
// flipping intentFilesLoaded.Store(true) for IPC observability). The
// production fix wires the parsed intent through NewReconciler so the
// daemons described in intent are actually spawned. Without this test
// a future refactor could regress back to "intent parsed, never
// reconciled" and the supervisor would sit idle.
//
// Test seam: reconcileSpawnFn / reconcileTerminateFn are package-
// private package vars set via setReconcileSpawnFnForTest /
// setReconcileTerminateFnForTest. Production callers MUST NOT reassign
// them; the seam exists so these tests can capture spawn fan-out
// without launching real child processes.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

const reconcileWiringTestTaskName = `\mcp-local-hub-memory-default`

// TestRunSupervise_SpawnsDaemonsFromIntent verifies that runSupervise
// invokes the production reconciler against the parsed supervisor
// intent. The test seeds supervisor-intent.json with one descriptor
// and installs a fake spawn closure; after the supervisor reaches
// reconcile-ready, the fake must have been called exactly once with
// that descriptor's task name.
//
// Wiring proof: the test fake's call counter — not the audit log —
// is the canonical signal. The production spawn fn (which emits the
// daemon-spawned audit event) is replaced by the fake when the seam
// is installed, so audit-log inspection would conflate "wiring fired"
// with "production spawn fn emitted." See
// TestProductionSpawnFn_FailureEmitsAuditEvent for the audit-event
// assertion against the production fn directly.
func TestRunSupervise_SpawnsDaemonsFromIntent(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	// Seed supervisor-intent.json with one descriptor. Command is a
	// no-op the spawn closure never executes — the fake captures the
	// call before any os/exec invocation.
	intentPath := filepath.Join(tmpHome, "supervisor-intent.json")
	intent := &api.SupervisorIntentFile{
		Version:   1,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: reconcileWiringTestTaskName,
				Server:   "memory",
				Daemon:   "default",
				Command:  "fake-noop-for-test",
				Args:     []string{"--noop"},
				Port:     9121,
			},
		},
	}
	if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	var (
		spawnCalled atomic.Int32
		spawnedName atomic.Value // string
	)
	cleanupSpawn := setReconcileSpawnFnForTest(func(d api.SupervisorDaemon) error {
		spawnCalled.Add(1)
		spawnedName.Store(d.TaskName)
		return nil
	})
	defer cleanupSpawn()

	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)
	defer cleanupExit()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for spawn fan-out. Production wiring runs Reconcile BEFORE
	// reconcileReady.Store(true), so observing a spawn call is the
	// definitive "wiring fired" signal.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if spawnCalled.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	exitCh <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervise did not exit on test-exit signal within 3s")
	}

	if spawnCalled.Load() != 1 {
		t.Fatalf("expected fake spawn to be called exactly once; got %d", spawnCalled.Load())
	}
	if name, _ := spawnedName.Load().(string); name != reconcileWiringTestTaskName {
		t.Fatalf("expected spawn task_name=%q, got %q", reconcileWiringTestTaskName, name)
	}

	// reconcile-ready must fire AFTER the spawn pass — the audit log
	// preserves emission order across both events.
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"reconcile-ready"`) {
		t.Fatalf("reconcile-ready event missing from audit log:\n%s", logStr)
	}
}

// TestRunSupervise_NoIntentNoSpawn verifies that runSupervise does
// NOT invoke the spawn closure when supervisor-intent.json is absent.
// This is the fresh-install / first-boot path: no descriptors means
// no daemons to start, and the supervisor must still reach
// reconcile-ready cleanly.
func TestRunSupervise_NoIntentNoSpawn(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	var spawnCalled atomic.Int32
	cleanupSpawn := setReconcileSpawnFnForTest(func(d api.SupervisorDaemon) error {
		spawnCalled.Add(1)
		return nil
	})
	defer cleanupSpawn()

	exitCh := make(chan struct{}, 1)
	cleanupExit := setSuperviseTestExitCh(exitCh)
	defer cleanupExit()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for the reconcile-ready event so we know runSupervise
	// reached the reconcile checkpoint with intent==nil.
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(eventsPath); err == nil && strings.Contains(string(data), `"event":"reconcile-ready"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	exitCh <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervise exited with err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervise did not exit on test-exit signal within 3s")
	}

	if spawnCalled.Load() != 0 {
		t.Fatalf("fake spawn must NOT fire on empty intent; got %d call(s)", spawnCalled.Load())
	}
}

// TestProductionSpawnFn_FailureEmitsAuditEvent verifies the
// production spawn fn (makeProductionSpawnFn) emits a
// daemon-spawn-failed event on cmd.Start failure. This test does NOT
// go through the runSupervise test seam — it exercises the production
// closure directly with a deliberately bad command path so cmd.Start
// fails inside StartWithJob / cmd.Start without ever forking a child.
//
// Why a direct test: the spawn seam replaces the production fn so the
// audit-event emit path can only be exercised against the production
// closure itself. This test pins the daemon-spawn-failed contract;
// TestRunSupervise_SpawnsDaemonsFromIntent above pins the wiring
// contract.
func TestProductionSpawnFn_FailureEmitsAuditEvent(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	// nil job → spawn fn falls back to plain cmd.Start (Windows: the
	// Job Object is best-effort; nil job is the audit-row codepath
	// when NewKillOnCloseJob itself failed).
	spawnFn := makeProductionSpawnFn(nil, events)

	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		// A command that does not resolve on PATH and does not exist
		// as an absolute path — guarantees cmd.Start() fails before
		// any child is forked.
		Command: filepath.Join(tmpHome, "this-binary-definitely-does-not-exist-on-disk"),
		Args:    []string{},
	}

	err = spawnFn(descriptor)
	if err == nil {
		t.Fatal("expected spawn fn to return error for nonexistent command, got nil")
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-spawn-failed"`) {
		t.Fatalf("daemon-spawn-failed event missing from audit log:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"task_name":"\\mcp-local-hub-memory-default"`) {
		t.Fatalf("daemon task_name missing from audit log:\n%s", logStr)
	}
}

// TestProductionSpawnFn_SuccessEmitsAuditEvent verifies the
// production spawn fn emits a daemon-spawned event on successful
// cmd.Start. Uses a platform-portable no-op shell built-in
// (Windows: cmd.exe /c echo; POSIX: true) so the child exits almost
// immediately and does not leak between tests.
func TestProductionSpawnFn_SuccessEmitsAuditEvent(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	spawnFn := makeProductionSpawnFn(nil, events)

	command, args := portableNoopCommand()
	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  command,
		Args:     args,
	}

	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on noop command: %v", err)
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-spawned"`) {
		t.Fatalf("daemon-spawned event missing from audit log:\n%s", logStr)
	}
}