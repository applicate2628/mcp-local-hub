// Package cli — production reconcile wiring tests.
//
// Spec §"Reconcile loop" + plan Task 7.1 production wiring.
//
// These tests pin the runSupervise → NewReconciler wiring contract:
// when supervisor-intent.json contains daemon descriptors, runSupervise
// MUST construct a Reconciler with production spawn/terminate closures
// and schedule Reconcile against the parsed intent before the supervisor
// reports reconcile-ready.
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
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"

	"github.com/gofrs/flock"
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

	// Wait for spawn fan-out. Reconcile is scheduled asynchronously, so
	// the fake spawn call is the definitive "wiring fired" signal.
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

	// reconcile-ready must fire once startup schedules the reconcile pass.
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

func TestRunSupervise_MalformedDaemonIntentFatalBeforeReady(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

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
	if err := os.WriteFile(filepath.Join(tmpHome, "daemon-intent.json"), []byte(`{"tasks":{"x":{"desired":"stopped","reason":"user-disabled","updated_at":"not-a-time"}}}`), 0o600); err != nil {
		t.Fatalf("seed malformed daemon-intent.json: %v", err)
	}

	var spawnCalled atomic.Int32
	cleanupSpawn := setReconcileSpawnFnForTest(func(d api.SupervisorDaemon) error {
		spawnCalled.Add(1)
		return nil
	})
	defer cleanupSpawn()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected malformed daemon-intent.json to abort supervise startup, got nil")
	}
	if spawnCalled.Load() != 0 {
		t.Fatalf("spawn must not fire after malformed daemon-intent.json; got %d call(s)", spawnCalled.Load())
	}

	logRaw, readErr := os.ReadFile(filepath.Join(tmpHome, "supervisor-events.log"))
	if readErr != nil {
		t.Fatalf("read events log: %v", readErr)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-intent-parse-failed"`) {
		t.Fatalf("daemon-intent-parse-failed event missing from audit log:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"event":"supervise-startup-failed"`) {
		t.Fatalf("supervise-startup-failed event missing from audit log:\n%s", logStr)
	}
	if strings.Contains(logStr, `"event":"ipc-listener-bound"`) {
		t.Fatalf("IPC listener must not bind on fatal daemon-intent parse error:\n%s", logStr)
	}
	if strings.Contains(logStr, `"event":"reconcile-ready"`) {
		t.Fatalf("reconcile-ready must not be emitted on fatal daemon-intent parse error:\n%s", logStr)
	}
}

func TestRunSupervise_MalformedSupervisorIntentFatalBeforeReady(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	if err := os.WriteFile(filepath.Join(tmpHome, "supervisor-intent.json"), []byte(`{"version":1,"daemons":[`), 0o600); err != nil {
		t.Fatalf("seed malformed supervisor-intent.json: %v", err)
	}

	var spawnCalled atomic.Int32
	cleanupSpawn := setReconcileSpawnFnForTest(func(d api.SupervisorDaemon) error {
		spawnCalled.Add(1)
		return nil
	})
	defer cleanupSpawn()

	cmd := newSuperviseCmd()
	cmd.SetArgs([]string{"--no-ipc"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected malformed supervisor-intent.json to abort supervise startup, got nil")
	}
	if spawnCalled.Load() != 0 {
		t.Fatalf("spawn must not fire after malformed supervisor-intent.json; got %d call(s)", spawnCalled.Load())
	}

	logRaw, readErr := os.ReadFile(filepath.Join(tmpHome, "supervisor-events.log"))
	if readErr != nil {
		t.Fatalf("read events log: %v", readErr)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"supervisor-intent-read-failed"`) {
		t.Fatalf("supervisor-intent-read-failed event missing from audit log:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"event":"supervise-startup-failed"`) {
		t.Fatalf("supervise-startup-failed event missing from audit log:\n%s", logStr)
	}
	if strings.Contains(logStr, `"event":"reconcile-ready"`) {
		t.Fatalf("reconcile-ready must not be emitted on fatal supervisor-intent parse error:\n%s", logStr)
	}
}

func TestLoadIntentFiles_MalformedDaemonIntentReturnsError(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	if err := os.WriteFile(filepath.Join(tmpHome, "daemon-intent.json"), []byte(`{"tasks":{"x":{"desired":"bogus","reason":"user-disabled","updated_at":"2026-05-17T00:00:00Z"}}}`), 0o600); err != nil {
		t.Fatalf("seed malformed daemon-intent.json: %v", err)
	}

	var loaded atomic.Bool
	_, _, err = loadIntentFiles(tmpHome, events, &loaded)
	if err == nil {
		t.Fatal("expected loadIntentFiles to return an error for malformed daemon-intent.json")
	}
	if loaded.Load() {
		t.Fatal("intentFilesLoaded must remain false when daemon-intent.json fails strict parse")
	}

	logRaw, readErr := os.ReadFile(eventsPath)
	if readErr != nil {
		t.Fatalf("read events log: %v", readErr)
	}
	if !strings.Contains(string(logRaw), `"event":"daemon-intent-parse-failed"`) {
		t.Fatalf("daemon-intent-parse-failed event missing from audit log:\n%s", string(logRaw))
	}
}

func TestLoadIntentFiles_RespectsDaemonIntentFlock(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	intentPath := filepath.Join(tmpHome, "daemon-intent.json")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(intentPath, []byte(`{"tasks":{"x":{"desired":"stopped","reason":"user-disabled","updated_at":"`+now+`"}}}`), 0o600); err != nil {
		t.Fatalf("seed daemon-intent.json: %v", err)
	}

	holder := flock.New(intentPath + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatalf("hold daemon-intent lock: %v", err)
	}
	defer holder.Unlock()

	prevTimeout := daemonIntentReadLockTimeout
	daemonIntentReadLockTimeout = 50 * time.Millisecond
	defer func() { daemonIntentReadLockTimeout = prevTimeout }()

	var loaded atomic.Bool
	_, _, err = loadIntentFiles(tmpHome, events, &loaded)
	if err == nil {
		t.Fatal("expected loadIntentFiles to fail closed when daemon-intent flock is held")
	}
	if loaded.Load() {
		t.Fatal("intentFilesLoaded must remain false when daemon-intent lock cannot be acquired")
	}

	raw, readErr := os.ReadFile(eventsPath)
	if readErr != nil {
		t.Fatalf("read events log: %v", readErr)
	}
	if !strings.Contains(string(raw), `"event":"daemon-intent-read-failed"`) {
		t.Fatalf("daemon-intent-read-failed event missing from audit log:\n%s", string(raw))
	}
}

func TestRunSupervise_ReconcileReadyBeforeSpawnCompletes(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

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
	if err := api.WriteSupervisorIntent(filepath.Join(tmpHome, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}

	spawnEntered := make(chan struct{})
	releaseSpawn := make(chan struct{})
	var spawnDone atomic.Bool
	cleanupSpawn := setReconcileSpawnFnForTest(func(d api.SupervisorDaemon) error {
		close(spawnEntered)
		<-releaseSpawn
		spawnDone.Store(true)
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
	defer func() {
		select {
		case exitCh <- struct{}{}:
		default:
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Log("supervise did not exit during cleanup")
		}
	}()

	select {
	case <-spawnEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("spawn did not enter before timeout")
	}

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(eventsPath)
		if strings.Contains(string(raw), `"event":"reconcile-ready"`) {
			if spawnDone.Load() {
				t.Fatal("test lost async window: spawn completed before reconcile-ready assertion")
			}
			close(releaseSpawn)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(releaseSpawn)
	t.Fatal("reconcile-ready was not emitted while startup spawn was blocked")
}

func TestLoadSupervisorCurrentRunning_SkipsDeadPID(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	pid := exitedProcessPID(t)
	if process.IsPidAlive(pid) {
		t.Fatalf("test precondition failed: pid %d is still alive after Wait", pid)
	}

	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			reconcileWiringTestTaskName: {
				State:      "running",
				CurrentPID: pid,
			},
		},
	}
	if err := api.WriteSupervisorState(filepath.Join(tmpHome, "supervisor-state.json"), state); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	got, gotPIDs, err := loadSupervisorCurrentRunning(tmpHome)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if got[reconcileWiringTestTaskName] {
		t.Fatalf("dead pid %d must not suppress startup spawn; currentRunning=%v", pid, got)
	}
	if len(got) != 0 {
		t.Fatalf("expected no current-running entries for stale state, got %v", got)
	}
	if len(gotPIDs) != 0 {
		t.Fatalf("expected no running PID entries for stale state, got %v", gotPIDs)
	}
}

func TestLoadSupervisorCurrentRunning_FatalOnReadError(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	if err := os.WriteFile(filepath.Join(tmpHome, "supervisor-state.json"), []byte(`{"version":1,"daemons":`), 0o600); err != nil {
		t.Fatalf("seed corrupt supervisor-state.json: %v", err)
	}
	got, gotPIDs, err := loadSupervisorCurrentRunning(tmpHome)
	if err == nil {
		t.Fatal("expected corrupt supervisor-state.json to return fatal startup error")
	}
	if len(got) != 0 || len(gotPIDs) != 0 {
		t.Fatalf("corrupt state must not produce running entries: currentRunning=%v pids=%v", got, gotPIDs)
	}
}

func TestLoadSupervisorCurrentRunning_RequiresStartTimeProof(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("native PID start-time proof exists only on Windows/Linux")
	}
	tmpHome := apitest.HardenedTempDir(t)
	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			reconcileWiringTestTaskName: {
				State:         "running",
				CurrentPID:    os.Getpid(),
				PIDGeneration: 1,
				StartedAt:     "2000-01-01T00:00:00Z",
			},
		},
	}
	if err := api.WriteSupervisorState(filepath.Join(tmpHome, "supervisor-state.json"), state); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	got, gotPIDs, err := loadSupervisorCurrentRunning(tmpHome)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if got[reconcileWiringTestTaskName] || len(gotPIDs) != 0 {
		t.Fatalf("mismatched started_at must fail closed: currentRunning=%v pids=%v", got, gotPIDs)
	}
}

