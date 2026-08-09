package cmaketrace

import (
	"encoding/json"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR18TraceAdmissionAccountsForEncodedLineNumberBytes(t *testing.T) {
	// Decimal digits alone remain below the admission ceiling here; the
	// indented array separators/whitespace are what push the encoded body over.
	lineCount := publicresult.MaxEncodedBytes / 12
	lines := make([]int, lineCount)
	for i := range lines {
		lines[i] = 100000000 + i
	}
	result := Result{ExecutedLines: []FileLines{{File: "f", Lines: lines}}}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= publicresult.MaxEncodedBytes {
		t.Fatalf("test fixture encoded bytes=%d, want over limit=%d", len(raw), publicresult.MaxEncodedBytes)
	}
	if !result.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("multi-digit executed line numbers escaped pre-marshal projection admission")
	}
}
