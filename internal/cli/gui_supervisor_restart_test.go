package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

type restartSelectContext struct {
	context.Context
	selected chan struct{}
	once     sync.Once
}

func (c *restartSelectContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.selected) })
	return c.Context.Done()
}

func TestSupervisorRestartPinsFirstAdmissionAcrossRespawn(t *testing.T) {
	for _, disposition := range []string{"replacement", "none", "published-before-admission"} {
		t.Run(disposition, func(t *testing.T) {
			makeOwner := func(pid int) *supervisorOwner {
				return &supervisorOwner{spawned: true, control: SupervisorControlBinding{proof: &supervisorControlProof{
					expected: SupervisorControlExpectation{StateDir: "fixture", ProductPath: "fixture"},
					owner:    api.SupervisorLockOwner{PID: pid, StartedAt: "2026-10-16T00:00:00Z"},
				}}}
			}
			a, b := makeOwner(41), makeOwner(42)
			m := newSupervisorManager(context.Background(), "fixture", false, time.Second, a)
			defer m.loopCancel()
			respawn := &supervisorManagerOperation{kind: "respawn", original: a, done: make(chan struct{})}
			m.operation = respawn
			if disposition == "published-before-admission" {
				m.current = b
			}
			var calls atomic.Int32
			m.restartFn = func(_ context.Context, operation *supervisorManagerOperation) (gui.SupervisorRestartResult, error) {
				calls.Add(1)
				if operation.original != b || operation.admissionTarget.PID != 42 {
					t.Error("admission did not retain actual already-published B")
				}
				m.mu.Lock()
				m.shuttingDown = true // Fixture has no actual child/automatic worker.
				m.mu.Unlock()
				return gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartRefused, Code: "fixture-B"}, nil
			}
			ctx := &restartSelectContext{Context: context.Background(), selected: make(chan struct{})}
			finished := make(chan gui.SupervisorRestartResult, 1)
			go func() { result, _ := m.restart(ctx); finished <- result }()
			<-ctx.selected // Actual request reached the respawn join after admission.
			m.mu.Lock()
			if disposition == "none" {
				m.current = nil
			} else {
				m.current = b
			}
			m.operation = nil
			close(respawn.done)
			m.mu.Unlock()
			result := <-finished
			if disposition == "published-before-admission" {
				if result.Code != "fixture-B" || calls.Load() != 1 {
					t.Fatalf("lost actual B admission: %#v calls=%d", result, calls.Load())
				}
			} else if result.Code != "RESTART_GENERATION_CONFLICT" || calls.Load() != 0 {
				t.Fatalf("retargeted after admission: %#v calls=%d", result, calls.Load())
			}
		})
	}
}

func TestSupervisorCreationSnapshotsSurviveNilOwnerAndStaleResult(t *testing.T) {
	m := newSupervisorManager(context.Background(), "fixture", false, time.Second, nil)
	defer m.loopCancel()
	operation := &supervisorManagerOperation{kind: "restart", done: make(chan struct{}), requestContext: context.Background(), observers: 1}
	receive := m.receiveCreationAttempt(operation)
	_ = receive(supervisorCreationSnapshot{Entered: true, PreStart: true})
	stale := gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartFailed, OriginalChildDisposition: "none"}
	_ = receive(supervisorCreationSnapshot{Entered: true, Created: true, PID: 41})
	m.observeRestart(operation, stale)
	m.mu.Lock()
	observed := cloneRestartResult(operation.result)
	m.mu.Unlock()
	if !observed.Spawned || observed.SpawnedPID != 41 || observed.OriginalChildDisposition != "pending" {
		t.Fatalf("stale result erased actual creation: %#v", observed)
	}
	waitErr, settleErr := errors.New("original Wait"), errors.New("original settlement")
	_ = receive(supervisorCreationSnapshot{Entered: true, Created: true, PID: 41, WaitReceived: true, WaitError: waitErr, SettlementError: settleErr, Finished: true})
	m.completeManagerOperation(operation, stale, settleErr)
	if !operation.result.Spawned || operation.result.OriginalChildDisposition != "settled" || operation.creation.GenerationAvailable || !operation.creation.WaitReceived || !errors.Is(operation.creation.WaitError, waitErr) || !errors.Is(operation.creation.SettlementError, settleErr) || operation.createdOwner != nil {
		t.Fatalf("nil usable owner lost original facts: %#v %#v", operation.result, operation.creation)
	}
	m.mu.Lock()
	pending := operation.mergeCreationResult(gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartPending})
	m.mu.Unlock()
	if pending.OriginalChildDisposition != "pending" {
		t.Fatal("received child Wait erased an operation's pending cleanup")
	}
	// The same publication guard preserves the standalone owner's actual
	// transfer projection when no GUI creation snapshot exists.
	standalone := &supervisorManagerOperation{}
	m.mu.Lock()
	standalone.publishResultLocked(gui.SupervisorRestartResult{OriginalChildDisposition: "transferred", TransferCommitted: true, Transferred: true})
	transferred := standalone.snapshotResultLocked(standalone.result)
	m.mu.Unlock()
	if transferred.OriginalChildDisposition != "transferred" || !transferred.TransferCommitted || !transferred.Transferred {
		t.Fatal("GUI publication changed standalone transfer facts")
	}
}

