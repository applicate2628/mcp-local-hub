package mcproute

import (
	"reflect"
	"testing"

	"mcp-local-hub/internal/daemon"
)

func TestHubStdioSettlementFieldsOnlyExposeKnownGeneration(t *testing.T) {
	good := daemon.IsolatedStdioSettlementDiagnostic{Sequence: 9, Complete: true, OriginalPID: 42, OriginalBirthFILETIME: 123}
	want := map[string]any{"role": "hub", "session_sequence": uint64(9), "complete": true, "original_known": true,
		"original_pid": 42, "original_birth_filetime": uint64(123)}
	if got := hubStdioSettlementFields(good); !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected known event fields: %#v", got)
	}
	good.OriginalBirthFILETIME = 0
	delete(want, "original_pid")
	delete(want, "original_birth_filetime")
	want["original_known"] = false
	if got := hubStdioSettlementFields(good); !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown event has partial identity: %#v", got)
	}
}
