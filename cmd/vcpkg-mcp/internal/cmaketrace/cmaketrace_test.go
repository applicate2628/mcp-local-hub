package cmaketrace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
)

// wellFormedTrace is a fixture json-v1 trace covering: a version header, an
// include() call, an add_subdirectory() call, a command name that differs in
// case between two occurrences ("message" / "MESSAGE"), and a defer field on
// one record. All paths are fixture-only, never a real filesystem location.
const wellFormedTrace = `{"version":{"major":1,"minor":0}}
{"file":"/proj/CMakeLists.txt","line":3,"cmd":"cmake_minimum_required","args":["VERSION","3.20"],"time":0.001,"frame":1,"global_frame":1}
{"file":"/proj/CMakeLists.txt","line":5,"cmd":"project","args":["myproj"],"time":0.002,"frame":1,"global_frame":2}
{"file":"/proj/CMakeLists.txt","line":8,"cmd":"include","args":["/proj/cmake/Utils.cmake"],"time":0.003,"frame":1,"global_frame":3}
{"file":"/proj/cmake/Utils.cmake","line":1,"cmd":"message","args":["loading utils"],"time":0.004,"frame":2,"global_frame":4}
{"file":"/proj/CMakeLists.txt","line":10,"cmd":"add_subdirectory","args":["src"],"time":0.005,"frame":1,"global_frame":5}
{"file":"/proj/src/CMakeLists.txt","line":1,"cmd":"add_library","args":["mylib","a.cpp"],"time":0.006,"frame":2,"global_frame":6}
{"file":"/proj/src/CMakeLists.txt","line":4,"cmd":"MESSAGE","args":["building mylib"],"time":0.007,"frame":2,"global_frame":7,"defer":"cbID1"}
`

// writeTrace writes content to a file under t.TempDir() and returns its
// absolute path. Fixture-only — never touches a real vcpkg/cmake install.
func writeTrace(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "trace.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("writing fixture trace: %v", err)
	}
	return p
}

func defaultDeps() Deps {
	return Deps{FS: DefaultFS()}
}

func TestTrace_WellFormed_IncludeChainAndExecutedLines(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	res := Trace(Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
	}
	if !res.VersionHeaderPresent {
		t.Errorf("VersionHeaderPresent = false, want true")
	}
	if res.MalformedLineCount != 0 {
		t.Errorf("MalformedLineCount = %d, want 0", res.MalformedLineCount)
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false")
	}
	if len(res.Records) != 7 {
		t.Fatalf("len(Records) = %d, want 7", len(res.Records))
	}

	wantChain := []IncludeChainEntry{
		{Kind: KindInclude, File: "/proj/CMakeLists.txt", Line: 8, Argument: "/proj/cmake/Utils.cmake"},
		{Kind: KindAddSubdirectory, File: "/proj/CMakeLists.txt", Line: 10, Argument: "src"},
	}
	if !reflect.DeepEqual(res.IncludeChain, wantChain) {
		t.Errorf("IncludeChain = %+v, want %+v", res.IncludeChain, wantChain)
	}

	wantLines := []FileLines{
		{File: "/proj/CMakeLists.txt", Lines: []int{3, 5, 8, 10}},
		{File: "/proj/cmake/Utils.cmake", Lines: []int{1}},
		{File: "/proj/src/CMakeLists.txt", Lines: []int{1, 4}},
	}
	if !reflect.DeepEqual(res.ExecutedLines, wantLines) {
		t.Errorf("ExecutedLines = %+v, want %+v", res.ExecutedLines, wantLines)
	}

	wantFiles := []string{"/proj/CMakeLists.txt", "/proj/cmake/Utils.cmake", "/proj/src/CMakeLists.txt"}
	if !reflect.DeepEqual(res.FilesInTrace, wantFiles) {
		t.Errorf("FilesInTrace = %+v, want %+v", res.FilesInTrace, wantFiles)
	}

	if len(res.Evidence.Paths) == 0 || res.Evidence.Paths[0] != path {
		t.Errorf("Evidence.Paths = %+v, want first entry %q", res.Evidence.Paths, path)
	}

	// The deferred record round-trips its Defer flag as a presence bit.
	found := false
	for _, r := range res.Records {
		if r.File == "/proj/src/CMakeLists.txt" && r.Line == 4 {
			found = true
			if !r.Defer {
				t.Errorf("record at src/CMakeLists.txt:4 Defer = false, want true")
			}
		}
	}
	if !found {
		t.Fatalf("expected record at src/CMakeLists.txt:4 not found in Records")
	}
}

