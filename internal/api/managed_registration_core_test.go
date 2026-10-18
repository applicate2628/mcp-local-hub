package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestW10_ManagedEntryOptionalMetadataRoundTrip catches a schema-1 reader that
// silently discards the optional managed-registration ownership projection.
func TestW10_ManagedEntryOptionalMetadataRoundTrip(t *testing.T) {
	var entries ManagedEntries
	if err := json.Unmarshal([]byte(`{"version":1,"entries":[{"client":"codex-cli","server":"serena","installed_at":"2026-09-10T00:00:00Z","owner":"managed-registration","transport":"http","enabled_state":"enabled","exists":true,"config_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries.Entries) != 1 {
		t.Fatalf("entries = %d, want one", len(entries.Entries))
	}
	owner := reflect.ValueOf(entries.Entries[0]).FieldByName("Owner")
	if !owner.IsValid() || owner.String() != "managed-registration" {
		t.Fatal("managed entry did not retain optional owner metadata")
	}
}
