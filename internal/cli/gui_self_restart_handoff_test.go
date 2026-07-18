package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

// TestSelfRestartHandoffEnvMatchesGUI pins the CLI-local handoff env const
// equal to the gui package's canonical name so the parent (gui handler)
// and the child (this CLI startup path) never drift on the signal.
func TestSelfRestartHandoffEnvMatchesGUI(t *testing.T) {
	if selfRestartHandoffEnv != gui.SelfRestartHandoffEnv {
		t.Fatalf("env const drift: cli=%q gui=%q", selfRestartHandoffEnv, gui.SelfRestartHandoffEnv)
	}
}

func TestRestartV3ChildSelectionLeavesLegacyGateOffHandoffPathUnchanged(t *testing.T) {
	structured, err := gui.EncodeSelfRestartHandoff(gui.SelfRestartHandoff{
		Version: 1, HandoffID: "h", Generation: "g", Sequence: 1,
		OldPort: 9125, TargetPort: 19125, ParentPID: 11,
		NoncePath: filepath.Join(t.TempDir(), "gui-restart-nonce"),
	})
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}
	for _, tc := range []struct {
		name    string
		gate    bool
		handoff string
		wantV3  bool
	}{
		{name: "normal gate off", gate: false, handoff: "", wantV3: false},
		{name: "legacy handoff gate off", gate: false, handoff: "1", wantV3: false},
		{name: "structured handoff gate off", gate: false, handoff: structured, wantV3: false},
		{name: "normal gate on", gate: true, handoff: "", wantV3: false},
		{name: "legacy handoff gate on", gate: true, handoff: "1", wantV3: false},
		{name: "structured handoff gate on", gate: true, handoff: structured, wantV3: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRestartV3ChildLaunch(tc.gate, tc.handoff); got != tc.wantV3 {
				t.Fatalf("isRestartV3ChildLaunch(%v,%q) = %v, want %v", tc.gate, tc.handoff, got, tc.wantV3)
			}
		})
	}
}

func TestConsumeSelfRestartHandoffClearsEnvironmentBeforeDispatch(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, `{"version":1}`)
	if got := consumeSelfRestartHandoff(`{"version":1}`); got != `{"version":1}` {
		t.Fatalf("consumed handoff = %q, want original JSON", got)
	}
	if got := os.Getenv(selfRestartHandoffEnv); got != "" {
		t.Fatalf("handoff environment survived consumption: %q", got)
	}
}

