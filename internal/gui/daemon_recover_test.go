package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

type fakeDaemonRecoverer struct {
	result    daemonrecovery.Result
	err       error
	calls     int
	taskName  string
	confirmed bool
}

type panicBeforeCommitDaemonRecoverer struct{}

func (*panicBeforeCommitDaemonRecoverer) Recover(context.Context, string, bool, func()) (daemonrecovery.Result, error) {
	panic("synthetic pre-commit recovery panic")
}

type immediatePanicAfterCommitDaemonRecoverer struct{}

func (*immediatePanicAfterCommitDaemonRecoverer) Recover(_ context.Context, _ string, _ bool, onTerminationCommitted func()) (daemonrecovery.Result, error) {
	onTerminationCommitted()
	panic("synthetic post-commit recovery panic")
}

func (f *fakeDaemonRecoverer) Recover(_ context.Context, taskName string, confirmed bool, onTerminationCommitted func()) (daemonrecovery.Result, error) {
	f.calls++
	f.taskName = taskName
	f.confirmed = confirmed
	if f.result.TerminationCommitted && onTerminationCommitted != nil {
		onTerminationCommitted()
	}
	return f.result, f.err
}

func performDaemonRecoverRequest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	installIsolatedAuditLock(t, s)
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err == nil {
		if confirmed, _ := request["confirm"].(bool); confirmed {
			if _, exists := request["audit_lock_attempt"]; !exists {
				correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 1)
				request["audit_lock_attempt"] = map[string]any{
					"attempt_id":      correlation.AttemptID,
					"occurrence_id":   correlation.OccurrenceID,
					"server_instance": correlation.ServerInstance,
				}
				encoded, marshalErr := json.Marshal(request)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				body = string(encoded)
			}
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/recover", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestDaemonRecoverPreCommitPanicTerminalizesDurableReservation(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	t.Cleanup(func() { s.events.Close() })
	s.daemonRecover = &panicBeforeCommitDaemonRecoverer{}
	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 911)
	body := fmt.Sprintf(`{"task_name":"mcp-local-hub-memory-default","confirm":true,"audit_lock_attempt":{"attempt_id":"%s","occurrence_id":"%s","server_instance":"%s"}}`,
		correlation.AttemptID, correlation.OccurrenceID, correlation.ServerInstance)
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/recover", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("handler did not re-panic")
			}
		}()
		s.mux.ServeHTTP(httptest.NewRecorder(), req)
	}()
	receipt, routeErr := s.auditLock.lookup(context.Background(), correlation)
	if routeErr != nil || receipt == nil {
		t.Fatalf("lookup after panic receipt=%+v err=%v", receipt, routeErr)
	}
	if receipt.Status != auditLockOccurrenceNotCommitted && receipt.Status != auditLockOccurrenceUncertain {
		t.Fatalf("receipt status after pre-commit panic=%q, want not_committed or uncertain", receipt.Status)
	}
}

func TestDaemonRecoverPostCommitPanicTerminalizesUncertainAndCompletesLease(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	t.Cleanup(func() { s.events.Close() })
	s.daemonRecover = &immediatePanicAfterCommitDaemonRecoverer{}
	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 912)
	body := fmt.Sprintf(`{"task_name":"mcp-local-hub-memory-default","confirm":true,"audit_lock_attempt":{"attempt_id":"%s","occurrence_id":"%s","server_instance":"%s"}}`,
		correlation.AttemptID, correlation.OccurrenceID, correlation.ServerInstance)
	req := httptest.NewRequest(http.MethodPost, "/api/daemon/recover", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("handler did not re-panic")
			}
		}()
		s.mux.ServeHTTP(httptest.NewRecorder(), req)
	}()
	receipt, routeErr := s.auditLock.lookup(context.Background(), correlation)
	if routeErr != nil || receipt == nil {
		t.Fatalf("lookup after post-commit panic receipt=%+v err=%v", receipt, routeErr)
	}
	if receipt.Status != auditLockOccurrenceUncertain || receipt.LockAuthorization != auditLockAuthorizationUncertain {
		t.Fatalf("receipt after post-commit panic=%+v, want bounded uncertain terminal", receipt)
	}
	if err := s.recoverySettlements.wait(); err != nil {
		t.Fatalf("post-commit panic left a live settlement lease: %v", err)
	}
}

func decodeDaemonRecoverBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	return body
}

func TestDaemonRecoverPostTerminalSnapshotFailurePreservesCommittedReceipt(t *testing.T) {
	adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
	defer adapter.close()
	correlation := validAuditLockCorrelation(adapter.serverInstance, 1)
	binding := auditLockOccurrenceBinding{
		serverInstance: correlation.ServerInstance,
		taskName:       `\demo/default`,
		confirm:        true,
	}
	reservation, reserveErr := adapter.reserve(context.Background(), correlation, binding)
	if reserveErr != nil {
		t.Fatalf("reserve: %v", reserveErr)
	}
	receipt, terminalErr := adapter.terminalize(
		reservation,
		auditLockOccurrenceCommittedError,
		"none",
		auditLockTerminalEvidence{
			HTTPStatus:           http.StatusInternalServerError,
			ErrorCode:            "RECOVER_RESPAWN_FAILED",
			TerminationCommitted: true,
		},
	)
	if terminalErr != nil {
		t.Fatalf("terminalize: %v", terminalErr)
	}

	// Simulate only the second, post-terminal snapshot read becoming
	// unavailable. The durable receipt must remain the outward truth instead of
	// being replaced by a pre-mutation adapter error.
	adapter.initErr = errors.New("synthetic post-terminal snapshot failure")
	s := &Server{auditLock: adapter}
	rec := httptest.NewRecorder()
	s.writeDaemonRecoverOccurrenceError(
		context.Background(),
		rec,
		http.StatusInternalServerError,
		"RECOVER_RESPAWN_FAILED",
		true,
		receipt,
	)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response daemonRecoverOccurrenceErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "RECOVER_RESPAWN_FAILED" ||
		!response.TerminationCommitted ||
		response.AuditLock.RecoveryReceipt == nil ||
		response.AuditLock.RecoveryReceipt.Status != auditLockOccurrenceCommittedError ||
		response.AuditLock.RecoveryReceipt.TerminationCommitState != auditLockTerminationStateCommitted ||
		len(response.AuditLock.RecoveryReceipts) != 1 {
		t.Fatalf("post-terminal fallback response=%+v", response)
	}
}

func TestDaemonRecoverRouteCommittedFlagPreservesHTTPMatrix(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "respawn failed",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureRespawnFailed},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_RESPAWN_FAILED",
		},
		{
			name:       "supervisor unavailable",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureSupervisorUnavailable},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RECOVER_SUPERVISOR_UNAVAILABLE",
		},
		{
			name:       "respawn setup",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureStateRead, Cause: api.ErrRespawnSetupFailure},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_STATE_READ_FAILED",
		},
		{
			name:       "audit durability",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureAuditDurability},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_AUDIT_DURABILITY_FAILED",
		},
		{
			name:       "unclassified",
			err:        errors.New("synthetic backend failure"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_UNCLASSIFIED_FAILURE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9125})
			fake := &fakeDaemonRecoverer{
				result: daemonrecovery.Result{TerminationCommitted: true},
				err:    tc.err,
			}
			s.daemonRecover = fake
			rec := performDaemonRecoverRequest(t, s, `{"task_name":"demo/default","confirm":true}`)
			if rec.Code != tc.wantStatus || fake.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, fake.calls, rec.Body.String())
			}
			var response daemonRecoverOccurrenceErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.wantCode ||
				!response.TerminationCommitted ||
				response.AuditLock.RecoveryReceipt == nil ||
				response.AuditLock.RecoveryReceipt.Status != auditLockOccurrenceCommittedError ||
				response.AuditLock.RecoveryReceipt.TerminationCommitState != auditLockTerminationStateCommitted {
				t.Fatalf("committed HTTP response=%+v", response)
			}
		})
	}
}

