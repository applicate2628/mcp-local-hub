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
	spawnFn := makeProductionSpawnFn(nil, events, NewDaemonRuntimeTracker())

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
	spawnFn := makeProductionSpawnFn(nil, events, tracker)
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

func TestProductionSpawnFn_EnvOverrideAppliedDeterministically(t *testing.T) {
	parent := []string{"BASE=parent"}
	overrides := map[string]string{
		"PATH": "first",
		"Path": "second",
		"FOO":  "foo",
		"BAR":  "bar",
	}
	want := []string{
		"BASE=parent",
		"BAR=bar",
		"FOO=foo",
		"PATH=first",
		"Path=second",
	}
	wantJoined := strings.Join(want, "\x00")

	for i := 0; i < 20; i++ {
		got := mergeDaemonEnv(parent, overrides)
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

	spawnFn := makeProductionSpawnFn(nil, events, NewDaemonRuntimeTracker())

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

func TestProductionSpawnFn_UpdatesTracker(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	spawnFn := makeProductionSpawnFn(nil, events, tracker)
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

	spawnFn := makeProductionSpawnFn(nil, events, NewDaemonRuntimeTracker())
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
