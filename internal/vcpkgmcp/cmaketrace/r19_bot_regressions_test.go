package cmaketrace

import (
	"encoding/json"
	"fmt"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR19TraceAdmissionAccountsForExecutedLineEntrySyntax(t *testing.T) {
	result := Result{ExecutedLines: make([]FileLines, 4000)}
	for i := range result.ExecutedLines {
		result.ExecutedLines[i] = FileLines{File: fmt.Sprintf("f%d", i), Lines: []int{1}}
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= publicresult.MaxEncodedBytes {
		t.Fatalf("fixture bytes=%d, want over %d", len(raw), publicresult.MaxEncodedBytes)
	}
	if !result.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("enclosing FileLines JSON syntax escaped projection admission")
	}
}