func TestTrace_NoVersionHeader_ParsedAnyway(t *testing.T) {
	content := `{"file":"/proj/CMakeLists.txt","line":1,"cmd":"project","args":["p"],"time":0.001,"frame":1,"global_frame":1}
{"file":"/proj/CMakeLists.txt","line":2,"cmd":"message","args":["hi"],"time":0.002,"frame":1,"global_frame":2}
`
	path := writeTrace(t, content)

	res := Trace(Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
	}
	if res.VersionHeaderPresent {
		t.Errorf("VersionHeaderPresent = true, want false (header omitted from this fixture)")
	}
	if res.MalformedLineCount != 0 {
		t.Errorf("MalformedLineCount = %d, want 0", res.MalformedLineCount)
	}
	if len(res.Records) != 2 {
		t.Errorf("len(Records) = %d, want 2", len(res.Records))
	}
}

func TestTrace_MalformedLinesCountedAndParseContinues(t *testing.T) {
	content := `{"version":{"major":1,"minor":0}}
{"file":"/proj/CMakeLists.txt","line":1,"cmd":"project","args":["p"],"time":0.001,"frame":1,"global_frame":1}
this is not json at all, a build got killed mid-line
{"file":"/proj/CMakeLists.txt","line":2,"cmd":"message","args":["hi"],"time":0.002,"frame":1,"global_frame":2}
{"no_cmd_field": true, "just": "some other json shape"}
{"file":"/proj/CMakeLists.txt","line":3,"cmd":"message","args":["bye"],"time":0.003,"frame":1,"global_frame":3}
`
	path := writeTrace(t, content)

	res := Trace(Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
	}
	if res.MalformedLineCount != 2 {
		t.Errorf("MalformedLineCount = %d, want 2", res.MalformedLineCount)
	}
	if len(res.Records) != 3 {
		t.Errorf("len(Records) = %d, want 3 (parse must continue past malformed lines)", len(res.Records))
	}
}

