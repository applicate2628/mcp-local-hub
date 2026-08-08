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
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
)

type blockingDaemonRecoverer struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
	result  daemonrecovery.Result
	err     error
}

func (f *blockingDaemonRecoverer) Recover(_ context.Context, _ string, _ bool, onTerminationCommitted func()) (daemonrecovery.Result, error) {
	f.calls.Add(1)
	if f.result.TerminationCommitted && onTerminationCommitted != nil {
		onTerminationCommitted()
	}
	f.once.Do(func() { close(f.started) })
	<-f.release
	return f.result, f.err
}

func sameOriginRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

func validAuditLockCorrelation(serverInstance string, n int) auditLockCorrelation {
	return auditLockCorrelation{
		AttemptID:      "11111111-1111-4111-8111-111111111111",
		OccurrenceID:   fmt.Sprintf("00000000-0000-4000-8000-%012x", n),
		ServerInstance: serverInstance,
	}
}

func correlationPOSTBody(c auditLockCorrelation, task string) string {
	return fmt.Sprintf(`{"task_name":%q,"confirm":true,"audit_lock_attempt":{"attempt_id":%q,"occurrence_id":%q,"server_instance":%q}}`,
		task, c.AttemptID, c.OccurrenceID, c.ServerInstance)
}

func acknowledgeBody(c auditLockCorrelation) string {
	return fmt.Sprintf(`{"attempt_id":%q,"occurrence_id":%q,"server_instance":%q,"acknowledge":true}`,
		c.AttemptID, c.OccurrenceID, c.ServerInstance)
}

func acknowledgeBodyWithExpectedPhysical(c auditLockCorrelation, snapshot api.SupervisorEventLockSnapshot) string {
	return acknowledgeBodyWithExpectedPhysicalForServer(c, c.ServerInstance, snapshot)
}

func acknowledgeBodyWithExpectedPhysicalForServer(c auditLockCorrelation, expectedServerInstance string, snapshot api.SupervisorEventLockSnapshot) string {
	return fmt.Sprintf(`{"attempt_id":%q,"occurrence_id":%q,"server_instance":%q,"acknowledge":true,"expected_physical":{"server_instance":%q,"revision":%d,"state":%q}}`,
		c.AttemptID, c.OccurrenceID, c.ServerInstance, expectedServerInstance, snapshot.Revision, snapshot.State)
}

func successfulTerminalEvidence(taskName string, terminationCommitted bool) auditLockTerminalEvidence {
	return auditLockTerminalEvidence{
		HTTPStatus: http.StatusOK,
		Success: &daemonRecoverSuccessEvidence{
			TaskName:             taskName,
			Reaped:               terminationCommitted,
			PortOwnerCheck:       string(daemonrecovery.PortOwnerUnbound),
			PortWaitOutcome:      string(daemonrecovery.PortWaitNotRequired),
			AuditHandoff:         string(daemonrecovery.AuditHandoffDurable),
			TerminationCommitted: terminationCommitted,
		},
	}
}

func directTestAuditLockTerminalization() auditLockTerminalizationConfig {
	return auditLockTerminalizationConfig{mode: auditLockTerminalizationDirectTest}
}

func boundedTestAuditLockTerminalization(runner auditLockTerminalRunner) auditLockTerminalizationConfig {
	return auditLockTerminalizationConfig{mode: auditLockTerminalizationBounded, runner: runner}
}

func newDirectTestAuditLockAdapterInStateDir(events *Broadcaster, stateDir string) *auditLockAdapter {
	return newAuditLockAdapterInStateDirWithTerminalizationBudget(events, stateDir, auditLockStoreLockTimeout, directTestAuditLockTerminalization())
}

func newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(
	events *Broadcaster,
	stateDir string,
	factory occurrenceStoreLockFactory,
	emit occurrenceStoreLockHealthEmitter,
) *auditLockAdapter {
	return newAuditLockAdapterInStateDirWithStoreLockDeps(events, stateDir, factory, emit, directTestAuditLockTerminalization())
}

func newBoundedTestAuditLockAdapterInStateDir(events *Broadcaster, stateDir string, budget time.Duration, runner auditLockTerminalRunner) *auditLockAdapter {
	return newAuditLockAdapterInStateDirWithTerminalizationBudget(events, stateDir, budget, boundedTestAuditLockTerminalization(runner))
}

