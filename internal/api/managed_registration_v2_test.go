package api

import (
	"encoding/json"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestW10_ManagedRegistrationV2WireContracts(t *testing.T) {
	raw, err := json.Marshal(managedRegistrationJournalFile{Schema: managedRegistrationJournalSchema})
	if err != nil || string(raw) == "" || !strings.Contains(string(raw), `"schema":"managed-registration-reconcile/v2"`) {
		t.Fatalf("journal wire = %s, %v", raw, err)
	}
	event, ok := managedRegistrationSettledEvent(ManagedRegistrationRow{GenerationID: "g", ClientID: "c", Settlement: clients.ManagedRegistrationSettlementSettled})
	if !ok || event.EventID != "managed-registration-reconcile-settled.v2" {
		t.Fatalf("settled event = %#v, %t", event, ok)
	}
}
