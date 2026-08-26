package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

type terminalTableRecoverer struct {
	result daemonrecovery.Result
	err    error
	calls  atomic.Int32
}

type committedTerminalRecoverer struct {
	calls atomic.Int32
}

func (f *committedTerminalRecoverer) Recover(_ context.Context, taskName string, _ bool, markCommitted func()) (daemonrecovery.Result, error) {
	f.calls.Add(1)
	markCommitted()
	return committedSettlementResult(taskName), nil
}

func (f *terminalTableRecoverer) Recover(_ context.Context, _ string, _ bool, markCommitted func()) (daemonrecovery.Result, error) {
	f.calls.Add(1)
	markCommitted()
	return f.result, f.err
}

func TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn(t *testing.T) {
	if os.Getenv(auditLockBlockingHelperEnv) == "1" {
		lock := flock.New(os.Getenv(auditLockHelperLockEnv))
		locked, err := lock.TryLock()
		if err != nil || !locked {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv(auditLockHelperEnteredEnv), []byte("entered"), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(time.Hour)
		return
	}
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer adapter.close()
	stateRootsBefore := guiTestStateRoots(t)
	adapter.terminalizationBudget = 5 * time.Second
	correlation := validAuditLockCorrelation(adapter.serverInstance, 991)
	binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\\timeout-worker`, confirm: true}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	entered := filepath.Join(t.TempDir(), "entered")
	var childPID atomic.Int64
	var receivedAllowance atomic.Int64
	var postCancelWatchdogExpired atomic.Bool
	adapter.terminalization = boundedTestAuditLockTerminalization(func(ctx context.Context, request auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
		receivedAllowance.Store(request.AllowanceMS)
		childCtx, cancelChild := context.WithCancel(ctx)
		defer cancelChild()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAuditLockTerminalWorkerCancellationAfterAcquisitionReapsBeforeReturn$")
		cmd.Env = append(withoutGUITestHelperEnvironment(os.Environ(), runtime.GOOS), auditLockBlockingHelperEnv+"=1", auditLockHelperLockEnv+"="+adapter.storePath+".lock", auditLockHelperEnteredEnv+"="+entered)
		runDone := make(chan error, 1)
		go func() {
			_, err := process.RunStrictlyContained(childCtx, process.StrictRunInvocation{Command: cmd, Input: []byte("{}"), InputLimit: 2, StdoutLimit: 1024, StderrLimit: 1024})
			runDone <- err
		}()

		watch := time.NewTicker(5 * time.Millisecond)
		defer watch.Stop()
		for {
			if _, err := os.Stat(entered); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				cancelChild()
				return auditLockTerminalWorkerResult{}, errors.Join(err, <-runDone)
			}
			select {
			case err := <-runDone:
				return auditLockTerminalWorkerResult{}, fmt.Errorf("contained helper exited before acquiring occurrence flock: %w", err)
			case <-ctx.Done():
				cancelChild()
				return auditLockTerminalWorkerResult{}, errors.Join(ctx.Err(), <-runDone)
			case <-watch.C:
			}
		}
		if cmd.Process == nil {
			cancelChild()
			return auditLockTerminalWorkerResult{}, errors.Join(errors.New("contained helper marker appeared without a process"), <-runDone)
		}
		childPID.Store(int64(cmd.Process.Pid))
		cancelChild()
		watchdog := time.NewTimer(time.Second)
		defer watchdog.Stop()
		select {
		case err := <-runDone:
			return auditLockTerminalWorkerResult{}, err
		case <-watchdog.C:
			postCancelWatchdogExpired.Store(true)
			_ = cmd.Process.Kill()
			return auditLockTerminalWorkerResult{}, errors.Join(errors.New("contained helper did not reap within post-cancel watchdog"), <-runDone)
		}
	})
	receipt, terminalErr := adapter.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, false))
	if terminalErr == nil || terminalErr.code != string(daemonRecoverErrorOutcomeUncertain) || receipt.Status != auditLockOccurrenceUncertain {
		t.Fatalf("terminal result=%+v err=%v", receipt, terminalErr)
	}
	if postCancelWatchdogExpired.Load() {
		t.Fatal("contained helper exceeded the post-cancel reap watchdog")
	}
	if got := receivedAllowance.Load(); got <= 0 || got > adapter.terminalizationBudget.Milliseconds() {
		t.Fatalf("worker allowance_ms=%d, want positive remaining allowance <= %d", got, adapter.terminalizationBudget.Milliseconds())
	}
	if pid := int(childPID.Load()); pid <= 0 || process.IsPidAlive(pid) {
		t.Fatalf("worker PID %d survived terminalize return", pid)
	}
	if !adapter.storeMu.TryLock() {
		t.Fatal("storeMu remained held after worker timeout")
	}
	adapter.storeMu.Unlock()
	probe := flock.New(adapter.storePath + ".lock")
	locked, err := probe.TryLock()
	if err != nil || !locked {
		t.Fatalf("fresh occurrence flock probe: locked=%t err=%v", locked, err)
	}
	if err := probe.Unlock(); err != nil {
		t.Fatal(err)
	}
	for root := range guiTestStateRoots(t) {
		if _, existed := stateRootsBefore[root]; !existed {
			t.Fatalf("contained helper left package TestMain state root %s", root)
		}
	}
}

func guiTestStateRoots(t *testing.T) map[string]struct{} {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "mcphub-gui-test-state-*"))
	if err != nil {
		t.Fatalf("enumerate GUI TestMain roots: %v", err)
	}
	result := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		result[filepath.Clean(match)] = struct{}{}
	}
	return result
}

func TestAuditLockTerminalizationDeadlineClassifiesWithoutProcessStartup(t *testing.T) {
	events := newEphemeralBroadcaster(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	received := events.Subscribe(ctx)
	adapter := newDirectTestAuditLockAdapterInStateDir(events, t.TempDir())
	defer adapter.close()
	adapter.terminalizationBudget = 250 * time.Millisecond
	correlation := validAuditLockCorrelation(adapter.serverInstance, 992)
	binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\\deadline-worker`, confirm: true}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	var runnerEntered atomic.Bool
	var runnerReturned atomic.Bool
	adapter.terminalization = boundedTestAuditLockTerminalization(func(ctx context.Context, _ auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
		runnerEntered.Store(true)
		<-ctx.Done()
		runnerReturned.Store(true)
		return auditLockTerminalWorkerResult{}, &process.StrictRunError{Kind: process.StrictRunTimeout, Cause: ctx.Err()}
	})

	receipt, terminalErr := adapter.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, false))
	if terminalErr == nil || terminalErr.code != string(daemonRecoverErrorOutcomeUncertain) || receipt.Status != auditLockOccurrenceUncertain {
		t.Fatalf("terminal result=%+v err=%v, want uncertain timeout", receipt, terminalErr)
	}
	if !runnerEntered.Load() || !runnerReturned.Load() {
		t.Fatalf("in-process deadline runner lifecycle entered=%t returned=%t, want both true", runnerEntered.Load(), runnerReturned.Load())
	}
	terminalEvents := 0
	for {
		select {
		case event := <-received:
			if event.Type != "daemon-recovery-terminal-worker-failure" {
				continue
			}
			terminalEvents++
			if got, _ := event.Body["failure_id"].(string); got != string(auditLockTerminalWorkerFailureTimeout) {
				t.Fatalf("failure event id=%q, want %q", got, auditLockTerminalWorkerFailureTimeout)
			}
		default:
			if terminalEvents != 1 {
				t.Fatalf("terminal failure event count=%d, want exactly 1", terminalEvents)
			}
			if !adapter.storeMu.TryLock() {
				t.Fatal("storeMu remained held after deadline classification")
			}
			adapter.storeMu.Unlock()
			return
		}
	}
}

