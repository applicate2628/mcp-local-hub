package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/daemonrecovery"
)

type scriptedOccurrenceStoreLockSpec struct {
	tryErr      error
	unavailable bool
	closeErr    error
	closeStart  chan struct{}
	closeResume <-chan struct{}
}

type scriptedOccurrenceStoreLockFactory struct {
	mu         sync.Mutex
	created    int
	closeCalls map[int]int
	specs      map[int]scriptedOccurrenceStoreLockSpec
}

func newScriptedOccurrenceStoreLockFactory() *scriptedOccurrenceStoreLockFactory {
	return &scriptedOccurrenceStoreLockFactory{
		closeCalls: make(map[int]int),
		specs:      make(map[int]scriptedOccurrenceStoreLockSpec),
	}
}

func (f *scriptedOccurrenceStoreLockFactory) newLock(_ string) occurrenceStoreLock {
	f.mu.Lock()
	f.created++
	index := f.created
	spec := f.specs[index]
	f.mu.Unlock()
	return &scriptedOccurrenceStoreLock{factory: f, index: index, spec: spec}
}

func (f *scriptedOccurrenceStoreLockFactory) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	totalCloseCalls := 0
	for _, calls := range f.closeCalls {
		totalCloseCalls += calls
	}
	return f.created, totalCloseCalls
}

func (f *scriptedOccurrenceStoreLockFactory) closeCount(index int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls[index]
}

type scriptedOccurrenceStoreLock struct {
	factory *scriptedOccurrenceStoreLockFactory
	index   int
	spec    scriptedOccurrenceStoreLockSpec
	start   sync.Once
}

func (l *scriptedOccurrenceStoreLock) TryLockContext(ctx context.Context, _ time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if l.spec.tryErr != nil {
		return false, l.spec.tryErr
	}
	return !l.spec.unavailable, nil
}

func (l *scriptedOccurrenceStoreLock) Close() error {
	l.factory.mu.Lock()
	l.factory.closeCalls[l.index]++
	l.factory.mu.Unlock()
	if l.spec.closeStart != nil {
		l.start.Do(func() { close(l.spec.closeStart) })
	}
	if l.spec.closeResume != nil {
		<-l.spec.closeResume
	}
	return l.spec.closeErr
}

type occurrenceStoreLockHealthEventRecorder struct {
	mu     sync.Mutex
	events []occurrenceStoreLockHealthEvent
}

func (r *occurrenceStoreLockHealthEventRecorder) record(event occurrenceStoreLockHealthEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *occurrenceStoreLockHealthEventRecorder) snapshot() []occurrenceStoreLockHealthEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]occurrenceStoreLockHealthEvent(nil), r.events...)
}

func assertOccurrenceStoreStrandedRoute(t *testing.T, routeErr *auditLockRouteError) {
	t.Helper()
	if routeErr == nil {
		t.Fatal("expected occurrence-store stranded route error")
	}
	if routeErr.status != http.StatusServiceUnavailable || routeErr.code != string(daemonRecoverErrorOccurrenceStoreLockStranded) {
		t.Fatalf("route error status=%d code=%q", routeErr.status, routeErr.code)
	}
	health := routeErr.occurrenceStoreHealth
	if health == nil || health.State != occurrenceStoreLockStranded || !health.RestartRequired || health.Revision == 0 {
		t.Fatalf("route health=%+v", health)
	}
}

func assertOccurrenceStoreStrandedWire(t *testing.T, routeErr *auditLockRouteError) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	writeAuditLockRouteError(recorder, routeErr)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("wire status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != string(daemonRecoverErrorOccurrenceStoreLockStranded) ||
		body["occurrence_store_health"] != string(occurrenceStoreLockStranded) ||
		body["restart_required"] != true || body["occurrence_store_health_revision"] == nil {
		t.Fatalf("wire body=%v", body)
	}
	return body
}