func TestRestartV3ChildStandbyBindUsesDedicatedBindDeadline(t *testing.T) {
	const budget = 75 * time.Millisecond
	var remaining time.Duration
	sentinel := errors.New("bind seam reached")
	_, err := bindRestartV3ChildStandby(context.Background(), budget, func(bindCtx context.Context) (net.Listener, error) {
		deadline, ok := bindCtx.Deadline()
		if !ok {
			t.Fatal("standby bind context has no deadline")
		}
		remaining = time.Until(deadline)
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("bind result = %v, want sentinel", err)
	}
	if remaining <= 0 || remaining > budget {
		t.Fatalf("standby bind budget = %v, want within (0,%v]", remaining, budget)
	}
}

func TestRestartV3_SamePortStandbyBindWaitsForParentClose(t *testing.T) {
	attempts := 0
	want := phaseGCLIListener{}
	bindRefusal := restartV3BindRefusedTestError()
	listener, err := bindRestartV3ChildStandby(context.Background(), time.Second, func(context.Context) (net.Listener, error) {
		attempts++
		if attempts < 3 {
			return nil, &net.OpError{Op: "listen", Net: "tcp", Err: bindRefusal}
		}
		return want, nil
	})
	if err != nil {
		t.Fatalf("bindRestartV3ChildStandby: %v", err)
	}
	if listener != want || attempts != 3 {
		t.Fatalf("listener=%v attempts=%d, want retained listener after 3 attempts", listener, attempts)
	}
}

type phaseGCLIListener struct{}

func (phaseGCLIListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (phaseGCLIListener) Close() error              { return nil }
func (phaseGCLIListener) Addr() net.Addr            { return phaseGCLIAddr{} }

type phaseGCLIAddr struct{}

func (phaseGCLIAddr) Network() string { return "tcp" }
func (phaseGCLIAddr) String() string  { return "127.0.0.1:9125" }

type phaseGCLIChild struct{}

func (phaseGCLIChild) PID() int                                     { return 4260 }
func (phaseGCLIChild) TerminateBeforeRelease(context.Context) error { return nil }
func (phaseGCLIChild) DetachAtRelease() error                       { return nil }

type phaseGCLILease struct{ releases int }

func (l *phaseGCLILease) Release() { l.releases++ }

type phaseGSupervisorStopper struct{ stops int }

func (s *phaseGSupervisorStopper) Stop(context.Context, int) error {
	s.stops++
	return nil
}

func TestRestartV3_SuccessfulHandoffExitSkipsManagerStop(t *testing.T) {
	var exitRequested atomic.Bool
	exits := 0
	exit := selfRestartProcessExitBoundary(&exitRequested, func() { exits++ })
	exit()
	manager := &phaseGSupervisorStopper{}
	if err := stopSupervisorManagerUnlessSelfRestart(context.Background(), manager, &exitRequested); err != nil {
		t.Fatalf("stopSupervisorManagerUnlessSelfRestart: %v", err)
	}
	if exits != 1 || !exitRequested.Load() || manager.stops != 0 {
		t.Fatalf("exits=%d requested=%t manager stops=%d, want 1/true/0", exits, exitRequested.Load(), manager.stops)
	}
}

func TestRestartV3_ParentCompositionUsesParserAwareArgvAndRetainedLease(t *testing.T) {
	lease := &phaseGCLILease{}
	var spawnedArgv []string
	var spawnedHandoff gui.SelfRestartHandoff
	runtime := restartV3ParentRuntime{
		SettingsGet: func(string) (string, error) { return "19125", nil },
		Spawn: func(argv []string, handoff gui.SelfRestartHandoff) (gui.RestartParentChild, error) {
			spawnedArgv = append([]string(nil), argv...)
			spawnedHandoff = handoff
			return phaseGCLIChild{}, nil
		},
		Confirm: func(context.Context, int, []byte, gui.AuthenticatedReadinessIdentity) error { return nil },
		Exit:    func() {},
	}
	deps, err := buildRestartV3ParentDependencies(
		context.Background(), newGuiCmdReal(), lease,
		filepath.Join(apitest.HardenedTempDir(t), gui.PidportFileLeaf),
		func() int { return 9125 },
		[]string{"gui", "--port", "9125", "--no-tray"}, runtime,
	)
	if err != nil {
		t.Fatalf("buildRestartV3ParentDependencies: %v", err)
	}
	if deps.Lease != lease {
		t.Fatal("coordinator did not retain the CLI-owned lease reference")
	}
	targetPort, err := deps.TargetPort(9125)
	if err != nil || targetPort != 19125 {
		t.Fatalf("TargetPort = %d, %v; want 19125", targetPort, err)
	}
	handoff := gui.SelfRestartHandoff{Version: 1, HandoffID: "h", Generation: "g", Sequence: 1, OldPort: 9125, TargetPort: 19125, ParentPID: os.Getpid(), NoncePath: filepath.Join(t.TempDir(), "nonce")}
	if _, err := deps.Spawn(handoff); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if want := []string{"gui", "--no-tray"}; !reflect.DeepEqual(spawnedArgv, want) {
		t.Fatalf("spawn argv = %q, want parser-rebuilt %q", spawnedArgv, want)
	}
	if spawnedHandoff != handoff {
		t.Fatalf("spawn handoff = %+v, want %+v", spawnedHandoff, handoff)
	}
}

func TestRestartV3_ChildStartupUsesStandbyContinuationAndCommits(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR", stateDir)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve test port: %v", err)
	}
	targetPort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	now := time.Now().UTC()
	deadlines := gui.DefaultRestartDeadlines()
	deadlines.Now = time.Now
	nonce := bytes.Repeat([]byte{0x39}, 32)
	generation := "generation-cli-f"
	generationHash := sha256.Sum256([]byte(generation))
	noncePath := filepath.Join(stateDir, fmt.Sprintf("%s-%x", api.GUIRestartNonceFileLeaf, generationHash[:]))
	if err := api.WriteStateFileBytesAtomic(noncePath, nonce); err != nil {
		t.Fatalf("write nonce file: %v", err)
	}
	store := gui.NewHandoffMarkerStore(stateDir, deadlines)
	started, err := store.Begin(gui.HandoffBegin{
		Generation: generation, Route: gui.HandoffRoutePortChange,
		OldPort: 9125, NewPort: targetPort, OldPID: os.Getppid(),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	nonceHash := sha256.Sum256(nonce)
	if _, err := store.Reserve(started.Generation, started.Sequence, now.Add(deadlines.Reservation), "sha256:"+fmt.Sprintf("%x", nonceHash[:]), os.Getpid()); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	raw, err := gui.EncodeSelfRestartHandoff(gui.SelfRestartHandoff{
		Version: 1, HandoffID: "handoff-cli-f", Generation: started.Generation,
		Sequence: started.Sequence, OldPort: 9125, TargetPort: targetPort,
		ParentPID: os.Getppid(), NoncePath: noncePath,
	})
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runRestartV3ChildStartup(ctx, restartV3ChildStartupConfig{
			Handoff: raw, PID: os.Getpid(), PidportPath: filepath.Join(stateDir, "gui.pidport"),
			Port: targetPort, Version: "phase-f-test", Deadlines: deadlines,
			StartRuntime: func(ctx context.Context, server *gui.Server, owner *gui.GUIListenerOwner, bound net.Listener, lease gui.SingleInstanceLease) error {
				ready := make(chan struct{})
				defer lease.Release()
				return server.ContinueWithGUIListener(ctx, ready, owner, bound)
			},
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		record, readErr := store.Read()
		if readErr != nil {
			cancel()
			t.Fatalf("Read committed marker: %v", readErr)
		}
		if record != nil && record.Phase == gui.HandoffPhaseCommitted {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("child did not commit before deadline; record=%+v", record)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRestartV3ChildStartup: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart child startup did not stop after cancellation")
	}
}

func TestRestartV3_ActivatedChildAcceptsSecondRestart(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR", stateDir)
	t.Setenv("MCPHUB_GUI_RESTART_V3", "1")

	firstProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve child target port: %v", err)
	}
	secondProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = firstProbe.Close()
		t.Fatalf("reserve second restart target port: %v", err)
	}
	childPort := firstProbe.Addr().(*net.TCPAddr).Port
	secondTargetPort := secondProbe.Addr().(*net.TCPAddr).Port
	_ = firstProbe.Close()
	_ = secondProbe.Close()

	now := time.Now().UTC()
	deadlines := gui.DefaultRestartDeadlines()
	deadlines.Now = time.Now
	nonce := bytes.Repeat([]byte{0x4a}, 32)
	generation := "generation-cli-child-second-restart"
	generationHash := sha256.Sum256([]byte(generation))
	noncePath := filepath.Join(stateDir, fmt.Sprintf("%s-%x", api.GUIRestartNonceFileLeaf, generationHash[:]))
	if err := api.WriteStateFileBytesAtomic(noncePath, nonce); err != nil {
		t.Fatalf("write nonce file: %v", err)
	}
	store := gui.NewHandoffMarkerStore(stateDir, deadlines)
	started, err := store.Begin(gui.HandoffBegin{
		Generation: generation, Route: gui.HandoffRoutePortChange,
		OldPort: 9125, NewPort: childPort, OldPID: os.Getppid(),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	nonceHash := sha256.Sum256(nonce)
	if _, err := store.Reserve(started.Generation, started.Sequence, now.Add(deadlines.Reservation), "sha256:"+fmt.Sprintf("%x", nonceHash[:]), os.Getpid()); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	raw, err := gui.EncodeSelfRestartHandoff(gui.SelfRestartHandoff{
		Version: 1, HandoffID: "handoff-cli-child-second-restart", Generation: started.Generation,
		Sequence: started.Sequence, OldPort: 9125, TargetPort: childPort,
		ParentPID: os.Getppid(), NoncePath: noncePath,
	})
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childServer := make(chan *gui.Server, 1)
	confirmStarted := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- runRestartV3ChildStartup(ctx, restartV3ChildStartupConfig{
			Handoff: raw, PID: os.Getpid(), PidportPath: filepath.Join(stateDir, "gui.pidport"),
			Port: childPort, Version: "phase-j-child-second-restart-test", Deadlines: deadlines,
			StartRuntime: func(runtimeCtx context.Context, server *gui.Server, owner *gui.GUIListenerOwner, bound net.Listener, lease gui.SingleInstanceLease) error {
				if owner != server.GUIListenerOwner() {
					return errors.New("activated child listener owner differs from server restart owner")
				}
				ownedLease, ok := lease.(*releaseOnceLease)
				if !ok {
					ownedLease = &releaseOnceLease{lease: lease}
				}
				defer ownedLease.Release()
				composed, err := composeGuiServerRestartV3(
					runtimeCtx,
					newGuiCmdReal(),
					ownedLease,
					childPort,
					filepath.Join(stateDir, "gui.pidport"),
					&guiServerStartup{server: server, listenerOwner: owner, bound: bound},
					[]string{"gui"},
					restartV3ParentRuntime{
						SettingsGet: func(string) (string, error) { return strconv.Itoa(secondTargetPort), nil },
						Spawn:       func([]string, gui.SelfRestartHandoff) (gui.RestartParentChild, error) { return phaseGCLIChild{}, nil },
						Confirm: func(confirmCtx context.Context, _ int, _ []byte, _ gui.AuthenticatedReadinessIdentity) error {
							select {
							case confirmStarted <- struct{}{}:
							default:
							}
							<-confirmCtx.Done()
							return confirmCtx.Err()
						},
						Exit: func() {},
					},
				)
				if err != nil {
					return err
				}
				childServer <- composed
				ready := make(chan struct{})
				return composed.ContinueWithGUIListener(runtimeCtx, ready, owner, bound)
			},
		})
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		record, readErr := store.Read()
		if readErr != nil {
			t.Fatalf("Read committed marker: %v", readErr)
		}
		if record != nil && record.Phase == gui.HandoffPhaseCommitted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not commit before deadline; record=%+v", record)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var server *gui.Server
	select {
	case server = <-childServer:
	case err := <-done:
		t.Fatalf("child runtime stopped before second restart request: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("activated child server was not published")
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/gui/restart", childPort), nil)
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", childPort))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("second restart status = %d body=%s, want 202 without nil-coordinator spawn_error", response.Code, response.Body.String())
	}
	select {
	case <-confirmStarted:
	case <-time.After(time.Second):
		t.Fatal("second restart coordinator did not begin standby confirmation")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runRestartV3ChildStartup: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restart child startup did not stop after cancellation")
	}
}

// TestAcquireHandoff_NoEnvSingleShot: without the handoff env, a busy lock
// returns ErrSingleInstanceBusy IMMEDIATELY (no retry) — identical to the
// prior direct AcquireSingleInstanceAt call. We hold the lock for the
// whole test, so a retrying acquire would block; a single-shot returns at
// once.
func TestAcquireHandoff_NoEnvSingleShot(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, "") // ensure not set
	pidport := filepath.Join(t.TempDir(), "gui.pidport")

	held, err := gui.AcquireSingleInstanceAt(pidport, 1)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer held.Release()

	start := time.Now()
	lock, err := acquireSingleInstanceWithHandoff(context.Background(), pidport, 1)
	elapsed := time.Since(start)
	if lock != nil {
		lock.Release()
		t.Fatalf("expected busy, got a lock")
	}
	if !errors.Is(err, gui.ErrSingleInstanceBusy) {
		t.Fatalf("err = %v, want ErrSingleInstanceBusy", err)
	}
	// Single-shot must be near-instant; a generous ceiling rules out an
	// accidental retry loop.
	if elapsed > 2*time.Second {
		t.Fatalf("single-shot acquire took %v, want near-instant (retry loop leaked?)", elapsed)
	}
}

// TestAcquireHandoff_EnvRetriesUntilRelease: with the handoff env set, a
// busy lock is RETRIED; once the holder releases (mid-flight, simulating
// the outgoing parent's exit) the child acquires it. Uses a short test
// deadline so the test is fast.
func TestAcquireHandoff_EnvRetriesUntilRelease(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, "1")

	// Shrink the handoff window for the test, restore after.
	origDeadline := selfRestartHandoffAcquireDeadline
	origBackoff := selfRestartHandoffAcquireBackoff
	selfRestartHandoffAcquireDeadline = 3 * time.Second
	selfRestartHandoffAcquireBackoff = 20 * time.Millisecond
	t.Cleanup(func() {
		selfRestartHandoffAcquireDeadline = origDeadline
		selfRestartHandoffAcquireBackoff = origBackoff
	})

	pidport := filepath.Join(t.TempDir(), "gui.pidport")
	held, err := gui.AcquireSingleInstanceAt(pidport, 1)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	// Release the held lock shortly after the acquire starts polling,
	// simulating the parent exiting and the OS releasing the flock.
	go func() {
		time.Sleep(150 * time.Millisecond)
		held.Release()
	}()

	lock, err := acquireSingleInstanceWithHandoff(context.Background(), pidport, 1)
	if err != nil {
		t.Fatalf("handoff acquire = %v, want success after release", err)
	}
	if lock == nil {
		t.Fatalf("nil lock on success")
	}
	lock.Release()
}

// TestAcquireHandoff_EnvDeadlineExpires: with the handoff env set but the
// holder NEVER releasing, the retry loop gives up at the deadline and
// returns the busy error so the normal handshake/--force flow still runs
// for a genuinely-occupied lock.
func TestAcquireHandoff_EnvDeadlineExpires(t *testing.T) {
	t.Setenv(selfRestartHandoffEnv, "1")

	origDeadline := selfRestartHandoffAcquireDeadline
	origBackoff := selfRestartHandoffAcquireBackoff
	selfRestartHandoffAcquireDeadline = 200 * time.Millisecond
	selfRestartHandoffAcquireBackoff = 20 * time.Millisecond
	t.Cleanup(func() {
		selfRestartHandoffAcquireDeadline = origDeadline
		selfRestartHandoffAcquireBackoff = origBackoff
	})

	pidport := filepath.Join(t.TempDir(), "gui.pidport")
	held, err := gui.AcquireSingleInstanceAt(pidport, 1)
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	defer held.Release()

	lock, err := acquireSingleInstanceWithHandoff(context.Background(), pidport, 1)
	if lock != nil {
		lock.Release()
		t.Fatalf("expected deadline-expiry busy, got a lock")
	}
	if !errors.Is(err, gui.ErrSingleInstanceBusy) {
		t.Fatalf("err = %v, want ErrSingleInstanceBusy", err)
	}
}
