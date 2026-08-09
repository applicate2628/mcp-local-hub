package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/daemonrecovery"
)

func committedSettlementResult(taskName string) daemonrecovery.Result {
	return daemonrecovery.Result{
		TaskName:             taskName,
		Reaped:               true,
		PortOwnerCheck:       daemonrecovery.PortOwnerReaped,
		PortWaitOutcome:      daemonrecovery.PortWaitNotRequired,
		AuditHandoff:         daemonrecovery.AuditHandoffDurable,
		TerminationCommitted: true,
	}
}

func startRecoverySettlementServer(t *testing.T, s *Server) (context.CancelFunc, <-chan struct{}, *error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan struct{})
	var result error
	go func() {
		result = s.Start(ctx, ready)
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("server never signaled ready")
	}
	return cancel, done, &result
}

func waitRecoverySettlementServer(t *testing.T, done <-chan struct{}, result *error) error {
	t.Helper()
	select {
	case <-done:
		return *result
	case <-time.After(2 * time.Second):
		t.Fatal("server did not return")
		return nil
	}
}

func beginLiveDaemonRecovery(t *testing.T, s *Server, correlation auditLockCorrelation, taskName string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		request, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%d/api/daemon/recover", s.Port()),
			strings.NewReader(correlationPOSTBody(correlation, taskName)),
		)
		if err != nil {
			done <- err
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", s.Port()))
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	return done
}

func waitLiveDaemonRecovery(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("live daemon recovery request did not return")
	}
}

func installDaemonRecoveryHandlerCompletion(s *Server) <-chan struct{} {
	done := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/recover", s.requireSameOrigin(func(w http.ResponseWriter, r *http.Request) {
		defer once.Do(func() { close(done) })
		s.daemonRecoverHandler(w, r)
	}))
	s.mux = mux
	return done
}

func waitDaemonRecoveryHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("server-side daemon recovery handler did not return")
	}
}

func requireNoRecoverySettlementPhase(t *testing.T, events <-chan Event, forbidden ...recoverySettlementPhase) {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event.Type != recoverySettlementEventType {
				continue
			}
			phase, _ := event.Body["phase"].(string)
			for _, candidate := range forbidden {
				if phase == string(candidate) {
					t.Fatalf("received unexpected recovery settlement phase %q: %#v", phase, event.Body)
				}
			}
		default:
			return
		}
	}
}

func requireRecoverySettlementEvent(t *testing.T, events <-chan Event, phase recoverySettlementPhase) Event {
	t.Helper()
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event := <-events:
			if event.Type != recoverySettlementEventType {
				continue
			}
			if got, _ := event.Body["phase"].(string); got != string(phase) {
				continue
			}
			return event
		case <-timeout.C:
			t.Fatalf("did not receive recovery settlement phase %q", phase)
			return Event{}
		}
	}
}

func waitShutdownDrain(t *testing.T, observed <-chan error) error {
	t.Helper()
	select {
	case err := <-observed:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("GUI HTTP drain did not finish")
		return nil
	}
}

