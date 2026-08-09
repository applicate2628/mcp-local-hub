package cmaketrace

import (
	"encoding/json"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR22TraceAdmissionAccountsForEscapedJSONStringBytes(t *testing.T) {
	result := Result{Records: []Record{{Args: []string{strings.Repeat("\x00", 50_000)}}}}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= publicresult.MaxEncodedBytes {
		t.Fatalf("fixture bytes=%d, want over %d", len(raw), publicresult.MaxEncodedBytes)
	}
	if !result.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("escaped JSON string bytes escaped pre-marshal projection admission")
	}
}

func TestR22EncodedJSONStringBytesMatchesJSONEncoder(t *testing.T) {
	for _, value := range []string{"plain", "<&>", "quote\"slash\\", "\x00\b\f\n\r\t", "\u2028\u2029", string([]byte{0xff})} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if got := encodedJSONStringBytes(value); got != len(raw) {
			t.Errorf("encodedJSONStringBytes(%q)=%d, json.Marshal bytes=%d (%q)", value, got, len(raw), raw)
		}
	}
}
