package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestClientConfigSettlementOwnerRejectsPartialLowerResult(t *testing.T) {
	owner := ClientConfigSettlementOwnerV1{}
	row, err := owner.Accept(ClientConfigOperationInstall, clients.CodexHTTPRelocationResult{
		Outcome:  clients.CodexHTTPRelocationCommitted,
		Readback: clients.CodexHTTPRelocationReadbackExact,
	})
	if !errors.Is(err, ErrClientConfigSettlementInvalid) {
		t.Fatalf("Accept error = %v, want CLIENT_CONFIG_SETTLEMENT_INVALID", err)
	}
	if row != (ClientConfigSettlementV1{}) {
		t.Fatalf("Accept row = %#v, want zero row", row)
	}
}

func TestClientConfigSettlementOwnerAcceptsPathFreeProjectV2(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	var emitted ClientConfigSettlementV1
	owner := ClientConfigSettlementOwnerV1{emit: func(row ClientConfigSettlementV1) error {
		emitted = row
		return nil
	}}
	row, err := owner.AcceptProject(ClientConfigOperationAdopt, clients.CodexHTTPRelocationResult{
		LogicalSource: "graphify", TargetEntry: "graphify", WriteTarget: "codex_project",
		DesiredTransport: clients.CodexTransportHTTP, Action: "replace",
		Outcome: clients.CodexHTTPRelocationCommitted, Readback: clients.CodexHTTPRelocationReadbackExact,
	}, fingerprint)
	if err != nil {
		t.Fatalf("AcceptProject: %v", err)
	}
	if row.SchemaVersion != clientConfigSettlementSchemaV2 || row.ProjectIdentitySHA256 != fingerprint ||
		row.WriteTarget != "codex_project" || row.Action != "replace" || row.CollisionReason != "" || !row.IsCommittedWrite() {
		t.Fatalf("project settlement = %#v", row)
	}
	if emitted != row {
		t.Fatalf("emitted = %#v, want %#v", emitted, row)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(encoded); strings.Contains(bytes, "project_root") || strings.Contains(bytes, "config.toml") ||
		!strings.Contains(bytes, `"project_identity_sha256":"`+fingerprint+`"`) {
		t.Fatalf("project settlement wire shape = %s", bytes)
	}
}

func TestClientConfigSettlementOwnerEventFailureRetainsCommittedRow(t *testing.T) {
	emitErr := errors.New("injected event failure")
	calls := 0
	owner := ClientConfigSettlementOwnerV1{emit: func(ClientConfigSettlementV1) error { calls++; return emitErr }}
	row, err := owner.Accept(ClientConfigOperationAdopt, clients.CodexHTTPRelocationResult{
		LogicalSource: "serena", TargetEntry: "serena-mcphub", WriteTarget: "codex_global",
		DesiredTransport: clients.CodexTransportHTTP, CollisionReason: "cross_layer_opposite_transport",
		Action: "relocate", Outcome: clients.CodexHTTPRelocationCommitted,
		Readback: clients.CodexHTTPRelocationReadbackExact,
	})
	if !errors.Is(err, ErrClientConfigSettlementEventFailed) {
		t.Fatalf("Accept error = %v, want CLIENT_CONFIG_SETTLEMENT_EVENT_FAILED", err)
	}
	if calls != 1 {
		t.Fatalf("event attempts = %d, want exactly one with no retry", calls)
	}
	if !row.IsCommittedWrite() || row.Operation != string(ClientConfigOperationAdopt) {
		t.Fatalf("Accept row = %#v, want retained committed adopt row", row)
	}
}