func TestRecoverySettlementRegistry_GUIShutdownJoinsCommittedRecovery(t *testing.T) {
	const taskName = `\demo/default`
	cases := []struct {
		name        string
		recoveryErr error
		wantStatus  string
	}{
		{
			name:       "committed success",
			wantStatus: auditLockOccurrenceCommittedSuccess,
		},
		{
			name:        "committed recovery failure",
			recoveryErr: &daemonrecovery.OperationError{Kind: daemonrecovery.FailureRespawnFailed, TaskName: taskName, Cause: errors.New("respawn rejected")},
			wantStatus:  auditLockOccurrenceCommittedError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{
				Port:                                    0,
				GUIShutdownDrainTimeout:                 30 * time.Millisecond,
				RecoverySettlementPostCommitBudget:      500 * time.Millisecond,
				RecoverySettlementTerminalizationBudget: 20 * time.Millisecond,
			})
			s.events.DisableGUIEventLog = true
			installIsolatedAuditLock(t, s)
			eventsCtx, cancelEvents := context.WithCancel(context.Background())
			defer cancelEvents()
			events := s.events.Subscribe(eventsCtx)
			fake := &blockingDaemonRecoverer{
				started: make(chan struct{}),
				release: make(chan struct{}),
				result:  committedSettlementResult(taskName),
				err:     tc.recoveryErr,
			}
			s.daemonRecover = fake
			shutdownObserved := make(chan error, 1)
			s.shutdownDrainObserved = func(err error) { shutdownObserved <- err }

			cancelServer, serverDone, serverResult := startRecoverySettlementServer(t, s)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(fake.release) }) }
			defer func() {
				release()
				cancelServer()
				_ = waitRecoverySettlementServer(t, serverDone, serverResult)
				s.events.Close()
			}()

			correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
			requestDone := beginLiveDaemonRecovery(t, s, correlation, taskName)
			select {
			case <-fake.started:
			case <-time.After(2 * time.Second):
				t.Fatal("committed recovery did not reach its blocking post-commit fake")
			}
			requireRecoverySettlementEvent(t, events, recoverySettlementPhaseCommitted)

			cancelServer()
			if shutdownErr := waitShutdownDrain(t, shutdownObserved); !errors.Is(shutdownErr, context.DeadlineExceeded) {
				t.Fatalf("ordinary GUI HTTP drain error=%v, want context deadline exceeded", shutdownErr)
			}
			// The fake is an intentionally unbounded post-commit recovery. After the
			// normal GUI HTTP drain has elapsed, Start must still be owned by the
			// settlement registry rather than return while this lease is committed.
			select {
			case <-serverDone:
				t.Fatalf("Start returned before committed recovery settled: %v", *serverResult)
			case <-time.After(100 * time.Millisecond):
			}

			release()
			startErr := waitRecoverySettlementServer(t, serverDone, serverResult)
			if !errors.Is(startErr, context.DeadlineExceeded) {
				t.Fatalf("Start error=%v, want ordinary GUI drain deadline", startErr)
			}
			if errors.Is(startErr, ErrRecoverySettlementDrainTimeout) {
				t.Fatalf("Start error=%v, committed recovery settled inside the registry budget", startErr)
			}
			requireRecoverySettlementEvent(t, events, recoverySettlementPhaseSettled)
			receipt, receiptErr := s.auditLock.lookup(context.Background(), correlation)
			if receiptErr != nil || receipt == nil || receipt.Status != tc.wantStatus {
				t.Fatalf("terminal receipt=%+v err=%v, want status %q", receipt, receiptErr, tc.wantStatus)
			}
			if got := fake.calls.Load(); got != 1 {
				t.Fatalf("recovery calls=%d, want exactly one", got)
			}
			if err := s.recoverySettlements.wait(); err != nil {
				t.Fatalf("settlement registry still reports work after terminal receipt: %v", err)
			}
			waitLiveDaemonRecovery(t, requestDone)
		})
	}
}