func TestAuditLockTerminalizationConfigIsExplicitPerInstance(t *testing.T) {
	const budget = 43 * time.Millisecond
	server := NewServer(Config{Port: 0, RecoverySettlementTerminalizationBudget: budget})
	defer server.auditLock.close()
	defer server.events.Close()
	if server.auditLock.initErr != nil {
		t.Fatal(server.auditLock.initErr)
	}
	if server.auditLock.terminalization.mode != auditLockTerminalizationBounded || server.auditLock.terminalization.runner == nil {
		t.Fatalf("production terminalization=%+v, want bounded real worker", server.auditLock.terminalization)
	}
	if reflect.ValueOf(server.auditLock.terminalization.runner).Pointer() != reflect.ValueOf(runAuditLockTerminalWorker).Pointer() {
		t.Fatal("production constructor did not bind the real same-binary worker")
	}
	if server.auditLock.terminalizationBudget != budget || server.recoverySettlements.terminalizationBudget != budget {
		t.Fatalf("budget adapter=%s registry=%s want=%s", server.auditLock.terminalizationBudget, server.recoverySettlements.terminalizationBudget, budget)
	}

	direct := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer direct.close()
	if direct.initErr != nil || direct.terminalization.mode != auditLockTerminalizationDirectTest || direct.terminalization.runner != nil {
		t.Fatalf("direct test terminalization=%+v init=%v", direct.terminalization, direct.initErr)
	}

	fakeRunner := auditLockTerminalRunner(func(context.Context, auditLockTerminalWorkerRequest) (auditLockTerminalWorkerResult, error) {
		return auditLockTerminalWorkerResult{}, nil
	})
	bounded := newBoundedTestAuditLockAdapterInStateDir(nil, t.TempDir(), budget, fakeRunner)
	defer bounded.close()
	if bounded.initErr != nil || bounded.terminalization.mode != auditLockTerminalizationBounded ||
		reflect.ValueOf(bounded.terminalization.runner).Pointer() != reflect.ValueOf(fakeRunner).Pointer() {
		t.Fatalf("bounded test terminalization=%+v init=%v", bounded.terminalization, bounded.initErr)
	}

	invalid := []struct {
		name   string
		config auditLockTerminalizationConfig
	}{
		{name: "zero", config: auditLockTerminalizationConfig{}},
		{name: "unknown", config: auditLockTerminalizationConfig{mode: 99}},
		{name: "bounded-nil", config: auditLockTerminalizationConfig{mode: auditLockTerminalizationBounded}},
		{name: "direct-nonnil", config: auditLockTerminalizationConfig{mode: auditLockTerminalizationDirectTest, runner: fakeRunner}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newAuditLockAdapterInStateDirWithTerminalizationBudget(nil, t.TempDir(), budget, tc.config)
			defer adapter.close()
			var configErr *auditLockTerminalizationConfigError
			if !errors.As(adapter.initErr, &configErr) {
				t.Fatalf("init error=%v, want typed terminalization config failure", adapter.initErr)
			}
			if adapter.serverInstance != "" {
				t.Fatalf("invalid config claimed adapter identity %q", adapter.serverInstance)
			}
		})
	}

	source, err := os.ReadFile("audit_lock_state.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"defaultAuditLockTerminalRunner", "os.Args", "os.Getenv(", "os.LookupEnv(", `".test"`} {
		if bytes.Contains(source, []byte(forbidden)) {
			t.Fatalf("terminalization composition retains ambient selector %q", forbidden)
		}
	}
}

func installIsolatedAuditLock(t *testing.T, s *Server) {
	t.Helper()
	s.auditLock.close()
	s.auditLock = newDirectTestAuditLockAdapterInStateDir(s.events, t.TempDir())
	if err := s.auditLock.ensureReady(); err != nil {
		t.Fatalf("isolated audit-lock adapter: %v", err)
	}
	t.Cleanup(s.auditLock.close)
}

func TestAuditLockOccurrenceSurvivesNewServerAndReplaysWithoutRecover(t *testing.T) {
	stateDir := t.TempDir()
	first := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	if err := first.ensureReady(); err != nil {
		t.Fatal(err)
	}
	origin := first.serverInstance
	correlation := validAuditLockCorrelation(origin, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: origin,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := first.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("first reserve=%+v err=%v", reservation, reserveErr)
	}
	if _, terminalErr := first.terminalize(
		reservation,
		auditLockOccurrenceCommittedSuccess,
		"none",
		successfulTerminalEvidence(binding.taskName, false),
	); terminalErr != nil {
		t.Fatalf("first terminalization: %v", terminalErr)
	}
	first.close()

	second := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer second.close()
	if err := second.ensureReady(); err != nil {
		t.Fatal(err)
	}
	if second.serverInstance == origin {
		t.Fatal("restart did not claim a fresh active server instance")
	}
	replay, replayErr := second.reserve(context.Background(), correlation, binding)
	if replayErr != nil ||
		replay.Novel ||
		replay.Terminal == nil ||
		replay.Receipt.Status != auditLockOccurrenceCommittedSuccess ||
		replay.Receipt.TaskName != `\demo/default` {
		t.Fatalf("durable replay=%+v err=%v", replay, replayErr)
	}
}

func TestAuditLockPriorServerInFlightBecomesUncertainAndBlocksNewCorrelation(t *testing.T) {
	stateDir := t.TempDir()
	first := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	origin := first.serverInstance
	correlation := validAuditLockCorrelation(origin, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: origin,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := first.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("first reserve=%+v err=%v", reservation, reserveErr)
	}
	first.close()

	second := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer second.close()
	replay, replayErr := second.reserve(context.Background(), correlation, binding)
	if replayErr != nil ||
		replay.Receipt.Status != auditLockOccurrenceUncertain ||
		replay.Receipt.TerminationCommitState != auditLockTerminationStateUnknown {
		t.Fatalf("restart replay=%+v err=%v", replay, replayErr)
	}

	next := validAuditLockCorrelation(second.serverInstance, 2)
	next.AttemptID = "22222222-2222-4222-8222-222222222222"
	_, nextErr := second.reserve(context.Background(), next, auditLockOccurrenceBinding{
		serverInstance: second.serverInstance,
		taskName:       binding.taskName,
		confirm:        true,
	})
	if nextErr == nil || nextErr.code != "RECOVER_ATTEMPT_CONFLICT" {
		t.Fatalf("new correlation behind uncertain record err=%v", nextErr)
	}
}

func TestAuditLockGenerationRejectsLateOldServerTerminalization(t *testing.T) {
	stateDir := t.TempDir()
	first := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer first.close()
	correlation := validAuditLockCorrelation(first.serverInstance, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: first.serverInstance,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := first.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	second := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer second.close()
	receipt, terminalErr := first.terminalize(
		reservation,
		auditLockOccurrenceCommittedSuccess,
		"none",
		successfulTerminalEvidence(binding.taskName, false),
	)
	if terminalErr == nil ||
		terminalErr.code != string(daemonRecoverErrorBaselineStale) ||
		receipt.Status != auditLockOccurrenceInFlight {
		t.Fatalf("late terminalization receipt=%+v err=%v", receipt, terminalErr)
	}
	persisted, lookupErr := second.lookup(context.Background(), correlation)
	if lookupErr != nil || persisted == nil || persisted.Status != auditLockOccurrenceUncertain {
		t.Fatalf("persisted restart outcome=%+v err=%v", persisted, lookupErr)
	}
}

func TestAuditLockACK_PriorGenerationTerminal_AllStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status string
	}{
		{name: "committed success", status: auditLockOccurrenceCommittedSuccess},
		{name: "committed error", status: auditLockOccurrenceCommittedError},
		{name: "not committed", status: auditLockOccurrenceNotCommitted},
		{name: "uncertain", status: auditLockOccurrenceUncertain},
	}

	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			first := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
			if err := first.ensureReady(); err != nil {
				t.Fatal(err)
			}
			originEpoch := first.currentMutationEpoch()
			correlation := validAuditLockCorrelation(originEpoch.ServerInstance, 200+index)
			correlation.AttemptID = fmt.Sprintf("20000000-0000-4000-8000-%012x", index)
			binding := auditLockOccurrenceBinding{
				serverInstance: originEpoch.ServerInstance,
				taskName:       fmt.Sprintf(`\demo/prior-generation-%d`, index),
				confirm:        true,
			}
			reservation, reserveErr := first.reserve(context.Background(), correlation, binding)
			if reserveErr != nil || !reservation.Novel || reservation.MutationEpoch != originEpoch {
				t.Fatalf("origin reserve=%+v err=%v epoch=%+v", reservation, reserveErr, originEpoch)
			}
			if tc.status != auditLockOccurrenceUncertain {
				terminal := auditLockTerminalEvidence{
					HTTPStatus: http.StatusInternalServerError,
					ErrorCode:  string(daemonRecoverErrorStateRead),
				}
				if tc.status == auditLockOccurrenceCommittedSuccess {
					terminal = successfulTerminalEvidence(binding.taskName, true)
				} else if tc.status == auditLockOccurrenceCommittedError {
					terminal.TerminationCommitted = true
				}
				if _, terminalErr := first.terminalize(reservation, tc.status, auditLockAuthorizationNone, terminal); terminalErr != nil {
					t.Fatalf("origin terminalization: %v", terminalErr)
				}
			}
			first.close()

			second := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
			defer second.close()
			if err := second.ensureReady(); err != nil {
				t.Fatal(err)
			}
			claimedEpoch := second.currentMutationEpoch()
			if claimedEpoch.ServerInstance == originEpoch.ServerInstance || claimedEpoch.Generation != originEpoch.Generation+1 {
				t.Fatalf("claimed epoch=%+v, origin=%+v", claimedEpoch, originEpoch)
			}
			retained, lookupErr := second.lookup(context.Background(), correlation)
			if lookupErr != nil || retained == nil || retained.Status != tc.status || retained.ServerInstance != originEpoch.ServerInstance {
				t.Fatalf("retained receipt=%+v err=%v, want status=%q origin=%q", retained, lookupErr, tc.status, originEpoch.ServerInstance)
			}

			blockedCorrelation := validAuditLockCorrelation(claimedEpoch.ServerInstance, 300+index)
			blockedCorrelation.AttemptID = fmt.Sprintf("30000000-0000-4000-8000-%012x", index)
			if _, blockedErr := second.reserve(context.Background(), blockedCorrelation, auditLockOccurrenceBinding{
				serverInstance: claimedEpoch.ServerInstance,
				taskName:       binding.taskName,
				confirm:        true,
			}); blockedErr == nil || blockedErr.code != string(daemonRecoverErrorAttemptConflict) {
				t.Fatalf("same-task fence before ACK err=%v", blockedErr)
			}

			if tc.status == auditLockOccurrenceCommittedSuccess {
				physical := api.SupervisorEventLockSnapshotForPath(second.logPath)
				wrongOriginRequest := auditLockAcknowledgeRequest{
					Correlation: correlation,
					ExpectedPhysical: &auditLockExpectedPhysical{
						ServerInstance: originEpoch.ServerInstance,
						Revision:       physical.Revision,
						State:          physical.State,
					},
				}
				if routeErr := second.acknowledgeRequest(context.Background(), wrongOriginRequest); routeErr == nil || routeErr.code != string(daemonRecoverErrorAckPreconditionRequired) {
					t.Fatalf("origin-bound physical proof err=%v, want current-epoch precondition", routeErr)
				}

				logger, err := api.OpenSupervisorEventLog(second.logPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := logger.EmitWithTimeout(api.SupervisorEvent{Severity: "info", Source: "test", Event: "advance-prior-generation-ack-revision"}, time.Second); err != nil {
					_ = logger.Close()
					t.Fatal(err)
				}
				if err := logger.Close(); err != nil {
					t.Fatal(err)
				}
				staleRequest, decodeErr := decodeAuditLockAcknowledge(json.RawMessage(
					acknowledgeBodyWithExpectedPhysicalForServer(correlation, claimedEpoch.ServerInstance, physical),
				))
				if decodeErr != nil {
					t.Fatalf("decode prior-generation ACK wire body: %v", decodeErr)
				}
				if routeErr := second.acknowledgeRequest(context.Background(), staleRequest); routeErr == nil || routeErr.code != string(daemonRecoverErrorAckPhysicalStateChanged) {
					t.Fatalf("stale current-epoch physical proof err=%v", routeErr)
				}
				stillRetained, staleLookupErr := second.lookup(context.Background(), correlation)
				if staleLookupErr != nil || stillRetained == nil || stillRetained.Status != tc.status {
					t.Fatalf("stale physical ACK consumed receipt=%+v err=%v", stillRetained, staleLookupErr)
				}
				physical = api.SupervisorEventLockSnapshotForPath(second.logPath)
				request, decodeErr := decodeAuditLockAcknowledge(json.RawMessage(
					acknowledgeBodyWithExpectedPhysicalForServer(correlation, claimedEpoch.ServerInstance, physical),
				))
				if decodeErr != nil {
					t.Fatalf("decode current-epoch ACK wire body: %v", decodeErr)
				}
				if routeErr := second.acknowledgeRequest(context.Background(), request); routeErr != nil {
					t.Fatalf("prior-generation committed-success ACK: %v", routeErr)
				}
			} else if routeErr := second.acknowledge(context.Background(), correlation); routeErr != nil {
				t.Fatalf("prior-generation ACK: %v", routeErr)
			}

			rotatedEpoch := second.currentMutationEpoch()
			if rotatedEpoch.Generation != claimedEpoch.Generation+1 || rotatedEpoch.ServerInstance == claimedEpoch.ServerInstance {
				t.Fatalf("rotated epoch=%+v, claimed=%+v", rotatedEpoch, claimedEpoch)
			}
			if consumed, consumedErr := second.lookup(context.Background(), correlation); consumedErr != nil || consumed != nil {
				t.Fatalf("consumed lookup=%+v err=%v", consumed, consumedErr)
			}
			if routeErr := second.acknowledge(context.Background(), correlation); routeErr != nil {
				t.Fatalf("idempotent repeated ACK: %v", routeErr)
			}
			if afterRepeated := second.currentMutationEpoch(); afterRepeated != rotatedEpoch {
				t.Fatalf("repeated ACK rotated epoch: before=%+v after=%+v", rotatedEpoch, afterRepeated)
			}

			admittedCorrelation := validAuditLockCorrelation(rotatedEpoch.ServerInstance, 400+index)
			admittedCorrelation.AttemptID = fmt.Sprintf("40000000-0000-4000-8000-%012x", index)
			admitted, admittedErr := second.reserve(context.Background(), admittedCorrelation, auditLockOccurrenceBinding{
				serverInstance: rotatedEpoch.ServerInstance,
				taskName:       binding.taskName,
				confirm:        true,
			})
			if admittedErr != nil || !admitted.Novel || admitted.MutationEpoch != rotatedEpoch {
				t.Fatalf("same-task admission after ACK=%+v err=%v epoch=%+v", admitted, admittedErr, rotatedEpoch)
			}
		})
	}
}