func TestLoadSupervisorCurrentRunning_UnsupportedIdentityFallsBackToAlivePID(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	const pid = 4242
	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			reconcileWiringTestTaskName: {
				State:         "running",
				CurrentPID:    pid,
				PIDGeneration: 1,
				StartedAt:     "2026-05-18T02:42:47Z",
			},
		},
	}
	if err := api.WriteSupervisorState(filepath.Join(tmpHome, "supervisor-state.json"), state); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		if proof.PID != pid {
			t.Fatalf("VerifyPIDIdentity pid = %d, want %d", proof.PID, pid)
		}
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(gotPID int) bool {
		return gotPID == pid
	}
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	got, gotPIDs, err := loadSupervisorCurrentRunning(tmpHome)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if !got[reconcileWiringTestTaskName] {
		t.Fatalf("unsupported identity with live pid must preserve current-running entry: %v", got)
	}
	if gotPIDs[reconcileWiringTestTaskName].PID != pid {
		t.Fatalf("running PID snapshot = %+v, want pid %d", gotPIDs[reconcileWiringTestTaskName], pid)
	}
}

func TestLoadSupervisorCurrentRunning_UnsupportedIdentitySkipsDeadPID(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	const pid = 4242
	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			reconcileWiringTestTaskName: {
				State:         "running",
				CurrentPID:    pid,
				PIDGeneration: 1,
				StartedAt:     "2026-05-18T02:42:47Z",
			},
		},
	}
	if err := api.WriteSupervisorState(filepath.Join(tmpHome, "supervisor-state.json"), state); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error {
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(int) bool {
		return false
	}
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	got, gotPIDs, err := loadSupervisorCurrentRunning(tmpHome)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if len(got) != 0 || len(gotPIDs) != 0 {
		t.Fatalf("unsupported identity with dead pid must not produce running entries: currentRunning=%v pids=%v", got, gotPIDs)
	}
}

func TestLoadSupervisorCurrentRunning_IdentityMismatchStillFailsClosed(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	state := &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			reconcileWiringTestTaskName: {
				State:         "running",
				CurrentPID:    4242,
				PIDGeneration: 1,
				StartedAt:     "2026-05-18T02:42:47Z",
			},
		},
	}
	if err := api.WriteSupervisorState(filepath.Join(tmpHome, "supervisor-state.json"), state); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(process.PIDIdentityProof) error {
		return process.ErrProcessIdentityMismatch
	}
	currentRunningIsPIDAliveFn = func(int) bool {
		t.Fatal("liveness fallback must not run for supported identity mismatch")
		return true
	}
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	got, gotPIDs, err := loadSupervisorCurrentRunning(tmpHome)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if len(got) != 0 || len(gotPIDs) != 0 {
		t.Fatalf("identity mismatch must stay fail-closed: currentRunning=%v pids=%v", got, gotPIDs)
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
	spawnFn := makeProductionSpawnFn(events, NewDaemonRuntimeTracker())

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

func TestProductionSpawnFn_TrackerFailureOnSpawnErr(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	spawnFn := makeProductionSpawnFn(events, tracker)
	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  filepath.Join(tmpHome, "this-binary-definitely-does-not-exist-on-disk"),
		Args:     []string{},
	}

	err = spawnFn(descriptor)
	if err == nil {
		t.Fatal("expected spawn fn to return error for nonexistent command, got nil")
	}
	entry, ok := tracker.Get(reconcileWiringTestTaskName)
	if !ok {
		t.Fatal("tracker entry missing after spawn failure")
	}
	if entry.State != daemonRuntimeStateBackoff || entry.CurrentPID != 0 || entry.LastError == "" {
		t.Fatalf("tracker entry after spawn failure = %+v, want backoff pid=0 last_error", entry)
	}
}

