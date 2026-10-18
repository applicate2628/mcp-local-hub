//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
)

// These are manager-boundary fixtures, not Windows peer/handle proof.
type restartBaseOnlyHeld struct{ pid, verifies, terminates, closes int }

func (h *restartBaseOnlyHeld) PID() int { return h.pid }
func (h *restartBaseOnlyHeld) VerifyIdentity(process.PIDIdentityProof) error {
	h.verifies++
	return nil
}
func (h *restartBaseOnlyHeld) Terminate() (bool, error) {
	h.terminates++
	return false, errors.New("unexpected termination")
}
func (h *restartBaseOnlyHeld) Close() error { h.closes++; return nil }

type restartWaitHeld struct {
	restartBaseOnlyHeld
	identity process.PIDHandleIdentity
	waits    int
}

func (h *restartWaitHeld) Identity() (process.PIDHandleIdentity, error) { return h.identity, nil }
func (h *restartWaitHeld) WaitExit(context.Context) (process.HeldPIDExitResult, error) {
	h.waits++
	return process.HeldPIDExitResult{}, errors.New("unexpected native wait")
}

func TestSupervisorRestartMissingHeldWaitRefusesBeforeControl(t *testing.T) {
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	defer m.loopCancel()
	held := &restartBaseOnlyHeld{pid: 41}
	op := &supervisorManagerOperation{target: api.SupervisorLockOwner{PID: 41, StartedAt: "2026-10-16T00:00:00Z"}, held: held}
	settled, killed, err := m.stopHeldRestartOriginal(context.Background(), op, held)
	if settled || killed || !errors.Is(err, process.ErrProcessIdentityUnsupported) || held.verifies != 0 || held.terminates != 0 || held.closes != 0 {
		t.Fatalf("base-only capability admitted: settled=%t killed=%t err=%v held=%#v", settled, killed, err, held)
	}
	// The observer borrows; its outer original operation receives this close.
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if held.closes != 1 {
		t.Fatal("original held close was not received once")
	}
}