func TestAuditLockACK_PriorGenerationRejectsLateTerminalizer(t *testing.T) {
	stateDir := t.TempDir()
	first := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer first.close()
	originEpoch := first.currentMutationEpoch()
	correlation := validAuditLockCorrelation(originEpoch.ServerInstance, 500)
	correlation.AttemptID = "50000000-0000-4000-8000-000000000500"
	binding := auditLockOccurrenceBinding{
		serverInstance: originEpoch.ServerInstance,
		taskName:       `\demo/late-prior-generation`,
		confirm:        true,
	}
	reservation, reserveErr := first.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("origin reserve=%+v err=%v", reservation, reserveErr)
	}
	terminal := auditLockTerminalEvidence{
		HTTPStatus: http.StatusInternalServerError,
		ErrorCode:  string(daemonRecoverErrorStateRead),
	}
	if _, terminalErr := first.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, terminal); terminalErr != nil {
		t.Fatalf("origin terminalization: %v", terminalErr)
	}

	first.storeMu.Lock()
	var releaseOnce sync.Once
	releaseLateTerminalizer := func() { releaseOnce.Do(first.storeMu.Unlock) }
	defer releaseLateTerminalizer()
	type terminalizeResult struct {
		receipt auditLockReceiptDTO
		err     *auditLockRouteError
	}
	lateStarted := make(chan struct{})
	lateResult := make(chan terminalizeResult, 1)
	go func() {
		close(lateStarted)
		receipt, routeErr := first.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, terminal)
		lateResult <- terminalizeResult{receipt: receipt, err: routeErr}
	}()
	<-lateStarted

	second := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer second.close()
	if err := second.ensureReady(); err != nil {
		t.Fatal(err)
	}
	claimedEpoch := second.currentMutationEpoch()
	if routeErr := second.acknowledge(context.Background(), correlation); routeErr != nil {
		t.Fatalf("prior-generation ACK: %v", routeErr)
	}
	rotatedEpoch := second.currentMutationEpoch()
	if rotatedEpoch.Generation != claimedEpoch.Generation+1 {
		t.Fatalf("ACK rotation=%+v, claimed=%+v", rotatedEpoch, claimedEpoch)
	}

	newCorrelation := validAuditLockCorrelation(rotatedEpoch.ServerInstance, 501)
	newCorrelation.AttemptID = "50000000-0000-4000-8000-000000000501"
	newReservation, newReserveErr := second.reserve(context.Background(), newCorrelation, auditLockOccurrenceBinding{
		serverInstance: rotatedEpoch.ServerInstance,
		taskName:       binding.taskName,
		confirm:        true,
	})
	if newReserveErr != nil || !newReservation.Novel {
		t.Fatalf("post-ACK reserve=%+v err=%v", newReservation, newReserveErr)
	}

	releaseLateTerminalizer()
	select {
	case result := <-lateResult:
		if result.err == nil || result.err.code != string(daemonRecoverErrorBaselineStale) {
			t.Fatalf("late terminalization receipt=%+v err=%v, want baseline stale", result.receipt, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late terminalizer did not return")
	}
	if afterLate := second.currentMutationEpoch(); afterLate != rotatedEpoch {
		t.Fatalf("late terminalizer changed epoch: before=%+v after=%+v", rotatedEpoch, afterLate)
	}
	if oldReceipt, oldLookupErr := second.lookup(context.Background(), correlation); oldLookupErr != nil || oldReceipt != nil {
		t.Fatalf("late terminalizer recreated old receipt=%+v err=%v", oldReceipt, oldLookupErr)
	}
	currentReceipt, currentLookupErr := second.lookup(context.Background(), newCorrelation)
	if currentLookupErr != nil || currentReceipt == nil || currentReceipt.Status != auditLockOccurrenceInFlight {
		t.Fatalf("late terminalizer changed current receipt=%+v err=%v", currentReceipt, currentLookupErr)
	}
}

func TestAuditLockStoreCorruptOversizeUnsupportedVersionFailsClosed(t *testing.T) {
	tests := map[string]string{
		"unknown field":   `{"version":1,"generation":1,"active_server_instance":"11111111-1111-4111-8111-111111111111","records":[],"future":true}` + "\n",
		"duplicate field": `{"version":1,"version":1,"generation":1,"active_server_instance":"11111111-1111-4111-8111-111111111111","records":[]}` + "\n",
		"unsupported":     `{"version":2,"generation":1,"active_server_instance":"11111111-1111-4111-8111-111111111111","records":[]}` + "\n",
		"oversized valid": `{"version":1,"generation":1,"active_server_instance":"11111111-1111-4111-8111-111111111111","records":` + strings.Repeat(" ", auditLockOccurrenceStoreMaxBytes) + `[]}` + "\n",
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			storePath := filepath.Join(stateDir, auditLockOccurrenceFileLeaf)
			if err := os.WriteFile(storePath, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(storePath)
			if err != nil {
				t.Fatal(err)
			}
			adapter := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
			defer adapter.close()
			if adapter.ensureReady() == nil {
				t.Fatal("corrupt store was accepted")
			}
			after, err := os.ReadFile(storePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != raw {
				t.Fatalf("corrupt store mutated:\nwant=%q\ngot =%q", raw, string(after))
			}
			afterInfo, err := os.Stat(storePath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(beforeInfo, afterInfo) {
				t.Fatal("corrupt store inode was replaced")
			}
		})
	}
}

func validAuditLockStoreTestRecord(status string) auditLockOccurrenceRecord {
	record := auditLockOccurrenceRecord{
		OriginServerInstance: "11111111-1111-4111-8111-111111111111",
		AttemptID:            "22222222-2222-4222-8222-222222222222",
		OccurrenceID:         "33333333-3333-4333-8333-333333333333",
		TaskName:             `\demo/default`,
		Confirm:              true,
		Status:               status,
		LockAuthorization:    "none",
	}
	switch status {
	case auditLockOccurrenceCommittedSuccess:
		record.HTTPStatus = http.StatusOK
		record.Success = &daemonRecoverSuccessEvidence{
			TaskName:             record.TaskName,
			PortOwnerCheck:       string(daemonrecovery.PortOwnerUnbound),
			PortWaitOutcome:      string(daemonrecovery.PortWaitNotRequired),
			AuditHandoff:         string(daemonrecovery.AuditHandoffDurable),
			TerminationCommitted: false,
		}
	case auditLockOccurrenceCommittedError, auditLockOccurrenceNotCommitted:
		record.HTTPStatus = http.StatusInternalServerError
		record.ErrorCode = string(daemonRecoverErrorStateRead)
	case auditLockOccurrenceUncertain:
		record.LockAuthorization = "uncertain"
	}
	return record
}

func validAuditLockStoreTestStore(record auditLockOccurrenceRecord) auditLockOccurrenceStore {
	return auditLockOccurrenceStore{
		Version:              auditLockOccurrenceStoreVersion,
		Generation:           1,
		ActiveServerInstance: "44444444-4444-4444-8444-444444444444",
		Records:              []auditLockOccurrenceRecord{record},
	}
}

func TestAuditLockMutationEpochPreservesVersionOneSchema(t *testing.T) {
	raw, err := json.Marshal(validAuditLockStoreTestStore(validAuditLockStoreTestRecord(auditLockOccurrenceUncertain)))
	if err != nil {
		t.Fatal(err)
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	wantStoreFields := map[string]struct{}{
		"version":                {},
		"generation":             {},
		"active_server_instance": {},
		"records":                {},
	}
	if len(store) != len(wantStoreFields) {
		t.Fatalf("version-1 store fields=%v", store)
	}
	for field := range store {
		if _, ok := wantStoreFields[field]; !ok {
			t.Fatalf("unexpected version-1 store field %q", field)
		}
	}
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(store["records"], &records); err != nil || len(records) != 1 {
		t.Fatalf("version-1 records=%v err=%v", records, err)
	}
	wantRecordFields := map[string]struct{}{
		"origin_server_instance": {},
		"attempt_id":             {},
		"occurrence_id":          {},
		"task_name":              {},
		"confirm":                {},
		"status":                 {},
		"http_status":            {},
		"error_code":             {},
		"lock_authorization":     {},
		"success":                {},
	}
	if len(records[0]) != len(wantRecordFields) {
		t.Fatalf("version-1 record fields=%v", records[0])
	}
	for field := range records[0] {
		if _, ok := wantRecordFields[field]; !ok {
			t.Fatalf("unexpected version-1 record field %q", field)
		}
	}
}

func TestAuditLockStoreOwnerEnumsAndStatusesAreExhaustive(t *testing.T) {
	for _, status := range auditLockOccurrenceStatusValues() {
		t.Run("status/"+status, func(t *testing.T) {
			if err := validateAuditLockStore(validAuditLockStoreTestStore(validAuditLockStoreTestRecord(status))); err != nil {
				t.Fatalf("valid status %q rejected: %v", status, err)
			}
		})
	}

	for _, owner := range daemonrecovery.PortOwnerChecks() {
		t.Run("port_owner_check/"+string(owner), func(t *testing.T) {
			record := validAuditLockStoreTestRecord(auditLockOccurrenceCommittedSuccess)
			record.Success.PortOwnerCheck = string(owner)
			if err := validateAuditLockStore(validAuditLockStoreTestStore(record)); err != nil {
				t.Fatalf("owner value %q rejected: %v", owner, err)
			}
		})
	}

	for _, outcome := range daemonrecovery.PortWaitOutcomes() {
		t.Run("port_wait_outcome/"+string(outcome), func(t *testing.T) {
			record := validAuditLockStoreTestRecord(auditLockOccurrenceCommittedSuccess)
			record.Success.PortWaitOutcome = string(outcome)
			if err := validateAuditLockStore(validAuditLockStoreTestStore(record)); err != nil {
				t.Fatalf("wait value %q rejected: %v", outcome, err)
			}
		})
	}

	for _, handoff := range daemonrecovery.AuditHandoffs() {
		t.Run("audit_handoff/"+string(handoff), func(t *testing.T) {
			record := validAuditLockStoreTestRecord(auditLockOccurrenceCommittedSuccess)
			record.Success.AuditHandoff = string(handoff)
			record.Success.TerminationCommitted = true
			record.LockAuthorization = auditLockAuthorization(handoff, true)
			if err := validateAuditLockStore(validAuditLockStoreTestStore(record)); err != nil {
				t.Fatalf("handoff value %q rejected: %v", handoff, err)
			}
		})
	}

	for _, authorization := range auditLockAuthorizationValues() {
		t.Run("lock_authorization/"+authorization, func(t *testing.T) {
			record := validAuditLockStoreTestRecord(auditLockOccurrenceInFlight)
			switch authorization {
			case "current_truth":
				record = validAuditLockStoreTestRecord(auditLockOccurrenceCommittedSuccess)
				record.Success.AuditHandoff = string(daemonrecovery.AuditHandoffReleasePending)
				record.Success.TerminationCommitted = true
				record.LockAuthorization = authorization
			case "uncertain":
				record = validAuditLockStoreTestRecord(auditLockOccurrenceUncertain)
			}
			if err := validateAuditLockStore(validAuditLockStoreTestStore(record)); err != nil {
				t.Fatalf("authorization %q rejected: %v", authorization, err)
			}
		})
	}

	for _, entry := range daemonRecoverErrorCatalog {
		if !entry.persistable {
			continue
		}
		code := entry.code
		t.Run("error_code/"+string(code), func(t *testing.T) {
			record := validAuditLockStoreTestRecord(auditLockOccurrenceNotCommitted)
			record.ErrorCode = string(code)
			if err := validateAuditLockStore(validAuditLockStoreTestStore(record)); err != nil {
				t.Fatalf("error code %q rejected: %v", code, err)
			}
		})
	}
}

func TestAuditLockStoreUnknownOwnerValuesFailClosedOnDecodeAndWrite(t *testing.T) {
	type invalidCase struct {
		name   string
		mutate func(*auditLockOccurrenceStore)
	}
	tests := []invalidCase{
		{name: "version", mutate: func(store *auditLockOccurrenceStore) { store.Version++ }},
		{name: "status", mutate: func(store *auditLockOccurrenceStore) { store.Records[0].Status = "future_status" }},
		{name: "lock_authorization", mutate: func(store *auditLockOccurrenceStore) { store.Records[0].LockAuthorization = "future_authorization" }},
		{name: "port_owner_check", mutate: func(store *auditLockOccurrenceStore) { store.Records[0].Success.PortOwnerCheck = "future_owner" }},
		{name: "port_wait_outcome", mutate: func(store *auditLockOccurrenceStore) { store.Records[0].Success.PortWaitOutcome = "future_wait" }},
		{name: "audit_handoff", mutate: func(store *auditLockOccurrenceStore) { store.Records[0].Success.AuditHandoff = "future_handoff" }},
		{name: "error_code", mutate: func(store *auditLockOccurrenceStore) {
			store.Records[0] = validAuditLockStoreTestRecord(auditLockOccurrenceNotCommitted)
			store.Records[0].ErrorCode = "RECOVER_FUTURE_FAILURE"
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name+"/decode", func(t *testing.T) {
			store := validAuditLockStoreTestStore(validAuditLockStoreTestRecord(auditLockOccurrenceCommittedSuccess))
			tc.mutate(&store)
			raw, err := json.MarshalIndent(store, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			stateDir := t.TempDir()
			storePath := filepath.Join(stateDir, auditLockOccurrenceFileLeaf)
			if err := os.WriteFile(storePath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(storePath)
			if err != nil {
				t.Fatal(err)
			}
			adapter := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
			defer adapter.close()
			if adapter.ensureReady() == nil {
				t.Fatal("unknown owner value was accepted at startup")
			}
			after, err := os.ReadFile(storePath)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(storePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, raw) || !os.SameFile(beforeInfo, afterInfo) {
				t.Fatal("invalid startup record mutated bytes or replaced the inode")
			}
		})

		t.Run(tc.name+"/write", func(t *testing.T) {
			adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
			defer adapter.close()
			before, err := os.ReadFile(adapter.storePath)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(adapter.storePath)
			if err != nil {
				t.Fatal(err)
			}
			store := validAuditLockStoreTestStore(validAuditLockStoreTestRecord(auditLockOccurrenceCommittedSuccess))
			store.Generation = adapter.generation
			store.ActiveServerInstance = adapter.serverInstance
			tc.mutate(&store)
			err = adapter.withStoreLock(context.Background(), "reject invalid owner value", func(_ *auditLockStoreOperation) error {
				return adapter.writeStoreLockHeld(store)
			})
			if err == nil {
				t.Fatal("unknown owner value was accepted by active writer")
			}
			after, readErr := os.ReadFile(adapter.storePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			afterInfo, statErr := os.Stat(adapter.storePath)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !bytes.Equal(after, before) || !os.SameFile(beforeInfo, afterInfo) {
				t.Fatal("invalid active write mutated bytes or replaced the inode")
			}
		})
	}
}

func TestAuditLockStoreRedactsProcessDetailAndUsesHardenedAtomicWrite(t *testing.T) {
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: adapter.serverInstance,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
	}
	if _, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceCommittedSuccess,
		"none",
		successfulTerminalEvidence(binding.taskName, true),
	); terminalErr != nil {
		t.Fatal(terminalErr)
	}
	after, err := os.Lstat(adapter.storePath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("occurrence store is a symlink: mode=%v", after.Mode())
	}

	// The inode-anchored reader invokes the platform's owner-only verifier.
	// On Windows os.FileMode does not encode the destination DACL.
	raw, err := api.ReadStateFileInodeAnchored(adapter.storePath)
	if err != nil {
		t.Fatal(err)
	}
	storeFields, err := decodeRequiredObject(raw,
		"version",
		"generation",
		"active_server_instance",
		"records",
	)
	if err != nil {
		t.Fatalf("store schema: %v", err)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(storeFields["records"], &records); err != nil || len(records) != 1 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	recordFields, err := decodeRequiredObject(records[0],
		"origin_server_instance",
		"attempt_id",
		"occurrence_id",
		"task_name",
		"confirm",
		"status",
		"http_status",
		"error_code",
		"lock_authorization",
		"success",
	)
	if err != nil {
		t.Fatalf("record schema: %v", err)
	}
	successFields, err := decodeRequiredObject(recordFields["success"],
		"task_name",
		"reaped",
		"port_owner_check",
		"port_wait_outcome",
		"audit_handoff",
		"termination_committed",
	)
	if err != nil {
		t.Fatalf("success schema: %v", err)
	}
	var persistedTask string
	if err := json.Unmarshal(successFields["task_name"], &persistedTask); err != nil || persistedTask != `\demo/default` {
		t.Fatalf("persisted task=%q err=%v", persistedTask, err)
	}
	for _, forbidden := range []string{
		`"pid"`,
		`"port"`,
		`"executable"`,
		`"arguments"`,
		`"path"`,
		`"cause"`,
		`"process_detail"`,
	} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("occurrence store contains forbidden process-detail field %s: %s", forbidden, raw)
		}
	}
}

func TestAuditLockTerminalWriteFailureRetainsUncertainFence(t *testing.T) {
	stateDir := t.TempDir()
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, stateDir)
	defer adapter.close()
	adapter.lockTimeout = 25 * time.Millisecond
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: adapter.serverInstance,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	held := flock.New(adapter.storePath + ".lock")
	locked, err := held.TryLock()
	if err != nil || !locked {
		t.Fatalf("hold store lock: locked=%t err=%v", locked, err)
	}
	receipt, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceCommittedSuccess,
		"none",
		successfulTerminalEvidence(binding.taskName, false),
	)
	if unlockErr := held.Unlock(); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if terminalErr == nil ||
		terminalErr.code != "RECOVER_OUTCOME_UNCERTAIN" ||
		receipt.Status != auditLockOccurrenceUncertain {
		t.Fatalf("terminal lock failure receipt=%+v err=%v", receipt, terminalErr)
	}

	raw, err := os.ReadFile(adapter.storePath)
	if err != nil {
		t.Fatal(err)
	}
	var store auditLockOccurrenceStore
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	if len(store.Records) != 1 || store.Records[0].Status != auditLockOccurrenceInFlight {
		t.Fatalf("store after terminal lock failure=%+v", store)
	}

	// The response is only safe if every same-process reader observes the
	// identical uncertain receipt while the durable record remains in_flight.
	for attempt := 0; attempt < 2; attempt++ {
		lookup, lookupErr := adapter.lookup(context.Background(), correlation)
		if lookupErr != nil || lookup == nil || lookup.Status != auditLockOccurrenceUncertain ||
			lookup.TerminationCommitState != auditLockTerminationStateUnknown {
			t.Fatalf("same-process lookup %d=%+v err=%v", attempt, lookup, lookupErr)
		}
	}
	replay, replayErr := adapter.reserve(context.Background(), correlation, binding)
	if replayErr != nil || replay.Novel || replay.Receipt.Status != auditLockOccurrenceUncertain ||
		replay.Receipt.TerminationCommitState != auditLockTerminationStateUnknown {
		t.Fatalf("same-process replay=%+v err=%v", replay, replayErr)
	}
	if acknowledgeErr := adapter.acknowledge(context.Background(), correlation); acknowledgeErr != nil {
		t.Fatalf("uncertain acknowledgement: %v", acknowledgeErr)
	}
	snapshot, snapshotErr := adapter.snapshot(context.Background(), nil)
	if snapshotErr != nil || len(snapshot.RecoveryReceipts) != 0 {
		t.Fatalf("post-ack snapshot=%+v err=%v", snapshot, snapshotErr)
	}
}

func auditLockSettlementCount(adapter *auditLockAdapter) int {
	count := 0
	adapter.settlements.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func TestAuditLockTerminalize_StoreMuTimeoutPublishesCell(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
	}{
		{
			name: "deadline",
			ctx:  context.Background,
		},
		{
			name: "canceled context",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
	}
	for testIndex, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
			adapter.lockTimeout = 25 * time.Millisecond
			correlation := validAuditLockCorrelation(adapter.serverInstance, 200+testIndex)
			binding := auditLockOccurrenceBinding{
				serverInstance: adapter.serverInstance,
				taskName:       fmt.Sprintf(`\\mcp-local-hub-settlement-timeout-%d`, testIndex),
				confirm:        true,
			}
			reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
			if reserveErr != nil || !reservation.Novel || reservation.Settlement == nil {
				adapter.close()
				t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
			}
			initial := reservation.Settlement.load()
			if initial == nil || initial.phase != auditLockSettlementInFlight {
				adapter.close()
				t.Fatalf("initial settlement=%+v", initial)
			}

			adapter.storeMu.Lock()
			receipt, terminalErr := adapter.terminalizeContext(
				tc.ctx(),
				reservation,
				auditLockOccurrenceNotCommitted,
				auditLockAuthorizationNone,
				auditLockTerminalEvidence{
					HTTPStatus: http.StatusInternalServerError,
					ErrorCode:  "RECOVER_STATE_READ_FAILED",
				},
			)
			adapter.storeMu.Unlock()
			if terminalErr == nil ||
				terminalErr.code != string(daemonRecoverErrorOutcomeUncertain) ||
				receipt.Status != auditLockOccurrenceUncertain ||
				receipt.TerminationCommitState != auditLockTerminationStateUnknown {
				adapter.close()
				t.Fatalf("terminalize receipt=%+v err=%v", receipt, terminalErr)
			}
			settlement := reservation.Settlement.load()
			if settlement == nil || settlement.phase != auditLockSettlementUncertain ||
				settlement.receipt.Status != auditLockOccurrenceUncertain {
				adapter.close()
				t.Fatalf("settlement=%+v", settlement)
			}

			raw, err := os.ReadFile(adapter.storePath)
			if err != nil {
				adapter.close()
				t.Fatal(err)
			}
			var durable auditLockOccurrenceStore
			if err := json.Unmarshal(raw, &durable); err != nil {
				adapter.close()
				t.Fatal(err)
			}
			if len(durable.Records) != 1 || durable.Records[0].Status != auditLockOccurrenceInFlight {
				adapter.close()
				t.Fatalf("durable store=%+v", durable)
			}

			lookup, lookupErr := adapter.lookup(context.Background(), correlation)
			if lookupErr != nil || lookup == nil || lookup.Status != auditLockOccurrenceUncertain {
				adapter.close()
				t.Fatalf("lookup=%+v err=%v", lookup, lookupErr)
			}
			replay, replayErr := adapter.reserve(context.Background(), correlation, binding)
			if replayErr != nil || replay.Novel || replay.Receipt.Status != auditLockOccurrenceUncertain ||
				replay.Settlement != reservation.Settlement {
				adapter.close()
				t.Fatalf("replay=%+v err=%v", replay, replayErr)
			}
			snapshot, snapshotErr := adapter.snapshot(context.Background(), nil)
			if snapshotErr != nil || len(snapshot.RecoveryReceipts) != 1 ||
				snapshot.RecoveryReceipts[0].Status != auditLockOccurrenceUncertain {
				adapter.close()
				t.Fatalf("snapshot=%+v err=%v", snapshot, snapshotErr)
			}

			blocked := validAuditLockCorrelation(adapter.serverInstance, 210+testIndex)
			blocked.AttemptID = fmt.Sprintf("20000000-0000-4000-8000-%012x", testIndex)
			if _, blockedErr := adapter.reserve(context.Background(), blocked, binding); blockedErr == nil ||
				blockedErr.code != string(daemonRecoverErrorAttemptConflict) {
				adapter.close()
				t.Fatalf("same-task reserve err=%v", blockedErr)
			}

			oldEpoch := adapter.currentMutationEpoch()
			if acknowledgeErr := adapter.acknowledge(context.Background(), correlation); acknowledgeErr != nil {
				adapter.close()
				t.Fatalf("uncertain acknowledgement: %v", acknowledgeErr)
			}
			if adapter.currentMutationEpoch() == oldEpoch {
				adapter.close()
				t.Fatal("final acknowledgement did not rotate the mutation epoch")
			}
			if auditLockSettlementCount(adapter) != 0 {
				adapter.close()
				t.Fatalf("settlement cells after acknowledgement=%d", auditLockSettlementCount(adapter))
			}
			if acknowledgeErr := adapter.acknowledge(context.Background(), correlation); acknowledgeErr != nil {
				adapter.close()
				t.Fatalf("repeated acknowledgement: %v", acknowledgeErr)
			}

			current := adapter.currentMutationEpoch()
			newCorrelation := validAuditLockCorrelation(current.ServerInstance, 220+testIndex)
			newCorrelation.AttemptID = fmt.Sprintf("22000000-0000-4000-8000-%012x", testIndex)
			newBinding := binding
			newBinding.serverInstance = current.ServerInstance
			newReservation, newReserveErr := adapter.reserve(context.Background(), newCorrelation, newBinding)
			if newReserveErr != nil || !newReservation.Novel {
				adapter.close()
				t.Fatalf("post-ACK reserve=%+v err=%v", newReservation, newReserveErr)
			}
			adapter.close()
			if auditLockSettlementCount(adapter) != 0 {
				t.Fatalf("settlement cells after close=%d", auditLockSettlementCount(adapter))
			}
		})
	}
}