func notCommittedTerminalEvidence() auditLockTerminalEvidence {
	return auditLockTerminalEvidence{
		HTTPStatus: http.StatusInternalServerError,
		ErrorCode:  "RECOVER_STATE_READ_FAILED",
	}
}

type hookedDaemonRecoverer struct {
	result       daemonrecovery.Result
	err          error
	beforeReturn func()
	calls        int
}

func (f *hookedDaemonRecoverer) Recover(_ context.Context, _ string, _ bool, onTerminationCommitted func()) (daemonrecovery.Result, error) {
	f.calls++
	if f.result.TerminationCommitted && onTerminationCommitted != nil {
		onTerminationCommitted()
	}
	if f.beforeReturn != nil {
		f.beforeReturn()
	}
	return f.result, f.err
}

func TestOccurrenceStoreLockHealth_ReleaseFailure_AllOperations(t *testing.T) {
	tests := []struct {
		name       string
		failAt     int
		operation  string
		outcome    occurrenceStoreDataOutcome
		exercise   func(*testing.T, *auditLockAdapter, auditLockCorrelation, auditLockOccurrenceBinding) *auditLockRouteError
		projection bool
	}{
		{
			name: "claim", failAt: 1, operation: "claim occurrence store", outcome: occurrenceStoreDataDurableProven,
			exercise: func(t *testing.T, adapter *auditLockAdapter, _ auditLockCorrelation, _ auditLockOccurrenceBinding) *auditLockRouteError {
				return adapter.ensureReady()
			},
		},
		{
			name: "reserve", failAt: 2, operation: "reserve occurrence", outcome: occurrenceStoreDataDurableProven, projection: true,
			exercise: func(t *testing.T, adapter *auditLockAdapter, correlation auditLockCorrelation, binding auditLockOccurrenceBinding) *auditLockRouteError {
				reservation, routeErr := adapter.reserve(context.Background(), correlation, binding)
				if !reservation.Novel || reservation.Receipt.Status != auditLockOccurrenceInFlight {
					t.Fatalf("durable reservation=%+v", reservation)
				}
				return routeErr
			},
		},
		{
			name: "terminalize", failAt: 3, operation: "terminalize occurrence", outcome: occurrenceStoreDataDurableProven,
			exercise: func(t *testing.T, adapter *auditLockAdapter, correlation auditLockCorrelation, binding auditLockOccurrenceBinding) *auditLockRouteError {
				reservation, routeErr := adapter.reserve(context.Background(), correlation, binding)
				if routeErr != nil {
					t.Fatal(routeErr)
				}
				receipt, terminalErr := adapter.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, notCommittedTerminalEvidence())
				if terminalErr != nil || receipt.Status != auditLockOccurrenceNotCommitted {
					t.Fatalf("durable terminal receipt=%+v err=%v", receipt, terminalErr)
				}
				return nil
			},
		},
		{
			name: "lookup", failAt: 4, operation: "lookup occurrence", outcome: occurrenceStoreDataUnproven,
			exercise: func(t *testing.T, adapter *auditLockAdapter, correlation auditLockCorrelation, binding auditLockOccurrenceBinding) *auditLockRouteError {
				reservation, routeErr := adapter.reserve(context.Background(), correlation, binding)
				if routeErr != nil {
					t.Fatal(routeErr)
				}
				if _, terminalErr := adapter.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, notCommittedTerminalEvidence()); terminalErr != nil {
					t.Fatal(terminalErr)
				}
				receipt, lookupErr := adapter.lookup(context.Background(), correlation)
				if receipt != nil {
					t.Fatalf("lookup returned unproven receipt=%+v", receipt)
				}
				return lookupErr
			},
		},
		{
			name: "snapshot", failAt: 2, operation: "snapshot occurrence store", outcome: occurrenceStoreDataUnproven,
			exercise: func(t *testing.T, adapter *auditLockAdapter, _ auditLockCorrelation, _ auditLockOccurrenceBinding) *auditLockRouteError {
				_, routeErr := adapter.snapshot(context.Background(), nil)
				return routeErr
			},
		},
		{
			name: "acknowledge", failAt: 4, operation: "acknowledge occurrence", outcome: occurrenceStoreDataDurableProven,
			exercise: func(t *testing.T, adapter *auditLockAdapter, correlation auditLockCorrelation, binding auditLockOccurrenceBinding) *auditLockRouteError {
				reservation, routeErr := adapter.reserve(context.Background(), correlation, binding)
				if routeErr != nil {
					t.Fatal(routeErr)
				}
				if _, terminalErr := adapter.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, notCommittedTerminalEvidence()); terminalErr != nil {
					t.Fatal(terminalErr)
				}
				return adapter.acknowledge(context.Background(), correlation)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			factory := newScriptedOccurrenceStoreLockFactory()
			factory.specs[test.failAt] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected release failure")}
			events := &occurrenceStoreLockHealthEventRecorder{}
			adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, stateDir, factory.newLock, events.record)
			correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
			binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\demo/default`, confirm: true}
			if test.name != "claim" {
				if readyErr := adapter.ensureReady(); readyErr != nil {
					t.Fatal(readyErr)
				}
			}

			routeErr := test.exercise(t, adapter, correlation, binding)
			if test.name != "terminalize" {
				assertOccurrenceStoreStrandedRoute(t, routeErr)
			}
			if test.projection {
				body := assertOccurrenceStoreStrandedWire(t, routeErr)
				if body["audit_lock"] == nil {
					t.Fatalf("durable reservation wire omitted audit_lock: %v", body)
				}
			}

			health := adapter.storeLockHealth.snapshot()
			if health.State != occurrenceStoreLockStranded || !health.RestartRequired {
				t.Fatalf("health=%+v", health)
			}
			recorded := events.snapshot()
			if len(recorded) != 1 || recorded[0].Operation != test.operation || recorded[0].DataOutcome != test.outcome || recorded[0].Snapshot != health {
				t.Fatalf("events=%+v health=%+v", recorded, health)
			}
			created, closeCalls := factory.counts()
			if created != test.failAt || closeCalls != test.failAt || factory.closeCount(test.failAt) != 1 {
				t.Fatalf("created=%d closeCalls=%d failedCloseCalls=%d", created, closeCalls, factory.closeCount(test.failAt))
			}

			_, repeatErr := adapter.snapshot(context.Background(), nil)
			assertOccurrenceStoreStrandedRoute(t, repeatErr)
			assertOccurrenceStoreStrandedWire(t, repeatErr)
			if adapter.storeLockHealth.snapshot() != health || len(events.snapshot()) != 1 {
				t.Fatalf("repeat changed permanent health=%+v events=%+v", adapter.storeLockHealth.snapshot(), events.snapshot())
			}
			createdAfterRepeat, closeAfterRepeat := factory.counts()
			if createdAfterRepeat != created || closeAfterRepeat != closeCalls {
				t.Fatalf("repeat retried physical lock: before=(%d,%d) after=(%d,%d)", created, closeCalls, createdAfterRepeat, closeAfterRepeat)
			}
			adapter.close()
			adapter.close()
			if _, closeAfterShutdown := factory.counts(); closeAfterShutdown != closeCalls {
				t.Fatalf("shutdown retried release: before=%d after=%d", closeCalls, closeAfterShutdown)
			}

			restarted := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
			defer restarted.close()
			if readyErr := restarted.ensureReady(); readyErr != nil {
				t.Fatalf("process restart did not recover: %v", readyErr)
			}
			restartedHealth := restarted.storeLockHealth.snapshot()
			if restartedHealth.State != occurrenceStoreLockReleased || restartedHealth.RestartRequired {
				t.Fatalf("restarted health=%+v", restartedHealth)
			}
		})
	}
}

func TestOccurrenceStoreLockHealth_AcquireFailureDoesNotPoison(t *testing.T) {
	tests := []struct {
		name string
		spec scriptedOccurrenceStoreLockSpec
	}{
		{name: "try-lock-error", spec: scriptedOccurrenceStoreLockSpec{tryErr: errors.New("injected acquire failure"), closeErr: errors.New("injected unacquired close failure")}},
		{name: "unavailable", spec: scriptedOccurrenceStoreLockSpec{unavailable: true, closeErr: errors.New("injected unacquired close failure")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := newScriptedOccurrenceStoreLockFactory()
			factory.specs[2] = test.spec
			events := &occurrenceStoreLockHealthEventRecorder{}
			adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, events.record)
			defer adapter.close()
			correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
			binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\demo/default`, confirm: true}
			if _, routeErr := adapter.reserve(context.Background(), correlation, binding); routeErr == nil || routeErr.code != string(daemonRecoverErrorAuditLockAdapterInit) {
				t.Fatalf("acquire route err=%v", routeErr)
			}
			health := adapter.storeLockHealth.snapshot()
			if health.State != occurrenceStoreLockReleased || health.RestartRequired || len(events.snapshot()) != 0 {
				t.Fatalf("acquire failure poisoned health=%+v events=%+v", health, events.snapshot())
			}
			if reservation, routeErr := adapter.reserve(context.Background(), correlation, binding); routeErr != nil || !reservation.Novel {
				t.Fatalf("next explicit acquire did not recover: reservation=%+v err=%v", reservation, routeErr)
			}
			created, closeCalls := factory.counts()
			if created != 3 || closeCalls != 3 || factory.closeCount(2) != 1 {
				t.Fatalf("created=%d closeCalls=%d failedCloseCalls=%d", created, closeCalls, factory.closeCount(2))
			}
		})
	}
}

