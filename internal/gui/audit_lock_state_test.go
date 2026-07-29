package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
)

type blockingDaemonRecoverer struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
	result  daemonrecovery.Result
}

func (f *blockingDaemonRecoverer) Recover(context.Context, string, bool) (daemonrecovery.Result, error) {
	f.calls.Add(1)
	f.once.Do(func() { close(f.started) })
	<-f.release
	return f.result, nil
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

func TestAuditLockOccurrenceSealPostACKReplay(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	t.Cleanup(func() {
		s.auditLock.close()
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
	s.mux.ServeHTTP(ack, sameOriginRequest(http.MethodDelete, "/api/daemon/recover/audit-lock-receipt", acknowledgeBody(correlation)))
	if ack.Code != http.StatusNoContent {
		t.Fatalf("ACK status=%d body=%s", ack.Code, ack.Body.String())
	}

	delayedReplay := httptest.NewRecorder()
	s.mux.ServeHTTP(delayedReplay, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
	if delayedReplay.Code != http.StatusConflict || fake.calls.Load() != 1 {
		t.Fatalf("post-ACK replay status=%d calls=%d body=%s", delayedReplay.Code, fake.calls.Load(), delayedReplay.Body.String())
	}
	if code := decodeDaemonRecoverBody(t, delayedReplay)["code"]; code != "RECOVER_OCCURRENCE_CONSUMED" {
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
	if snapshot.RecoveryReceipt == nil ||
		snapshot.RecoveryReceipt.Status != "consumed" ||
		snapshot.RecoveryReceipt.LockAuthorization != "none" {
		t.Fatalf("consumed snapshot=%+v", snapshot)
	}

	repeatedACK := httptest.NewRecorder()
	s.mux.ServeHTTP(repeatedACK, sameOriginRequest(http.MethodDelete, "/api/daemon/recover/audit-lock-receipt", acknowledgeBody(correlation)))
	if repeatedACK.Code != http.StatusNoContent {
		t.Fatalf("repeated ACK status=%d body=%s", repeatedACK.Code, repeatedACK.Body.String())
	}
}

func TestAuditLockOccurrenceCapacityIncludesConsumedSeals(t *testing.T) {
	a := newAuditLockAdapter(nil)
	defer a.close()
	binding := auditLockOccurrenceBinding{
		serverInstance: a.serverInstance,
		taskName:       `\mcp-local-hub-memory-default`,
		confirm:        true,
	}
	for i := 0; i < auditLockOccurrenceCapacity; i++ {
		correlation := validAuditLockCorrelation(a.serverInstance, i)
		reservation, reserveErr := a.reserve(correlation, binding)
		if reserveErr != nil || !reservation.Novel {
			t.Fatalf("reserve %d = %+v err=%v", i, reservation, reserveErr)
		}
		a.terminalize(correlation, "committed_success", "none", auditLockTerminalEvidence{HTTPStatus: http.StatusOK})
		if ackErr := a.acknowledge(correlation); ackErr != nil {
			t.Fatalf("ACK %d: %v", i, ackErr)
		}
	}

	overflow := validAuditLockCorrelation(a.serverInstance, auditLockOccurrenceCapacity)
	if _, reserveErr := a.reserve(overflow, binding); reserveErr == nil || reserveErr.code != "RECOVER_OCCURRENCE_CAPACITY_EXCEEDED" {
		t.Fatalf("record 65 err=%v", reserveErr)
	}
	consumed := validAuditLockCorrelation(a.serverInstance, 0)
	if _, replayErr := a.reserve(consumed, binding); replayErr == nil || replayErr.code != "RECOVER_OCCURRENCE_CONSUMED" {
		t.Fatalf("consumed replay err=%v", replayErr)
	}
}

func TestAuditLockACKReplayRaceNeverCreatesANewOccurrence(t *testing.T) {
	a := newAuditLockAdapter(nil)
	defer a.close()
	binding := auditLockOccurrenceBinding{
		serverInstance: a.serverInstance,
		taskName:       `\mcp-local-hub-memory-default`,
		confirm:        true,
	}
	for i := 0; i < 16; i++ {
		correlation := validAuditLockCorrelation(a.serverInstance, i)
		if reservation, reserveErr := a.reserve(correlation, binding); reserveErr != nil || !reservation.Novel {
			t.Fatalf("reserve %d = %+v err=%v", i, reservation, reserveErr)
		}
		a.terminalize(correlation, "committed_success", "none", auditLockTerminalEvidence{HTTPStatus: http.StatusOK})

		start := make(chan struct{})
		var ackErr *auditLockRouteError
		var replay auditLockReservation
		var replayErr *auditLockRouteError
		var wait sync.WaitGroup
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			ackErr = a.acknowledge(correlation)
		}()
		go func() {
			defer wait.Done()
			<-start
			replay, replayErr = a.reserve(correlation, binding)
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
		} else if replayErr.code != "RECOVER_OCCURRENCE_CONSUMED" {
			t.Fatalf("race %d replay err=%v", i, replayErr)
		}
	}
}

func TestAuditLockCorrelationRejectsBeforeRecover(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	t.Cleanup(func() {
		s.auditLock.close()
		s.events.Close()
	})
	fake := &fakeDaemonRecoverer{}
	s.daemonRecover = fake
	valid := validAuditLockCorrelation(s.auditLock.serverInstance, 1)

	tests := []string{
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
	s.events.DisableGUIEventLog = true
	t.Cleanup(func() {
		s.auditLock.close()
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