func TestRecoverySettlementRegistry_DrainTimeoutFailsLoud(t *testing.T) {
	const taskName = `\demo/default`
	s := NewServer(Config{
		Port:                                    0,
		GUIShutdownDrainTimeout:                 30 * time.Millisecond,
		RecoverySettlementPostCommitBudget:      80 * time.Millisecond,
		RecoverySettlementTerminalizationBudget: 20 * time.Millisecond,
	})
	s.events.DisableGUIEventLog = true
	installIsolatedAuditLock(t, s)
	handlerDone := installDaemonRecoveryHandlerCompletion(s)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := s.events.Subscribe(eventsCtx)
	fake := &blockingDaemonRecoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  committedSettlementResult(taskName),
	}
	s.daemonRecover = fake
	shutdownObserved := make(chan error, 1)
	s.shutdownDrainObserved = func(err error) { shutdownObserved <- err }

	cancelServer, serverDone, serverResult := startRecoverySettlementServer(t, s)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(fake.release) }) }
	defer func() {
		release()
		cancelServer()
		_ = waitRecoverySettlementServer(t, serverDone, serverResult)
		s.events.Close()
	}()

	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
	requestDone := beginLiveDaemonRecovery(t, s, correlation, taskName)
	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("committed recovery did not reach its blocking post-commit fake")
	}
	requireRecoverySettlementEvent(t, events, recoverySettlementPhaseCommitted)

	cancelServer()
	if shutdownErr := waitShutdownDrain(t, shutdownObserved); !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("ordinary GUI HTTP drain error=%v, want context deadline exceeded", shutdownErr)
	}
	startErr := waitRecoverySettlementServer(t, serverDone, serverResult)
	if !errors.Is(startErr, context.DeadlineExceeded) {
		t.Fatalf("Start error=%v, want ordinary GUI drain deadline", startErr)
	}
	if !errors.Is(startErr, ErrRecoverySettlementDrainTimeout) {
		t.Fatalf("Start error=%v, want %v", startErr, ErrRecoverySettlementDrainTimeout)
	}
	timeoutEvent := requireRecoverySettlementEvent(t, events, recoverySettlementPhaseDrainTimeout)
	if timeoutEvent.Body["event"] != recoverySettlementDrainTimeoutEvent ||
		timeoutEvent.Body["failure_id"] != recoverySettlementDrainTimeoutCode {
		t.Fatalf("timeout event body=%#v", timeoutEvent.Body)
	}
	receipt, receiptErr := s.auditLock.lookup(context.Background(), correlation)
	if receiptErr != nil || receipt == nil || receipt.Status != auditLockOccurrenceInFlight {
		t.Fatalf("timeout receipt=%+v err=%v, want durable in_flight", receipt, receiptErr)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("recovery calls=%d, want exactly one", got)
	}
	release()
	waitDaemonRecoveryHandler(t, handlerDone)
	waitLiveDaemonRecovery(t, requestDone)
	if err := s.recoverySettlements.wait(); !errors.Is(err, ErrRecoverySettlementDrainTimeout) {
		t.Fatalf("registry wait after late handler completion error=%v, want cached %v", err, ErrRecoverySettlementDrainTimeout)
	}
}

type panicAfterCommitDaemonRecoverer struct {
	calls     atomic.Int32
	committed chan struct{}
	panicNow  chan struct{}
}

func (f *panicAfterCommitDaemonRecoverer) Recover(
	_ context.Context,
	_ string,
	_ bool,
	onTerminationCommitted func(),
) (daemonrecovery.Result, error) {
	f.calls.Add(1)
	onTerminationCommitted()
	close(f.committed)
	<-f.panicNow
	panic("test panic after committed termination")
}