// TestAppendSupervisorIntentChannel_WinsOverMergedEnv is the bot PR #246 P2
// supervisor-side guard: the MCPHUB_SUPERVISOR_INTENT_PATH channel var must be
// appended LAST so it wins over any same-key entry a manifest/overlay merge
// produced (Go's exec honors the last occurrence of a duplicate key). This is
// the clobber-immunity property that keeps the serena CHILD's manifest env from
// redirecting the proxy's control-plane intent-path lookup.
func TestAppendSupervisorIntentChannel_WinsOverMergedEnv(t *testing.T) {
	const realPath = `C:\real\state\supervisor-intent.json`
	// Simulate a merged env where the serena CHILD's manifest env tried to set
	// HOME (and even a hostile/leftover intent-path) — the channel injection
	// must still place the canonical value LAST.
	merged := []string{
		"PATH=/usr/bin",
		"HOME=/child/overlay/home",
		api.SupervisorIntentPathEnvVar + "=/child/overlay/bogus-intent.json",
	}
	got := appendSupervisorIntentChannel(merged, realPath)

	// The LAST occurrence of the channel key must carry realPath.
	wantLast := api.SupervisorIntentPathEnvVar + "=" + realPath
	if got[len(got)-1] != wantLast {
		t.Fatalf("channel var must be appended last; got tail=%q want=%q\nfull=%v", got[len(got)-1], wantLast, got)
	}
	// HOME from the child env is preserved (the channel does NOT strip it — the
	// proxy reads the explicit channel var, not HOME, for the intent path).
	if !containsExact(got, "HOME=/child/overlay/home") {
		t.Errorf("child HOME must survive (it still applies to the serena child); got=%v", got)
	}
}

// TestAppendSupervisorIntentChannel_NilEnvMaterializesParent proves that when
// cmd.Env is nil (no manifest/overlay env → child inherits os.Environ()), the
// helper materializes the parent env so the appended channel var survives
// instead of replacing the whole inherited block with a single var.
func TestAppendSupervisorIntentChannel_NilEnvMaterializesParent(t *testing.T) {
	const realPath = `C:\real\state\supervisor-intent.json`
	got := appendSupervisorIntentChannel(nil, realPath)
	if len(got) <= 1 {
		t.Fatalf("nil cmd.Env must materialize os.Environ() before appending; got len=%d", len(got))
	}
	if got[len(got)-1] != api.SupervisorIntentPathEnvVar+"="+realPath {
		t.Fatalf("channel var must be the last entry; got=%q", got[len(got)-1])
	}
	// At least one inherited parent var should be present (os.Environ is
	// non-empty in any real test process).
	if len(got) < 2 {
		t.Fatal("expected inherited parent env entries alongside the channel var")
	}
}

// containsExact reports whether ss contains the exact string s.
func containsExact(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// readFileString reads path and returns its contents as a string, failing the
// test on error. Tiny test-local helper for the spawn-event-log assertions.
func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// readFileStringIfExists reads path and returns its contents, or "" when the
// file does not exist. The supervisor event log file is created lazily on the
// first Emit, so a test that asserts NO event was emitted must tolerate a
// never-created log file rather than treating its absence as a failure.
func readFileStringIfExists(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// serenaProxyArgs returns a canonical serena-proxy wrapper argv for the given
// task name + port. Mirrors what api.BuildSupervisorDaemonsForSerena emits.
func serenaProxyArgs(taskName string, port int) []string {
	return []string{
		"daemon", "serena-proxy",
		"--server", "serena",
		"--workspace", `C:\work\alpha`,
		"--port", strconv.Itoa(port),
		"--task-name", taskName,
	}
}

// TestReconcile_ExcludesLegacyNilSpecSerenaProxyFromDesiredSet is the bot
// PR #246 r2 P2 guard. r1 expressed the legacy-nil-spec serena-proxy skip as
// `return nil` INSIDE the spawn closure — but the production controller's
// executeSideEffect treats a nil spawn error as SUCCESS, posts EvHealthOK, and
// transitions the task StSpawning → StRunning, leaving a PHANTOM running daemon
// (no process started) in supervisor state + IPC. r2 moves the skip into the
// reconcile desired-set construction so the row is EXCLUDED before any EvStart /
// spawn fires: it is never spawned, never marked running, never churns backoff.
// This test drives Reconcile on the direct-spawn path (EventLoop nil → r.spawn
// is the desired-set sink) and asserts the legacy row never reaches spawn while
// the single warn event is emitted once at the exclusion point.
func TestReconcile_ExcludesLegacyNilSpecSerenaProxyFromDesiredSet(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const legacyTask = `\mcp-local-hub-serena-deadbeef`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:    legacyTask,
				Server:      "serena",
				Daemon:      "deadbeef",
				Command:     "fake-noop",
				Args:        serenaProxyArgs(legacyTask, 9121),
				RuntimeSpec: nil, // pre-redesign / stale row
			},
		},
	}

	spawned := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { return nil },
	)
	r.Events = events
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	if len(spawned) != 0 {
		t.Fatalf("legacy nil-spec serena-proxy row must be EXCLUDED from the desired set (never reach spawn); got spawned=%v", spawned)
	}
	logStr := readFileString(t, eventsPath)
	if !strings.Contains(logStr, `"event":"legacy-serena-descriptor-skipped"`) {
		t.Fatalf("exclusion must emit a legacy-serena-descriptor-skipped warn event:\n%s", logStr)
	}
	if !strings.Contains(logStr, legacyTask) {
		t.Fatalf("skip event must name the excluded task %q:\n%s", legacyTask, logStr)
	}
	// The warn must fire exactly ONCE per reconcile pass for the row (it is the
	// desired-set construction point, not a per-spawn loop).
	if n := strings.Count(logStr, `"event":"legacy-serena-descriptor-skipped"`); n != 1 {
		t.Fatalf("legacy-serena-descriptor-skipped must be emitted exactly once at the exclusion point; got %d:\n%s", n, logStr)
	}
}

// TestReconcile_LegacyRunningSerenaProxy_HonorsStop is the bot PR #246 r2 P2
// guard (supervise_reconcile.go:168). The legacy nil-spec exclusion is
// SPAWN-ONLY (gated on !running): a legacy serena-proxy row that is ALREADY
// RUNNING (e.g. hydrated from supervisor-state.json on a warm restart, or
// spawned by a pre-redesign supervisor) and marked stopped in daemon-intent.json
// must still reach the `isStopped && running` terminate branch — an unconditional
// exclusion would strand the live process so an operator stop could never stop it.
func TestReconcile_LegacyRunningSerenaProxy_HonorsStop(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const legacyTask = `\mcp-local-hub-serena-deadbeef`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName:    legacyTask,
			Server:      "serena",
			Daemon:      "deadbeef",
			Command:     "fake-noop",
			Args:        serenaProxyArgs(legacyTask, 9121),
			RuntimeSpec: nil, // pre-redesign / stale row
		}},
	}
	now := time.Now().UTC()
	daemonIntent := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			legacyTask: {Desired: api.IntentDesiredStopped, UpdatedAt: now},
		},
	}

	spawned, terminated := []string{}, []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { terminated = append(terminated, d.TaskName); return nil },
	)
	r.Events = events
	// currentRunning marks the legacy row RUNNING.
	r.Reconcile(intent, daemonIntent, map[string]bool{legacyTask: true}, now)

	if len(spawned) != 0 {
		t.Fatalf("a stopped row must not be spawned; got spawned=%v", spawned)
	}
	if len(terminated) != 1 || terminated[0] != legacyTask {
		t.Fatalf("a RUNNING legacy nil-spec serena-proxy marked stopped must be TERMINATED (not skipped); got terminated=%v", terminated)
	}
	// It is being terminated, NOT excluded — the spawn-exclusion skip warn must
	// NOT fire for a running row.
	logStr := readFileStringIfExists(t, eventsPath)
	if strings.Contains(logStr, `"event":"legacy-serena-descriptor-skipped"`) {
		t.Fatalf("a running row honored for stop must NOT emit the spawn-exclusion skip event:\n%s", logStr)
	}
}

