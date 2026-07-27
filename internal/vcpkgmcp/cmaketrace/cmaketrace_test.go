package cmaketrace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
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

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

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

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

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

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

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

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

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

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

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

	res := Trace(context.Background(), Args{TracePath: missing}, defaultDeps())

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

func (fakeUnreadableFS) Open(p string) (io.ReadCloser, error) {
	return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrPermission}
}

func TestTrace_Unreadable(t *testing.T) {
	res := Trace(context.Background(), Args{TracePath: "irrelevant-fake-path.json"}, Deps{FS: fakeUnreadableFS{}})

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonTraceUnreadable {
		t.Fatalf("Status/Reason = %v/%v, want unknown/trace_unreadable", res.Status, res.Reason)
	}
}

func TestTrace_FileAndCommandFilters(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	t.Run("file filter narrows to one file", func(t *testing.T) {
		res := Trace(context.Background(), Args{TracePath: path, File: "/proj/cmake/Utils.cmake"}, defaultDeps())
		if res.Status != evidence.StatusOK {
			t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
		}
		if len(res.Records) != 1 || res.Records[0].File != "/proj/cmake/Utils.cmake" {
			t.Fatalf("Records = %+v, want exactly 1 record from /proj/cmake/Utils.cmake", res.Records)
		}
	})

	t.Run("command filter is case-insensitive across differing-case occurrences", func(t *testing.T) {
		res := Trace(context.Background(), Args{TracePath: path, Command: "message"}, defaultDeps())
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
		res := Trace(context.Background(), Args{TracePath: path, File: "/proj/src/CMakeLists.txt", Command: "MESSAGE"}, defaultDeps())
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
		res := Trace(context.Background(), Args{TracePath: path, File: "/proj/cmake/Utils.cmake"}, defaultDeps())
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

	res := Trace(context.Background(), Args{TracePath: path, MaxRecords: 3}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status = %v, Reason = %v, want ok", res.Status, res.Reason)
	}
	if len(res.Records) != 3 {
		t.Fatalf("len(Records) = %d, want 3", len(res.Records))
	}
	if !res.Truncated {
		t.Errorf("Truncated = false, want true")
	}
	if res.InputIncomplete {
		t.Errorf("InputIncomplete = true, want false: MaxRecords capping is not malformed input")
	}
	if len(res.InputIncompleteReasons) != 0 {
		t.Errorf("InputIncompleteReasons = %v, want empty for a complete capped input", res.InputIncompleteReasons)
	}
}

func TestTrace_MaxRecordsZeroUsesDefaultAndDoesNotTruncateASmallTrace(t *testing.T) {
	path := writeTrace(t, wellFormedTrace)

	res := Trace(context.Background(), Args{TracePath: path, MaxRecords: 0}, defaultDeps())

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

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

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
		res := Trace(context.Background(), Args{TracePath: path, File: "/proj/never/Seen.cmake"}, defaultDeps())
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
	//
	// VACUOUS-TEST FIX (2026-07-27): the only assertion that distinguished this
	// subtest from its sibling was `if l == 999`, against a fixture whose lines
	// are {3,5,8,10}. No code path in parse.go can synthesise a line number
	// absent from the records, so that check was unfalsifiable, and the
	// remaining two checks are subsumed by the verbatim DeepEqual on
	// ExecutedLines/FilesInTrace asserted earlier in this file. The DISTINCTION
	// the subtest is named for — that the two absence claims are observably
	// different — was tested by nothing.
	//
	// It now asserts that distinction directly, by running both queries and
	// comparing their shapes. Collapsing the two (e.g. dropping FilesInTrace,
	// or listing every queried file in it) fails this.
	t.Run("known file with an unobserved line is a distinct absence claim", func(t *testing.T) {
		const known, unseen = "/proj/CMakeLists.txt", "/proj/never/Seen.cmake"

		res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

		inFiles := func(want string) bool {
			for _, f := range res.FilesInTrace {
				if f == want {
					return true
				}
			}
			return false
		}
		linesFor := func(want string) ([]int, bool) {
			for _, fl := range res.ExecutedLines {
				if fl.File == want {
					return fl.Lines, true
				}
			}
			return nil, false
		}

		// Claim A (file never processed): absent from FilesInTrace entirely,
		// so NOTHING about any line inside it is knowable.
		if inFiles(unseen) {
			t.Fatalf("a file that never appears in the trace is listed in FilesInTrace %v — the caller can no "+
				"longer tell \"never processed\" from \"processed, line not observed\"", res.FilesInTrace)
		}
		if _, ok := linesFor(unseen); ok {
			t.Fatalf("a never-seen file has an ExecutedLines entry; positive evidence was invented for it")
		}

		// Claim B (file processed, line not observed): PRESENT in FilesInTrace,
		// with an exact observed-line set. Pinning the set verbatim is what
		// makes "line N was not observed" a checked consequence rather than an
		// unfalsifiable guess about one arbitrary number.
		if !inFiles(known) {
			t.Fatalf("%s is missing from FilesInTrace %v, so its lines cannot be interpreted at all", known, res.FilesInTrace)
		}
		lines, ok := linesFor(known)
		if !ok {
			t.Fatalf("%s missing from ExecutedLines entirely", known)
		}
		wantLines := []int{3, 5, 8, 10}
		if !reflect.DeepEqual(lines, wantLines) {
			t.Fatalf("ExecutedLines[%s] = %v, want exactly %v — the absence of any other line is only a real claim "+
				"if the observed set itself is pinned", known, lines, wantLines)
		}
	})
}

func TestTrace_MidRecordInputIsIndependentlyMarkedIncomplete(t *testing.T) {
	content := `{"version":{"major":1,"minor":0}}
{"file":"/proj/CMakeLists.txt","line":1,"cmd":"project","args":["p"]}
{"file":"/proj/CMakeLists.txt","line":2,"cmd":"message","args":["cut off"]
`
	path := writeTrace(t, content)

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status/Reason = %v/%v, want ok with positive evidence retained", res.Status, res.Reason)
	}
	if !res.InputIncomplete {
		t.Fatalf("InputIncomplete = false, want true for a trace ending mid-record")
	}
	if !reflect.DeepEqual(res.InputIncompleteReasons, []Reason{ReasonInputMalformed}) {
		t.Errorf("InputIncompleteReasons = %v, want [%q]", res.InputIncompleteReasons, ReasonInputMalformed)
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false: input incompleteness is independent of the MaxRecords cap")
	}
	if res.MalformedLineCount != 1 {
		t.Errorf("MalformedLineCount = %d, want 1", res.MalformedLineCount)
	}

	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshaling Result: %v", err)
	}
	if !strings.Contains(string(wire), `"input_incomplete":true`) || !strings.Contains(string(wire), `"input_incomplete_reasons":["input_malformed"]`) {
		t.Errorf("wire Result = %s, want explicit input incompleteness disclosure", wire)
	}
}

