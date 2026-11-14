package cmaketrace

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

// Keep these oracles independent of the fixture generator. A fast benchmark
// which silently drops records, arguments or indexes is not a useful baseline.
func TestTraceBenchmarkFixtures(t *testing.T) {
	for _, pattern := range []string{"repeated", "unique", "mixed", "long_args"} {
		t.Run(pattern, func(t *testing.T) {
			payload := benchmarkTracePayload(t, 8, pattern)
			parsed, err := parseTraceStream(context.Background(), strings.NewReader(payload), Limits{})
			if err != nil || len(parsed.records) != 8 || !parsed.versionHeaderPresent || parsed.incomplete() {
				t.Fatalf("fixture: records=%d header=%v incomplete=%v err=%v", len(parsed.records), parsed.versionHeaderPresent, parsed.incomplete(), err)
			}
			commands := []string{"set", "include", "add_subdirectory", "MESSAGE"}
			for i, got := range parsed.records {
				want := Record{File: "/fixture/CMakeLists.txt", Line: i + 1, Cmd: "set", Args: []string{"X", "Y"}, Time: 1.25, Frame: 1, GlobalFrame: 1}
				switch pattern {
				case "unique":
					want.File = fmt.Sprintf("/fixture/unit-%06d.cmake", i)
				case "mixed":
					want.Cmd = commands[i%4]
					want.Defer = i%4 == 3
				case "long_args":
					want.Args = []string{"X", strings.Repeat("λ", 256)}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("record %d: got %#v, want %#v", i, got, want)
				}
			}
			path := writeTrace(t, payload)
			result := Trace(context.Background(), Args{TracePath: path}, DefaultDeps())
			if result.Status != evidence.StatusOK || result.InputIncomplete || result.Truncated || !reflect.DeepEqual(result.Records, parsed.records) {
				t.Fatalf("public result: status=%s reason=%s incomplete=%v truncated=%v", result.Status, result.Reason, result.InputIncomplete, result.Truncated)
			}
			wantFiles := []string{"/fixture/CMakeLists.txt"}
			wantLines := []FileLines{{File: wantFiles[0], Lines: []int{1, 2, 3, 4, 5, 6, 7, 8}}}
			if pattern == "unique" {
				wantFiles, wantLines = nil, nil
				for i := 0; i < 8; i++ {
					name := fmt.Sprintf("/fixture/unit-%06d.cmake", i)
					wantFiles = append(wantFiles, name)
					wantLines = append(wantLines, FileLines{File: name, Lines: []int{i + 1}})
				}
			}
			if !reflect.DeepEqual(result.FilesInTrace, wantFiles) || !reflect.DeepEqual(result.ExecutedLines, wantLines) {
				t.Fatalf("wrong derived indexes: files=%v lines=%v", result.FilesInTrace, result.ExecutedLines)
			}
			var wantChain []IncludeChainEntry
			if pattern == "mixed" {
				for _, line := range []int{2, 3, 6, 7} {
					kind := KindInclude
					if line%4 == 3 {
						kind = KindAddSubdirectory
					}
					wantChain = append(wantChain, IncludeChainEntry{Kind: kind, File: "/fixture/CMakeLists.txt", Line: line, Argument: "X"})
				}
			}
			if !reflect.DeepEqual(result.IncludeChain, wantChain) {
				t.Fatalf("include chain: got %v, want %v", result.IncludeChain, wantChain)
			}
			encoded, err := publicresult.MarshalIndent(result)
			if err != nil || len(encoded) > publicresult.MaxEncodedBytes {
				t.Fatalf("encoding: bytes=%d err=%v", len(encoded), err)
			}
			var roundTrip Result
			if err := json.Unmarshal(encoded, &roundTrip); err != nil || !reflect.DeepEqual(result, roundTrip) {
				t.Fatalf("public JSON round trip: %v", err)
			}
		})
	}
}

func TestTraceBenchmarkPublicProjection(t *testing.T) {
	path := writeTrace(t, benchmarkTracePayload(t, 50000, "repeated"))
	result := Trace(context.Background(), Args{TracePath: path}, DefaultDeps())
	if result.Status != evidence.StatusOK || result.InputIncomplete || !result.Truncated || len(result.Records) != DefaultMaxRecords || len(result.ExecutedLines) != 1 || len(result.ExecutedLines[0].Lines) != 50000 {
		t.Fatalf("large fixture: status=%s records=%d incomplete=%v truncated=%v", result.Status, len(result.Records), result.InputIncomplete, result.Truncated)
	}
	encoded, err := publicresult.MarshalIndent(result)
	if err != nil || len(encoded) > publicresult.MaxEncodedBytes {
		t.Fatalf("public encoding: bytes=%d err=%v", len(encoded), err)
	}
	var wire struct {
		Status          Status                   `json:"status"`
		InputIncomplete bool                     `json:"input_incomplete"`
		Truncated       bool                     `json:"truncated"`
		Projection      *publicresult.Projection `json:"result_projection"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Status != evidence.StatusOK || wire.InputIncomplete || !wire.Truncated || wire.Projection == nil || wire.Projection.Complete || len(wire.Projection.Omissions) == 0 {
		t.Fatalf("public projection lost completeness/omission evidence: %+v", wire)
	}
}