// lspWorkspaceProxyArgs returns the flat argv an LSP workspace-proxy descriptor
// carries (the shape api.BuildSupervisorDaemonForLSP emits).
func lspWorkspaceProxyArgsForTest(port int, workspace, language string) []string {
	return []string{
		"daemon", "workspace-proxy",
		"--port", strconv.Itoa(port),
		"--workspace", workspace,
		"--language", language,
	}
}

func lspProxyDaemonForTest(taskName, workspace, language string, port int) api.SupervisorDaemon {
	return api.SupervisorDaemon{
		TaskName:  taskName,
		Server:    "mcp-language-server",
		Daemon:    "lsp-" + language,
		Command:   "fake-noop",
		Args:      lspWorkspaceProxyArgsForTest(port, workspace, language),
		Workspace: workspace,
		Port:      port,
	}
}

// TestReconcile_ExcludesOrphanedLSPProxyFromDesiredSet is the orphaned-LSP-
// daemon quarantine guard. `mcphub workspace unregister` could remove a
// (workspace_key, language) registry row WITHOUT removing the paired
// supervisor-intent descriptor, so the reconcile would spawn the now-unbacked
// LSP proxy, which loads the registry, misses its row, exits 1 "not registered",
// and churns through restart backoff into quarantine. With the
// LSPRegistryHasRow predicate reporting "no backing row", the descriptor MUST be
// EXCLUDED from the spawn-desired set (never reach spawn) and a single warn must
// fire at the exclusion point.
func TestReconcile_ExcludesOrphanedLSPProxyFromDesiredSet(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const orphanTask = `\mcp-local-hub-lsp-deadbeef-python`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			lspProxyDaemonForTest(orphanTask, `C:\work\alpha`, "python", 9200),
		},
	}

	spawned := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { return nil },
	)
	r.Events = events
	// Predicate reports NO backing registry row — the orphan case.
	r.LSPRegistryHasRow = func(d api.SupervisorDaemon) bool { return false }
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	if len(spawned) != 0 {
		t.Fatalf("orphaned LSP workspace-proxy row (no registry row) must be EXCLUDED from the desired set; got spawned=%v", spawned)
	}
	logStr := readFileString(t, eventsPath)
	if !strings.Contains(logStr, `"event":"orphaned-lsp-descriptor-skipped"`) {
		t.Fatalf("exclusion must emit an orphaned-lsp-descriptor-skipped warn event:\n%s", logStr)
	}
	if !strings.Contains(logStr, orphanTask) {
		t.Fatalf("skip event must name the excluded task %q:\n%s", orphanTask, logStr)
	}
	if n := strings.Count(logStr, `"event":"orphaned-lsp-descriptor-skipped"`); n != 1 {
		t.Fatalf("orphaned-lsp-descriptor-skipped must be emitted exactly once at the exclusion point; got %d:\n%s", n, logStr)
	}
}

// TestReconcile_SpawnsBackedLSPProxy proves the orphan exclusion is precise:
// an LSP workspace-proxy descriptor WHOSE registry row still exists (predicate
// returns true) must still spawn normally and emit no skip event.
func TestReconcile_SpawnsBackedLSPProxy(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const backedTask = `\mcp-local-hub-lsp-deadbeef-python`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			lspProxyDaemonForTest(backedTask, `C:\work\alpha`, "python", 9200),
		},
	}

	spawned := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { return nil },
	)
	r.Events = events
	// Predicate reports a backing registry row — the healthy case.
	r.LSPRegistryHasRow = func(d api.SupervisorDaemon) bool { return true }
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	if len(spawned) != 1 || spawned[0] != backedTask {
		t.Fatalf("a backed LSP workspace-proxy row must spawn normally; got spawned=%v", spawned)
	}
	logStr := readFileStringIfExists(t, eventsPath)
	if strings.Contains(logStr, `"event":"orphaned-lsp-descriptor-skipped"`) {
		t.Fatalf("a backed LSP row must NOT be excluded:\n%s", logStr)
	}
}

// TestReconcile_OrphanedLSPProxy_NilPredicateSpawns pins the nil-predicate
// default: when no LSPRegistryHasRow seam is wired (the legacy/test path), the
// exclusion is inert and the LSP row spawns — preserving the pre-fix
// spawn-everything behavior so the guard is opt-in via production wiring only.
func TestReconcile_OrphanedLSPProxy_NilPredicateSpawns(t *testing.T) {
	const lspTask = `\mcp-local-hub-lsp-deadbeef-python`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			lspProxyDaemonForTest(lspTask, `C:\work\alpha`, "python", 9200),
		},
	}

	spawned := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { return nil },
	)
	// LSPRegistryHasRow left nil — the guard must be inert.
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	if len(spawned) != 1 || spawned[0] != lspTask {
		t.Fatalf("nil-predicate path must spawn the LSP row (exclusion is opt-in); got spawned=%v", spawned)
	}
}

// TestReconcile_RunningOrphanedLSPProxy_HonorsStop pins that the orphan
// exclusion is SPAWN-ONLY (gated on !running): an orphaned LSP row that is
// ALREADY RUNNING and marked stopped in daemon-intent.json must still reach the
// `isStopped && running` terminate branch — an unconditional exclusion would
// strand the live proxy so an operator stop could never stop it.
func TestReconcile_RunningOrphanedLSPProxy_HonorsStop(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const lspTask = `\mcp-local-hub-lsp-deadbeef-python`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			lspProxyDaemonForTest(lspTask, `C:\work\alpha`, "python", 9200),
		},
	}
	now := time.Now().UTC()
	daemonIntent := &api.DaemonIntentFile{
		Tasks: map[string]api.DaemonIntent{
			lspTask: {Desired: api.IntentDesiredStopped, UpdatedAt: now},
		},
	}

	spawned, terminated := []string{}, []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { terminated = append(terminated, d.TaskName); return nil },
	)
	r.Events = events
	// Even though the row is orphaned (no registry row), it is RUNNING + stopped.
	r.LSPRegistryHasRow = func(d api.SupervisorDaemon) bool { return false }
	r.Reconcile(intent, daemonIntent, map[string]bool{lspTask: true}, now)

	if len(spawned) != 0 {
		t.Fatalf("a stopped row must not be spawned; got spawned=%v", spawned)
	}
	if len(terminated) != 1 || terminated[0] != lspTask {
		t.Fatalf("a RUNNING orphaned LSP proxy marked stopped must be TERMINATED (not excluded); got terminated=%v", terminated)
	}
	logStr := readFileStringIfExists(t, eventsPath)
	if strings.Contains(logStr, `"event":"orphaned-lsp-descriptor-skipped"`) {
		t.Fatalf("a running row honored for stop must NOT emit the spawn-exclusion skip event:\n%s", logStr)
	}
}