func TestAuditLockSettlementCell_ACKWinsLatePublication(t *testing.T) {
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	correlation := validAuditLockCorrelation(adapter.serverInstance, 230)
	binding := auditLockOccurrenceBinding{
		serverInstance: adapter.serverInstance,
		taskName:       `\\mcp-local-hub-settlement-late-publication`,
		confirm:        true,
	}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel || reservation.Settlement == nil {
		adapter.close()
		t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
	}
	oldCell := reservation.Settlement
	uncertain := oldCell.publish(auditLockSettlementUncertain, auditLockUncertainReceipt(reservation))
	if uncertain == nil || uncertain.phase != auditLockSettlementUncertain {
		adapter.close()
		t.Fatalf("uncertain settlement=%+v", uncertain)
	}
	oldEpoch := adapter.currentMutationEpoch()
	if acknowledgeErr := adapter.acknowledge(context.Background(), correlation); acknowledgeErr != nil {
		adapter.close()
		t.Fatalf("acknowledge: %v", acknowledgeErr)
	}
	if adapter.currentMutationEpoch() == oldEpoch {
		adapter.close()
		t.Fatal("acknowledgement did not rotate the mutation epoch")
	}
	if auditLockSettlementCount(adapter) != 0 {
		adapter.close()
		t.Fatalf("settlement cells after acknowledgement=%d", auditLockSettlementCount(adapter))
	}

	current := adapter.currentMutationEpoch()
	newCorrelation := validAuditLockCorrelation(current.ServerInstance, 231)
	newCorrelation.AttemptID = "23100000-0000-4000-8000-000000000231"
	newBinding := binding
	newBinding.serverInstance = current.ServerInstance
	newReservation, newReserveErr := adapter.reserve(context.Background(), newCorrelation, newBinding)
	if newReserveErr != nil || !newReservation.Novel || newReservation.Settlement == nil {
		adapter.close()
		t.Fatalf("new reserve=%+v err=%v", newReservation, newReserveErr)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			phase := auditLockSettlementUncertain
			lateReceipt := auditLockUncertainReceipt(reservation)
			if index%2 == 0 {
				phase = auditLockSettlementDurableTerminal
				lateReceipt.Status = auditLockOccurrenceNotCommitted
			}
			oldCell.publish(phase, lateReceipt)
		}(i)
	}
	close(start)
	wait.Wait()

	settlement := oldCell.load()
	if settlement == nil || settlement.phase != auditLockSettlementUncertain ||
		settlement.receipt.Status != auditLockOccurrenceUncertain {
		adapter.close()
		t.Fatalf("old settlement changed after ACK=%+v", settlement)
	}
	if adapter.currentMutationEpoch() != current {
		adapter.close()
		t.Fatalf("late publication changed epoch from %+v to %+v", current, adapter.currentMutationEpoch())
	}
	oldLookup, oldLookupErr := adapter.lookup(context.Background(), correlation)
	if oldLookupErr != nil || oldLookup != nil {
		adapter.close()
		t.Fatalf("late publication resurrected old receipt=%+v err=%v", oldLookup, oldLookupErr)
	}
	newLookup, newLookupErr := adapter.lookup(context.Background(), newCorrelation)
	if newLookupErr != nil || newLookup == nil || newLookup.Status != auditLockOccurrenceInFlight {
		adapter.close()
		t.Fatalf("new receipt=%+v err=%v", newLookup, newLookupErr)
	}
	if cell := adapter.settlementCell(current.Generation, auditLockOccurrenceRecord{
		OriginServerInstance: newCorrelation.ServerInstance,
		AttemptID:            newCorrelation.AttemptID,
		OccurrenceID:         newCorrelation.OccurrenceID,
	}); cell != newReservation.Settlement {
		adapter.close()
		t.Fatalf("current settlement cell=%p want=%p", cell, newReservation.Settlement)
	}
	adapter.close()
	if auditLockSettlementCount(adapter) != 0 {
		t.Fatalf("settlement cells after close=%d", auditLockSettlementCount(adapter))
	}
}