func TestTrace_EmptyFile(t *testing.T) {
	path := writeTrace(t, "")

	res := Trace(Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonTraceEmpty {
		t.Fatalf("Status/Reason = %v/%v, want unknown/trace_empty", res.Status, res.Reason)
	}
}

func TestTrace_NotJSONLines(t *testing.T) {
	content := `-- Configuring done
-- Generating done
CMake Error at CMakeLists.txt:10 (message):
  Something went wrong for reasons unrelated to json-v1 tracing
`
	path := writeTrace(t, content)

	res := Trace(Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonNotJSONLines {
		t.Fatalf("Status/Reason = %v/%v, want unknown/not_json_lines", res.Status, res.Reason)
	}
	if res.MalformedLineCount != 4 {
		t.Errorf("MalformedLineCount = %d, want 4", res.MalformedLineCount)
	}
}

func TestTrace_MissingPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.json")

	res := Trace(Args{TracePath: missing}, defaultDeps())

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonTraceNotFound {
		t.Fatalf("Status/Reason = %v/%v, want unknown/trace_not_found", res.Status, res.Reason)
	}
	if len(res.Evidence.Paths) == 0 || res.Evidence.Paths[0] != missing {
		t.Errorf("Evidence.Paths = %+v, want first entry %q", res.Evidence.Paths, missing)
	}
}

// fakeUnreadableFS simulates an I/O error that is NOT "not exist" (e.g. a
// permission error) so ReasonTraceUnreadable is exercised without relying on
// platform-specific file permission behavior.
type fakeUnreadableFS struct{}

func (fakeUnreadableFS) ReadFile(p string) ([]byte, error) {
	return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
}

func TestTrace_Unreadable(t *testing.T) {
	res := Trace(Args{TracePath: "irrelevant-fake-path.json"}, Deps{FS: fakeUnreadableFS{}})

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonTraceUnreadable {
		t.Fatalf("Status/Reason = %v/%v, want unknown/trace_unreadable", res.Status, res.Reason)
	}
}

func TestTrace_FileAndCommandFilters(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	t.Run("file filter narrows to one file", func(t *testing.T) {
		res := Trace(Args{TracePath: path, File: "/proj/cmake/Utils.cmake"}, defaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
		}
		if len(res.Records) != 1 || res.Records[0].File != "/proj/cmake/Utils.cmake" {
			t.Fatalf("Records = %+v, want exactly 1 record from /proj/cmake/Utils.cmake", res.Records)
		}
	})

	t.Run("command filter is case-insensitive across differing-case occurrences", func(t *testing.T) {
		res := Trace(Args{TracePath: path, Command: "message"}, defaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
		}
		if len(res.Records) != 2 {
			t.Fatalf("len(Records) = %d, want 2 (one \"message\", one \"MESSAGE\")", len(res.Records))
		}
		for _, r := range res.Records {
			if !strings.EqualFold(r.Cmd, "message") {
				t.Errorf("record cmd = %q, want case-insensitive match of \"message\"", r.Cmd)
			}
		}
	})

	t.Run("file and command combined", func(t *testing.T) {
		res := Trace(Args{TracePath: path, File: "/proj/src/CMakeLists.txt", Command: "MESSAGE"}, defaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
		}
		if len(res.Records) != 1 || res.Records[0].Line != 4 {
			t.Fatalf("Records = %+v, want exactly 1 record at line 4", res.Records)
		}
	})

	// IncludeChain and ExecutedLines are never narrowed by these filters --
	// they reflect the whole trace regardless.
	t.Run("include chain and executed lines stay whole-trace under a filter", func(t *testing.T) {
		res := Trace(Args{TracePath: path, File: "/proj/cmake/Utils.cmake"}, defaultDeps())
		if len(res.IncludeChain) != 2 {
			t.Errorf("len(IncludeChain) = %d, want 2 (unfiltered)", len(res.IncludeChain))
		}
		if len(res.FilesInTrace) != 3 {
			t.Errorf("len(FilesInTrace) = %d, want 3 (unfiltered)", len(res.FilesInTrace))
		}
	})
}

func TestTrace_MaxRecordsCapsAndSetsTruncated(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	res := Trace(Args{TracePath: path, MaxRecords: 3}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
	}
	if len(res.Records) != 3 {
		t.Fatalf("len(Records) = %d, want 3", len(res.Records))
	}
	if !res.Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestTrace_MaxRecordsZeroUsesDefaultAndDoesNotTruncateASmallTrace(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	res := Trace(Args{TracePath: path, MaxRecords: 0}, defaultDeps())

	if res.Truncated {
		t.Errorf("Truncated = true, want false (7 records is well under DefaultMaxRecords=%d)", DefaultMaxRecords)
	}
	if len(res.Records) != 7 {
		t.Errorf("len(Records) = %d, want 7", len(res.Records))
	}
}

func TestTrace_ArgsPreservedVerbatim(t *testing.T) {
	// The JSON text below encodes an empty-string argument and a
	// newline-embedding argument via the standard JSON \n escape -- exactly
	// what a real cmake json-v1 trace emits for a multi-line expanded value,
	// since a raw control byte is not legal inside a JSON string.
	content := `{"version":{"major":1,"minor":0}}
{"file":"/proj/CMakeLists.txt","line":20,"cmd":"message","args":["","line1\nline2"],"time":0.01,"frame":1,"global_frame":10}
`
	path := writeTrace(t, content)

	res := Trace(Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
	}
	if len(res.Records) != 1 {
		t.Fatalf("len(Records) = %d, want 1", len(res.Records))
	}
	want := []string{"", "line1\nline2"}
	if !reflect.DeepEqual(res.Records[0].Args, want) {
		t.Errorf("Args = %q, want %q", res.Records[0].Args, want)
	}
}

func TestTrace_FileNeverInTrace_AbsenceIsNotADeadBranchClaim(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	// Querying a file that never appears anywhere in the trace: Records
	// filtered to it is empty, so the tool reports no_records_matched --
	// but the caller must be able to tell THIS apart from "the file ran but
	// this particular line never executed" (checked below).
	t.Run("unseen file yields no_records_matched and is absent from FilesInTrace", func(t *testing.T) {
		res := Trace(Args{TracePath: path, File: "/proj/never/Seen.cmake"}, defaultDeps())
		if res.Status != evidence.StatusUnknown || res.Reason != ReasonNoRecordsMatched {
			t.Fatalf("Status/Reason = %v/%v, want unknown/no_records_matched", res.Status, res.Reason)
		}
		if len(res.Records) != 0 {
			t.Errorf("Records = %+v, want empty", res.Records)
		}
		for _, f := range res.FilesInTrace {
			if f == "/proj/never/Seen.cmake" {
				t.Fatalf("FilesInTrace unexpectedly contains the never-seen file: %+v", res.FilesInTrace)
			}
		}
		// Evidence and the whole-trace indices are still populated even on
		// an unknown verdict -- "always return evidence" (see lastfailure's
		// LogPaths precedent, mirrored here for IncludeChain/FilesInTrace).
		if len(res.IncludeChain) == 0 {
			t.Errorf("IncludeChain unexpectedly empty on an unknown verdict")
		}
	})

	// A file that DID appear in the trace, queried for a line that never
	// executed within it: the file itself IS in FilesInTrace, but the line
	// is absent from its ExecutedLines entry -- a structurally different
	// (and weaker) absence claim than the file-never-seen case above.
	t.Run("known file with an unobserved line is a distinct absence claim", func(t *testing.T) {
		res := Trace(Args{TracePath: path}, defaultDeps())
		var sawKnownFile bool
		for _, fl := range res.ExecutedLines {
			if fl.File != "/proj/CMakeLists.txt" {
				continue
			}
			sawKnownFile = true
			for _, l := range fl.Lines {
				if l == 999 {
					t.Fatalf("line 999 unexpectedly present in a fixture that never recorded it")
				}
			}
		}
		if !sawKnownFile {
			t.Fatalf("/proj/CMakeLists.txt missing from ExecutedLines entirely")
		}
		var fileKnown bool
		for _, f := range res.FilesInTrace {
			if f == "/proj/CMakeLists.txt" {
				fileKnown = true
			}
		}
		if !fileKnown {
			t.Fatalf("/proj/CMakeLists.txt missing from FilesInTrace")
		}
	})
}
