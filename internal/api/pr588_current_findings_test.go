package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
)

func TestInstallOptsMayWriteClients_SkipClientConfigWrites(t *testing.T) {
	if installOptsMayWriteClients(InstallOpts{SkipClientConfigWrites: true}) {
		t.Fatal("SkipClientConfigWrites=true must suppress client routing resolution")
	}
	if !installOptsMayWriteClients(InstallOpts{}) {
		t.Fatal("ordinary install must retain client routing resolution")
	}
}

func TestRestoreSerenaReconcileAppliedOwned_UnavailableClientStaysRetryable(t *testing.T) {
	client := newReconcileFakeClient("claude-code")
	client.exists = false

	results, err := RestoreSerenaReconcileAppliedOwned([]SerenaOwnedRestoreRequest{{
		Client:                     client.name,
		BaselineBytes:              []byte(`{"mcpServers":{}}`),
		ExpectedAppliedFingerprint: "expected",
		BaselinePresent:            true,
	}}, map[string]clients.Client{client.name: client})
	if err == nil {
		t.Fatal("unavailable client must return a retryable rollback error")
	}
	if len(results) != 1 || results[0].Status != SerenaOwnedRestoreFailed {
		t.Fatalf("results=%+v, want one retryable failed row", results)
	}
	if client.addCalls != 0 || client.removeCalls != 0 || client.restoreCalls != 0 {
		t.Fatalf("unavailable client was mutated: add=%d remove=%d restore=%d", client.addCalls, client.removeCalls, client.restoreCalls)
	}
}

func TestWakeIdleSerenaDaemonWithAuditSink_RoutesNudgeWarningToCaller(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	defer SetDaemonStateRootForTest(stateDir)()

	task := `\mcp-local-hub-serena-idle-route-audit`
	if err := NewAPI().WriteSerenaIdleStop(task, time.Now().UTC()); err != nil {
		t.Fatalf("seed idle stop: %v", err)
	}

	nudgeErr := errors.New("synthetic reconcile failure")
	origRec, origReady := serenaWakeReconcileFn, serenaWakeReadinessFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, nudgeErr
	}
	serenaWakeReadinessFn = func(context.Context, string, int, time.Duration) error { return nil }
	t.Cleanup(func() { serenaWakeReconcileFn, serenaWakeReadinessFn = origRec, origReady })

	var gotLevel, gotEvent string
	var gotFields map[string]any
	sink := func(level, event string, fields map[string]any) error {
		gotLevel, gotEvent, gotFields = level, event, fields
		return nil
	}
	if err := NewAPI().WakeIdleSerenaDaemonWithAuditSink(context.Background(), task, 9204, "route", sink); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if gotLevel != "warn" || gotEvent != "serena-idle-wake-reconcile-nudge-failed" {
		t.Fatalf("audit=(%q,%q), want caller-owned warning", gotLevel, gotEvent)
	}
	if gotFields["task_name"] != task || gotFields["err"] != nudgeErr.Error() {
		t.Fatalf("audit fields=%v, want task and redacted cause", gotFields)
	}
}

func TestWakeIdleSerenaDaemonWithAuditSink_RoutesStopReadDiagnosticToCaller(t *testing.T) {
	origRead := serenaWakeReadStopFn
	t.Cleanup(func() { serenaWakeReadStopFn = origRead })

	const event = "synthetic-stop-read-diagnostic"
	serenaWakeReadStopFn = func(_ string, sink func(string, string, map[string]any) error) (DaemonIntent, error) {
		if sink == nil {
			t.Fatal("stop reader received nil audit sink")
		}
		if err := sink("warn", event, map[string]any{"source": "supervisor-intent"}); err != nil {
			return DaemonIntent{}, err
		}
		return DaemonIntent{}, nil
	}

	var gotEvent string
	if err := NewAPI().WakeIdleSerenaDaemonWithAuditSink(context.Background(), `\route-audit`, 9204, "route", func(_ string, event string, _ map[string]any) error {
		gotEvent = event
		return nil
	}); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if gotEvent != event {
		t.Fatalf("stop-read diagnostic event=%q, want caller-owned %q", gotEvent, event)
	}
}