func TestTrace_InvalidMandatoryRecordFieldsAreMalformedNotEvidence(t *testing.T) {
	content := `{"version":{"major":1,"minor":0}}
{"file":"/proj/CMakeLists.txt","line":7,"cmd":"project","args":["p"]}
{"cmd":"message","args":["missing file and line"]}
{"line":8,"cmd":"message","args":["missing file"]}
{"file":"/proj/CMakeLists.txt","cmd":"message","args":["missing line"]}
{"file":"/proj/CMakeLists.txt","line":0,"cmd":"message","args":["zero line"]}
{"file":"/proj/CMakeLists.txt","line":9}
`
	path := writeTrace(t, content)

	res := Trace(context.Background(), Args{TracePath: path}, defaultDeps())

	if res.Status != evidence.StatusOK {
		t.Fatalf("Status/Reason = %v/%v, want ok with the one valid record retained", res.Status, res.Reason)
	}
	if res.MalformedLineCount != 5 {
		t.Errorf("MalformedLineCount = %d, want 5 invalid command records", res.MalformedLineCount)
	}
	if !res.InputIncomplete {
		t.Errorf("InputIncomplete = false, want true when invalid records make absence conclusions unsupported")
	}
	if len(res.Records) != 1 || res.Records[0].Line != 7 {
		t.Fatalf("Records = %+v, want only the valid record at line 7", res.Records)
	}
	if !reflect.DeepEqual(res.FilesInTrace, []string{"/proj/CMakeLists.txt"}) {
		t.Errorf("FilesInTrace = %+v, want only the valid record's file", res.FilesInTrace)
	}
	if !reflect.DeepEqual(res.ExecutedLines, []FileLines{{File: "/proj/CMakeLists.txt", Lines: []int{7}}}) {
		t.Errorf("ExecutedLines = %+v, want no manufactured line 0 evidence", res.ExecutedLines)
	}
}

