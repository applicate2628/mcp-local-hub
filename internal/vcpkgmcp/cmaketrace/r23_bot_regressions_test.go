package cmaketrace

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR23TraceAdmissionAccountsForRecordObjectSyntax(t *testing.T) {
	result := Result{Records: make([]Record, 4_000)}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= publicresult.MaxEncodedBytes {
		t.Fatalf("fixture bytes=%d, want over %d", len(raw), publicresult.MaxEncodedBytes)
	}
	if !result.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("record object syntax escaped pre-marshal projection admission")
	}
}

func TestR23ParserRetainedRecordByteCeiling(t *testing.T) {
	line := `{"file":"a.cmake","line":1,"cmd":"message","args":["","","","","","","","","","","","","","","",""]}` + "\n"
	lim := Limits{MaxTraceBytes: 1 << 20, MaxLineBytes: 1 << 10, MaxParsedRecords: 100, MaxRetainedRecordBytes: 128}
	res, err := parseTraceStream(context.Background(), strings.NewReader(line), lim)
	if err != nil {
		t.Fatal(err)
	}
	if !res.hitRetainedRecordLimit || len(res.records) != 0 {
		t.Fatalf("hitRetainedRecordLimit=%v records=%d, want true/0", res.hitRetainedRecordLimit, len(res.records))
	}
	if !slices.Contains(res.incompleteReasons(), ReasonRetainedRecordLimit) {
		t.Fatalf("incompleteReasons=%v, want %q", res.incompleteReasons(), ReasonRetainedRecordLimit)
	}
}
