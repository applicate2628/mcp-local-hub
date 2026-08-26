package api

import (
	"errors"
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