// =====================================================================
// Pre-submission cross-family review, round 2 (F26).
// =====================================================================

// countingReadCloser reports how many bytes were actually pulled off the
// underlying reader, which is how "did we stream or did we materialize the
// whole thing" is OBSERVED rather than assumed.
type countingReadCloser struct {
	r      io.Reader
	read   int64
	closed bool
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error { c.closed = true; return nil }

type streamFS struct {
	content string
	last    *countingReadCloser
}

func (s *streamFS) Open(string) (io.ReadCloser, error) {
	s.last = &countingReadCloser{r: strings.NewReader(s.content)}
	return s.last, nil
}

// cancelAfterReader cancels the request PART WAY THROUGH the stream, which
// is the only way to exercise the in-loop cancellation check: a context
// already canceled at entry is caught by the cheap guard before the file is
// even opened, so it proves nothing about the parse loop.
type cancelAfterReader struct {
	r       io.Reader
	cancel  context.CancelFunc
	after   int64
	read    int64
	closed  bool
	tripped bool
}

func (c *cancelAfterReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if !c.tripped && c.read >= c.after {
		c.tripped = true
		c.cancel()
	}
	return n, err
}

func (c *cancelAfterReader) Close() error { c.closed = true; return nil }

// F26: cancellation observed DURING the parse stops the read mid-stream and
// fails closed. Without the in-loop check, the whole trace is parsed and
// indexed for a caller that has already gone away.
func TestF26_CancellationDuringTheParseStopsReadingMidStream(t *testing.T) {
	var big strings.Builder
	big.WriteString("{\"version\":{\"major\":1,\"minor\":0}}\n")
	for i := 1; i <= 200000; i++ {
		fmt.Fprintf(&big, "{\"file\":\"/p/CMakeLists.txt\",\"line\":%d,\"cmd\":\"message\",\"args\":[\"x\"]}\n", i)
	}
	content := big.String()

	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelAfterReader{r: strings.NewReader(content), cancel: cancel, after: 4096}
	deps := Deps{FS: fsReturning(reader)}

	res := Trace(ctx, Args{TracePath: "irrelevant"}, deps)

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonCanceled {
		t.Fatalf("status=%v reason=%v, want unknown/canceled", res.Status, res.Reason)
	}
	if len(res.Records) != 0 {
		t.Fatalf("a canceled parse returned %d records; it must fail closed", len(res.Records))
	}
	// The load-bearing assertion: we stopped EARLY. Reading the whole file
	// and only then noticing the cancellation is the defect being fixed.
	if reader.read > int64(len(content))/4 {
		t.Fatalf("read %d of %d bytes after cancellation fired at %d — the parse loop is not observing "+
			"the context", reader.read, len(content), reader.after)
	}
	if !reader.closed {
		t.Fatal("the trace reader was not closed on the cancellation path")
	}
}

// fsReturning hands out one prepared reader, so a test can observe exactly
// how much of the stream the parser consumed.
func fsReturning(rc io.ReadCloser) FS { return &singleReaderFS{rc: rc} }

type singleReaderFS struct{ rc io.ReadCloser }

func (s *singleReaderFS) Open(string) (io.ReadCloser, error) { return s.rc, nil }

// F26: a request canceled BEFORE the call fails closed without reading at all.
//
// VACUOUS-TEST FIX (2026-07-27): the read-volume and reader-closed assertions
// used to be guarded by `if fs.last != nil && ...`. Trace returns at its
// ctx.Err() check BEFORE deps.FS.Open is ever called, so fs.last is ALWAYS nil
// and both bodies were unreachable — the sibling test's own comment above
// states that fact ("a context already canceled at entry is caught by the cheap
// guard before the file is even opened"), yet the dead guards remained, along
// with a 200 000-line fixture built solely to feed them.
//
// "StopsReading" is now asserted as the fact it actually is: Open was never
// called. That is falsifiable — moving the cancellation check below the Open
// fails it — and it is a STRONGER claim than a read-volume bound.
func TestF26_CanceledRequestFailsClosedAndStopsReading(t *testing.T) {
	// Small on purpose: the content must never be read, and pinning that is
	// the point of the test.
	fs := &streamFS{content: "{\"version\":{\"major\":1,\"minor\":0}}\n" +
		"{\"file\":\"/p/CMakeLists.txt\",\"line\":1,\"cmd\":\"message\",\"args\":[\"x\"]}\n"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := Trace(ctx, Args{TracePath: "irrelevant"}, Deps{FS: fs})

	if res.Status != evidence.StatusUnknown || res.Reason != ReasonCanceled {
		t.Fatalf("status=%v reason=%v, want unknown/canceled", res.Status, res.Reason)
	}
	if len(res.Records) != 0 || len(res.ExecutedLines) != 0 || len(res.IncludeChain) != 0 {
		t.Fatalf("a canceled parse returned partial data (records=%d executed=%d chain=%d); it must fail closed",
			len(res.Records), len(res.ExecutedLines), len(res.IncludeChain))
	}
	if fs.last != nil {
		t.Fatalf("the trace file was OPENED (%d bytes read) under an already-canceled context — a caller that has "+
			"gone away must cost no I/O at all, and an opened handle is also one more thing to leak", fs.last.read)
	}
}

// F26: the reader is closed on EVERY path, including the success path.
func TestF26_TraceReaderIsClosedOnTheSuccessPath(t *testing.T) {
	fs := &streamFS{content: wellFormedTrace}

	res := Trace(context.Background(), Args{TracePath: "irrelevant"}, Deps{FS: fs})

	if res.Status != evidence.StatusOK {
		t.Fatalf("status=%v reason=%v, want ok", res.Status, res.Reason)
	}
	if !fs.last.closed {
		t.Fatal("the trace reader was not closed on the success path")
	}
}

// F26: an absurdly long line is refused, and the refusal is REPORTED through
// the single incompleteness channel rather than silently dropped.
func TestF26_OverlongLineIsRefusedAndReportedAsIncomplete(t *testing.T) {
	content := "{\"version\":{\"major\":1,\"minor\":0}}\n" +
		"{\"file\":\"/p/CMakeLists.txt\",\"line\":1,\"cmd\":\"project\",\"args\":[\"p\"]}\n" +
		"{\"file\":\"/p/x.cmake\",\"line\":1,\"cmd\":\"message\",\"args\":[\"" + strings.Repeat("z", MaxLineBytes+1) + "\"]}\n"
	fs := &streamFS{content: content}

	res := Trace(context.Background(), Args{TracePath: "irrelevant"}, Deps{FS: fs})

	if res.Status != evidence.StatusOK {
		t.Fatalf("status=%v reason=%v, want ok — the valid records remain positive evidence", res.Status, res.Reason)
	}
	if !res.InputIncomplete {
		t.Fatal("InputIncomplete = false; an over-ceiling line means the input was not fully read")
	}
	var sawLineLimit bool
	for _, r := range res.InputIncompleteReasons {
		if r == ReasonLineLimit {
			sawLineLimit = true
		}
	}
	if !sawLineLimit {
		t.Fatalf("InputIncompleteReasons = %v, want it to include %q", res.InputIncompleteReasons, ReasonLineLimit)
	}
	// Parsing must stay in SYNC: the valid record before the monster line is
	// still present, proving the oversized line was drained to its newline
	// rather than leaving the reader mid-record.
	if len(res.Records) != 1 {
		t.Fatalf("records = %d, want 1 (the valid record preceding the oversized line)", len(res.Records))
	}
}

// F26: independent incompleteness causes coexist and are ALL reported. One
// scalar reason field could only ever have shown the first.
func TestF26_MultipleIncompletenessCausesAreAllReported(t *testing.T) {
	content := "{\"version\":{\"major\":1,\"minor\":0}}\n" +
		"this line is not json at all\n" +
		"{\"file\":\"/p/CMakeLists.txt\",\"line\":1,\"cmd\":\"project\",\"args\":[\"p\"]}\n" +
		"{\"file\":\"/p/x.cmake\",\"line\":1,\"cmd\":\"message\",\"args\":[\"" + strings.Repeat("z", MaxLineBytes+1) + "\"]}\n"
	fs := &streamFS{content: content}

	res := Trace(context.Background(), Args{TracePath: "irrelevant"}, Deps{FS: fs})

	want := map[Reason]bool{ReasonInputMalformed: false, ReasonLineLimit: false}
	for _, r := range res.InputIncompleteReasons {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for reason, seen := range want {
		if !seen {
			t.Fatalf("InputIncompleteReasons = %v, missing %q — every independent cause must be reported",
				res.InputIncompleteReasons, reason)
		}
	}
}

// F26: an empty trace is still distinguished from one whose every line was
// malformed, without buffering the file to TrimSpace it whole.
func TestF26_EmptyAndWhitespaceOnlyTracesStillReportTraceEmpty(t *testing.T) {
	for _, content := range []string{"", "   \n\t\n  \n"} {
		res := Trace(context.Background(), Args{TracePath: "irrelevant"}, Deps{FS: &streamFS{content: content}})
		if res.Status != evidence.StatusUnknown || res.Reason != ReasonTraceEmpty {
			t.Fatalf("content %q -> status=%v reason=%v, want unknown/trace_empty", content, res.Status, res.Reason)
		}
	}
}

// F26: the record ceiling bounds parse memory, and because IncludeChain /
// ExecutedLines / FilesInTrace are all DERIVED from the record set, proving
// this one ceiling binds proves the index and response ceilings bind too.
// The limit is injected so the bound is actually EXERCISED rather than
// declared and never reached.
func TestF26_RecordCeilingBoundsParseIndexAndResponseTogether(t *testing.T) {
	var content strings.Builder
	content.WriteString("{\"version\":{\"major\":1,\"minor\":0}}\n")
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&content, "{\"file\":\"/p/f%d.cmake\",\"line\":%d,\"cmd\":\"include\",\"args\":[\"/p/x.cmake\"]}\n", i, i)
	}

	deps := Deps{FS: &streamFS{content: content.String()}, Limits: Limits{MaxParsedRecords: 5}}
	res := Trace(context.Background(), Args{TracePath: "irrelevant"}, deps)

	if res.Status != evidence.StatusOK {
		t.Fatalf("status=%v reason=%v, want ok — records read before the ceiling stay positive evidence", res.Status, res.Reason)
	}
	if len(res.Records) != 5 {
		t.Fatalf("records = %d, want 5 (the injected MaxParsedRecords)", len(res.Records))
	}
	// The derived indexes are bounded by the same ceiling, which is the
	// whole reason one knob is enough.
	if len(res.ExecutedLines) != 5 || len(res.FilesInTrace) != 5 || len(res.IncludeChain) != 5 {
		t.Fatalf("derived indexes not bounded by the record ceiling: executed=%d files=%d chain=%d, want 5 each",
			len(res.ExecutedLines), len(res.FilesInTrace), len(res.IncludeChain))
	}
	var sawRecordLimit bool
	for _, r := range res.InputIncompleteReasons {
		if r == ReasonRecordLimit {
			sawRecordLimit = true
		}
	}
	if !sawRecordLimit {
		t.Fatalf("InputIncompleteReasons = %v, want it to include %q — a bounded parse must not look complete",
			res.InputIncompleteReasons, ReasonRecordLimit)
	}
}

// F26: the whole-file byte ceiling stops the read and is reported.
func TestF26_ByteCeilingStopsTheReadAndIsReported(t *testing.T) {
	var content strings.Builder
	content.WriteString("{\"version\":{\"major\":1,\"minor\":0}}\n")
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&content, "{\"file\":\"/p/CMakeLists.txt\",\"line\":%d,\"cmd\":\"message\",\"args\":[\"x\"]}\n", i)
	}
	full := content.String()

	fs := &streamFS{content: full}
	deps := Deps{FS: fs, Limits: Limits{MaxTraceBytes: 1024}}
	res := Trace(context.Background(), Args{TracePath: "irrelevant"}, deps)

	var sawByteLimit bool
	for _, r := range res.InputIncompleteReasons {
		if r == ReasonByteLimit {
			sawByteLimit = true
		}
	}
	if !sawByteLimit {
		t.Fatalf("InputIncompleteReasons = %v, want it to include %q", res.InputIncompleteReasons, ReasonByteLimit)
	}
	if fs.last.read >= int64(len(full)) {
		t.Fatalf("read %d of %d bytes — the byte ceiling did not stop the read", fs.last.read, len(full))
	}
	if len(res.Records) == 0 {
		t.Fatal("records read before the ceiling must be retained as positive evidence")
	}
}