func TestOccurrenceStoreLockHealth_CancelAndTimeoutReleaseLease(t *testing.T) {
	factory := newScriptedOccurrenceStoreLockFactory()
	adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, nil)
	defer adapter.close()
	adapter.lockTimeout = 10 * time.Millisecond
	adapter.storeMu.Lock()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, routeErr := adapter.lookup(cancelled, validAuditLockCorrelation(adapter.serverInstance, 1)); routeErr == nil {
		adapter.storeMu.Unlock()
		t.Fatal("cancelled lookup unexpectedly succeeded")
	}
	if health := adapter.storeLockHealth.snapshot(); health.State != occurrenceStoreLockReleased || health.RestartRequired {
		adapter.storeMu.Unlock()
		t.Fatalf("cancelled health=%+v", health)
	}

	if _, routeErr := adapter.snapshot(context.Background(), nil); routeErr == nil {
		adapter.storeMu.Unlock()
		t.Fatal("timed-out snapshot unexpectedly succeeded")
	}
	if health := adapter.storeLockHealth.snapshot(); health.State != occurrenceStoreLockReleased || health.RestartRequired {
		adapter.storeMu.Unlock()
		t.Fatalf("timeout health=%+v", health)
	}
	adapter.storeMu.Unlock()

	if _, routeErr := adapter.snapshot(context.Background(), nil); routeErr != nil {
		t.Fatalf("post-timeout snapshot: %v", routeErr)
	}
	created, closeCalls := factory.counts()
	if created != 2 || closeCalls != 2 {
		t.Fatalf("pre-physical cancellation created lock: created=%d closeCalls=%d", created, closeCalls)
	}
}

