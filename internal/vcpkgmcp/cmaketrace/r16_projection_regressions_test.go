package cmaketrace

import (
	"encoding/json"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestR16TraceProjectionRetainsIdentityAndEnumeratesCollections(t *testing.T) {
	result := Result{
		Status:       evidence.StatusOK,
		IncludeChain: []IncludeChainEntry{{Kind: KindInclude, File: "/src/CMakeLists.txt", Line: 4, Argument: "child.cmake"}},
		Records:      []Record{{File: "/src/CMakeLists.txt", Line: 4, Cmd: "include"}},
		ExecutedLines: []FileLines{{
			File: "/src/CMakeLists.txt", Lines: []int{4},
		}},
		FilesInTrace: []string{"/src/CMakeLists.txt"},
		Evidence:     evidence.Evidence{Paths: []string{"/trace/build.jsonl"}},
	}
	body, err := json.Marshal(result.PublicResultProjection())
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Evidence   evidence.Evidence       `json:"evidence"`
		Projection publicresult.Projection `json:"result_projection"`
	}
	if err := json.Unmarshal(body, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Evidence.Paths) != 1 || projected.Evidence.Paths[0] != "/trace/build.jsonl" {
		t.Fatalf("projected evidence = %+v, want trace identity", projected.Evidence)
	}
	omissions := map[string]publicresult.Omission{}
	for _, omission := range projected.Projection.Omissions {
		omissions[omission.Field] = omission
	}
	for _, field := range []string{"include_chain", "records", "executed_lines", "files_in_trace"} {
		omission, ok := omissions[field]
		if !ok || omission.Omitted == nil || *omission.Omitted != 1 {
			t.Fatalf("omissions = %+v, want %s omitted=1", projected.Projection.Omissions, field)
		}
	}
}

func TestR16TraceAdmissionRejectsLargeAggregateBeforeEncoding(t *testing.T) {
	result := Result{
		Records:  make([]Record, 0, 6000),
		Evidence: evidence.Evidence{Paths: []string{"/trace/large-build.jsonl"}},
	}
	for i := 0; i < cap(result.Records); i++ {
		result.Records = append(result.Records, Record{File: "/src/" + strings.Repeat("x", 64), Cmd: "set", Args: []string{"value"}})
	}
	if !result.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("large trace aggregate was admitted for full encoding")
	}
	body, err := publicresult.MarshalIndent(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > publicresult.MaxEncodedBytes || !strings.Contains(string(body), `"result_projection"`) || !strings.Contains(string(body), "/trace/large-build.jsonl") {
		t.Fatalf("projected body bytes=%d body=%s", len(body), body)
	}
}