func TestDaemonRecoverRouteRequiresSameOriginPOST(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	fake := &fakeDaemonRecoverer{}
	s.daemonRecover = fake

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/recover", strings.NewReader(`{"task_name":"x","confirm":true}`))
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || fake.calls != 0 {
		t.Fatalf("cross-origin status=%d calls=%d body=%s", rec.Code, fake.calls, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/daemon/recover", nil)
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || fake.calls != 0 {
		t.Fatalf("GET status=%d calls=%d body=%s", rec.Code, fake.calls, rec.Body.String())
	}
}

func TestDaemonRecoverRouteRequiresExplicitConfirmationBeforeInvocation(t *testing.T) {
	for _, body := range []string{
		`{"task_name":"mcp-local-hub-memory-default"}`,
		`{"task_name":"mcp-local-hub-memory-default","confirm":false}`,
	} {
		t.Run(body, func(t *testing.T) {
			s := NewServer(Config{Port: 9125})
			fake := &fakeDaemonRecoverer{}
			s.daemonRecover = fake

			rec := performDaemonRecoverRequest(t, s, body)
			if rec.Code != http.StatusPreconditionFailed {
				t.Fatalf("status=%d want=412 body=%s", rec.Code, rec.Body.String())
			}
			if fake.calls != 0 {
				t.Fatalf("recover invoked %d times without confirmation", fake.calls)
			}
			if code := decodeDaemonRecoverBody(t, rec)["code"]; code != "RECOVER_CONFIRMATION_REQUIRED" {
				t.Fatalf("code=%v", code)
			}
		})
	}
}

func TestKnownRecoveryContractGoldenMatrix(t *testing.T) {
	foreignPath := `C:\Users\<owner>\foreign.exe`
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown task",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureUnknownTask, TaskName: `\missing`},
			wantStatus: http.StatusBadRequest,
			wantCode:   "RECOVER_UNKNOWN_TASK",
		},
		{
			name: "foreign owner",
			err: &daemonrecovery.OperationError{
				Kind:     daemonrecovery.FailureRefusedPortOwner,
				TaskName: `\mcp-local-hub-memory-default`,
				Candidate: &daemonrecovery.ReapCandidate{
					PID:      44000,
					Verdict:  daemonrecovery.VerdictForeign,
					Identity: process.ProcessIdentity{ExecutablePath: foreignPath, CommandLine: foreignPath + " --attacker"},
				},
			},
			wantStatus: http.StatusConflict,
			wantCode:   "RECOVER_REFUSED_PORT_OWNER",
		},
		{
			name:       "respawn failed",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureRespawnFailed, Respawn: api.RespawnResult{Code: "RESPAWN_FAILED", Message: foreignPath}},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_RESPAWN_FAILED",
		},
		{
			name:       "supervisor unavailable",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureSupervisorUnavailable, Cause: errors.New("dial " + foreignPath)},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RECOVER_SUPERVISOR_UNAVAILABLE",
		},
		{
			name:       "state read failed",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureStateRead, Cause: errors.New("read " + foreignPath)},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_STATE_READ_FAILED",
		},
		{
			name:       "respawn setup failure",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureStateRead, Cause: api.ErrRespawnSetupFailure},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_STATE_READ_FAILED",
		},
		{
			name:       "request canceled before kill",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureRequestCanceled, Cause: context.Canceled},
			wantStatus: http.StatusRequestTimeout,
			wantCode:   "RECOVER_REQUEST_CANCELED",
		},
		{
			name:       "kill-boundary owner recheck timed out",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureBoundaryProbeTimeout, Cause: context.DeadlineExceeded},
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "RECOVER_BOUNDARY_PROBE_TIMEOUT",
		},
		{
			name:       "detached respawn budget insufficient",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureRespawnBudgetInsufficient, Cause: daemonrecovery.ErrInsufficientRespawnBudget},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RECOVER_RESPAWN_BUDGET_INSUFFICIENT",
		},
		{
			name:       "unclassified failure",
			err:        &daemonrecovery.OperationError{Kind: daemonrecovery.FailureKind("future_failure"), Cause: errors.New("unclassified " + foreignPath)},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_UNCLASSIFIED_FAILURE",
		},
		{
			name:       "raw non-operation error",
			err:        errors.New("unknown raw failure " + foreignPath),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_UNCLASSIFIED_FAILURE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9125})
			fake := &fakeDaemonRecoverer{err: tc.err}
			s.daemonRecover = fake

			rec := performDaemonRecoverRequest(t, s, `{"task_name":"mcp-local-hub-memory-default","confirm":true}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeDaemonRecoverBody(t, rec)
			if body["code"] != tc.wantCode || body["error"] != "internal error" {
				t.Fatalf("body=%v", body)
			}
			if committed, ok := body["termination_committed"]; !ok || committed != false {
				t.Fatalf("body=%v want termination_committed=false for a zero-result pre-kill failure", body)
			}
			if _, ok := body["reaped"]; ok {
				t.Fatalf("body=%v unexpectedly exposes the narrower reaped field", body)
			}
			if strings.Contains(rec.Body.String(), foreignPath) || strings.Contains(rec.Body.String(), "44000") {
				t.Fatalf("response leaked process details: %s", rec.Body.String())
			}
			if fake.calls != 1 || !fake.confirmed || fake.taskName != `\mcp-local-hub-memory-default` {
				t.Fatalf("fake calls=%d confirmed=%v task=%q", fake.calls, fake.confirmed, fake.taskName)
			}
		})
	}
}

func TestDaemonRecoverRouteSuccessReturnsOnlySafeAcceptedFields(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	fake := &fakeDaemonRecoverer{result: daemonrecovery.Result{
		TaskName:             `\mcp-local-hub-memory-default`,
		Reaped:               true,
		PortOwnerCheck:       daemonrecovery.PortOwnerReaped,
		PortWaitOutcome:      daemonrecovery.PortWaitStillBound,
		AuditHandoff:         daemonrecovery.AuditHandoffDurable,
		TerminationCommitted: true,
	}}
	s.daemonRecover = fake

	rec := performDaemonRecoverRequest(t, s, `{"task_name":"mcp-local-hub-memory-default","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// audit_handoff joins the pinned set deliberately: it names a lock-release
	// outcome, never a path, PID, or command line, so it carries nothing this
	// test exists to keep out of the response.
	got := decodeDaemonRecoverBody(t, rec)
	auditLock, ok := got["audit_lock"].(map[string]any)
	if !ok {
		t.Fatalf("body=%v missing typed audit_lock snapshot", got)
	}
	receipts, receiptsOK := auditLock["recovery_receipts"].([]any)
	if auditLock["scope"] != auditLockScope ||
		auditLock["state"] != "released" ||
		auditLock["recovery_receipt"] == nil ||
		!receiptsOK ||
		len(receipts) != 1 {
		t.Fatalf("audit_lock=%v", auditLock)
	}
	if instance, ok := auditLock["server_instance"].(string); !ok || validateAuditLockCorrelationValue("server_instance", instance) != nil {
		t.Fatalf("audit_lock server_instance=%v is not canonical UUIDv4", auditLock["server_instance"])
	}
	delete(got, "audit_lock")
	want := map[string]any{
		"task_name":             `\mcp-local-hub-memory-default`,
		"state":                 "respawn_accepted",
		"reaped":                true,
		"port_owner_check":      "reaped",
		"port_wait_outcome":     "still_bound",
		"audit_handoff":         "durable",
		"termination_committed": true,
	}
	if !mapsEqual(got, want) {
		t.Fatalf("body=%v want=%v", got, want)
	}
	if fake.calls != 1 || !fake.confirmed {
		t.Fatalf("calls=%d confirmed=%v", fake.calls, fake.confirmed)
	}
}

func TestDaemonRecover_ReservePostRenameErrorRunsOnce(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	installIsolatedAuditLock(t, s)
	t.Cleanup(func() { s.events.Close() })
	fake := &fakeDaemonRecoverer{result: daemonrecovery.Result{
		TaskName:             `\mcp-local-hub-memory-default`,
		Reaped:               true,
		PortOwnerCheck:       daemonrecovery.PortOwnerReaped,
		PortWaitOutcome:      daemonrecovery.PortWaitReleased,
		AuditHandoff:         daemonrecovery.AuditHandoffDurable,
		TerminationCommitted: true,
	}}
	s.daemonRecover = fake
	writeCalls := 0
	s.auditLock.writeStateFileLockHeld = func(path string, raw []byte) error {
		writeCalls++
		if err := api.WriteStateFileBytesLockHeld(path, raw); err != nil {
			return err
		}
		if writeCalls == 1 {
			return errAuditLockPostRenameTest
		}
		return nil
	}
	correlation := validAuditLockCorrelation(s.auditLock.serverInstance, 701)
	body := correlationPOSTBody(correlation, "mcp-local-hub-memory-default")

	first := httptest.NewRecorder()
	s.mux.ServeHTTP(first, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
	if first.Code != http.StatusOK || fake.calls != 1 {
		t.Fatalf("first status=%d calls=%d body=%s", first.Code, fake.calls, first.Body.String())
	}
	firstBody := decodeDaemonRecoverBody(t, first)
	firstAuditLock, ok := firstBody["audit_lock"].(map[string]any)
	if !ok || firstAuditLock["recovery_receipt"] == nil {
		t.Fatalf("first body lacks retained novel receipt: %v", firstBody)
	}

	replay := httptest.NewRecorder()
	s.mux.ServeHTTP(replay, sameOriginRequest(http.MethodPost, "/api/daemon/recover", body))
	if replay.Code != http.StatusOK || fake.calls != 1 {
		t.Fatalf("replay status=%d calls=%d body=%s", replay.Code, fake.calls, replay.Body.String())
	}
}

// An unconfirmed lock RELEASE is a warning on a SUCCEEDED recovery, and the
// distinction is the whole point of the AuditHandoff channel: the termination
// and the respawn both committed, so telling the operator "failed" would invite
// a re-run of a destructive operation that already happened.
//
// The GUI is also the process where the verdict actually bites — unlike the
// one-shot CLI, it lives for hours, so a lock it still holds blocks the
// supervisor and `mcphub install` for that whole time. That makes reporting it
// on a 200 (rather than swallowing it) load-bearing, not cosmetic.
func TestDaemonRecoverRouteReleaseUnconfirmedIsAWarningOnSuccessNotAFailure(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	fake := &fakeDaemonRecoverer{result: daemonrecovery.Result{
		TaskName:             `\mcp-local-hub-memory-default`,
		Reaped:               true,
		PortOwnerCheck:       daemonrecovery.PortOwnerReaped,
		PortWaitOutcome:      daemonrecovery.PortWaitReleased,
		AuditHandoff:         daemonrecovery.AuditHandoffReleaseUnconfirmed,
		TerminationCommitted: true,
	}}
	s.daemonRecover = fake

	rec := performDaemonRecoverRequest(t, s, `{"task_name":"mcp-local-hub-memory-default","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("an unconfirmed lock release must stay a SUCCESS — the reap and the respawn already "+
			"committed, so a non-200 invites the operator to retry a destructive op; status=%d body=%s",
			rec.Code, rec.Body.String())
	}
	body := decodeDaemonRecoverBody(t, rec)
	if body["audit_handoff"] != "release_unconfirmed" {
		t.Fatalf("the warning must reach the operator, or a lock this long-lived process still holds "+
			"blocks the supervisor invisibly; body=%v", body)
	}
	if body["state"] != "respawn_accepted" {
		t.Fatalf("state must still report the accepted respawn; body=%v", body)
	}
}

// The complement: a clean handoff must NOT raise the warning, or the signal
// becomes noise the operator learns to ignore.
func TestDaemonRecoverRouteDurableHandoffRaisesNoWarning(t *testing.T) {
	s := NewServer(Config{Port: 9125})
	s.daemonRecover = &fakeDaemonRecoverer{result: daemonrecovery.Result{
		TaskName:        `\mcp-local-hub-memory-default`,
		PortOwnerCheck:  daemonrecovery.PortOwnerUnbound,
		PortWaitOutcome: daemonrecovery.PortWaitNotRequired,
		AuditHandoff:    daemonrecovery.AuditHandoffDurable,
	}}

	rec := performDaemonRecoverRequest(t, s, `{"task_name":"mcp-local-hub-memory-default","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := decodeDaemonRecoverBody(t, rec)["audit_handoff"]; got != "durable" {
		t.Fatalf("a confirmed release must report durable, not %v", got)
	}
}

func TestDaemonRecoverRouteRespawnFailureReportsCommittedTerminationWithoutProcessDetail(t *testing.T) {
	leakyPath := `C:\Users\<owner>\mcphub.exe`
	tests := []struct {
		name       string
		result     daemonrecovery.Result
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "confirmed reap then supervisor unavailable",
			result: daemonrecovery.Result{
				Reaped:               true,
				PortOwnerCheck:       daemonrecovery.PortOwnerReaped,
				TerminationCommitted: true,
			},
			err: &daemonrecovery.OperationError{
				Kind:  daemonrecovery.FailureSupervisorUnavailable,
				Cause: errors.New("dial failed after reap of " + leakyPath),
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RECOVER_SUPERVISOR_UNAVAILABLE",
		},
		{
			name: "termination unconfirmed then respawn rejected",
			result: daemonrecovery.Result{
				Reaped:               false,
				PortOwnerCheck:       daemonrecovery.PortOwnerTerminationUnconfirmed,
				TerminationCommitted: true,
			},
			err: &daemonrecovery.OperationError{
				Kind:    daemonrecovery.FailureRespawnFailed,
				Respawn: api.RespawnResult{Code: "RESPAWN_FAILED", Message: leakyPath},
			},
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_RESPAWN_FAILED",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Config{Port: 9125})
			s.daemonRecover = &fakeDaemonRecoverer{result: tc.result, err: tc.err}

			rec := performDaemonRecoverRequest(t, s, `{"task_name":"mcp-local-hub-memory-default","confirm":true}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := decodeDaemonRecoverBody(t, rec)
			if body["code"] != tc.wantCode || body["error"] != "internal error" || body["termination_committed"] != true {
				t.Fatalf("body=%v want redacted error with termination_committed=true", body)
			}
			if _, ok := body["reaped"]; ok {
				t.Fatalf("body=%v unexpectedly exposes the narrower reaped field", body)
			}
			if strings.Contains(rec.Body.String(), leakyPath) || strings.Contains(rec.Body.String(), "dial failed") {
				t.Fatalf("response leaked process detail: %s", rec.Body.String())
			}
		})
	}
}

func TestDaemonRecoverHTTPFailureMapsStableKinds(t *testing.T) {
	tests := []struct {
		name       string
		kind       daemonrecovery.FailureKind
		wantStatus int
		wantCode   string
	}{
		{
			name:       "local state or owner-sidecar failure",
			kind:       daemonrecovery.FailureStateRead,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_STATE_READ_FAILED",
		},
		{
			name:       "supervisor transport unavailable",
			kind:       daemonrecovery.FailureSupervisorUnavailable,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RECOVER_SUPERVISOR_UNAVAILABLE",
		},
		{
			name:       "respawn setup failure remains a redacted internal error",
			kind:       daemonrecovery.FailureStateRead,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "RECOVER_STATE_READ_FAILED",
		},
		{
			name:       "request canceled before kill",
			kind:       daemonrecovery.FailureRequestCanceled,
			wantStatus: http.StatusRequestTimeout,
			wantCode:   "RECOVER_REQUEST_CANCELED",
		},
		{
			name:       "boundary probe timeout",
			kind:       daemonrecovery.FailureBoundaryProbeTimeout,
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "RECOVER_BOUNDARY_PROBE_TIMEOUT",
		},
		{
			name:       "detached respawn budget insufficient",
			kind:       daemonrecovery.FailureRespawnBudgetInsufficient,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "RECOVER_RESPAWN_BUDGET_INSUFFICIENT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, code := daemonRecoverHTTPFailure(&daemonrecovery.OperationError{Kind: tc.kind})
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("mapping=(%d,%q) want=(%d,%q)", status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range b {
		if a[key] != value {
			return false
		}
	}
	return true
}