func TestOccurrenceStoreLockHealth_NoLockOrderDeadlock(t *testing.T) {
	closeStarted := make(chan struct{})
	closeResume := make(chan struct{})
	factory := newScriptedOccurrenceStoreLockFactory()
	factory.specs[3] = scriptedOccurrenceStoreLockSpec{
		closeErr:    errors.New("injected release failure"),
		closeStart:  closeStarted,
		closeResume: closeResume,
	}
	eventDelivered := make(chan occurrenceStoreLockHealthEvent, 1)
	var adapter *auditLockAdapter
	emit := func(event occurrenceStoreLockHealthEvent) {
		// These blocking acquisitions would deadlock if the emitter ran while
		// either owner lock was held.
		adapter.storeLockHealth.mu.Lock()
		adapter.storeLockHealth.mu.Unlock()
		adapter.storeMu.Lock()
		adapter.storeMu.Unlock()
		eventDelivered <- event
	}
	adapter = newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, emit)
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\demo/default`, confirm: true}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	terminalDone := make(chan *auditLockRouteError, 1)
	go func() {
		_, routeErr := adapter.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, notCommittedTerminalEvidence())
		terminalDone <- routeErr
	}()
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal release did not reach injected close")
	}
	if health := adapter.storeLockHealth.snapshot(); health.State != occurrenceStoreLockOutstanding {
		t.Fatalf("health while close outstanding=%+v", health)
	}

	waiterDone := make(chan *auditLockRouteError, 1)
	go func() {
		_, routeErr := adapter.snapshot(context.Background(), nil)
		waiterDone <- routeErr
	}()
	close(closeResume)

	select {
	case routeErr := <-terminalDone:
		if routeErr != nil {
			t.Fatalf("durable terminal result was downgraded: %v", routeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminalizer deadlocked")
	}
	select {
	case routeErr := <-waiterDone:
		assertOccurrenceStoreStrandedRoute(t, routeErr)
	case <-time.After(2 * time.Second):
		t.Fatal("waiting operation deadlocked")
	}
	select {
	case event := <-eventDelivered:
		if event.Operation != "terminalize occurrence" || event.DataOutcome != occurrenceStoreDataDurableProven {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("health event deadlocked")
	}
	if created, closeCalls := factory.counts(); created != 3 || closeCalls != 3 {
		t.Fatalf("stranded waiter entered physical store: created=%d closeCalls=%d", created, closeCalls)
	}
}

func TestOccurrenceStoreLockHealth_TerminalizeAfterStrandedPreservesUncertaintyAndReturns503(t *testing.T) {
	factory := newScriptedOccurrenceStoreLockFactory()
	factory.specs[3] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected release failure")}
	events := &occurrenceStoreLockHealthEventRecorder{}
	adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, events.record)
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\demo/default`, confirm: true}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	if _, poisonErr := adapter.snapshot(context.Background(), nil); poisonErr == nil {
		t.Fatal("snapshot release failure did not strand health")
	}
	healthBefore := adapter.storeLockHealth.snapshot()
	receipt, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceNotCommitted,
		auditLockAuthorizationNone,
		notCommittedTerminalEvidence(),
	)
	assertOccurrenceStoreStrandedRoute(t, terminalErr)
	if receipt.Status != auditLockOccurrenceUncertain || receipt.TerminationCommitState != auditLockTerminationStateUnknown {
		t.Fatalf("terminal receipt=%+v, want preserved effective uncertainty", receipt)
	}
	if terminalErr.auditLockStateProjection == nil ||
		terminalErr.auditLockStateProjection.RecoveryReceipt == nil ||
		terminalErr.auditLockStateProjection.RecoveryReceipt.Status != auditLockOccurrenceUncertain {
		t.Fatalf("terminal route projection=%+v", terminalErr.auditLockStateProjection)
	}
	if adapter.storeLockHealth.snapshot() != healthBefore {
		t.Fatalf("terminal re-entry changed permanent health: before=%+v after=%+v", healthBefore, adapter.storeLockHealth.snapshot())
	}
	if created, closeCalls := factory.counts(); created != 3 || closeCalls != 3 {
		t.Fatalf("terminal re-entry reached physical store: created=%d closeCalls=%d", created, closeCalls)
	}
	recorded := events.snapshot()
	if len(recorded) != 1 || recorded[0].Operation != "snapshot occurrence store" || recorded[0].DataOutcome != occurrenceStoreDataUnproven {
		t.Fatalf("events=%+v", recorded)
	}
}

