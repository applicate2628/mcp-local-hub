package cmaketrace

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

// A result may outlive its parse. Returning fewer records must not leave
// references to discarded payloads in that result's backing array. The fixture
// deliberately keeps indexes from discarded records, so clearing too early
// would change the public result and fail independently of the tail check.
func TestTraceReturnedRecordsClearDiscardedReferences(t *testing.T) {
	const payload = `{"version":{"major":1,"minor":0}}
{"file":"/fixture/b.cmake","line":1,"cmd":"set","args":["KEPT","first"]}
{"file":"/fixture/a.cmake","line":2,"cmd":"include","args":["included.cmake","discarded-include-payload"]}
{"file":"/fixture/b.cmake","line":3,"cmd":"MESSAGE","args":["message-payload"]}
{"file":"/fixture/b.cmake","line":4,"cmd":"set","args":["SECOND","discarded-second-set-payload"]}
{"file":"/fixture/a.cmake","line":5,"cmd":"add_subdirectory","args":["subdir","discarded-subdirectory-payload"]}
`
	inputRecords := []Record{
		{File: "/fixture/b.cmake", Line: 1, Cmd: "set", Args: []string{"KEPT", "first"}},
		{File: "/fixture/a.cmake", Line: 2, Cmd: "include", Args: []string{"included.cmake", "discarded-include-payload"}},
		{File: "/fixture/b.cmake", Line: 3, Cmd: "MESSAGE", Args: []string{"message-payload"}},
		{File: "/fixture/b.cmake", Line: 4, Cmd: "set", Args: []string{"SECOND", "discarded-second-set-payload"}},
		{File: "/fixture/a.cmake", Line: 5, Cmd: "add_subdirectory", Args: []string{"subdir", "discarded-subdirectory-payload"}},
	}
	for _, tc := range []struct {
		name      string
		file      string
		command   string
		max       int
		indices   []int
		truncated bool
	}{
		{name: "unchanged", indices: []int{0, 1, 2, 3, 4}},
		{name: "cap_only", max: 1, indices: []int{0}, truncated: true},
		{name: "file_filter", file: "/fixture/b.cmake", indices: []int{0, 2, 3}},
		{name: "command_filter", command: "message", indices: []int{2}},
		{name: "filter_and_cap", file: "/fixture/b.cmake", max: 1, indices: []int{0}, truncated: true},
		{name: "no_matches", command: "find_package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTrace(t, payload)
			got := Trace(context.Background(), Args{TracePath: path, File: tc.file, Command: tc.command, MaxRecords: tc.max}, DefaultDeps())
			wantRecords := make([]Record, 0, len(tc.indices))
			for _, index := range tc.indices {
				wantRecords = append(wantRecords, inputRecords[index])
			}
			want := Result{
				Status: evidence.StatusOK,
				IncludeChain: []IncludeChainEntry{
					{Kind: KindInclude, File: "/fixture/a.cmake", Line: 2, Argument: "included.cmake"},
					{Kind: KindAddSubdirectory, File: "/fixture/a.cmake", Line: 5, Argument: "subdir"},
				},
				Records: wantRecords,
				ExecutedLines: []FileLines{
					{File: "/fixture/a.cmake", Lines: []int{2, 5}},
					{File: "/fixture/b.cmake", Lines: []int{1, 3, 4}},
				},
				FilesInTrace:         []string{"/fixture/a.cmake", "/fixture/b.cmake"},
				VersionHeaderPresent: true,
				Truncated:            tc.truncated,
				Evidence:             evidence.Evidence{Paths: []string{path}},
			}
			if len(tc.indices) == 0 {
				want.Status = evidence.StatusUnknown
				want.Reason = ReasonNoRecordsMatched
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("trace result changed: got %#v, want %#v", got, want)
			}
			gotJSON, err := publicresult.MarshalIndent(got)
			if err != nil {
				t.Fatalf("encode actual result: %v", err)
			}
			wantJSON, err := publicresult.MarshalIndent(want)
			if err != nil {
				t.Fatalf("encode expected result: %v", err)
			}
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("public result changed:\ngot: %s\nwant: %s", gotJSON, wantJSON)
			}

			// The parser owns this backing array exclusively. Reslicing exposes
			// references the collector can still see beyond the published length.
			// This is a reachability regression, not a timing/RSS/heap-size test.
			backing := got.Records[:cap(got.Records)]
			for index := len(got.Records); index < len(backing); index++ {
				if !reflect.DeepEqual(backing[index], Record{}) {
					t.Errorf("discarded backing record %d retains references: %#v", index, backing[index])
				}
			}
		})
	}
}