func TestAuditLockSettlementCell_DurableTerminalDoesNotDowngrade(t *testing.T) {
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 240)
	binding := auditLockOccurrenceBinding{
		serverInstance: adapter.serverInstance,
		taskName:       `\\mcp-local-hub-settlement-durable`,
		confirm:        true,
	}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel || reservation.Settlement == nil {
		t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
	}
	receipt, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceNotCommitted,
		auditLockAuthorizationNone,
		auditLockTerminalEvidence{
			HTTPStatus: http.StatusInternalServerError,
			ErrorCode:  "RECOVER_STATE_READ_FAILED",
		},
	)
	if terminalErr != nil || receipt.Status != auditLockOccurrenceNotCommitted {
		t.Fatalf("terminal receipt=%+v err=%v", receipt, terminalErr)
	}
	settlement := reservation.Settlement.load()
	if settlement == nil || settlement.phase != auditLockSettlementDurableTerminal ||
		settlement.receipt.Status != auditLockOccurrenceNotCommitted {
		t.Fatalf("durable settlement=%+v", settlement)
	}
	if auditLockSettlementCount(adapter) != 0 {
		t.Fatalf("indexed cells after durable terminal=%d", auditLockSettlementCount(adapter))
	}
	late := reservation.Settlement.publish(auditLockSettlementUncertain, auditLockUncertainReceipt(reservation))
	if late == nil || late.phase != auditLockSettlementDurableTerminal ||
		late.receipt.Status != auditLockOccurrenceNotCommitted {
		t.Fatalf("late downgrade result=%+v", late)
	}
	lookup, lookupErr := adapter.lookup(context.Background(), correlation)
	if lookupErr != nil || lookup == nil || lookup.Status != auditLockOccurrenceNotCommitted {
		t.Fatalf("lookup=%+v err=%v", lookup, lookupErr)
	}
	if acknowledgeErr := adapter.acknowledge(context.Background(), correlation); acknowledgeErr != nil {
		t.Fatalf("acknowledge: %v", acknowledgeErr)
	}
}