func TestSupervisorCreatorPreStartObservationAndCompatibility(t *testing.T) {
	var snapshots []supervisorCreationSnapshot
	owner, err := ensureSupervisorRunningObserved(nil, "fixture", false, time.Second, func(snapshot supervisorCreationSnapshot) error {
		snapshots = append(snapshots, snapshot)
		return nil
	})
	if owner != nil || err == nil || len(snapshots) == 0 {
		t.Fatalf("missing original admission refusal: %v %v", owner, err)
	}
	last := snapshots[len(snapshots)-1]
	if !last.Entered || !last.PreStart || !last.Finished || last.Created || last.PID != 0 || last.GenerationAvailable || last.WaitReceived {
		t.Fatalf("pre-Start refusal invented creation: %#v", last)
	}
	// Compile-time guard for registration's existing callable and return shape.
	var compatible func(context.Context, string, bool, time.Duration) (*supervisorOwner, error) = ensureSupervisorRunning
	if owner, err := compatible(nil, "fixture", false, time.Second); owner != nil || err == nil {
		t.Fatalf("compatibility projection changed: %v %v", owner, err)
	}
	want := errors.New("fixture attempt receiver failure")
	if owner, err := ensureSupervisorRunningObserved(nil, "fixture", false, time.Second, func(supervisorCreationSnapshot) error { return want }); owner != nil || !errors.Is(err, want) {
		t.Fatalf("observer failure replaced original admission: owner=%v err=%v", owner, err)
	}
}

func TestSupervisorRestartReplacementTopologyPreservesActorThenAbsentIntent(t *testing.T) {
	for _, test := range []struct {
		name     string
		original *supervisorOwner
		external bool
		intent   *api.SupervisorIntentFile
		wantGUI  bool
	}{
		{"GUI-created", &supervisorOwner{spawned: true}, false, &api.SupervisorIntentFile{OwnerMode: api.OwnerModeSupervise}, true},
		{"adopted", &supervisorOwner{}, false, &api.SupervisorIntentFile{}, false},
		{"live-external-after-failed-initialization", nil, true, &api.SupervisorIntentFile{}, false},
		{"absent-legacy-GUI", nil, false, nil, true},
		{"absent-standalone-intent", nil, false, &api.SupervisorIntentFile{OwnerMode: api.OwnerModeSupervise}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode := test.intent.EffectiveOwnerMode()
			if !mode.Valid() {
				t.Fatal("invalid fixture mode")
			}
			op := &supervisorManagerOperation{original: test.original, externalOriginalQualified: test.external}
			if got := op.retainsGUIReplacement(mode); got != test.wantGUI {
				t.Fatalf("GUI retention=%t want=%t", got, test.wantGUI)
			}
		})
	}
}

func TestSupervisorRestartInitializationAndConcurrentCoalescing(t *testing.T) {
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	defer m.loopCancel()
	m.initialized = false
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	m.restartFn = func(context.Context, *supervisorManagerOperation) (gui.SupervisorRestartResult, error) {
		calls.Add(1)
		close(entered)
		<-release
		return gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartFailed, Code: "RESTART_NAMESPACE_CONFLICT"}, nil
	}
	if result, _ := m.restart(context.Background()); result.Code != "RESTART_NOT_READY" || calls.Load() != 0 {
		t.Fatalf("startup admitted restart: %#v", result)
	}
	if err := m.initialize(nil, "test"); err != nil {
		t.Fatal(err)
	}
	first, second := make(chan gui.SupervisorRestartResult, 1), make(chan gui.SupervisorRestartResult, 1)
	go func() { result, _ := m.restart(context.Background()); first <- result }()
	<-entered
	observer := &restartSelectContext{Context: context.Background(), selected: make(chan struct{})}
	go func() { result, _ := m.restart(observer); second <- result }()
	<-observer.selected // Second request reached the original operation's select.
	releaseOnce.Do(func() { close(release) })
	if a, b := <-first, <-second; a.Code != "RESTART_NAMESPACE_CONFLICT" || b.Code != a.Code || calls.Load() != 1 {
		t.Fatalf("not coalesced: %#v %#v calls=%d", a, b, calls.Load())
	}
}