func TestSupervisorRestartNativeGenerationMismatchNeverControlsCurrentObject(t *testing.T) {
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	defer m.loopCancel()
	oldTime := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	held := &restartWaitHeld{restartBaseOnlyHeld: restartBaseOnlyHeld{pid: 41}, identity: process.PIDHandleIdentity{PID: 41, StartedAt: oldTime.Add(time.Second)}}
	op := &supervisorManagerOperation{target: api.SupervisorLockOwner{PID: 41, StartedAt: oldTime.Format(time.RFC3339Nano)}, held: held}
	settled, killed, err := m.stopHeldRestartOriginal(context.Background(), op, held)
	if !settled || killed || err != nil || held.verifies != 0 || held.terminates != 0 || held.waits != 0 {
		t.Fatalf("different generation controlled: settled=%t killed=%t err=%v held=%#v", settled, killed, err, held)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if held.closes != 1 {
		t.Fatal("same held generation not closed once")
	}
	// Namespace clearance is intentionally not asserted by this native fact.
}

func TestSupervisorRestartGUIOwnedPropagatesCapturedStrictMode(t *testing.T) {
	for _, captured := range []bool{false, true} {
		dir := apitest.HardenedTempDir(t)
		// This saved input deliberately disagrees. It is NOT the active GUI's
		// runtime launch operand; both actual command builders consume captured.
		if err := api.WriteSupervisorIntent(filepath.Join(dir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1, StrictMode: !captured}); err != nil {
			t.Fatal(err)
		}
		saved, err := api.ReadSupervisorIntent(filepath.Join(dir, "supervisor-intent.json"))
		if err != nil || saved.StrictMode == captured {
			t.Fatalf("fixture did not disagree: %#v %v", saved, err)
		}
		m := newSupervisorManager(context.Background(), "test", captured, time.Second, nil)
		cause := errors.New("controlled creation refusal")
		calls := 0
		m.spawnFn = func(_ context.Context, _ string, actual bool, _ time.Duration, _ supervisorAttemptReceiver) (*supervisorOwner, error) {
			calls++
			if actual != captured {
				t.Errorf("GUI captured strict input lost: captured=%t saved=%t actual=%t", captured, saved.StrictMode, actual)
			}
			return nil, cause
		}
		op := &supervisorManagerOperation{expected: SupervisorControlExpectation{StateDir: dir}}
		result, err := m.restartGUIOwned(context.Background(), op, gui.SupervisorRestartResult{OriginalSettled: true}, nil)
		if calls != 1 || !errors.Is(err, cause) || result.Spawned || op.resourcesPending {
			t.Fatalf("GUI creation boundary changed: %#v %v calls=%d", result, err, calls)
		}
		m.loopCancel()
	}
}

func TestSupervisorRestartReapedGUILoserIsFailedNotOwnedPending(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	defer m.loopCancel()
	exited := make(chan struct{})
	close(exited)
	waitErr := errors.New("original loser Wait result")
	loser := &supervisorOwner{createdPID: 41, exitDone: exited, exitResult: exitInfo{exitErr: waitErr}}
	m.spawnFn = func(_ context.Context, _ string, _ bool, _ time.Duration, receive supervisorAttemptReceiver) (*supervisorOwner, error) {
		_ = receive(supervisorCreationSnapshot{Entered: true, Created: true, PID: 41, WaitReceived: true, WaitError: waitErr, Finished: true})
		return loser, nil
	}
	op := &supervisorManagerOperation{expected: SupervisorControlExpectation{StateDir: dir}}
	result, err := m.restartGUIOwned(context.Background(), op, gui.SupervisorRestartResult{OriginalSettled: true, OriginalChildDisposition: "none"}, nil)
	if result.Outcome != gui.SupervisorRestartFailed || result.Code != "RESTART_NAMESPACE_CONFLICT" || result.OriginalChildDisposition != "settled" || !result.Spawned || result.SpawnedPID != 41 || op.resourcesPending || !errors.Is(err, waitErr) || result.Ready {
		t.Fatalf("reaped loser became successful/pending: %#v %v", result, err)
	}
}

// Root supplies the already qualified real GUI manager/namespace and admits
// this actual restart invocation. This is unbound in ordinary unit suites.
func runGUIOwnedRestartHTTPReceivingFixture(t *testing.T, ctx context.Context, manager *supervisorManager) {
	t.Helper()
	if ctx == nil || manager == nil {
		t.Fatal("qualified live GUI manager/context required")
	}
	const originPort = 9081 // Same virtual configured origin as GUI transport fixtures; no listener.
	server := gui.NewServer(gui.Config{Port: originPort})
	t.Cleanup(server.Broadcaster().Close)
	var actual gui.SupervisorRestartResult
	var actualErr error
	invocations := 0
	server.SetSupervisorRestartHandler(func(request context.Context) (gui.SupervisorRestartResult, error) {
		invocations++
		actual, actualErr = manager.restart(request)
		return actual, actualErr
	})
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/supervisor/restart", originPort), nil).WithContext(ctx)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if invocations != 1 || response.Code != http.StatusOK {
		t.Fatalf("guarded callback not received: calls=%d status=%d body=%s", invocations, response.Code, response.Body.String())
	}
	if actualErr != nil || actual.Outcome != gui.SupervisorRestartRestarted || actual.OriginalChildDisposition != "retained" || !actual.OriginalSettled {
		t.Fatalf("actual GUI-owned publication failed: %#v %v", actual, actualErr)
	}
	var received struct {
		Outcome                  gui.SupervisorRestartOutcome `json:"outcome"`
		Code                     string                       `json:"code"`
		OriginalChildDisposition string                       `json:"original_child_disposition"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &received); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || received.Outcome != gui.SupervisorRestartRestarted || received.Code != "" || received.OriginalChildDisposition != "retained" {
		t.Fatalf("real HTTP projection rejected actual retained creator: status=%d body=%s", response.Code, response.Body.String())
	}
}

type restartPendingHeld struct {
	restartBaseOnlyHeld
	joined       chan struct{}
	release      chan struct{}
	waits        int
	joinErr      error
	firstContext context.Context
	firstEntered chan struct{}
}

func (h *restartPendingHeld) WaitExit(ctx context.Context) (process.HeldPIDExitResult, error) {
	h.waits++
	if h.waits == 1 {
		if h.firstEntered != nil {
			close(h.firstEntered)
		}
		if h.firstContext != nil && ctx != h.firstContext {
			return process.HeldPIDExitResult{}, errors.New("first native phase lost its actual context")
		}
		<-ctx.Done()
		return process.HeldPIDExitResult{}, ctx.Err()
	}
	if ctx.Done() != nil {
		return process.HeldPIDExitResult{}, errors.New("passive join replaced original ownership with another budget")
	}
	close(h.joined)
	<-h.release
	return process.HeldPIDExitResult{Exited: true, ExitCodeAvailable: true, ExitCode: 9}, h.joinErr
}

// This enlarges the exact same-executor custody window with controlled native
// result operands. Actual Windows wait/event/peer proof remains the N oracle.
func TestSupervisorRestartPendingSameHandleRetainsGUIUntilOriginalJoinAndClose(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(dir))
	m := newSupervisorManager(context.Background(), "test", false, time.Second, nil)
	defer m.loopCancel()
	held := &restartPendingHeld{restartBaseOnlyHeld: restartBaseOnlyHeld{pid: 41}, joined: make(chan struct{}), release: make(chan struct{}), joinErr: errors.New("synthetic exit status read failure")}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(held.release) })
	entered := make(chan struct{})
	m.restartFn = func(ctx context.Context, op *supervisorManagerOperation) (gui.SupervisorRestartResult, error) {
		op.held = held
		close(entered)
		_, firstErr := m.receiveRestartNativeExit(op, "exit", "request", ctx, held)
		exited, _, err := m.joinRestartHeldOriginal(op, held, true, firstErr, ctx, true)
		if !exited {
			t.Error("lost positive held exit")
		}
		err = errors.Join(err, held.Close())
		op.held = nil
		return gui.SupervisorRestartResult{Outcome: gui.SupervisorRestartFailed, OriginalSettled: exited, OriginalChildDisposition: "settled", Code: "RESTART_FAILED"}, err
	}
	caller, cancel := context.WithCancel(context.Background())
	requestDone := make(chan struct{})
	go func() { defer close(requestDone); _, _ = m.restart(caller) }()
	<-entered
	m.mu.Lock()
	op := m.operation
	m.mu.Unlock()
	cancel()
	<-requestDone
	<-held.joined
	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop(context.Background(), 5000) }()
	<-m.ctx.Done() // The original Stop latch/cancel has actually executed.
	select {
	case <-op.done:
		t.Fatal("original operation completed before held exit")
	default:
	}
	select {
	case err := <-stopDone:
		t.Fatalf("GUI released pending original: %v", err)
	default:
	}
	if held.closes != 0 || held.terminates != 0 {
		t.Fatal("pending wait was closed/terminated")
	}
	releaseOnce.Do(func() { close(held.release) })
	if err := <-stopDone; !errors.Is(err, context.Canceled) || !errors.Is(err, held.joinErr) {
		t.Fatalf("original phase/later cause lost: %v", err)
	}
	if held.waits != 2 || held.closes != 1 || held.terminates != 0 || op.resourcesPending {
		t.Fatalf("original identity/settlement changed: %#v", held)
	}
	if len(op.nativeExit) != 2 || op.nativeExit[0].Result.Exited || op.nativeExit[0].ContextError == "" || !op.nativeExit[1].Result.Exited || !op.nativeExit[1].Result.ExitCodeAvailable || op.nativeExit[1].Result.ExitCode != 9 {
		t.Fatalf("ordered actual phase/native facts lost: %#v", op.nativeExit)
	}
}

func TestSupervisorRestartPendingReceivesLiveOriginalPhaseBeforePassiveJoin(t *testing.T) {
	m := newSupervisorManager(context.Background(), "fixture", false, time.Second, nil)
	defer m.loopCancel()
	deadline, bounded := t.Deadline()
	if !bounded {
		t.Fatal("original phase fixture requires the test runner's existing deadline")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	held := &restartPendingHeld{joined: make(chan struct{}), release: make(chan struct{}), firstContext: ctx, firstEntered: make(chan struct{})}
	operation := &supervisorManagerOperation{}
	responseErr := errors.New("actual error-response receiving operand")
	finished := make(chan error, 1)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(held.release) }) }
	var err error
	received := false
	join := func() {
		if !received {
			err = <-finished
			received = true
		}
	}
	t.Cleanup(func() { cancel(); unblock(); join() })
	go func() {
		_, _, err := m.joinRestartHeldOriginal(operation, held, false, responseErr, ctx, false)
		finished <- err
	}()
	<-held.firstEntered
	select {
	case <-held.joined:
		t.Error("passive continuation overtook original phase receiving")
	default:
	}
	cancel()
	<-held.joined
	m.mu.Lock()
	first := operation.nativeExit[0]
	m.mu.Unlock()
	if first.ContextOwner != "request" || first.Deadline == nil || !first.Deadline.Equal(deadline) || first.ContextError == "" || !first.Result.Exited && first.Error == "" {
		t.Errorf("actual first phase context/outcome missing: %#v", first)
	}
	unblock()
	join()
	if !errors.Is(err, responseErr) || !errors.Is(err, context.Canceled) || held.waits != 2 || held.terminates != 0 || len(operation.nativeExit) != 2 || operation.nativeExit[1].ContextOwner != "retained-GUI-custody" {
		t.Errorf("same-object pending receiving lost causes/order: %v %#v", err, operation.nativeExit)
	}
}