func TestNewServer_UsesOneTerminalizationBudgetForAdapterAndRegistry(t *testing.T) {
	const budget = 37 * time.Millisecond
	s := NewServer(Config{Port: 0, RecoverySettlementTerminalizationBudget: budget})
	defer s.auditLock.close()
	defer s.events.Close()
	if s.auditLock.terminalizationBudget != budget {
		t.Fatalf("adapter terminalization budget=%s, want %s", s.auditLock.terminalizationBudget, budget)
	}
	if s.recoverySettlements.terminalizationBudget != budget {
		t.Fatalf("registry terminalization budget=%s, want %s", s.recoverySettlements.terminalizationBudget, budget)
	}
	if s.auditLock.lockTimeout != auditLockStoreLockTimeout {
		t.Fatalf("nonterminal lock timeout=%s, want %s", s.auditLock.lockTimeout, auditLockStoreLockTimeout)
	}
}

func TestAuditLockTerminalWorkerResultMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		result     auditLockTerminalWorkerResult
		runnerErr  error
		wantCode   daemonRecoverErrorCode
		wantStatus string
	}{
		{"malformed", auditLockTerminalWorkerResult{}, errors.New("malformed worker output"), daemonRecoverErrorOutcomeUncertain, auditLockOccurrenceUncertain},
		{"oversized", auditLockTerminalWorkerResult{}, errors.New("worker output exceeds bound"), daemonRecoverErrorOutcomeUncertain, auditLockOccurrenceUncertain},
		{"baseline stale", auditLockTerminalWorkerResult{Version: auditLockTerminalWorkerProtocolVersion, Outcome: "baseline_stale", Status: 409, Code: string(daemonRecoverErrorBaselineStale)}, nil, daemonRecoverErrorBaselineStale, auditLockOccurrenceInFlight},
		{"durable", auditLockTerminalWorkerResult{Version: auditLockTerminalWorkerProtocolVersion, Outcome: "durable_terminal"}, nil, "", auditLockOccurrenceCommittedSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
			defer a.close()
			correlation := validAuditLockCorrelation(a.serverInstance, 1000)
			binding := auditLockOccurrenceBinding{serverInstance: a.serverInstance, taskName: `\\worker-matrix`, confirm: true}
			reservation, err := a.reserve(context.Background(), correlation, binding)
			if err != nil {
				t.Fatal(err)
			}
			a.terminalization = boundedTestAuditLockTerminalization(func(_ context.Context, _ auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
				result := tc.result
				if result.Outcome == "durable_terminal" {
					result.Receipt = auditLockReceiptDTO{AttemptID: reservation.Receipt.AttemptID, OccurrenceID: reservation.Receipt.OccurrenceID, ServerInstance: reservation.Receipt.ServerInstance, TaskName: reservation.Receipt.TaskName, Status: auditLockOccurrenceCommittedSuccess, LockAuthorization: auditLockAuthorizationNone}
				}
				return result, tc.runnerErr
			})
			receipt, routeErr := a.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, false))
			if tc.wantCode == "" {
				if routeErr != nil || receipt.Status != tc.wantStatus {
					t.Fatalf("receipt=%+v err=%v", receipt, routeErr)
				}
				return
			}
			if routeErr == nil || routeErr.code != string(tc.wantCode) || receipt.Status != tc.wantStatus {
				t.Fatalf("receipt=%+v err=%v", receipt, routeErr)
			}
		})
	}
}