func TestSupervisorRestartStopJoinsOriginalOperation(t *testing.T) {
	t.Cleanup(api.SetDaemonStateRootForTest(apitest.HardenedTempDir(t)))
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cause := errors.New("original creator receiving failure")
	m.restartFn = func(ctx context.Context, _ *supervisorManagerOperation) (gui.SupervisorRestartResult, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartFailed}, cause
	}
	requestDone := make(chan struct{})
	go func() { defer close(requestDone); _, _ = m.restart(context.Background()) }()
	<-entered
	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop(context.Background(), 5000) }()
	<-canceled
	select {
	case err := <-stopDone:
		t.Fatalf("shutdown outran original operation: %v", err)
	default:
	}
	m.mu.Lock()
	retained := m.operation != nil && m.shuttingDown
	m.mu.Unlock()
	if !retained {
		t.Fatal("shutdown lost original operation")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-stopDone; !errors.Is(err, cause) {
		t.Fatalf("lost original cause: %v", err)
	}
	<-requestDone
}

// The native outcome is synthetic here; the required API producer/marshal and
// manager receiving are real. This does not certify native-to-GUI behavior.
func TestSupervisorRestartLaterFinalizationReceivesCheckedProducerFailure(t *testing.T) {
	// Both subtests own the same package logger serially; never t.Parallel.
	for _, later := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-exit-with-observer", true: "finalization-after-pending-observer"}[later], func(t *testing.T) {
			m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
			original := logGUIDiagnosticEventRequired
			t.Cleanup(func() { logGUIDiagnosticEventRequired = original })
			entered, releaseCreator := make(chan struct{}), make(chan struct{})
			logging, releaseLogger := make(chan struct{}), make(chan struct{})
			var creatorOnce, loggerOnce sync.Once
			unblockCreator := func() { creatorOnce.Do(func() { close(releaseCreator) }) }
			unblockLogger := func() { loggerOnce.Do(func() { close(releaseLogger) }) }
			old := api.SupervisorLockOwner{PID: 41, StartedAt: "2026-10-16T00:00:00Z"}
			created := api.SupervisorLockOwner{PID: 52, StartedAt: "2026-10-16T00:00:01Z"}
			oldCause, waitCause, settlementCause := errors.New("original target outcome"), errors.New("created child Wait outcome"), errors.New("created child settlement outcome")
			var operation *supervisorManagerOperation
			var before gui.SupervisorRestartResult
			var checkedErrors []error
			receivedWait := supervisorCreationSnapshot{Entered: true, Created: true, PID: 52, GenerationAvailable: true, Generation: created, WaitReceived: true, WaitError: waitCause, Finished: true}
			logGUIDiagnosticEventRequired = func(level, event string, fields map[string]any) error {
				creation := fields["creation_attempt"].(map[string]any)
				if !(later && event == "supervisor-restart-native-exit") {
					wantReceiving, wantDisposition := "native-exit-observations", "retained"
					if later {
						wantReceiving = "manager-finalization"
					}
					if fields["receiving"] != wantReceiving || creation["wait_received"] != false {
						t.Errorf("incoherent diagnostic cut: %#v", fields)
					}
					if later && fields["original_child_disposition"] != wantDisposition {
						t.Errorf("stale result paired with new creation: %#v", fields)
					}
					if later {
						m.mu.Lock()
						barrierResult := cloneRestartResult(operation.result)
						barrierErr := operation.err
						barrierCreation := operation.creation
						m.mu.Unlock()
						ownerError, _ := fields["owner_error"].(string)
						if fields["code"] != "RESTART_DIAGNOSTIC_UNCONFIRMED" || barrierResult.Code != fields["code"] || barrierResult.PerStepError["diagnostic"] != "native exit diagnostic delivery incomplete" || !errors.Is(barrierErr, checkedErrors[0]) || !strings.Contains(ownerError, checkedErrors[0].Error()) || !strings.Contains(ownerError, oldCause.Error()) || barrierCreation.WaitReceived || barrierCreation.SettlementError != nil {
							t.Errorf("later barrier did not retain the preceding diagnostic result and unsettled creation cut: fields=%#v result=%#v err=%v creation=%#v", fields, barrierResult, barrierErr, barrierCreation)
						}
					}
					close(logging)
					<-releaseLogger
					if creation["wait_received"] != false || creation["settlement_error"] != nil {
						t.Error("later receiving mutated an immutable diagnostic cut")
					}
				}
				// Use the actual existing checked producer's deterministic marshal
				// failure, with no append/namespace effect or bare-error substitute.
				fields["controlled_invalid_value"] = make(chan struct{})
				err := api.LogHubMcpEventRequired(level, event, fields)
				checkedErrors = append(checkedErrors, err)
				return err
			}
			m.restartFn = func(ctx context.Context, op *supervisorManagerOperation) (gui.SupervisorRestartResult, error) {
				operation = op
				m.mu.Lock()
				op.target = old
				op.nativeExit = []supervisorRestartNativeExitObservation{{Phase: "exit", ContextOwner: "request"}}
				m.mu.Unlock()
				_ = m.receiveCreationAttempt(op)(supervisorCreationSnapshot{Entered: true, Created: true, PID: 52, GenerationAvailable: true, Generation: created, Finished: true})
				candidate := gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartFailed, Ready: true, OriginalGeneration: &old, SelectedGeneration: &created, OriginalSettled: true, Phase: "complete", Code: "RESTART_FAILED", OriginalChildDisposition: "retained", NamespaceDisposition: "occupied", PerStepError: map[string]string{"original": "original target incomplete"}}
				m.observeRestart(op, candidate)
				m.mu.Lock()
				before = op.snapshotResultLocked(op.result)
				m.mu.Unlock()
				close(entered)
				if later {
					<-ctx.Done()
					<-releaseCreator
				}
				return candidate, oldCause
			}
			caller, cancel := context.WithCancel(context.Background())
			type returned struct {
				result gui.SupervisorRestartResult
				err    error
			}
			requestDone := make(chan returned, 1)
			var returnedValue returned
			joined := false
			joinRequest := func() {
				if !joined {
					returnedValue = <-requestDone
					joined = true
				}
			}
			t.Cleanup(func() {
				cancel()
				unblockCreator()
				unblockLogger()
				joinRequest()
				if operation != nil {
					<-operation.done
				}
				m.loopCancel()
			})
			go func() { result, err := m.restart(caller); requestDone <- returned{result, err} }()
			<-entered
			if later {
				cancel()
				joinRequest()
				if returnedValue.result.Outcome != gui.SupervisorRestartPending {
					t.Errorf("caller did not observe pending: %#v", returnedValue.result)
				}
				unblockCreator()
			}
			<-logging
			receivedWait.SettlementError = settlementCause
			_ = m.receiveCreationAttempt(operation)(receivedWait)
			unblockLogger()
			<-operation.done
			joinRequest()
			m.mu.Lock()
			final := cloneRestartResult(operation.result)
			finalErr := operation.err
			creation := operation.creation
			m.mu.Unlock()
			if final.OriginalChildDisposition != "settled" || final.SpawnedPID != 52 || !final.OriginalSettled || final.OriginalGeneration.PID != 41 || final.SelectedGeneration.PID != 52 || final.Code != "RESTART_DIAGNOSTIC_UNCONFIRMED" || !errors.Is(finalErr, oldCause) || !errors.Is(creation.WaitError, waitCause) || !errors.Is(creation.SettlementError, settlementCause) {
				t.Errorf("final publication lost acquired outcomes: %#v %v %#v", final, finalErr, creation)
			}
			for _, producerErr := range checkedErrors {
				var actual *json.UnsupportedTypeError
				if producerErr == nil || !errors.As(producerErr, &actual) || !errors.Is(finalErr, producerErr) {
					t.Errorf("actual checked failure erased: %v / %v", producerErr, finalErr)
				}
			}
			if before.OriginalChildDisposition != "retained" || before.PerStepError["diagnostic"] != "" || before.OriginalGeneration.PID != 41 {
				t.Errorf("immutable earlier manager copy changed: %#v", before)
			}
			if later {
				if len(checkedErrors) != 2 || returnedValue.result.OriginalChildDisposition != "pending" || !errors.Is(finalErr, context.Canceled) || !errors.Is(m.loopErr, oldCause) {
					t.Errorf("pending/final aggregate facts lost: %#v %v", returnedValue, m.loopErr)
				}
				for _, producerErr := range checkedErrors {
					if !errors.Is(m.loopErr, producerErr) {
						t.Error("later aggregate lost checked delivery error")
					}
				}
				stopErr := m.Stop(context.Background(), 5000)
				if !errors.Is(stopErr, oldCause) {
					t.Error("GUI finalizer lost later original outcome")
				}
				for _, producerErr := range checkedErrors {
					if !errors.Is(stopErr, producerErr) {
						t.Error("GUI finalizer lost later checked delivery error")
					}
				}
			} else if len(checkedErrors) != 1 || returnedValue.result.OriginalChildDisposition != "settled" || !errors.Is(returnedValue.err, oldCause) {
				t.Errorf("completed observer received stale result: %#v", returnedValue)
			}
		})
	}
}