func TestDaemonRecoverRouteDemoDefaultNormalizedTaskReplayAndFence(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	t.Cleanup(s.events.Close)
	fake := &fakeDaemonRecoverer{result: daemonrecovery.Result{
		PortOwnerCheck:  daemonrecovery.PortOwnerUnbound,
		PortWaitOutcome: daemonrecovery.PortWaitNotRequired,
		AuditHandoff:    daemonrecovery.AuditHandoffDurable,
	}}
	s.daemonRecover = fake

	first := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
	firstBody := correlationPOSTBody(first, "demo/default")
	firstResponse := httptest.NewRecorder()
	s.mux.ServeHTTP(firstResponse, sameOriginRequest(http.MethodPost, "/api/daemon/recover", firstBody))
	if firstResponse.Code != http.StatusOK ||
		fake.calls != 1 ||
		fake.taskName != `\demo/default` {
		t.Fatalf("first status=%d calls=%d task=%q body=%s", firstResponse.Code, fake.calls, fake.taskName, firstResponse.Body.String())
	}

	replay := httptest.NewRecorder()
	s.mux.ServeHTTP(replay, sameOriginRequest(http.MethodPost, "/api/daemon/recover", firstBody))
	if replay.Code != http.StatusOK || fake.calls != 1 {
		t.Fatalf("replay status=%d calls=%d body=%s", replay.Code, fake.calls, replay.Body.String())
	}

	sameTask := validAuditLockCorrelation(s.auditLock.serverInstance, 2)
	sameTask.AttemptID = "22222222-2222-4222-8222-222222222222"
	conflict := httptest.NewRecorder()
	s.mux.ServeHTTP(conflict, sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(sameTask, "demo/default")))
	if conflict.Code != http.StatusConflict || fake.calls != 1 {
		t.Fatalf("same-task conflict status=%d calls=%d body=%s", conflict.Code, fake.calls, conflict.Body.String())
	}

	other := validAuditLockCorrelation(s.auditLock.serverInstance, 3)
	other.AttemptID = "33333333-3333-4333-8333-333333333333"
	otherResponse := httptest.NewRecorder()
	s.mux.ServeHTTP(otherResponse, sameOriginRequest(http.MethodPost, "/api/daemon/recover", correlationPOSTBody(other, "demo/other")))
	if otherResponse.Code != http.StatusOK ||
		fake.calls != 2 ||
		fake.taskName != `\demo/other` {
		t.Fatalf("other-task status=%d calls=%d task=%q body=%s", otherResponse.Code, fake.calls, fake.taskName, otherResponse.Body.String())
	}

	raw, err := os.ReadFile(s.auditLock.storePath)
	if err != nil {
		t.Fatal(err)
	}
	var store auditLockOccurrenceStore
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatal(err)
	}
	if len(store.Records) != 2 ||
		store.Records[0].TaskName != `\demo/default` ||
		store.Records[1].TaskName != `\demo/other` {
		t.Fatalf("canonical durable tasks=%+v", store.Records)
	}
}

func TestAuditLockCorrelationUniquenessAndExactReplay(t *testing.T) {
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer adapter.close()
	first := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: adapter.serverInstance,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := adapter.reserve(context.Background(), first, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("first reserve=%+v err=%v", reservation, reserveErr)
	}
	if _, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceNotCommitted,
		"none",
		auditLockTerminalEvidence{
			HTTPStatus: http.StatusInternalServerError,
			ErrorCode:  "RECOVER_STATE_READ_FAILED",
		},
	); terminalErr != nil {
		t.Fatal(terminalErr)
	}

	replay, replayErr := adapter.reserve(context.Background(), first, binding)
	if replayErr != nil ||
		replay.Novel ||
		replay.Terminal == nil ||
		replay.Receipt.TaskName != `\demo/default` {
		t.Fatalf("exact replay=%+v err=%v", replay, replayErr)
	}

	assertConflict := func(name string, correlation auditLockCorrelation, candidate auditLockOccurrenceBinding) {
		t.Helper()
		if _, conflictErr := adapter.reserve(context.Background(), correlation, candidate); conflictErr == nil ||
			conflictErr.code != "RECOVER_ATTEMPT_CONFLICT" {
			t.Fatalf("%s err=%v", name, conflictErr)
		}
	}
	attemptReuse := validAuditLockCorrelation(adapter.serverInstance, 2)
	assertConflict("attempt-only reuse", attemptReuse, binding)

	occurrenceReuse := validAuditLockCorrelation(adapter.serverInstance, 1)
	occurrenceReuse.AttemptID = "22222222-2222-4222-8222-222222222222"
	assertConflict("occurrence-only reuse", occurrenceReuse, binding)

	changedTask := binding
	changedTask.taskName = `\demo/changed`
	assertConflict("changed task", first, changedTask)

	changedServer := first
	changedServer.ServerInstance = "44444444-4444-4444-8444-444444444444"
	changedServerBinding := binding
	changedServerBinding.serverInstance = changedServer.ServerInstance
	assertConflict("changed server binding", changedServer, changedServerBinding)

	sameTask := validAuditLockCorrelation(adapter.serverInstance, 3)
	sameTask.AttemptID = "33333333-3333-4333-8333-333333333333"
	assertConflict("second tuple for unresolved task", sameTask, binding)

	distinct := validAuditLockCorrelation(adapter.serverInstance, 4)
	distinct.AttemptID = "55555555-5555-4555-8555-555555555555"
	distinctBinding := binding
	distinctBinding.taskName = `\demo/other`
	distinctReservation, distinctErr := adapter.reserve(context.Background(), distinct, distinctBinding)
	if distinctErr != nil || !distinctReservation.Novel {
		t.Fatalf("distinct task reserve=%+v err=%v", distinctReservation, distinctErr)
	}

	invalidConfirmation := binding
	invalidConfirmation.confirm = false
	if _, invalidErr := adapter.reserve(context.Background(), first, invalidConfirmation); invalidErr == nil ||
		invalidErr.code != "RECOVER_CORRELATION_INVALID" {
		t.Fatalf("changed confirmation err=%v", invalidErr)
	}
}