func TestOccurrenceStoreLockHealth_RouteErrorPlusReleaseFailurePromotes503(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   daemonRecoverErrorCode
	}{
		{name: "attempt conflict", status: http.StatusConflict, code: daemonRecoverErrorAttemptConflict},
		{name: "baseline stale", status: http.StatusConflict, code: daemonRecoverErrorBaselineStale},
		{name: "capacity", status: http.StatusServiceUnavailable, code: daemonRecoverErrorOccurrenceCapacity},
		{name: "receipt in flight", status: http.StatusConflict, code: daemonRecoverErrorReceiptInFlight},
		{name: "ack precondition", status: http.StatusConflict, code: daemonRecoverErrorAckPreconditionRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := newScriptedOccurrenceStoreLockFactory()
			factory.specs[2] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected release failure")}
			events := &occurrenceStoreLockHealthEventRecorder{}
			adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, events.record)
			defer adapter.close()
			originalRoute := &auditLockRouteError{status: test.status, code: string(test.code)}
			operationErr := adapter.withStoreLock(context.Background(), "route failure "+test.name, func(_ *auditLockStoreOperation) error {
				return originalRoute
			})
			mapped := auditLockRouteErrorFromStoreError(operationErr, http.StatusInternalServerError, daemonRecoverErrorAuditLockAdapterInit)
			assertOccurrenceStoreStrandedRoute(t, mapped)
			var retainedRoute *auditLockRouteError
			var retainedHealth *occurrenceStoreLockStrandedError
			if !errors.As(mapped.cause, &retainedRoute) || retainedRoute != originalRoute || !errors.As(mapped.cause, &retainedHealth) {
				t.Fatalf("joined causes route=%p want=%p health=%v cause=%v", retainedRoute, originalRoute, retainedHealth, mapped.cause)
			}
			if created, closeCalls := factory.counts(); created != 2 || closeCalls != 2 || factory.closeCount(2) != 1 {
				t.Fatalf("created=%d closeCalls=%d failedCloseCalls=%d", created, closeCalls, factory.closeCount(2))
			}
			recorded := events.snapshot()
			if len(recorded) != 1 || recorded[0].Operation != "route failure "+test.name || recorded[0].DataOutcome != occurrenceStoreDataUnproven {
				t.Fatalf("events=%+v", recorded)
			}
		})
	}
}