func TestAuditLockTerminalWorker_RealSameBinaryProtocol(t *testing.T) {
	result, err := runAuditLockTerminalWorker(context.Background(), auditLockTerminalWorkerRequest{
		Version: 0, Confirm: true, AllowanceMS: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != auditLockTerminalWorkerProtocolVersion || result.Outcome != "rejected" || result.Failure != string(auditLockTerminalWorkerFailureProtocolInvalid) {
		t.Fatalf("same-binary worker result=%+v, want v1 protocol rejection", result)
	}
}

func TestAuditLockTerminalWorker_RealNonzeroStderrIsCappedAndNotPublished(t *testing.T) {
	t.Setenv(auditLockTerminalWorkerStderrHelperEnv, "1")
	_, err := runAuditLockTerminalWorker(context.Background(), auditLockTerminalWorkerRequest{
		Version: 0, Confirm: true, AllowanceMS: 1000,
	})
	var workerErr *auditLockTerminalWorkerRunError
	if !errors.As(err, &workerErr) || workerErr.failure != auditLockTerminalWorkerFailureExecutionFailed {
		t.Fatalf("worker error=%v, want real execution failure", err)
	}
	if workerErr.stderr.bytes <= auditLockTerminalWorkerStderrMaxBytes || !workerErr.stderr.truncated {
		t.Fatalf("stderr capture=%+v, want capped >%d bytes", workerErr.stderr, auditLockTerminalWorkerStderrMaxBytes)
	}
	if strings.Contains(err.Error(), auditLockTerminalWorkerStderrMarker) {
		t.Fatal("terminal worker error exposed raw stderr")
	}
	events := NewBroadcaster()
	events.DisableGUIEventLog = true
	defer events.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := events.Subscribe(ctx)
	adapter := &auditLockAdapter{events: events}
	adapter.publishTerminalWorkerFailure(auditLockReservation{}, workerErr.failure, workerErr.stdout.bytes, workerErr.stdout.truncated, workerErr.stderr.bytes, workerErr.stderr.truncated)
	for {
		select {
		case event := <-received:
			if event.Type != "daemon-recovery-terminal-worker-failure" {
				continue
			}
			if got, _ := event.Body["failure_id"].(string); got != string(auditLockTerminalWorkerFailureExecutionFailed) {
				t.Fatalf("failure id=%q", got)
			}
			if got, _ := event.Body["stderr_bytes"].(int); got <= auditLockTerminalWorkerStderrMaxBytes {
				t.Fatalf("stderr_bytes=%d, want >%d", got, auditLockTerminalWorkerStderrMaxBytes)
			}
			if got, _ := event.Body["stderr_truncated"].(bool); !got {
				t.Fatal("stderr truncation missing")
			}
			if _, leaked := event.Body["stderr"]; leaked || strings.Contains(fmt.Sprint(event.Body), auditLockTerminalWorkerStderrMarker) {
				t.Fatal("terminal worker event exposed raw stderr")
			}
			return
		case <-time.After(time.Second):
			t.Fatal("missing terminal worker failure event")
		}
	}
}

func TestAuditLockTerminalWorkerFailureEventsUseStableSafeIDs(t *testing.T) {
	cases := []struct {
		name   string
		runner error
		wantID auditLockTerminalWorkerFailure
	}{
		{"timeout", &process.StrictRunError{Kind: process.StrictRunTimeout, Cause: context.DeadlineExceeded}, auditLockTerminalWorkerFailureTimeout},
		{"containment", &process.StrictRunError{Kind: process.StrictRunContainmentFailed, Cause: errors.New("job creation failed")}, auditLockTerminalWorkerFailureContainmentFailed},
		{"protocol", newAuditLockTerminalWorkerRunError(auditLockTerminalWorkerFailureProtocolInvalid, errors.New("malformed worker result"), nil), auditLockTerminalWorkerFailureProtocolInvalid},
		{"execution", errors.New("worker exited nonzero"), auditLockTerminalWorkerFailureExecutionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := NewBroadcaster()
			events.DisableGUIEventLog = true
			defer events.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			received := events.Subscribe(ctx)
			a := newDirectTestAuditLockAdapterInStateDir(events, t.TempDir())
			defer a.close()
			correlation := validAuditLockCorrelation(a.serverInstance, 1200)
			binding := auditLockOccurrenceBinding{serverInstance: a.serverInstance, taskName: `\\worker-failure-id`, confirm: true}
			reservation, err := a.reserve(context.Background(), correlation, binding)
			if err != nil {
				t.Fatal(err)
			}
			a.terminalization = boundedTestAuditLockTerminalization(func(context.Context, auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
				return auditLockTerminalWorkerResult{}, tc.runner
			})
			_, routeErr := a.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, false))
			if routeErr == nil || routeErr.code != string(daemonRecoverErrorOutcomeUncertain) {
				t.Fatalf("route error=%v, want public uncertainty", routeErr)
			}
			var terminalEvents int
			for {
				select {
				case event := <-received:
					if event.Type != "daemon-recovery-terminal-worker-failure" {
						continue
					}
					terminalEvents++
					if got, _ := event.Body["failure_id"].(string); got != string(tc.wantID) {
						t.Fatalf("failure event id=%q, want %q", got, tc.wantID)
					}
					if _, leaked := event.Body["stderr"]; leaked {
						t.Fatal("terminal failure event contains raw stderr")
					}
				default:
					if terminalEvents != 1 {
						t.Fatalf("terminal failure event count=%d, want exactly 1", terminalEvents)
					}
					return
				}
			}
		})
	}
}