func TestAuditLockFinalACKClearsReceiptsAndRotatesBaseline(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	t.Cleanup(func() {
		s.events.Close()
	})
	fake := &blockingDaemonRecoverer{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result: daemonrecovery.Result{
			TaskName:        `\mcp-local-hub-memory-default`,
			PortOwnerCheck:  daemonrecovery.PortOwnerUnbound,
			PortWaitOutcome: daemonrecovery.PortWaitNotRequired,
			AuditHandoff:    daemonrecovery.AuditHandoffDurable,
		},
	}
	s.daemonRecover = fake
	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
	body := correlationPOSTBody(correlation, "mcp-local-hub-memory-default")

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
		firstDone <- rec
	}()
	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first recovery did not enter the deterministic in-flight window")
	}

	inFlight := httptest.NewRecorder()
	s.mux.ServeHTTP(inFlight, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
	if inFlight.Code != http.StatusOK || fake.calls.Load() != 1 {
		t.Fatalf("in-flight replay status=%d calls=%d body=%s", inFlight.Code, fake.calls.Load(), inFlight.Body.String())
	}
	var inFlightBody map[string]any
	if err := json.Unmarshal(inFlight.Body.Bytes(), &inFlightBody); err != nil || inFlightBody["state"] != "recovery_in_flight" {
		t.Fatalf("in-flight replay body=%s err=%v", inFlight.Body.String(), err)
	}

	inFlightACK := httptest.NewRecorder()
	s.mux.ServeHTTP(inFlightACK, sameOriginRequest(http.MethodDelete, "/api/daemon/recover/audit-lock-receipt", acknowledgeBody(correlation)))
	if inFlightACK.Code != http.StatusConflict {
		t.Fatalf("in-flight ACK status=%d body=%s", inFlightACK.Code, inFlightACK.Body.String())
	}

	close(fake.release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first recovery status=%d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first recovery did not finish")
	}

	ack := httptest.NewRecorder()
	s.mux.ServeHTTP(ack, sameOriginRequest(http.MethodDelete, "/api/daemon/recover/audit-lock-receipt", acknowledgeBodyWithExpectedPhysical(
		correlation,
		api.SupervisorEventLockSnapshotForPath(s.auditLock.logPath),
	)))
	if ack.Code != http.StatusNoContent {
		t.Fatalf("ACK status=%d body=%s", ack.Code, ack.Body.String())
	}
	if s.auditLock.serverInstance == correlation.ServerInstance {
		t.Fatal("final ACK did not rotate the active server instance")
	}

	delayedReplay := httptest.NewRecorder()
	s.mux.ServeHTTP(delayedReplay, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
	if delayedReplay.Code != http.StatusConflict || fake.calls.Load() != 1 {
		t.Fatalf("post-ACK replay status=%d calls=%d body=%s", delayedReplay.Code, fake.calls.Load(), delayedReplay.Body.String())
	}
	if code := decodeDaemonRecoverBody(t, delayedReplay)["code"]; code != "RECOVER_BASELINE_STALE" {
		t.Fatalf("post-ACK code=%v", code)
	}

	lookup := httptest.NewRecorder()
	target := fmt.Sprintf("/api/daemon/recover/audit-lock-state?attempt_id=%s&occurrence_id=%s&server_instance=%s",
		correlation.AttemptID, correlation.OccurrenceID, correlation.ServerInstance)
	s.mux.ServeHTTP(lookup, sameOriginRequest(http.MethodGet, target, ""))
	if lookup.Code != http.StatusOK {
		t.Fatalf("lookup status=%d body=%s", lookup.Code, lookup.Body.String())
	}
	var snapshot auditLockStateDTO
	if err := json.Unmarshal(lookup.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.RecoveryReceipt != nil ||
		len(snapshot.RecoveryReceipts) != 0 ||
		snapshot.ServerInstance == correlation.ServerInstance {
		t.Fatalf("cleared snapshot=%+v", snapshot)
	}

	repeatedACK := httptest.NewRecorder()
	s.mux.ServeHTTP(repeatedACK, sameOriginRequest(http.MethodDelete, "/api/daemon/recover/audit-lock-receipt", acknowledgeBody(correlation)))
	if repeatedACK.Code != http.StatusNoContent {
		t.Fatalf("repeated ACK status=%d body=%s", repeatedACK.Code, repeatedACK.Body.String())
	}
}

func TestAuditLockFinalAcknowledgementRotatesInstanceAndBoundsCapacity(t *testing.T) {
	a := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer a.close()
	correlations := make([]auditLockCorrelation, 0, auditLockOccurrenceCapacity)
	for i := 0; i < auditLockOccurrenceCapacity; i++ {
		correlation := validAuditLockCorrelation(a.serverInstance, i)
		correlation.AttemptID = fmt.Sprintf("10000000-0000-4000-8000-%012x", i)
		taskName := fmt.Sprintf(`\mcp-local-hub-capacity-%d`, i)
		binding := auditLockOccurrenceBinding{
			serverInstance: a.serverInstance,
			taskName:       taskName,
			confirm:        true,
		}
		reservation, reserveErr := a.reserve(context.Background(), correlation, binding)
		if reserveErr != nil || !reservation.Novel {
			t.Fatalf("reserve %d = %+v err=%v", i, reservation, reserveErr)
		}
		if _, terminalErr := a.terminalize(reservation, auditLockOccurrenceNotCommitted, "none", auditLockTerminalEvidence{
			HTTPStatus: http.StatusInternalServerError,
			ErrorCode:  "RECOVER_STATE_READ_FAILED",
		}); terminalErr != nil {
			t.Fatalf("terminalize %d: %v", i, terminalErr)
		}
		correlations = append(correlations, correlation)
	}

	overflow := validAuditLockCorrelation(a.serverInstance, auditLockOccurrenceCapacity)
	overflow.AttemptID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	overflowBinding := auditLockOccurrenceBinding{
		serverInstance: a.serverInstance,
		taskName:       `\mcp-local-hub-capacity-overflow`,
		confirm:        true,
	}
	if _, reserveErr := a.reserve(context.Background(), overflow, overflowBinding); reserveErr == nil || reserveErr.code != "RECOVER_OCCURRENCE_CAPACITY_EXCEEDED" {
		t.Fatalf("record 65 err=%v", reserveErr)
	}

	originalInstance := a.serverInstance
	for i, correlation := range correlations {
		if ackErr := a.acknowledge(context.Background(), correlation); ackErr != nil {
			t.Fatalf("ACK %d: %v", i, ackErr)
		}
	}
	snapshot, snapshotErr := a.snapshot(context.Background(), nil)
	if snapshotErr != nil || len(snapshot.RecoveryReceipts) != 0 || snapshot.ServerInstance == originalInstance {
		t.Fatalf("final compacted snapshot=%+v err=%v", snapshot, snapshotErr)
	}
}

func TestAuditLockACKReplayRaceNeverCreatesANewOccurrence(t *testing.T) {
	a := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer a.close()
	for i := 0; i < 16; i++ {
		correlation := validAuditLockCorrelation(a.serverInstance, i)
		binding := auditLockOccurrenceBinding{
			serverInstance: a.serverInstance,
			taskName:       `\mcp-local-hub-memory-default`,
			confirm:        true,
		}
		reservation, reserveErr := a.reserve(context.Background(), correlation, binding)
		if reserveErr != nil || !reservation.Novel {
			t.Fatalf("reserve %d = %+v err=%v", i, reservation, reserveErr)
		}
		if _, terminalErr := a.terminalize(reservation, auditLockOccurrenceNotCommitted, "none", auditLockTerminalEvidence{
			HTTPStatus: http.StatusInternalServerError,
			ErrorCode:  "RECOVER_STATE_READ_FAILED",
		}); terminalErr != nil {
			t.Fatalf("terminalize %d: %v", i, terminalErr)
		}

		start := make(chan struct{})
		var ackErr *auditLockRouteError
		var replay auditLockReservation
		var replayErr *auditLockRouteError
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			ackErr = a.acknowledge(context.Background(), correlation)
		}()
		go func() {
			defer wait.Done()
			<-start
			replay, replayErr = a.reserve(context.Background(), correlation, binding)
		}()
		close(start)
		wait.Wait()

		if ackErr != nil {
			t.Fatalf("ACK %d: %v", i, ackErr)
		}
		if replayErr == nil {
			if replay.Novel || replay.Terminal == nil {
				t.Fatalf("race %d returned invalid replay %+v", i, replay)
			}
		} else if replayErr.code != "RECOVER_BASELINE_STALE" {
			t.Fatalf("race %d replay err=%v", i, replayErr)
		}
	}
}

func TestAuditLockCommittedSuccessRejectsUnobservedPhysicalRevisionChange(t *testing.T) {
	a := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer a.close()
	correlation := validAuditLockCorrelation(a.serverInstance, 47)
	binding := auditLockOccurrenceBinding{serverInstance: a.serverInstance, taskName: `\\mcp-local-hub-stale-ack`, confirm: true}
	reservation, reserveErr := a.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
	}
	if _, terminalErr := a.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, true)); terminalErr != nil {
		t.Fatalf("terminalize: %v", terminalErr)
	}
	expected := api.SupervisorEventLockSnapshotForPath(a.logPath)
	logger, err := api.OpenSupervisorEventLog(a.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := logger.EmitWithTimeout(api.SupervisorEvent{Severity: "info", Source: "test", Event: "advance-physical-revision"}, time.Second); err != nil {
		_ = logger.Close()
		t.Fatal(err)
	}
	_ = logger.Close()
	if current := api.SupervisorEventLockSnapshotForPath(a.logPath); current == expected {
		t.Fatal("physical revision did not advance")
	}
	errRoute := a.acknowledgeRequest(context.Background(), auditLockAcknowledgeRequest{
		Correlation:      correlation,
		ExpectedPhysical: &auditLockExpectedPhysical{ServerInstance: correlation.ServerInstance, Revision: expected.Revision, State: api.SupervisorEventLockReleased},
	})
	if errRoute == nil || errRoute.status != http.StatusConflict || errRoute.code != string(daemonRecoverErrorAckPhysicalStateChanged) {
		t.Fatalf("stale ACK error=%v", errRoute)
	}
	receipt, lookupErr := a.lookup(context.Background(), correlation)
	if lookupErr != nil || receipt == nil || receipt.Status != auditLockOccurrenceCommittedSuccess {
		t.Fatalf("stale ACK mutated receipt=%+v err=%v", receipt, lookupErr)
	}
}

func TestDecodeAuditLockAcknowledgeExpectedPhysicalIsStrict(t *testing.T) {
	c := validAuditLockCorrelation("33333333-3333-4333-8333-333333333333", 9)
	valid := acknowledgeBodyWithExpectedPhysical(c, api.SupervisorEventLockSnapshot{State: api.SupervisorEventLockReleased, Revision: 7})
	if request, routeErr := decodeAuditLockAcknowledge(json.RawMessage(valid)); routeErr != nil || request.ExpectedPhysical == nil || request.ExpectedPhysical.Revision != 7 {
		t.Fatalf("valid strict ACK request=%+v err=%v", request, routeErr)
	}
	currentServerInstance := "44444444-4444-4444-8444-444444444444"
	splitIdentity := acknowledgeBodyWithExpectedPhysicalForServer(
		c,
		currentServerInstance,
		api.SupervisorEventLockSnapshot{State: api.SupervisorEventLockReleased, Revision: 8},
	)
	request, routeErr := decodeAuditLockAcknowledge(json.RawMessage(splitIdentity))
	if routeErr != nil || request.Correlation.ServerInstance != c.ServerInstance ||
		request.ExpectedPhysical == nil || request.ExpectedPhysical.ServerInstance != currentServerInstance {
		t.Fatalf("origin/current ACK wire split request=%+v err=%v", request, routeErr)
	}
	invalid := []string{
		strings.Replace(valid, `"revision":7`, `"revision":7,"revision":8`, 1),
		strings.Replace(valid, `"state":"released"`, `"state":"released","extra":true`, 1),
		strings.Replace(valid, `"revision":7`, `"revision":7.5`, 1),
		strings.Replace(valid, `"revision":7`, `"revision":18446744073709551616`, 1),
		strings.Replace(valid, `"state":"released"`, `"state":"outstanding"`, 1),
	}
	for _, raw := range invalid {
		if _, routeErr := decodeAuditLockAcknowledge(json.RawMessage(raw)); routeErr == nil || routeErr.status != http.StatusBadRequest {
			t.Fatalf("invalid strict ACK body=%s err=%v", raw, routeErr)
		}
	}
}

func TestAuditLockAcknowledgementStatusPolicy(t *testing.T) {
	terminalCases := []struct {
		name   string
		status string
	}{
		{name: "committed error", status: auditLockOccurrenceCommittedError},
		{name: "not committed", status: auditLockOccurrenceNotCommitted},
	}
	for index, tc := range terminalCases {
		t.Run(tc.name, func(t *testing.T) {
			a := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
			defer a.close()
			correlation := validAuditLockCorrelation(a.serverInstance, 80+index)
			binding := auditLockOccurrenceBinding{serverInstance: a.serverInstance, taskName: fmt.Sprintf(`\\mcp-local-hub-policy-%d`, index), confirm: true}
			reservation, reserveErr := a.reserve(context.Background(), correlation, binding)
			if reserveErr != nil || !reservation.Novel {
				t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
			}
			if _, terminalErr := a.terminalize(reservation, tc.status, auditLockAuthorizationNone, auditLockTerminalEvidence{HTTPStatus: http.StatusInternalServerError, ErrorCode: "RECOVER_STATE_READ_FAILED"}); terminalErr != nil {
				t.Fatalf("terminalize: %v", terminalErr)
			}
			if routeErr := a.acknowledge(context.Background(), correlation); routeErr != nil {
				t.Fatalf("manual terminal acknowledgement: %v", routeErr)
			}
		})
	}
	a := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer a.close()
	correlation := validAuditLockCorrelation(a.serverInstance, 99)
	binding := auditLockOccurrenceBinding{serverInstance: a.serverInstance, taskName: `\\mcp-local-hub-policy-success`, confirm: true}
	reservation, reserveErr := a.reserve(context.Background(), correlation, binding)
	if reserveErr != nil || !reservation.Novel {
		t.Fatalf("reserve=%+v err=%v", reservation, reserveErr)
	}
	if _, terminalErr := a.terminalize(reservation, auditLockOccurrenceCommittedSuccess, auditLockAuthorizationNone, successfulTerminalEvidence(binding.taskName, true)); terminalErr != nil {
		t.Fatalf("terminalize: %v", terminalErr)
	}
	if routeErr := a.acknowledge(context.Background(), correlation); routeErr == nil || routeErr.code != string(daemonRecoverErrorAckPreconditionRequired) {
		t.Fatalf("committed-success without expected physical error=%v", routeErr)
	}
	if routeErr := a.acknowledgeRequest(context.Background(), auditLockAcknowledgeRequest{Correlation: correlation, ExpectedPhysical: &auditLockExpectedPhysical{ServerInstance: correlation.ServerInstance, Revision: api.SupervisorEventLockSnapshotForPath(a.logPath).Revision, State: api.SupervisorEventLockReleased}}); routeErr != nil {
		t.Fatalf("committed-success exact acknowledgement: %v", routeErr)
	}
}