func TestSupervisorRestartCommitUsesOriginalLifecycleThenStopLatch(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	t.Setenv("LOCALAPPDATA", dir)
	lease, err := gui.AcquireSingleInstanceAt(filepath.Join(dir, "gui.pidport"), 9125)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	defer func() {
		if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	defer m.loopCancel()
	m.lifecycle, m.generation = lease.OwnerLifecycle(), lease.OwnerLifecycle().Generation()
	selected := api.SupervisorLockOwner{PID: 41, StartedAt: "2026-10-16T00:00:00Z"}
	op := &supervisorManagerOperation{kind: "restart", done: make(chan struct{}), selected: selected, expected: SupervisorControlExpectation{StateDir: dir, ProductPath: filepath.Join(dir, "test.exe")}}
	m.operation = op
	held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holderDone <- m.lifecycle.WithActiveGeneration(m.generation, func(gui.GUIOwnerRecord) error {
			close(held)
			<-release
			m.mu.Lock()
			m.shuttingDown = true
			m.mu.Unlock()
			return nil
		})
	}()
	<-held
	commitDone := make(chan error, 1)
	go func() { commitDone <- m.commitSupervisorTransfer(context.Background(), op, selected) }()
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if err := <-commitDone; err == nil || op.transferCommitted {
		t.Fatalf("commit bypassed original stop: %v", err)
	}
	m.mu.Lock()
	m.shuttingDown = false
	m.mu.Unlock()
	if err := m.commitSupervisorTransfer(context.Background(), op, selected); err != nil {
		t.Fatal(err)
	}
	if !op.transferCommitted {
		t.Fatal("ready active operation did not commit")
	}
	// Commit releases both locks before any caller can enter Handoff.
	if err := m.lifecycle.WithActiveGeneration(m.generation, func(gui.GUIOwnerRecord) error { m.mu.Lock(); m.shuttingDown = true; m.mu.Unlock(); return nil }); err != nil {
		t.Fatal(err)
	}
	if !op.transferCommitted {
		t.Fatal("later stop revoked original commitment")
	}
	m.operation = nil // Fixture has no actual creator, child or closer.
}