func TestAuditLockTerminalWorkerWriteErrorUsesExactLockedReread(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writeFirst  bool
		wantDurable bool
	}{
		{name: "write then error proves durable", writeFirst: true, wantDurable: true},
		{name: "error before write remains uncertain", writeFirst: false, wantDurable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
			defer adapter.close()
			// This test owns the direct durable-write invariant, not the
			// same-binary transport exercised by the dedicated runner tests.
			adapter.terminalization = directTestAuditLockTerminalization()
			correlation := validAuditLockCorrelation(adapter.serverInstance, 2000)
			binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\\writer-error`, confirm: true}
			reservation, err := adapter.reserve(context.Background(), correlation, binding)
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("writer returned after durable replacement")
			adapter.writeStateFileLockHeld = func(path string, raw []byte) error {
				if tc.writeFirst {
					if err := api.WriteStateFileBytesLockHeld(path, raw); err != nil {
						return err
					}
				}
				return sentinel
			}
			receipt, routeErr := adapter.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, false))
			if tc.wantDurable {
				if routeErr != nil || receipt.Status != auditLockOccurrenceCommittedSuccess {
					t.Fatalf("receipt=%+v err=%v, want durable terminal", receipt, routeErr)
				}
				return
			}
			if routeErr == nil || routeErr.code != string(daemonRecoverErrorOutcomeUncertain) || receipt.Status != auditLockOccurrenceUncertain {
				t.Fatalf("receipt=%+v err=%v, want uncertain", receipt, routeErr)
			}
		})
	}
}

func TestDaemonRecoverTerminalWorkerTimeoutSettlesEveryOrdinaryBranch(t *testing.T) {
	const taskName = `\demo/terminal-table`
	base := committedSettlementResult(taskName)
	for _, tc := range []struct {
		name   string
		result daemonrecovery.Result
		err    error
	}{
		{name: "recover error", result: base, err: errors.New("recoverer rejected after commit")},
		{name: "invalid port owner", result: func() daemonrecovery.Result { r := base; r.PortOwnerCheck = "invalid"; return r }()},
		{name: "invalid port wait", result: func() daemonrecovery.Result { r := base; r.PortWaitOutcome = "invalid"; return r }()},
		{name: "invalid audit handoff", result: func() daemonrecovery.Result { r := base; r.AuditHandoff = "invalid"; return r }()},
		{name: "success", result: base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9125, RecoverySettlementTerminalizationBudget: 30 * time.Millisecond})
			s.events.DisableGUIEventLog = true
			installIsolatedAuditLock(t, s)
			s.auditLock.terminalizationBudget = 30 * time.Millisecond
			defer s.events.Close()
			defer s.auditLock.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			events := s.events.Subscribe(ctx)
			fake := &terminalTableRecoverer{result: tc.result, err: tc.err}
			s.daemonRecover = fake
			s.auditLock.terminalization = boundedTestAuditLockTerminalization(func(context.Context, auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
				return auditLockTerminalWorkerResult{}, &process.StrictRunError{Kind: process.StrictRunTimeout, Cause: context.DeadlineExceeded}
			})
			correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 3000)
			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(correlation, taskName)))
			if response.Code != http.StatusConflict {
				t.Fatalf("response status=%d body=%s, want 409 uncertainty", response.Code, response.Body.String())
			}
			if got := fake.calls.Load(); got != 1 {
				t.Fatalf("recovery calls=%d, want 1", got)
			}
			requireRecoverySettlementEvent(t, events, recoverySettlementPhaseCommitted)
			requireRecoverySettlementEvent(t, events, recoverySettlementPhaseSettled)
			if err := s.recoverySettlements.wait(); err != nil {
				t.Fatalf("settlement registry wait=%v", err)
			}
			requireNoRecoverySettlementPhase(t, events, recoverySettlementPhaseCommitted, recoverySettlementPhaseSettled, recoverySettlementPhaseDrainTimeout)
		})
	}
}

const (
	auditLockR6ReceiverHelperEnv = "MCPHUB_AUDIT_LOCK_R6_RECEIVER_HELPER"
	auditLockR6StateRootEnv      = "MCPHUB_AUDIT_LOCK_R6_STATE_ROOT"
)

func TestAuditLockTerminalWorker_RealHTTPEventPersistenceAndSecondRun(t *testing.T) {
	if os.Getenv(auditLockR6ReceiverHelperEnv) == "1" {
		stateRoot := consumeAuditLockR6ReceiverEnvironment(t)
		runAuditLockR6ReceiverScenario(t, stateRoot)
		return
	}
	stateRoot := t.TempDir()
	if !auditLockR6CrossProcessStateOverrideAvailable(t, stateRoot) {
		t.Skip("real receiver harness requires -tags=test_state_path_env to isolate the hidden child")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAuditLockTerminalWorker_RealHTTPEventPersistenceAndSecondRun$")
	cmd.Env = append(withoutGUITestHelperEnvironment(os.Environ(), runtime.GOOS),
		auditLockR6ReceiverHelperEnv+"=1", auditLockR6StateRootEnv+"="+stateRoot)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("isolated receiver: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	for name, raw := range map[string][]byte{"outer stdout": stdout.Bytes(), "outer stderr": stderr.Bytes()} {
		if count := bytes.Count(raw, []byte(auditLockTerminalWorkerStderrMarker)); count != 0 {
			t.Fatalf("%s leaked raw marker %d times", name, count)
		}
	}
	assertAuditLockMarkerAbsentFromStateRoot(t, stateRoot)
}

func consumeAuditLockR6ReceiverEnvironment(t *testing.T) string {
	t.Helper()
	stateRoot, stateSet := os.LookupEnv(auditLockR6StateRootEnv)
	receiver, receiverSet := os.LookupEnv(auditLockR6ReceiverHelperEnv)
	if !stateSet || !receiverSet || receiver != "1" {
		t.Fatal("R6 receiver framing was not validated before test entry")
	}
	t.Cleanup(func() {
		if err := os.Setenv(auditLockR6ReceiverHelperEnv, receiver); err != nil {
			t.Errorf("restore R6 receiver marker: %v", err)
		}
		if err := os.Setenv(auditLockR6StateRootEnv, stateRoot); err != nil {
			t.Errorf("restore R6 state root marker: %v", err)
		}
	})
	if err := os.Unsetenv(auditLockR6ReceiverHelperEnv); err != nil {
		t.Fatalf("consume R6 receiver marker: %v", err)
	}
	if err := os.Unsetenv(auditLockR6StateRootEnv); err != nil {
		t.Fatalf("consume R6 state root marker: %v", err)
	}
	if _, present := os.LookupEnv(auditLockR6ReceiverHelperEnv); present {
		t.Fatal("R6 receiver marker remained after consumption")
	}
	if _, present := os.LookupEnv(auditLockR6StateRootEnv); present {
		t.Fatal("R6 state root marker remained after consumption")
	}
	return stateRoot
}

func auditLockR6CrossProcessStateOverrideAvailable(t *testing.T, stateRoot string) bool {
	t.Helper()
	restore := api.SetDaemonStateRootForTest("")
	defer restore()
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateRoot)
	resolved, err := api.DaemonStateDirReadOnly()
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(resolved), filepath.Clean(stateRoot))
}

func runAuditLockR6ReceiverScenario(t *testing.T, stateRoot string) {
	t.Helper()
	if stateRoot == "" {
		t.Fatal("missing isolated state root")
	}
	restoreState := api.SetDaemonStateRootForTest(stateRoot)
	t.Cleanup(restoreState)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", stateRoot)
	server := NewServer(Config{Port: 9125, RecoverySettlementTerminalizationBudget: 2 * time.Second})
	defer server.events.Close()
	defer server.auditLock.close()
	// Production activates the occurrence store only after the GUI listener is
	// owned. This direct-handler receiver harness bypasses that lifecycle, so
	// explicitly establish the same precondition before exercising recovery.
	if err := server.auditLock.activateStore(context.Background()); err != nil {
		t.Fatalf("activate isolated occurrence store: %v", err)
	}
	recoverer := &committedTerminalRecoverer{}
	server.daemonRecover = recoverer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := server.events.Subscribe(ctx)

	const firstTask = `\r6/first`
	firstCorrelation := validAuditLockCorrelation(server.auditLock.serverInstance, 6101)
	t.Setenv(auditLockTerminalWorkerStderrHelperEnv, "1")
	firstResponse := httptest.NewRecorder()
	server.mux.ServeHTTP(firstResponse, sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(firstCorrelation, firstTask)))
	if firstResponse.Code != http.StatusConflict {
		t.Fatalf("first response=%d body=%s, want 409", firstResponse.Code, firstResponse.Body.String())
	}
	firstBody := decodeDaemonRecoverBody(t, firstResponse)
	if firstBody["code"] != string(daemonRecoverErrorOutcomeUncertain) {
		t.Fatalf("first response body=%v", firstBody)
	}

	if err := os.Unsetenv(auditLockTerminalWorkerStderrHelperEnv); err != nil {
		t.Fatal(err)
	}
	if _, stillSet := os.LookupEnv(auditLockTerminalWorkerStderrHelperEnv); stillSet {
		t.Fatal("stderr test discriminator remained set")
	}
	const secondTask = `\r6/second`
	secondCorrelation := validAuditLockCorrelation(server.auditLock.serverInstance, 6102)
	secondCorrelation.AttemptID = "22222222-2222-4222-8222-222222222222"
	secondResponse := httptest.NewRecorder()
	server.mux.ServeHTTP(secondResponse, sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(secondCorrelation, secondTask)))
	if secondResponse.Code != http.StatusOK {
		storeRaw, _ := os.ReadFile(server.auditLock.storePath)
		t.Fatalf("second response=%d body=%s events=%v store=%s, want 200", secondResponse.Code, secondResponse.Body.String(), drainAuditLockR6Events(events), storeRaw)
	}
	if got := recoverer.calls.Load(); got != 2 {
		t.Fatalf("recoverer calls=%d, want 2", got)
	}
	if err := server.recoverySettlements.wait(); err != nil {
		t.Fatalf("settlement registry wait=%v", err)
	}
	if !server.auditLock.storeMu.TryLock() {
		t.Fatal("storeMu retained after second response")
	}
	server.auditLock.storeMu.Unlock()
	probe := flock.New(server.auditLock.storePath + ".lock")
	locked, err := probe.TryLock()
	if err != nil || !locked {
		t.Fatalf("fresh occurrence flock: locked=%t err=%v", locked, err)
	}
	if err := probe.Unlock(); err != nil {
		t.Fatal(err)
	}

	observed := drainAuditLockR6Events(events)
	wantStdout := auditLockTerminalWorkerResult{
		Version: auditLockTerminalWorkerProtocolVersion,
		Outcome: "durable_terminal",
		Receipt: auditLockReceiptDTO{
			AttemptID: firstCorrelation.AttemptID, OccurrenceID: firstCorrelation.OccurrenceID,
			ServerInstance: firstCorrelation.ServerInstance, TaskName: firstTask,
			Status: auditLockOccurrenceCommittedSuccess, LockAuthorization: auditLockAuthorizationNone,
			TerminationCommitState: auditLockTerminationStateCommitted,
		},
	}
	wantStdoutRaw, err := json.Marshal(wantStdout)
	if err != nil {
		t.Fatal(err)
	}
	wantStderrBytes := len(strings.Repeat(auditLockTerminalWorkerStderrMarker, auditLockTerminalWorkerStderrMaxBytes/len(auditLockTerminalWorkerStderrMarker)+1))
	workerFailures, settledFirst, settledSecond := 0, 0, 0
	var failureEvent Event
	for _, event := range observed {
		switch event.Type {
		case "daemon-recovery-terminal-worker-failure":
			workerFailures++
			failureEvent = event
		case recoverySettlementEventType:
			if event.Body["phase"] != string(recoverySettlementPhaseSettled) {
				continue
			}
			switch event.Body["occurrence_id"] {
			case firstCorrelation.OccurrenceID:
				settledFirst++
			case secondCorrelation.OccurrenceID:
				settledSecond++
			}
		}
	}
	if workerFailures != 1 || settledFirst != 1 || settledSecond != 1 {
		t.Fatalf("events: worker_failures=%d settled_first=%d settled_second=%d all=%v", workerFailures, settledFirst, settledSecond, observed)
	}
	assertAuditLockR6FailureMetadata(t, failureEvent.Body, len(wantStdoutRaw), wantStderrBytes)

	server.events.Close()
	tail := api.NewAPI().ReadGUIEventLogTail(100)
	persistedFailures := 0
	for _, entry := range tail {
		if entry.Type != "daemon-recovery-terminal-worker-failure" {
			continue
		}
		persistedFailures++
		assertAuditLockR6FailureMetadata(t, entry.Body, len(wantStdoutRaw), wantStderrBytes)
	}
	if persistedFailures != 1 {
		t.Fatalf("persisted worker failure rows=%d, want 1", persistedFailures)
	}
	for name, raw := range map[string][]byte{
		"first HTTP body":  firstResponse.Body.Bytes(),
		"second HTTP body": secondResponse.Body.Bytes(),
	} {
		if bytes.Contains(raw, []byte(auditLockTerminalWorkerStderrMarker)) {
			t.Fatalf("%s exposed raw marker", name)
		}
	}
	assertAuditLockMarkerAbsentFromStateRoot(t, stateRoot)
}

func assertAuditLockR6FailureMetadata(t *testing.T, body map[string]any, wantStdoutBytes, wantStderrBytes int) {
	t.Helper()
	if body["failure_id"] != string(auditLockTerminalWorkerFailureExecutionFailed) ||
		auditLockR6Numeric(body["stdout_bytes"]) != wantStdoutBytes || body["stdout_truncated"] != false ||
		auditLockR6Numeric(body["stderr_bytes"]) != wantStderrBytes || body["stderr_truncated"] != true {
		t.Fatalf("worker failure metadata=%v want stdout=%d stderr=%d", body, wantStdoutBytes, wantStderrBytes)
	}
	if bytes.Contains([]byte(fmt.Sprint(body)), []byte(auditLockTerminalWorkerStderrMarker)) {
		t.Fatal("worker failure metadata exposed raw marker")
	}
}

func auditLockR6Numeric(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return -1
	}
}

func drainAuditLockR6Events(events <-chan Event) []Event {
	var observed []Event
	for {
		select {
		case event := <-events:
			observed = append(observed, event)
		default:
			return observed
		}
	}
}

func assertAuditLockMarkerAbsentFromStateRoot(t *testing.T, stateRoot string) {
	t.Helper()
	err := filepath.WalkDir(stateRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(raw, []byte(auditLockTerminalWorkerStderrMarker)) {
			return fmt.Errorf("raw marker leaked into %s", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