func TestRecoverySettlementRegistry_PanicAfterCommitTerminalizesAndSettles(t *testing.T) {
	const taskName = `\demo/default`
	s := NewServer(Config{
		Port:                                    0,
		GUIShutdownDrainTimeout:                 30 * time.Millisecond,
		RecoverySettlementPostCommitBudget:      80 * time.Millisecond,
		RecoverySettlementTerminalizationBudget: 20 * time.Millisecond,
	})
	s.events.DisableGUIEventLog = true
	installIsolatedAuditLock(t, s)
	handlerDone := installDaemonRecoveryHandlerCompletion(s)
	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := s.events.Subscribe(eventsCtx)
	fake := &panicAfterCommitDaemonRecoverer{
		committed: make(chan struct{}),
		panicNow:  make(chan struct{}),
	}
	s.daemonRecover = fake
	shutdownObserved := make(chan error, 1)
	s.shutdownDrainObserved = func(err error) { shutdownObserved <- err }

	cancelServer, serverDone, serverResult := startRecoverySettlementServer(t, s)
	var panicOnce sync.Once
	triggerPanic := func() { panicOnce.Do(func() { close(fake.panicNow) }) }
	defer func() {
		triggerPanic()
		cancelServer()
		_ = waitRecoverySettlementServer(t, serverDone, serverResult)
		s.events.Close()
	}()

	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
	requestDone := beginLiveDaemonRecovery(t, s, correlation, taskName)
	select {
	case <-fake.committed:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery did not commit before the panic guard")
	}
	requireRecoverySettlementEvent(t, events, recoverySettlementPhaseCommitted)

	cancelServer()
	if shutdownErr := waitShutdownDrain(t, shutdownObserved); !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("ordinary GUI HTTP drain error=%v, want context deadline exceeded", shutdownErr)
	}
	triggerPanic()
	startErr := waitRecoverySettlementServer(t, serverDone, serverResult)
	if !errors.Is(startErr, context.DeadlineExceeded) {
		t.Fatalf("Start error=%v, want ordinary GUI drain deadline", startErr)
	}
	if errors.Is(startErr, ErrRecoverySettlementDrainTimeout) {
		t.Fatalf("Start error=%v, post-commit panic terminalization must complete the settlement lease", startErr)
	}
	requireRecoverySettlementEvent(t, events, recoverySettlementPhaseSettled)
	waitDaemonRecoveryHandler(t, handlerDone)
	waitLiveDaemonRecovery(t, requestDone)
	receipt, receiptErr := s.auditLock.lookup(context.Background(), correlation)
	if receiptErr != nil || receipt == nil || receipt.Status != auditLockOccurrenceInFlight {
		t.Fatalf("panic receipt=%+v err=%v, want the shutdown-closed adapter's durable in_flight record", receipt, receiptErr)
	}
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("recovery calls=%d, want exactly one", got)
	}
	if err := s.recoverySettlements.wait(); err != nil {
		t.Fatalf("registry wait after post-commit panic=%v, want settled", err)
	}
	requireNoRecoverySettlementPhase(t, events, recoverySettlementPhaseDrainTimeout)
}

func TestRecoverySettlementRegistry_ClosedAdmissionRejectsBeforeReserve(t *testing.T) {
	const taskName = `\demo/default`
	s := NewServer(Config{Port: 9125})
	s.events.DisableGUIEventLog = true
	installIsolatedAuditLock(t, s)
	defer s.events.Close()
	fake := &blockingDaemonRecoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  committedSettlementResult(taskName),
	}
	s.daemonRecover = fake
	s.recoverySettlements.closeAdmission()
	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)

	request := sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(correlation, taskName))
	response := httptest.NewRecorder()
	s.mux.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode rejection body: %v", err)
	}
	if body["code"] != string(daemonRecoverErrorOccurrenceCapacity) {
		t.Fatalf("rejection body=%#v, want existing no-admission code", body)
	}
	if got := fake.calls.Load(); got != 0 {
		t.Fatalf("recovery calls=%d after closed admission, want 0", got)
	}
	receipt, receiptErr := s.auditLock.lookup(context.Background(), correlation)
	if receiptErr != nil || receipt != nil {
		t.Fatalf("closed admission reserved an occurrence: receipt=%+v err=%v", receipt, receiptErr)
	}
}

func TestRecoverySettlementRegistry_TimeoutAfterSettlementDoesNotSynthesizeFailure(t *testing.T) {
	var events []Event
	registry := newRecoverySettlementRegistry(time.Second, time.Second, func(event Event) {
		events = append(events, event)
	})
	lease, admitted := registry.admit(`\demo/default`, auditLockCorrelation{
		AttemptID:      "11111111-1111-4111-8111-111111111111",
		OccurrenceID:   "00000000-0000-4000-8000-000000000001",
		ServerInstance: "22222222-2222-4222-8222-222222222222",
	})
	if !admitted {
		t.Fatal("lease admission unexpectedly rejected")
	}
	lease.markCommitted()
	lease.complete()

	if err := registry.failDrainTimeout(); err != nil {
		t.Fatalf("timeout after concurrent settlement=%v, want nil", err)
	}
	if len(events) != 2 ||
		events[0].Body["phase"] != string(recoverySettlementPhaseCommitted) ||
		events[1].Body["phase"] != string(recoverySettlementPhaseSettled) {
		t.Fatalf("events=%#v, want committed then settled without drain timeout", events)
	}
}