// oversizedLineGenerator streams `remaining` bytes of 'a' followed by a single
// newline, then EOF. It fills the caller's buffer in place and allocates
// NOTHING itself, so every byte the parse is charged with in the test below is
// the parser's own retention, not the fixture's.
type oversizedLineGenerator struct {
	remaining int64
	wroteNL   bool
}

func (g *oversizedLineGenerator) Read(p []byte) (int, error) {
	if g.remaining > 0 {
		n := int64(len(p))
		if n > g.remaining {
			n = g.remaining
		}
		for i := int64(0); i < n; i++ {
			p[i] = 'a'
		}
		g.remaining -= n
		return int(n), nil
	}
	if !g.wroteNL {
		g.wroteNL = true
		p[0] = '\n'
		return 1, nil
	}
	return 0, io.EOF
}

// PR #591 P1 (parse.go): MaxLineBytes must bound the READ, not merely classify
// a line that was already materialized. bufio.Reader.ReadString — what readLine
// used to call — appends until it finds the delimiter, so a single 64 MiB line
// was fully allocated (twice: the collected fragments plus the joined string)
// and only THEN compared against a 1 KiB cap. The ceiling could report the
// overflow but could not bound the memory it exists to bound, which is exactly
// the DoS an attacker-supplied trace file gets for free.
//
// TotalAlloc is cumulative and GC-independent, so this measures allocation
// CHURN rather than live heap: the pre-fix implementation must charge at least
// the line length, the fixed post-fix implementation charges roughly one
// reader buffer. The budget sits two orders of magnitude below the former and
// two above the latter, so it is decisive without being flaky.
func TestF26_LineCeilingBoundsTheReadNotJustTheClassification(t *testing.T) {
	const lineBytes = 64 << 20 // one 64 MiB line, no newline until the very end
	const allocBudget = 8 << 20
	lim := Limits{MaxTraceBytes: 1 << 30, MaxLineBytes: 1 << 10, MaxParsedRecords: 10}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	res, err := parseTraceStream(context.Background(), &oversizedLineGenerator{remaining: lineBytes}, lim)
	if err != nil {
		t.Fatalf("parseTraceStream: unexpected error %v", err)
	}
	runtime.ReadMemStats(&after)

	// Behaviour must be unchanged: the oversized line is still drained, still
	// counted as a ceiling trip, and still counted as unusable input.
	if !res.hitLineLimit {
		t.Fatalf("hitLineLimit = false, want true — the oversized line must still be REPORTED, not silently dropped")
	}
	if res.malformedCount != 1 {
		t.Fatalf("malformedCount = %d, want 1", res.malformedCount)
	}
	if len(res.records) != 0 {
		t.Fatalf("records = %d, want 0 — an over-cap line is never parsed", len(res.records))
	}

	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > allocBudget {
		t.Fatalf("parsing ONE %d-byte line allocated %d bytes, over the %d-byte budget: MaxLineBytes (%d) is bounding the classification but not the READ — the line is being materialized before the cap is consulted",
			lineBytes, allocated, allocBudget, lim.MaxLineBytes)
	}
}