// TestReconcile_ExcludedLegacyRow_PostsNoEvStart proves the exclusion happens
// at the EventLoop path too: when an EventLoop is wired (the production A.2
// path), the excluded legacy row must NOT produce an EvStart event. EvStart is
// the sole source of the StSpawning → spawn → EvHealthOK → StRunning chain that
// would mark the phantom daemon running; excluding it here is the actual fix.
func TestReconcile_ExcludedLegacyRow_PostsNoEvStart(t *testing.T) {
	const legacyTask = `\mcp-local-hub-serena-deadbeef`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:    legacyTask,
				Server:      "serena",
				Daemon:      "deadbeef",
				Command:     "fake-noop",
				Args:        serenaProxyArgs(legacyTask, 9121),
				RuntimeSpec: nil,
			},
		},
	}

	loop := api.NewEventLoop(16)
	posted := []api.LoopEvent{}
	loop.RegisterHandler(func(ev api.LoopEvent) { posted = append(posted, ev) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go loop.Run(ctx)

	r := NewReconciler(
		func(d api.SupervisorDaemon) error {
			t.Fatalf("EventLoop path must not call spawn directly")
			return nil
		},
		func(d api.SupervisorDaemon) error { return nil },
	)
	r.EventLoop = loop
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	// Allow the loop to drain anything that WAS posted (none should be EvStart).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(posted) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, ev := range posted {
		if ev.Kind == api.EvStart && ev.TaskName == legacyTask {
			t.Fatalf("excluded legacy nil-spec serena-proxy row must NOT post EvStart; got %+v", posted)
		}
	}
}

// TestReconcile_SpecBearingSerenaProxy_StaysInDesiredSet is the positive
// control: a serena-proxy row WITH a RuntimeSpec is NOT excluded — it reaches
// the spawn sink normally. The exclusion is scoped to nil-RuntimeSpec
// serena-proxy rows only.
func TestReconcile_SpecBearingSerenaProxy_StaysInDesiredSet(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const task = `\mcp-local-hub-serena-cafef00d`
	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName: task,
				Server:   "serena",
				Daemon:   "cafef00d",
				Command:  "fake-noop",
				Args:     serenaProxyArgs(task, 9121),
				RuntimeSpec: &api.DaemonRuntimeSpec{
					SpecVersion:   api.DaemonRuntimeSpecVersion,
					ChildCommand:  "uvx",
					ChildArgs:     []string{"serena", "--project", `C:\work\alpha`, "--context", "codex-placeholder"},
					UpstreamPort:  19121,
					ExternalPort:  9121,
					WorkspacePath: `C:\work\alpha`,
				},
			},
		},
	}

	spawned := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { return nil },
	)
	r.Events = events
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	if len(spawned) != 1 || spawned[0] != task {
		t.Fatalf("spec-bearing serena-proxy must stay in the desired set and reach spawn; got %v", spawned)
	}
	// No warn event should fire for a spec-bearing row; the log file may not
	// even exist (no Emit ever happened), so tolerate a missing file.
	logStr := readFileStringIfExists(t, eventsPath)
	if strings.Contains(logStr, `"event":"legacy-serena-descriptor-skipped"`) {
		t.Fatalf("a spec-bearing serena-proxy row must NOT be excluded:\n%s", logStr)
	}
}

// TestReconcile_NilSpecGlobalDaemon_StaysInDesiredSet proves the exclusion is
// scoped to serena-proxy rows ONLY, not nil-RuntimeSpec rows in general. A
// global daemon (Args do NOT carry serena-proxy) with a nil RuntimeSpec is
// legitimately nil and must stay in the desired set.
func TestReconcile_NilSpecGlobalDaemon_StaysInDesiredSet(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:    reconcileWiringTestTaskName,
				Server:      "memory",
				Daemon:      "default",
				Command:     "fake-noop",
				Args:        []string{"daemon", "--server", "memory", "--daemon", "default"}, // NOT serena-proxy
				RuntimeSpec: nil,                                                             // legitimately nil for a global daemon
			},
		},
	}

	spawned := []string{}
	r := NewReconciler(
		func(d api.SupervisorDaemon) error { spawned = append(spawned, d.TaskName); return nil },
		func(d api.SupervisorDaemon) error { return nil },
	)
	r.Events = events
	r.Reconcile(intent, &api.DaemonIntentFile{}, map[string]bool{}, time.Now().UTC())

	if len(spawned) != 1 || spawned[0] != reconcileWiringTestTaskName {
		t.Fatalf("nil-spec GLOBAL daemon must stay in the desired set (exclusion is serena-proxy-scoped); got %v", spawned)
	}
	logStr := readFileStringIfExists(t, eventsPath)
	if strings.Contains(logStr, `"event":"legacy-serena-descriptor-skipped"`) {
		t.Fatalf("a nil-spec GLOBAL daemon must NOT be excluded:\n%s", logStr)
	}
}