func TestSupervisorRestartCommonCompositionNormalReusedAndRoute(t *testing.T) {
	t.Setenv("MCPHUB_GUI_RESTART_V3", "0")
	dir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	t.Setenv("LOCALAPPDATA", dir)
	lease, err := gui.AcquireSingleInstanceAt(filepath.Join(dir, "gui.pidport"), 9125)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	defer func() {
		if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	for _, reused := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "reused"}[reused], func(t *testing.T) {
			var startup *guiServerStartup
			if reused {
				startup = &guiServerStartup{server: gui.NewServer(gui.Config{})}
			}
			s, err := composeGuiServerRestartV3(context.Background(), &cobra.Command{}, nil, 0, "", startup, nil, restartV3ParentRuntime{})
			if err != nil {
				t.Fatal(err)
			}
			if reused && s != startup.server {
				t.Fatal("reused server replaced")
			}
			m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
			defer m.loopCancel()
			m.initialized = false // Prove route reaches coordinator without effects.
			composeSupervisorRestart(s, m, lease.OwnerLifecycle())
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/supervisor/restart", nil)
			response := httptest.NewRecorder()
			s.ServeHTTP(response, req)
			if !strings.Contains(response.Body.String(), "RESTART_NOT_READY") {
				t.Fatalf("callback not composed: %s", response.Body.String())
			}
			routeResponse := httptest.NewRecorder()
			s.RouteHandler().ServeHTTP(routeResponse, req)
			if routeResponse.Code != http.StatusNotFound {
				t.Fatalf("route daemon exposes restart: %d", routeResponse.Code)
			}
		})
	}
}