func TestOccurrenceStoreLockHealth_TerminalRouteErrorPlusReleasePreservesInFlightData(t *testing.T) {
	stateDir := t.TempDir()
	factory := newScriptedOccurrenceStoreLockFactory()
	factory.specs[3] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected release failure")}
	events := &occurrenceStoreLockHealthEventRecorder{}
	adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, stateDir, factory.newLock, events.record)
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{serverInstance: adapter.serverInstance, taskName: `\demo/default`, confirm: true}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	// A second process claim changes the durable epoch after the first
	// adapter's pre-check. The first terminalizer therefore returns baseline
	// stale from inside the physical-lock wrapper while its Close also fails.
	reclaimer := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	if readyErr := reclaimer.ensureReady(); readyErr != nil {
		t.Fatal(readyErr)
	}
	reclaimer.close()

	receipt, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceNotCommitted,
		auditLockAuthorizationNone,
		notCommittedTerminalEvidence(),
	)
	assertOccurrenceStoreStrandedRoute(t, terminalErr)
	if receipt.Status != auditLockOccurrenceInFlight || receipt.TerminationCommitState != auditLockTerminationStateUnknown {
		t.Fatalf("terminal receipt=%+v, want unchanged in-flight data outcome", receipt)
	}
	if terminalErr.auditLockStateProjection == nil ||
		terminalErr.auditLockStateProjection.RecoveryReceipt == nil ||
		terminalErr.auditLockStateProjection.RecoveryReceipt.Status != auditLockOccurrenceInFlight {
		t.Fatalf("terminal projection=%+v", terminalErr.auditLockStateProjection)
	}
	var retainedRoute *auditLockRouteError
	var retainedHealth *occurrenceStoreLockStrandedError
	if !errors.As(terminalErr.cause, &retainedRoute) || retainedRoute.code != string(daemonRecoverErrorBaselineStale) || !errors.As(terminalErr.cause, &retainedHealth) {
		t.Fatalf("joined terminal causes route=%v health=%v cause=%v", retainedRoute, retainedHealth, terminalErr.cause)
	}
	if created, closeCalls := factory.counts(); created != 3 || closeCalls != 3 {
		t.Fatalf("created=%d closeCalls=%d", created, closeCalls)
	}
	recorded := events.snapshot()
	if len(recorded) != 1 || recorded[0].Operation != "terminalize occurrence" || recorded[0].DataOutcome != occurrenceStoreDataUnproven {
		t.Fatalf("events=%+v", recorded)
	}
}