func TestProductionSpawnFn_EnvOverrideAppliedDeterministically(t *testing.T) {
	parent := []string{"BASE=parent"}
	overrides := map[string]string{
		"PATH": "first",
		"Path": "second",
		"FOO":  "foo",
		"BAR":  "bar",
	}
	// Phase 2.7 mergeDaemonEnv contract: keys are emitted sorted by
	// uppercase-normalized form (so case-insensitive collisions yield
	// one entry on Windows). The overall sort order is therefore
	// platform-dependent for cases where two keys share a normalized
	// form within the same layer.
	//
	// POSIX (case-sensitive env keys): all five keys survive. Sort by
	// uppercase normalize is stable for non-colliding keys but here
	// both "PATH" and "Path" normalize to "PATH" → adjacent in the
	// output; the within-layer sort applied them in lexicographic
	// order ("PATH" then "Path"), so the last-write-wins entry is
	// "Path=second" and the slot ordering is preserved by the final
	// sort. Two distinct map keys both stored at entries["PATH"] is
	// impossible — Go maps can't hold two keys differing only by
	// the helper's normalized form. On POSIX the helper does NOT
	// normalize, so entries["PATH"] and entries["Path"] are separate
	// buckets and both survive.
	//
	// Windows (case-insensitive env keys): "PATH" and "Path" share
	// normalized key "PATH"; within-layer sort applies "PATH=first"
	// first, then "Path=second" overwrites it, so the surviving
	// entry is "Path=second".
	var want []string
	if runtime.GOOS == "windows" {
		want = []string{
			"BAR=bar",
			"BASE=parent",
			"FOO=foo",
			"Path=second",
		}
	} else {
		want = []string{
			"BAR=bar",
			"BASE=parent",
			"FOO=foo",
			"PATH=first",
			"Path=second",
		}
	}
	wantJoined := strings.Join(want, "\x00")

	for i := 0; i < 20; i++ {
		got := mergeDaemonEnv(parent, overrides, nil)
		if gotJoined := strings.Join(got, "\x00"); gotJoined != wantJoined {
			t.Fatalf("iteration %d mergeDaemonEnv order = %#v, want %#v", i, got, want)
		}
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

	spawnFn := makeProductionSpawnFn(events, NewDaemonRuntimeTracker())

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

// TestProductionSpawnFn_EmitsDaemonExitedOnChildExit verifies the
// production spawn fn emits a daemon-exited event when the child
// process terminates. The emit happens asynchronously inside the
// cmd.Wait() goroutine, so the test polls the events log up to a
// generous timeout.
//
// Regression guard: without this emit, fast-exiting wrappers (e.g.,
// uvx fails to fetch a package, port already bound, env vars
// missing) leave no trace — supervisor-state.json shows
// state="idle" with bumped pid_generation and the operator has zero
// data on why. This is the diagnostic gap that left serena-claude
// / serena-codex in a 35-cycle silent crash loop on the operator's
// machine until the daemon-exited emit was wired in.
func TestProductionSpawnFn_EmitsDaemonExitedOnChildExit(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	spawnFn := makeProductionSpawnFn(events, NewDaemonRuntimeTracker())

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

	// portableNoopCommand returns within ~10-50 ms on warm machines,
	// but Windows CI stagger plus event-log flush latency can push
	// the visible-event window to a few hundred ms. Generous 10 s
	// timeout keeps the test reliable without slowing CI when the
	// event lands quickly.
	deadline := time.Now().Add(10 * time.Second)
	var logStr string
	for time.Now().Before(deadline) {
		logRaw, _ := os.ReadFile(eventsPath)
		logStr = string(logRaw)
		if strings.Contains(logStr, `"event":"daemon-exited"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logStr, `"event":"daemon-exited"`) {
		t.Fatalf("daemon-exited event never appeared within 10s:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"task_name":"\\mcp-local-hub-memory-default"`) {
		t.Fatalf("daemon-exited event missing task_name:\n%s", logStr)
	}
	if !strings.Contains(logStr, `"exit_code":0`) {
		t.Fatalf("daemon-exited event missing exit_code=0:\n%s", logStr)
	}
}

// TestProductionSpawnFn_BlockingCrashSendNeverDropsExit is the audit P3
// Finding-1 guard: the per-child wait goroutine posts EVERY child exit to
// crashCh with a BLOCKING send, so a real child-exit is NEVER dropped even
// when many exits are pending unprocessed. The pre-fix non-blocking
// select{...default:drop+audit} silently lost the respawn signal whenever
// the buffered crashCh was full, leaving the daemon dead with only a
// warn-log trace.
//
// We deliberately under-size crashCh (cap 4) relative to the number of
// spawned-then-exited children (70) so the buffer fills and the wait
// goroutines MUST block on the send. With the old non-blocking send,
// 70-4 = 66 events would be dropped; with the blocking send, all 70 are
// delivered once the channel is drained.
func TestProductionSpawnFn_BlockingCrashSendNeverDropsExit(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	const numChildren = 70
	const crashChCap = 4 // intentionally << numChildren so the buffer fills

	crashCh := make(chan crashEvent, crashChCap)
	shutdown := make(chan struct{}) // never closed in this test
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), "", nil, "", crashCh, shutdown, nil, false,
	)

	command, args := portableNoopCommand()
	// Spawn all children FIRST, with the drainer NOT yet running, so their
	// wait goroutines fill crashCh and then block on the blocking send.
	for i := 0; i < numChildren; i++ {
		descriptor := api.SupervisorDaemon{
			TaskName: reconcileWiringTestTaskName,
			Server:   "memory",
			Daemon:   "default",
			Command:  command,
			Args:     args,
		}
		if err := spawnFn(descriptor); err != nil {
			t.Fatalf("spawn fn failed on noop command (child %d): %v", i, err)
		}
	}

	// Now drain crashCh and count delivered exits. A blocking send means
	// every one of the 70 wait goroutines eventually delivers its event.
	// Timeout generously: noop children exit within ~10-50 ms each but
	// Windows Ck stagger + per-child goroutine scheduling can spread the
	// 70 deliveries over a few seconds.
	got := 0
	deadline := time.After(30 * time.Second)
	for got < numChildren {
		select {
		case <-crashCh:
			got++
		case <-deadline:
			t.Fatalf("only %d/%d child exits delivered to crashCh before timeout (blocking-send regression: a real exit was dropped)", got, numChildren)
		}
	}

	// No event should have been dropped, so the drop-marker audit event
	// (now removed) must NOT appear, and neither should the shutdown-
	// abandon event (shutdown was never closed).
	logRaw, _ := os.ReadFile(eventsPath)
	logStr := string(logRaw)
	if strings.Contains(logStr, `"event":"respawn-dispatcher-backlog-full"`) {
		t.Fatalf("backlog-full drop event present (a child exit was dropped):\n%s", logStr)
	}
	if strings.Contains(logStr, `"event":"child-exit-abandoned-on-shutdown"`) {
		t.Fatalf("shutdown-abandon event present though shutdown never fired:\n%s", logStr)
	}
}

// TestProductionSpawnFn_CrashSendAbandonsOnShutdown proves the blocking
// send does not leak the wait goroutine when the supervisor is shutting
// down: once crashShutdown is closed (the bridge has stopped draining
// crashCh), a wait goroutine blocked on a full crashCh abandons the send,
// emits child-exit-abandoned-on-shutdown, and returns.
func TestProductionSpawnFn_CrashSendAbandonsOnShutdown(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	// cap 0 (unbuffered) crashCh that NOBODY drains, so the wait
	// goroutine's send blocks immediately and can only proceed by
	// abandoning on shutdown.
	crashCh := make(chan crashEvent)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), "", nil, "", crashCh, shutdown, nil, false,
	)

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

	// Give the child time to exit and its wait goroutine time to reach the
	// blocking send (where it parks, since crashCh has no reader), then
	// trigger shutdown.
	time.Sleep(300 * time.Millisecond)
	close(shutdown)

	deadline := time.Now().Add(10 * time.Second)
	var logStr string
	for time.Now().Before(deadline) {
		logRaw, _ := os.ReadFile(eventsPath)
		logStr = string(logRaw)
		if strings.Contains(logStr, `"event":"child-exit-abandoned-on-shutdown"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(logStr, `"event":"child-exit-abandoned-on-shutdown"`) {
		t.Fatalf("child-exit-abandoned-on-shutdown event never appeared within 10s (wait goroutine did not abandon on shutdown — leak risk):\n%s", logStr)
	}
}

func TestProductionSpawnFn_UpdatesTracker(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	spawnFn := makeProductionSpawnFn(events, tracker)
	descriptor := api.SupervisorDaemon{
		TaskName: reconcileWiringTestTaskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=TestProductionTerminateFn_HelperSleep"},
		Env:      map[string]string{"MCPHUB_PRODUCTION_TERMINATE_HELPER": "1"},
	}

	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on helper command: %v", err)
	}
	entry, ok := tracker.Get(reconcileWiringTestTaskName)
	if !ok {
		t.Fatal("tracker entry missing after spawn success")
	}
	if entry.State != daemonRuntimeStateRunning || entry.CurrentPID <= 0 || entry.PIDGeneration != 1 || entry.StartedAt.IsZero() {
		t.Fatalf("tracker entry after spawn success = %+v, want running pid>0 generation=1 started_at", entry)
	}
	pid := entry.CurrentPID
	t.Cleanup(func() {
		_ = process.TerminatePID(pid)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if !process.IsPidAlive(pid) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("helper child pid %d still alive after cleanup terminate", pid)
	})
}

func TestProductionSpawnFn_ReapsExitedChildProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX zombie reaping assertion uses ps state output")
	}
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	spawnFn := makeProductionSpawnFn(events, NewDaemonRuntimeTracker())
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

	pid := spawnedPIDFromEventLog(t, eventsPath)
	deadline := time.Now().Add(3 * time.Second)
	var lastState string
	for time.Now().Before(deadline) {
		state, exists := psStateForPID(pid)
		if !exists {
			return
		}
		lastState = state
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("spawned child pid %d was not reaped before deadline; last ps state %q", pid, lastState)
}

func TestProductionTerminateFn_UpdatesTracker(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("production terminate identity gate fails closed without a native PID identity probe")
	}
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	cmd := exec.Command(os.Args[0], "-test.run=TestProductionTerminateFn_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PRODUCTION_TERMINATE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper child started without Process")
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitCh:
		default:
		}
	})

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(reconcileWiringTestTaskName, pid, time.Now().UTC())
	terminateFn := makeProductionTerminateFn(events, map[string]runningProcessIdentity{
		reconcileWiringTestTaskName: {
			PID:           pid,
			PIDGeneration: 1,
			StartedAt:     startedAtForPID(t, pid),
		},
	}, tracker)
	if err := terminateFn(api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}); err != nil {
		t.Fatalf("terminate fn: %v", err)
	}

	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("helper child pid %d did not exit after terminate", pid)
	}
	if process.IsPidAlive(pid) {
		t.Fatalf("helper child pid %d still reported alive after terminate", pid)
	}
	entry, ok := tracker.Get(reconcileWiringTestTaskName)
	if !ok {
		t.Fatal("tracker entry missing after terminate")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || !entry.StartedAt.IsZero() {
		t.Fatalf("tracker entry after terminate = %+v, want idle pid=0 zero started_at", entry)
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	for _, event := range []string{`"event":"daemon-terminate-requested"`, `"event":"daemon-terminated"`} {
		if !strings.Contains(logStr, event) {
			t.Fatalf("%s missing from audit log:\n%s", event, logStr)
		}
	}
	if strings.Contains(logStr, `"event":"daemon-terminate-failed"`) {
		t.Fatalf("unexpected daemon-terminate-failed event:\n%s", logStr)
	}
}

func TestProductionTerminateFn_IdentityMismatchAtEntry(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	cmd := exec.Command(copyCurrentTestBinaryAsReconcileMcphub(t), "-test.run=TestProductionTerminateFn_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PRODUCTION_TERMINATE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start foreign helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("foreign helper child started without Process")
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	pid := cmd.Process.Pid
	if !process.IsPidAlive(pid) {
		t.Fatalf("test precondition failed: foreign helper pid %d must be alive", pid)
	}
	if pidMatchesMcphub(pid) {
		t.Skipf("foreign helper pid %d unexpectedly matches mcphub identity", pid)
	}

	terminateFn := makeProductionTerminateFn(events, map[string]runningProcessIdentity{
		reconcileWiringTestTaskName: {
			PID:           pid,
			PIDGeneration: 1,
			StartedAt:     startedAtForPID(t, pid),
		},
	}, NewDaemonRuntimeTracker())
	if err := terminateFn(api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}); err != nil {
		t.Fatalf("terminate fn: %v", err)
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-terminate-aborted-pid-reuse"`) {
		t.Fatalf("daemon-terminate-aborted-pid-reuse event missing from audit log:\n%s", logStr)
	}
	for _, event := range []string{`"event":"daemon-terminate-requested"`, `"event":"daemon-terminated"`, `"event":"daemon-terminate-failed"`} {
		if strings.Contains(logStr, event) {
			t.Fatalf("unexpected %s event after entry identity mismatch:\n%s", event, logStr)
		}
	}
}