func TestDaemonRecoverRouteRequiresDurableCorrelationBeforeRecover(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	t.Cleanup(func() {
		s.events.Close()
	})
	fake := &fakeDaemonRecoverer{}
	s.daemonRecover = fake
	valid := validAuditLockCorrelation(s.auditLock.serverInstance, 1)

	tests := []string{
		`{"task_name":"mcp-local-hub-memory-default","confirm":true}`,
		`{"task_name":"mcp-local-hub-memory-default","confirm":true,"audit_lock_attempt":null}`,
		fmt.Sprintf(`{"task_name":"mcp-local-hub-memory-default","confirm":true,"audit_lock_attempt":{"attempt_id":"%s","attempt_id":"%s","occurrence_id":"%s","server_instance":"%s"}}`,
			valid.AttemptID, valid.AttemptID, valid.OccurrenceID, valid.ServerInstance),
		fmt.Sprintf(`{"task_name":"mcp-local-hub-memory-default","confirm":true,"audit_lock_attempt":{"attempt_id":42,"occurrence_id":"%s","server_instance":"%s"}}`,
			valid.OccurrenceID, valid.ServerInstance),
		fmt.Sprintf(`{"task_name":"mcp-local-hub-memory-default","confirm":true,"audit_lock_attempt":{"attempt_id":"%s","occurrence_id":"%s","server_instance":"%s"}}`,
			"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", valid.OccurrenceID, valid.ServerInstance),
	}
	for _, body := range tests {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
		if rec.Code != http.StatusBadRequest || decodeDaemonRecoverBody(t, rec)["code"] != "RECOVER_CORRELATION_INVALID" {
			t.Fatalf("invalid POST status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	stale := valid
	stale.ServerInstance = "44444444-4444-4444-8444-444444444444"
	staleResponse := httptest.NewRecorder()
	s.mux.ServeHTTP(staleResponse, sameOriginRequest(
		http.MethodPost,
		"/api/daemon/recover",
		correlationPOSTBody(stale, "mcp-local-hub-memory-default"),
	))
	if staleResponse.Code != http.StatusConflict ||
		decodeDaemonRecoverBody(t, staleResponse)["code"] != "RECOVER_BASELINE_STALE" {
		t.Fatalf("stale POST status=%d body=%s", staleResponse.Code, staleResponse.Body.String())
	}

	s.auditLock.lockTimeout = 25 * time.Millisecond
	held := flock.New(s.auditLock.storePath + ".lock")
	locked, lockErr := held.TryLock()
	if lockErr != nil || !locked {
		t.Fatalf("hold store lock: locked=%t err=%v", locked, lockErr)
	}
	unpersistable := httptest.NewRecorder()
	s.mux.ServeHTTP(unpersistable, sameOriginRequest(
		http.MethodPost,
		"/api/daemon/recover",
		correlationPOSTBody(valid, "mcp-local-hub-memory-default"),
	))
	if unlockErr := held.Unlock(); unlockErr != nil {
		t.Fatal(unlockErr)
	}
	if unpersistable.Code != http.StatusInternalServerError ||
		decodeDaemonRecoverBody(t, unpersistable)["code"] != "AUDIT_LOCK_ADAPTER_INIT_FAILED" {
		t.Fatalf("unpersistable POST status=%d body=%s", unpersistable.Code, unpersistable.Body.String())
	}
	if fake.calls != 0 {
		t.Fatalf("Recover calls=%d after invalid correlation", fake.calls)
	}

	partialGET := httptest.NewRecorder()
	s.mux.ServeHTTP(partialGET, sameOriginRequest(http.MethodGet,
		"/api/daemon/recover/audit-lock-state?attempt_id="+valid.AttemptID, ""))
	if partialGET.Code != http.StatusBadRequest {
		t.Fatalf("partial GET status=%d body=%s", partialGET.Code, partialGET.Body.String())
	}

	partialDELETE := httptest.NewRecorder()
	s.mux.ServeHTTP(partialDELETE, sameOriginRequest(http.MethodDelete,
		"/api/daemon/recover/audit-lock-receipt",
		fmt.Sprintf(`{"attempt_id":%q,"server_instance":%q,"acknowledge":true}`, valid.AttemptID, valid.ServerInstance)))
	if partialDELETE.Code != http.StatusBadRequest {
		t.Fatalf("partial DELETE status=%d body=%s", partialDELETE.Code, partialDELETE.Body.String())
	}
}

func TestAuditLockPendingSettlementPublishesReleased(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	s.events.DisableGUIEventLog = true
	t.Cleanup(func() {
		s.events.Close()
		api.ResetSupervisorEventLockStateForPathForTest(s.auditLock.logPath)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := s.events.Subscribe(ctx)

	logger, err := api.OpenSupervisorEventLog(s.auditLock.logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	inWrite := make(chan struct{})
	release := make(chan struct{})
	restoreWrite := api.SetSupervisorEventWriteFnForTest(func(*api.SupervisorEventLog, []byte) error {
		close(inWrite)
		<-release
		return nil
	})
	defer restoreWrite()

	emitDone := make(chan error, 1)
	go func() {
		emitDone <- logger.EmitWithTimeout(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityInfo,
			Source:   api.SupervisorEventSourceLifecycle,
			Event:    "audit-lock-settlement-test",
		}, 30*time.Second)
	}()
	<-inWrite
	s.auditLock.armPendingSettlement()
	close(release)
	if emitErr := <-emitDone; emitErr != nil {
		t.Fatalf("healthy emit: %v", emitErr)
	}

	select {
	case event := <-events:
		if event.Type != "audit-lock-state" ||
			event.Body["state"] != api.SupervisorEventLockReleased ||
			event.Body["server_instance"] != s.auditLock.serverInstance ||
			event.Body["recovery_receipt"] != nil {
			t.Fatalf("settlement event=%+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("released settlement event was not published")
	}
}

func TestAuditLockObserverRejectsDelayedTerminalBehindNewOutstanding(t *testing.T) {
	verifyAuditLockObserverTerminalMonotonicity(t)
}

func TestAuditLockObserverTerminalHintRechecksPhysicalHighWater(t *testing.T) {
	verifyAuditLockObserverTerminalMonotonicity(t)
}

func TestAuditLockObserverPublishesNewestTerminalExactlyOnce(t *testing.T) {
	verifyAuditLockObserverTerminalMonotonicity(t)
}

func verifyAuditLockObserverTerminalMonotonicity(t *testing.T) {
	t.Helper()
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	s.events.DisableGUIEventLog = true
	t.Cleanup(func() {
		s.events.Close()
		api.ResetSupervisorEventLockStateForPathForTest(s.auditLock.logPath)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := s.events.Subscribe(ctx)

	logger, err := api.OpenSupervisorEventLog(s.auditLock.logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	entered := make(chan int, 2)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var writeCall atomic.Int32
	restoreWrite := api.SetSupervisorEventWriteFnForTest(func(*api.SupervisorEventLog, []byte) error {
		call := int(writeCall.Add(1) - 1)
		entered <- call
		<-releases[call]
		return nil
	})
	defer restoreWrite()

	emit := func(name string) chan error {
		done := make(chan error, 1)
		go func() {
			done <- logger.EmitWithTimeout(api.SupervisorEvent{
				Severity: api.SupervisorEventSeverityInfo,
				Source:   api.SupervisorEventSourceLifecycle,
				Event:    name,
			}, 30*time.Second)
		}()
		return done
	}

	firstDone := emit("audit-lock-delayed-terminal-first")
	if call := <-entered; call != 0 {
		t.Fatalf("first write call=%d", call)
	}
	initial, subscription := api.SubscribeSupervisorEventLockState(
		s.auditLock.logPath,
		func(api.SupervisorEventLockSnapshot) {},
	)
	if initial != (api.SupervisorEventLockSnapshot{
		State:    api.SupervisorEventLockOutstanding,
		Revision: 1,
	}) {
		t.Fatalf("initial=%+v", initial)
	}
	s.auditLock.mu.Lock()
	s.auditLock.watching = true
	s.auditLock.observedRevision = initial.Revision
	s.auditLock.subscription = subscription
	s.auditLock.mu.Unlock()

	close(releases[0])
	if emitErr := <-firstDone; emitErr != nil {
		t.Fatal(emitErr)
	}
	delayedTerminal := api.SupervisorEventLockSnapshotForPath(s.auditLock.logPath)
	if delayedTerminal != (api.SupervisorEventLockSnapshot{
		State:    api.SupervisorEventLockReleased,
		Revision: 2,
	}) {
		t.Fatalf("delayed terminal=%+v", delayedTerminal)
	}

	secondDone := emit("audit-lock-delayed-terminal-second")
	if call := <-entered; call != 1 {
		t.Fatalf("second write call=%d", call)
	}
	s.auditLock.observeSettlement(delayedTerminal)
	s.auditLock.mu.Lock()
	watching := s.auditLock.watching
	observedRevision := s.auditLock.observedRevision
	s.auditLock.mu.Unlock()
	if !watching || observedRevision != 3 {
		t.Fatalf("stale claim watching=%t observedRevision=%d", watching, observedRevision)
	}
	select {
	case event := <-events:
		t.Fatalf("stale terminal published %+v", event)
	default:
	}

	close(releases[1])
	if emitErr := <-secondDone; emitErr != nil {
		t.Fatal(emitErr)
	}
	currentTerminal := api.SupervisorEventLockSnapshotForPath(s.auditLock.logPath)
	s.auditLock.observeSettlement(currentTerminal)
	select {
	case event := <-events:
		if event.Type != "audit-lock-state" ||
			event.Body["revision"] != uint64(4) ||
			event.Body["state"] != api.SupervisorEventLockReleased {
			t.Fatalf("current terminal event=%+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("current terminal revision did not publish")
	}
	select {
	case event := <-events:
		t.Fatalf("terminal observer published more than once: %+v", event)
	default:
	}
}
