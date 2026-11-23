package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestWindowsProductEntrySetDiscriminatorIsStrict(t *testing.T) {
	legacy := `"` + windowsCanonicalEntrySetSchemaV1 + `"`
	current := `"` + binaryadmission.WindowsCanonicalEntrySetSchemaV2 + `"`
	for _, reader := range []struct {
		name string
		new  func() json.Unmarshaler
	}{
		{"journal", func() json.Unmarshaler { return new(windowsProductRecoveryJournalV3) }},
		{"receipt", func() json.Unmarshaler { return new(UpgradeReceiptV3) }},
	} {
		t.Run(reader.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				raw  string
				want string
			}{
				{"escaped_duplicate_schema", `{"entry_set":{"schema":` + current + `,"\u0073chema":` + legacy + `}}`, "duplicate decoded schema member"},
				{"case_folded_schema", `{"entry_set":{"Schema":` + legacy + `}}`, "reserved schema member spelling is not exact"},
				{"unknown_outer_field", `{"unknown":true,"entry_set":{"schema":` + legacy + `}}`, "unknown field"},
				{"current_field_in_legacy_arm", `{"entry_set":{"schema":` + legacy + `,"bundle":{}}}`, "unknown field"},
				{"unsupported_arm", `{"entry_set":{"schema":"unsupported"}}`, "unsupported nested canonical entry schema"},
				{"trailing_record", `{"entry_set":{"schema":` + legacy + `}} {}`, "trailing JSON"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := reader.new().UnmarshalJSON([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
						t.Fatalf("error = %v, want %q", err, tc.want)
					}
				})
			}
		})
	}

	raw := []byte(`{"entry_set":{"schema":` + legacy + `}}`)
	var journal windowsProductRecoveryJournalV3
	if err := json.Unmarshal(raw, &journal); err != nil || journal.EntrySet == nil || journal.EntryBundle != nil {
		t.Fatalf("legacy journal arm did not decode: %v", err)
	}
	var receipt UpgradeReceiptV3
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.EntrySet == nil || receipt.EntryBundle != nil {
		t.Fatalf("legacy receipt arm did not decode: %v", err)
	}

	t.Run("public_receipt", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			raw  string
			want string
		}{
			{"escaped_duplicate_schema", `{"schema":"upgrade-receipt-v3","\u0073chema":"upgrade-receipt-v1"}`, "duplicate decoded schema member"},
			{"unknown_schema", `{"schema":"unknown"}`, "unsupported upgrade receipt schema"},
			{"case_alias_schema", `{"Schema":"upgrade-receipt-v1"}`, "reserved schema member spelling is not exact"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := DecodeUpgradeReceipt([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
			})
		}
	})
}