func TestProductionTerminateFn_PidIdentityBoundToHandle(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("production terminate identity gate fails closed without a native PID identity probe")
	}
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	cmd := exec.Command(os.Args[0], "-test.run=TestProductionTerminateFn_HelperSleep")
	cmd.Env = append(os.Environ(), "MCPHUB_PRODUCTION_TERMINATE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("helper child started without Process")
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitCh:
		default:
		}
	})

	terminateFn := makeProductionTerminateFn(events, map[string]runningProcessIdentity{
		reconcileWiringTestTaskName: {
			PID:           pid,
			PIDGeneration: 1,
			StartedAt:     "2000-01-01T00:00:00Z",
		},
	}, NewDaemonRuntimeTracker())
	if err := terminateFn(api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}); err != nil {
		t.Fatalf("identity mismatch should abort as non-fatal PID-reuse guard, got err: %v", err)
	}
	if !process.IsPidAlive(pid) {
		t.Fatalf("pid %d was terminated despite started_at identity mismatch", pid)
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-terminate-aborted-pid-reuse"`) {
		t.Fatalf("daemon-terminate-aborted-pid-reuse event missing from audit log:\n%s", logStr)
	}
	if strings.Contains(logStr, `"event":"daemon-terminated"`) {
		t.Fatalf("daemon-terminated must not be emitted on identity mismatch:\n%s", logStr)
	}
}

func TestProductionTerminateFn_AlreadyExitedReturnsSuccess(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	pid := exitedProcessPID(t)
	if process.IsPidAlive(pid) {
		t.Fatalf("test precondition failed: pid %d is still alive after Wait", pid)
	}

	terminateFn := makeProductionTerminateFn(events, map[string]runningProcessIdentity{
		reconcileWiringTestTaskName: {PID: pid},
	}, NewDaemonRuntimeTracker())
	if err := terminateFn(api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}); err != nil {
		t.Fatalf("terminate fn: %v", err)
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-terminate-already-exited"`) {
		t.Fatalf("daemon-terminate-already-exited event missing from audit log:\n%s", logStr)
	}
	for _, event := range []string{`"event":"daemon-terminate-requested"`, `"event":"daemon-terminated"`, `"event":"daemon-terminate-failed"`} {
		if strings.Contains(logStr, event) {
			t.Fatalf("unexpected %s event after already-exited PID:\n%s", event, logStr)
		}
	}
}