func TestAuditLockRouteErrorFromStoreError_JoinedOrderDoesNotChangeStrandedPrecedence(t *testing.T) {
	health := occurrenceStoreLockHealthSnapshot{State: occurrenceStoreLockStranded, Revision: 7, RestartRequired: true}
	healthErr := &occurrenceStoreLockStrandedError{Operation: "test", DataOutcome: occurrenceStoreDataUnproven, Health: health, Cause: errors.New("release failed")}
	projection := &auditLockStateDTO{OccurrenceStoreHealth: occurrenceStoreLockStranded, OccurrenceStoreHealthRevision: 7, RestartRequired: true}
	routeErr := &auditLockRouteError{status: http.StatusConflict, code: string(daemonRecoverErrorBaselineStale), auditLockStateProjection: projection}
	orders := []struct {
		name string
		err  error
	}{
		{name: "route then health", err: errors.Join(routeErr, healthErr)},
		{name: "health then route", err: errors.Join(healthErr, routeErr)},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			mapped := auditLockRouteErrorFromStoreError(order.err, http.StatusInternalServerError, daemonRecoverErrorAuditLockAdapterInit)
			assertOccurrenceStoreStrandedRoute(t, mapped)
			if mapped.auditLockStateProjection != projection {
				t.Fatalf("projection was not retained: got=%p want=%p", mapped.auditLockStateProjection, projection)
			}
			var retainedRoute *auditLockRouteError
			var retainedHealth *occurrenceStoreLockStrandedError
			if !errors.As(mapped.cause, &retainedRoute) || retainedRoute != routeErr || !errors.As(mapped.cause, &retainedHealth) || retainedHealth != healthErr {
				t.Fatalf("joined causes route=%p health=%p cause=%v", retainedRoute, retainedHealth, mapped.cause)
			}
		})
	}
}

