package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

func TestAttachClientApplyResultIdempotentNoWriteJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		bit  bool
		want string
	}{
		{name: "first-write", bit: false, want: `{"state":"committed","receipt_id":"r","server":"s","daemon":"d","client":"mimocode","entry_key":"s","endpoint":"e","before_sha256":"a","after_sha256":"b"}`},
		{name: "settled-repeat", bit: true, want: `{"state":"committed","receipt_id":"r","server":"s","daemon":"d","client":"mimocode","entry_key":"s","endpoint":"e","before_sha256":"a","after_sha256":"b","idempotent_no_write":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(AttachClientApplyResult{State: "committed", ReceiptID: "r", Server: "s", Daemon: "d", Client: "mimocode", EntryKey: "s", Endpoint: "e", BeforeSHA256: "a", AfterSHA256: "b", IdempotentNoWrite: tc.bit})
			if err != nil || string(got) != tc.want {
				t.Fatalf("result JSON=%s err=%v want=%s", got, err, tc.want)
			}
		})
	}
}

func TestAttachExisting_AbsentManifestWithoutIntentRefusesBeforeSupervisorProbe(t *testing.T) {
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", t.TempDir())
	withDaemonStateRootOverride(t, t.TempDir())
	plan, err := NewAPI().BuildAttachClientPlan(context.Background(), AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"})
	var refusal *AttachPlanError
	if plan != nil || !errors.As(err, &refusal) || refusal.Code != "intent-unavailable" {
		t.Fatalf("absent manifest not typed no-write refusal: plan=%+v err=%v", plan, err)
	}
}

func TestAttachExisting_OriginErrorCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		origin    clients.SelectedEntryOriginV1
		committed bool
		want      string
	}{
		{name: "direct-new", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedDirect}},
		{name: "http-unowned-legacy", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedHTTP}, want: "client-origin-disabled-or-unsupported-transport"},
		{name: "http-committed", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedHTTP}, committed: true},
		{name: "direct-committed", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedDirect}, committed: true, want: "client-origin-target-owned-direct"},
		{name: "higher-committed", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginHigherOrMixed}, committed: true, want: "client-origin-higher-or-mixed"},
		{name: "missing-new", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginMissing}, want: "client-origin-missing"},
		{name: "missing-committed", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginMissing}, committed: true, want: "client-origin-missing"},
		{name: "zero", origin: clients.SelectedEntryOriginV1{Version: 1}, want: "client-origin-unverified"},
		{name: "unknown", origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: "future-source"}, committed: true, want: "client-origin-unverified"},
		{name: "wrong-version", origin: clients.SelectedEntryOriginV1{Version: 2, Verdict: clients.SelectedEntryOriginTargetOwnedDirect}, want: "client-origin-unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachOriginPlanCode(tc.origin, tc.committed); got != tc.want {
				t.Fatalf("origin refusal code=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAttachExisting_StatusWireRetainsPrivateIdentityWithoutPublicFieldChange(t *testing.T) {
	raw := json.RawMessage(`{"state":"running","daemons":[{"task_name":"\\mcp-local-hub-codegraph-default","server":"codegraph","daemon":"default","command":"mcphub","args":["daemon","--server","codegraph","--daemon","default"],"port":9303,"state":"Running","current_pid":7200,"started_at":"2026-09-28T10:00:00Z"}]}`)
	rows, err := decodeSupervisorIPCStatusResult(raw)
	if err != nil || len(rows) != 1 {
		t.Fatalf("status wire decode: rows=%+v err=%v", rows, err)
	}
	if rows[0].CommandInternal != "mcphub" || len(rows[0].ArgsInternal) != 5 || rows[0].StartedAtInternal != "2026-09-28T10:00:00Z" {
		t.Fatalf("existing producer fields lost: %+v", rows[0])
	}
	public, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secretField := range []string{"command", "args", "started_at"} {
		if strings.Contains(string(public), `"`+secretField+`"`) {
			t.Fatalf("private status identity became public: %s", public)
		}
	}
}

func attachObservedFixture(t *testing.T) attachObserved {
	t.Helper()
	imagePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	return attachObserved{
		manifest:     &config.ServerManifest{Name: "codegraph", Kind: config.KindGlobal, Transport: config.TransportStdioBridge, Daemons: []config.DaemonSpec{{Name: "default", Port: 9303}}},
		manifestHash: "manifest-hash",
		intent: &SupervisorIntentFile{IntentGeneration: 7, Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-codegraph-default`, Server: "codegraph", Daemon: "default",
			Command: imagePath, Args: []string{"daemon", "--server", "codegraph", "--daemon", "default"},
			Port: 9303, ManifestHash: "manifest-hash",
		}}},
		owner: SupervisorLockOwner{PID: 7100, StartedAt: "2026-09-28T09:00:00Z"},
		rows: []DaemonStatus{{Server: "codegraph", Daemon: "default", TaskName: `\mcp-local-hub-codegraph-default`,
			State: "Running", Port: 9303, PID: 7200, CommandInternal: imagePath,
			ArgsInternal:      []string{"daemon", "--server", "codegraph", "--daemon", "default"},
			StartedAtInternal: start.Format(time.RFC3339Nano)}},
		childStartedAt: start, listenerPID: 7200,
		readiness: MCPReadinessResult{State: MCPReadinessReady},
		origin: clients.SelectedEntryOriginV1{Version: 1, Verdict: clients.SelectedEntryOriginTargetOwnedDirect,
			ClientID: "mimocode", EntryName: "codegraph", Scope: "known-global", SourceKind: "write-target",
			TargetPathInternal: `C:\fixture\mimocode.json`, TargetBytesSHA256: "bytes-hash", EntryTokenSHA256: "token-hash"},
	}
}

func TestAttachExisting_WrongListenerPIDCannotProducePlan(t *testing.T) {
	obs := attachObservedFixture(t)
	obs.listenerPID = 7300
	plan, err := admitAttachObserved(AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, obs)
	if err == nil || plan != nil {
		t.Fatalf("wrong listener produced plan: plan=%+v err=%v", plan, err)
	}
}

func TestAttachExisting_OneExactObservedClientProducesRedactedPlan(t *testing.T) {
	obs := attachObservedFixture(t)
	plan, err := admitAttachObserved(AttachClientRequest{Server: "codegraph", Daemon: "default", Client: "mimocode"}, obs)
	if err != nil || plan == nil {
		t.Fatalf("exact observed target refused: plan=%+v err=%v", plan, err)
	}
	if plan.EntryKey != "codegraph" || plan.Endpoint != "http://127.0.0.1:9303/mcp" || plan.BeforeTransport != "direct" || plan.AfterTransport != "http" || plan.EntryTokenSHA256 != "token-hash" {
		t.Fatalf("plan lost selected-only redacted projection: %+v", plan)
	}
}