func TestProductionTerminateFn_TerminateRaceMarksExitedAndPersists(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(reconcileWiringTestTaskName, 4242, time.Unix(1700000000, 0).UTC())

	prevQuery := productionQueryPIDStateFn
	prevVerify := productionVerifyPIDIdentityFn
	prevTerminate := productionTerminatePIDWithIdentityFn
	productionQueryPIDStateFn = func(pid int) (process.PIDState, error) {
		if pid != 4242 {
			t.Fatalf("QueryPIDState pid = %d, want 4242", pid)
		}
		return process.PIDStateAlive, nil
	}
	productionVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		if proof.PID != 4242 {
			t.Fatalf("VerifyPIDIdentity pid = %d, want 4242", proof.PID)
		}
		return nil
	}
	productionTerminatePIDWithIdentityFn = func(proof process.PIDIdentityProof) error {
		if proof.PID != 4242 {
			t.Fatalf("TerminatePIDWithIdentity pid = %d, want 4242", proof.PID)
		}
		return process.ErrProcessAlreadyExited
	}
	t.Cleanup(func() {
		productionQueryPIDStateFn = prevQuery
		productionVerifyPIDIdentityFn = prevVerify
		productionTerminatePIDWithIdentityFn = prevTerminate
	})

	terminateFn := makeProductionTerminateFnWithStatePath(events, map[string]runningProcessIdentity{
		reconcileWiringTestTaskName: {
			PID:           4242,
			PIDGeneration: 1,
			StartedAt:     time.Unix(1700000000, 0).UTC().Format(time.RFC3339Nano),
		},
	}, tracker, statePath)
	if err := terminateFn(api.SupervisorDaemon{TaskName: reconcileWiringTestTaskName}); err != nil {
		t.Fatalf("terminate fn: %v", err)
	}

	entry, ok := tracker.Get(reconcileWiringTestTaskName)
	if !ok {
		t.Fatal("tracker entry missing")
	}
	if entry.State != daemonRuntimeStateIdle || entry.CurrentPID != 0 || !entry.StartedAt.IsZero() {
		t.Fatalf("tracker entry after already-exited race = %+v, want idle pid=0 zero started_at", entry)
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		t.Fatalf("read persisted supervisor state: %v", err)
	}
	got := state.Daemons[reconcileWiringTestTaskName]
	if got.State != daemonRuntimeStateIdle || got.CurrentPID != 0 || got.StartedAt != "" {
		t.Fatalf("persisted daemon state = %+v, want idle pid=0 empty started_at", got)
	}

	logRaw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	logStr := string(logRaw)
	if !strings.Contains(logStr, `"event":"daemon-terminate-already-exited"`) {
		t.Fatalf("daemon-terminate-already-exited event missing from audit log:\n%s", logStr)
	}
	for _, event := range []string{`"event":"daemon-terminated"`, `"event":"daemon-terminate-failed"`} {
		if strings.Contains(logStr, event) {
			t.Fatalf("unexpected %s event after already-exited race:\n%s", event, logStr)
		}
	}
}

func startedAtForPID(t *testing.T, pid int) string {
	t.Helper()
	ts, ok := process.ProcessStartTime(pid)
	if !ok {
		t.Skipf("started_at proof unavailable on %s", runtime.GOOS)
		return ""
	}
	return ts.UTC().Format(time.RFC3339Nano)
}

func TestProductionTerminateFn_HelperSleep(t *testing.T) {
	if os.Getenv("MCPHUB_PRODUCTION_TERMINATE_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func exitedProcessPID(t *testing.T) int {
	t.Helper()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd.exe", "/c", "exit", "0")
	} else {
		cmd = exec.Command("sh", "-c", "exit 0")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start short-lived child: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("short-lived child started without Process")
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait short-lived child: %v", err)
	}
	return pid
}

func spawnedPIDFromEventLog(t *testing.T, path string) int {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	re := regexp.MustCompile(`"pid":([0-9]+)`)
	match := re.FindSubmatch(raw)
	if match == nil {
		t.Fatalf("daemon-spawned pid missing from audit log:\n%s", string(raw))
	}
	pid, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parse pid %q: %v", string(match[1]), err)
	}
	return pid
}

func psStateForPID(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	state := strings.TrimSpace(string(out))
	if state == "" {
		return "", false
	}
	return state, true
}

func copyCurrentTestBinaryAsReconcileMcphub(t *testing.T) string {
	t.Helper()

	srcPath, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	name := "mcphub"
	if runtime.GOOS == "windows" {
		name = "mcphub.exe"
	}
	dstPath := filepath.Join(t.TempDir(), name)
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatalf("open test binary: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create helper binary: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copy helper binary: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close helper binary: %v", err)
	}
	return dstPath
}

// TestResolveSpawnIntentChannelPath is the bot PR #246 r2 P3 guard. The spawn fn
// must hand a serena-proxy the supervisor's ALREADY-RESOLVED intent path (the
// sibling of statePath, the dir the supervisor reads its own intent from), NOT a
// fresh api.DefaultSupervisorIntentPath() resolution — which re-runs
// DaemonStateDir and diverges under MCPHUB_STATE_DIR_OVERRIDE /
// SetDaemonStateRootForTest / a POSIX child-overlaid HOME, handing the proxy a
// path where its own descriptor does not exist.
func TestResolveSpawnIntentChannelPath(t *testing.T) {
	// statePath set -> intent path is its sibling supervisor-intent.json,
	// regardless of any ambient DaemonStateDir override.
	base := t.TempDir()
	statePath := filepath.Join(base, "supervisor-state.json")
	got, err := resolveSpawnIntentChannelPath(statePath)
	if err != nil {
		t.Fatalf("resolveSpawnIntentChannelPath(%q) error = %v", statePath, err)
	}
	if want := filepath.Join(base, "supervisor-intent.json"); got != want {
		t.Fatalf("resolved intent path = %q, want %q (must be the sibling of statePath, not DefaultSupervisorIntentPath)", got, want)
	}

	// statePath empty (the makeProductionSpawnFn test/manual wrapper) -> fall
	// back to DefaultSupervisorIntentPath (the pre-r2 behavior).
	def, derr := api.DefaultSupervisorIntentPath()
	gotEmpty, eerr := resolveSpawnIntentChannelPath("")
	if (derr == nil) != (eerr == nil) {
		t.Fatalf("empty-statePath fallback error mismatch: default err=%v, helper err=%v", derr, eerr)
	}
	if derr == nil && gotEmpty != def {
		t.Fatalf("empty statePath must fall back to DefaultSupervisorIntentPath %q, got %q", def, gotEmpty)
	}
}
