package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// Env vars the release-file helper child reads. The sentinel gates the helper
// so it no-ops under a normal `go test` run; the release-path tells it which
// file to poll — the child blocks (polling) until the parent creates that file,
// then exits 0. This lets a test hold a real spawned child alive across a
// simulated supersession WITHOUT any timing sleeps: the child's exit is driven
// deterministically by the parent creating the release file.
const (
	staleExitHelperSentinelEnv = "MCPHUB_STALE_EXIT_RELEASE_HELPER"
	staleExitHelperReleaseEnv  = "MCPHUB_STALE_EXIT_RELEASE_PATH"
)

// TestStaleExitReleaseHelper is the release-file helper subprocess. Under a
// normal `go test` run the sentinel is unset and it no-ops. When the production
// spawn closure launches THIS test binary as a child
// (Command=os.Args[0], Args=-test.run=^TestStaleExitReleaseHelper$), the child
// polls for the release file and exits 0 once it appears — so the parent
// controls exactly when cmd.Wait returns.
func TestStaleExitReleaseHelper(t *testing.T) {
	if os.Getenv(staleExitHelperSentinelEnv) != "1" {
		return
	}
	releasePath := os.Getenv(staleExitHelperReleaseEnv)
	if releasePath == "" {
		os.Exit(3)
	}
	// Poll for the release file. Bounded so a broken test cannot hang a child
	// forever (the test process itself has a package timeout anyway).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(releasePath); err == nil {
			os.Exit(0)
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(4)
}

// waitForEventMarker polls the supervisor events log until it contains the
// given marker or the deadline elapses. Returns the log body (possibly empty).
func waitForEventMarker(t *testing.T, eventsPath, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var body string
	for time.Now().Before(deadline) {
		if b, err := readSupervisorEventsLog(eventsPath); err == nil {
			body = b
			if strings.Contains(body, marker) {
				return body
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return body
}

// TestWaitGoroutine_StaleExitNoCrashPostNoClear is the P1a source-side
// end-to-end guard (spec test 3): spawn a REAL child A through the production
// spawn closure; SUPERSEDE it via a second MarkSpawned (bumping the generation
// exactly as the terminate-first-then-respawn liveness path would); then release
// A. The late exit of the superseded child A must:
//   - NOT post anything on crashCh (the drop at the source),
//   - leave the tracker showing the NEW child (fakePIDB, genB) untouched,
//   - emit daemon-stale-exit-ignored.
//
// The "late exit window" is engineered by holding child A alive (release-file)
// across the simulated supersession, NOT by sleeps.
func TestWaitGoroutine_StaleExitNoCrashPostNoClear(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	releasePath := filepath.Join(tmpHome, "release-A.signal")
	t.Setenv(staleExitHelperSentinelEnv, "1")
	t.Setenv(staleExitHelperReleaseEnv, releasePath)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, tracker, statePath, nil, "", crashCh, shutdown, nil, false,
	)

	taskName := reconcileWiringTestTaskName
	descriptor := api.SupervisorDaemon{
		TaskName: taskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestStaleExitReleaseHelper$"},
	}
	// Spawn child A. MarkSpawned inside the closure bumps generation to 1.
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn child A failed: %v", err)
	}
	entryA, ok := tracker.Get(taskName)
	if !ok || entryA.PIDGeneration != 1 {
		t.Fatalf("after spawn A: entry=%+v ok=%v, want generation=1", entryA, ok)
	}

	// SUPERSEDE child A: a second MarkSpawned (as the respawn after a terminate
	// would) bumps the generation to 2 and records a NEW (fake) current PID. The
	// wait goroutine for A is still alive (child A blocked on the release file),
	// so its eventual exit carries the STALE generation 1.
	const fakePIDB = 987654
	genB := tracker.MarkSpawned(taskName, fakePIDB, time.Now().UTC())
	if genB != 2 {
		t.Fatalf("supersede MarkSpawned returned generation %d, want 2", genB)
	}

	// Release child A so its wait goroutine fires with the stale generation.
	if err := os.WriteFile(releasePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("write release file: %v", err)
	}

	// The stale-exit-ignored event must fire (proves the wait goroutine ran and
	// took the stale-drop branch).
	body := waitForEventMarker(t, eventsPath, "daemon-stale-exit-ignored", 30*time.Second)
	if !strings.Contains(body, "daemon-stale-exit-ignored") {
		t.Fatalf("daemon-stale-exit-ignored not emitted within 30s; log:\n%s", body)
	}

	// crashCh must stay EMPTY — the stale exit was dropped at the source.
	select {
	case ev := <-crashCh:
		t.Fatalf("stale exit posted crashEvent %+v; want NO post (dropped at source)", ev)
	case <-time.After(200 * time.Millisecond):
	}

	// The tracker must still show the NEW child B, untouched by the stale exit.
	entryB, ok := tracker.Get(taskName)
	if !ok {
		t.Fatal("tracker entry missing after stale exit")
	}
	if entryB.State != daemonRuntimeStateRunning || entryB.CurrentPID != fakePIDB || entryB.PIDGeneration != 2 {
		t.Fatalf("stale exit clobbered current tracking: %+v, want running pid=%d generation=2", entryB, fakePIDB)
	}
}

// TestWaitGoroutine_CurrentExitPostsWithGeneration is spec test 4: spawn child A
// with NO supersession, release it, and assert the crashEvent carries the
// correct {PID, PIDGeneration} of the current child.
func TestWaitGoroutine_CurrentExitPostsWithGeneration(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)

	releasePath := filepath.Join(tmpHome, "release-current.signal")
	t.Setenv(staleExitHelperSentinelEnv, "1")
	t.Setenv(staleExitHelperReleaseEnv, releasePath)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	tracker := NewDaemonRuntimeTracker()
	statePath := filepath.Join(tmpHome, "supervisor-state.json")
	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, tracker, statePath, nil, "", crashCh, shutdown, nil, false,
	)

	taskName := reconcileWiringTestTaskName
	descriptor := api.SupervisorDaemon{
		TaskName: taskName,
		Server:   "memory",
		Daemon:   "default",
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestStaleExitReleaseHelper$"},
	}
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn child failed: %v", err)
	}
	entry, ok := tracker.Get(taskName)
	if !ok || entry.PIDGeneration != 1 {
		t.Fatalf("after spawn: entry=%+v ok=%v, want generation=1", entry, ok)
	}
	spawnedPID := entry.CurrentPID

	// Release the child; its exit is the CURRENT generation → posts a crashEvent.
	if err := os.WriteFile(releasePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("write release file: %v", err)
	}

	select {
	case ev := <-crashCh:
		if ev.PID != spawnedPID {
			t.Fatalf("crashEvent.PID = %d, want spawned pid %d", ev.PID, spawnedPID)
		}
		if ev.PIDGeneration != 1 {
			t.Fatalf("crashEvent.PIDGeneration = %d, want 1", ev.PIDGeneration)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("current-generation exit did not post a crashEvent within 30s")
	}
	_ = shutdown
}
