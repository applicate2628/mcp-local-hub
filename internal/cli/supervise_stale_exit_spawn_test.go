package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
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
	staleExitCustodyFixtureEnv = "MCPHUB_STALE_EXIT_CUSTODY_FIXTURE"
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
	if fixture := os.Getenv(staleExitCustodyFixtureEnv); fixture != "" {
		runStaleExitCustodyFixture(t, fixture, releasePath)
		return
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

// A closed fixture on the existing sentinel/selector-qualified helper entry.
// The child remains in the hosting StdioHost's inherited original Job. The
// helper retains its actual creation object and one Wait, never PID authority.
type staleExitCustodyFixture struct {
	Version           int
	Role, Root, Nonce string
	Descendant        bool
}

type staleExitCustodyRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Nonce string `json:"nonce"`
	} `json:"params"`
	CreatorPID    int                                  `json:"creatorPid,omitempty"`
	CreatorNative process.PublisherNativeIncarnationV1 `json:"creatorNative"`
}

// The fixture producer and its caller share this exact private wire. Native
// values are closed current-self observations, never process-control authority.
type stdioCLICustodyReply struct {
	Nonce       string                                `json:"nonce"`
	RootPID     int                                   `json:"rootPid"`
	ChildPID    int                                   `json:"childPid"`
	Held        bool                                  `json:"held"`
	RootNative  process.PublisherNativeIncarnationV1  `json:"rootNative"`
	ChildNative *process.PublisherNativeIncarnationV1 `json:"childNative,omitempty"`
}

func staleExitCustodyPublish(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func staleExitCustodyReceive(ctx context.Context, path string, value any) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			return json.Unmarshal(raw, value)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func runStaleExitCustodyFixture(t *testing.T, raw, releasePath string) {
	t.Helper()
	var fixture staleExitCustodyFixture
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 4096 || !json.Valid([]byte(raw)) || decoder.Decode(&fixture) != nil || fixture.Version != 1 ||
		!filepath.IsAbs(fixture.Root) || fixture.Root != filepath.Dir(releasePath) ||
		fixture.Nonce != filepath.Base(fixture.Root) || (fixture.Role != "root" && fixture.Role != "descendant") ||
		fixture.Role == "descendant" && (fixture.Descendant || filepath.Base(releasePath) != "descendant.release") ||
		fixture.Role == "root" && filepath.Base(releasePath) != "root.release" {
		t.Fatal("invalid closed custody fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	selfObservation, captureErr := process.ObserveCurrentPublisherNative(ctx)
	if selfObservation == nil {
		t.Fatalf("fixture current-self observation unavailable: %v", captureErr)
	}
	primaryErr, closeErr := selfObservation.CaptureErrors()
	if err := errors.Join(captureErr, primaryErr, closeErr, selfObservation.Validate(ctx)); err != nil {
		t.Fatalf("fixture current-self observation incomplete: %v", err)
	}
	self := selfObservation.NativeSnapshot()
	reply := func(request staleExitCustodyRequest, pid int, held bool) {
		if request.JSONRPC != "2.0" || len(request.ID) == 0 || request.Method != "fixture/liveness" || request.Params.Nonce == "" {
			t.Fatal("invalid custody liveness request")
		}
		if err := selfObservation.Validate(ctx); err != nil {
			t.Fatalf("fixture current-self observation changed: %v", err)
		}
		response := struct {
			JSONRPC string               `json:"jsonrpc"`
			ID      json.RawMessage      `json:"id"`
			Result  stdioCLICustodyReply `json:"result"`
		}{JSONRPC: "2.0", ID: request.ID}
		response.Result.Nonce, response.Result.RootPID, response.Result.ChildPID, response.Result.Held = request.Params.Nonce, request.CreatorPID, pid, held
		response.Result.RootNative = self
		if held {
			if request.CreatorPID <= 0 || request.CreatorNative.PID != uint32(request.CreatorPID) || request.CreatorNative.NativeBirthFILETIME == 0 {
				t.Fatal("descendant request lacks its root native identity")
			}
			response.Result.RootNative = request.CreatorNative
			response.Result.ChildNative = &self
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(os.Stdout, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.Role == "descendant" {
		if _, err := fmt.Fprintln(os.Stderr, "custody descendant holds inherited stderr"); err != nil {
			t.Fatal(err)
		}
		if err := staleExitCustodyPublish(filepath.Join(fixture.Root, "descendant.ready"), fixture); err != nil {
			t.Fatal(err)
		}
		var request staleExitCustodyRequest
		if err := staleExitCustodyReceive(ctx, filepath.Join(fixture.Root, "descendant.request"), &request); err != nil {
			t.Fatal(err)
		}
		reply(request, os.Getpid(), true)
		var release string
		if err := staleExitCustodyReceive(ctx, releasePath, &release); err != nil {
			t.Fatal(err)
		}
		return
	}
	if fixture.Descendant {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		childFixture := fixture
		childFixture.Role, childFixture.Descendant = "descendant", false
		childRaw, err := json.Marshal(childFixture)
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(exe, "-test.run=^TestStaleExitReleaseHelper$", "-test.count=1", "-test.timeout=15s")
		process.NoConsole(child)
		child.Dir = fixture.Root
		child.Env = append(withoutCLITestHelperEnvironment(os.Environ()),
			staleExitHelperSentinelEnv+"=1", staleExitHelperReleaseEnv+"="+filepath.Join(fixture.Root, "descendant.release"), staleExitCustodyFixtureEnv+"="+string(childRaw))
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		waited := make(chan error, 1)
		go func() { waited <- child.Wait() }()
		defer func() {
			select {
			case err := <-waited:
				var exit *exec.ExitError
				if err != nil && !errors.As(err, &exit) {
					t.Errorf("retired fixture child Wait: %v", err)
				}
				return
			default:
			}
			killErr := child.Process.Kill()
			if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				t.Errorf("retained fixture child termination: %v", killErr)
			}
			select {
			case err := <-waited:
				var exit *exec.ExitError
				if err != nil && !errors.As(err, &exit) {
					t.Errorf("fixture child Wait: %v", err)
				}
			case <-ctx.Done():
				t.Error("retained fixture child Wait not received")
			}
		}()
		var ready staleExitCustodyFixture
		if err := staleExitCustodyReceive(ctx, filepath.Join(fixture.Root, "descendant.ready"), &ready); err != nil {
			t.Fatal(err)
		}
		if ready != childFixture {
			t.Fatal("descendant barrier belongs to another fixture")
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request staleExitCustodyRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			t.Fatal(err)
		}
		request.CreatorPID = os.Getpid()
		request.CreatorNative = self
		if fixture.Descendant {
			if err := staleExitCustodyPublish(filepath.Join(fixture.Root, "descendant.request"), request); err != nil {
				t.Fatal(err)
			}
		} else {
			reply(request, 0, false)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
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
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, tracker, statePath, nil, "", crashCh, context.Background(), nil, false,
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
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, tracker, statePath, nil, "", crashCh, context.Background(), nil, false,
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
}