func TestDaemonRecoverHandler_TerminalizationStranded_AllFiveExits(t *testing.T) {
	validResult := daemonrecovery.Result{
		PortOwnerCheck:  daemonrecovery.PortOwnerUnbound,
		PortWaitOutcome: daemonrecovery.PortWaitNotRequired,
		AuditHandoff:    daemonrecovery.AuditHandoffDurable,
	}
	tests := []struct {
		name   string
		result daemonrecovery.Result
		err    error
	}{
		{name: "recovery failure", result: validResult, err: &daemonrecovery.OperationError{Kind: daemonrecovery.FailureRespawnFailed, Cause: errors.New("injected recovery failure")}},
		{name: "invalid port owner", result: daemonrecovery.Result{PortWaitOutcome: daemonrecovery.PortWaitNotRequired, AuditHandoff: daemonrecovery.AuditHandoffDurable}},
		{name: "invalid port wait", result: daemonrecovery.Result{PortOwnerCheck: daemonrecovery.PortOwnerUnbound, AuditHandoff: daemonrecovery.AuditHandoffDurable}},
		{name: "invalid audit handoff", result: daemonrecovery.Result{PortOwnerCheck: daemonrecovery.PortOwnerUnbound, PortWaitOutcome: daemonrecovery.PortWaitNotRequired}},
		{name: "success", result: validResult},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9125})
			s.auditLock.close()
			s.events.DisableGUIEventLog = true
			t.Cleanup(s.events.Close)
			factory := newScriptedOccurrenceStoreLockFactory()
			factory.specs[3] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected release failure")}
			events := &occurrenceStoreLockHealthEventRecorder{}
			s.auditLock = newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, events.record)
			correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
			var poisonErr *auditLockRouteError
			fake := &hookedDaemonRecoverer{result: test.result, err: test.err}
			fake.beforeReturn = func() {
				_, poisonErr = s.auditLock.snapshot(context.Background(), nil)
			}
			s.daemonRecover = fake

			response := httptest.NewRecorder()
			s.mux.ServeHTTP(response, sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(correlation, `\demo/default`)))
			assertOccurrenceStoreStrandedRoute(t, poisonErr)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 6 ||
				body["code"] != string(daemonRecoverErrorOccurrenceStoreLockStranded) ||
				body["occurrence_store_health"] != string(occurrenceStoreLockStranded) ||
				body["restart_required"] != true ||
				body["occurrence_store_health_revision"] == nil ||
				body["audit_lock"] == nil {
				t.Fatalf("stranded terminal wire=%#v", body)
			}
			auditLock, _ := body["audit_lock"].(map[string]any)
			receipt, _ := auditLock["recovery_receipt"].(map[string]any)
			if receipt["status"] != auditLockOccurrenceUncertain || receipt["termination_commit_state"] != auditLockTerminationStateUnknown {
				t.Fatalf("preserved terminal receipt=%#v audit_lock=%#v", receipt, auditLock)
			}
			if fake.calls != 1 {
				t.Fatalf("recover calls=%d", fake.calls)
			}
			if created, closeCalls := factory.counts(); created != 3 || closeCalls != 3 {
				t.Fatalf("terminal handler retried physical store: created=%d closeCalls=%d", created, closeCalls)
			}
			recorded := events.snapshot()
			if len(recorded) != 1 || recorded[0].Operation != "snapshot occurrence store" || recorded[0].DataOutcome != occurrenceStoreDataUnproven {
				t.Fatalf("events=%+v", recorded)
			}
		})
	}
}

func TestWriteDaemonRecoverTerminalError_NonHealthRetainsUncertain409(t *testing.T) {
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	reservation := auditLockReservation{
		Receipt: auditLockReceiptDTO{
			AttemptID:      correlation.AttemptID,
			OccurrenceID:   correlation.OccurrenceID,
			ServerInstance: correlation.ServerInstance,
			TaskName:       `\demo/default`,
			Status:         auditLockOccurrenceInFlight,
		},
	}
	receipt := auditLockUncertainReceipt(reservation)
	recorder := httptest.NewRecorder()
	(&Server{auditLock: adapter}).writeDaemonRecoverTerminalError(
		recorder,
		receipt,
		&auditLockRouteError{status: http.StatusInternalServerError, code: string(daemonRecoverErrorAuditLockAdapterInit)},
	)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response daemonRecoverOccurrenceErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != string(daemonRecoverErrorOutcomeUncertain) ||
		response.TerminationCommitState != auditLockTerminationStateUnknown ||
		response.AuditLock.RecoveryReceipt == nil ||
		response.AuditLock.RecoveryReceipt.Status != auditLockOccurrenceUncertain {
		t.Fatalf("non-health terminal response=%+v", response)
	}
}
