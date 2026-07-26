// Package cmaketrace implements vcpkg_cmake_trace, a PARSER over a CMake
// configure trace the operator already produced with
// `cmake --trace-expand --trace-format=json-v1 ...`. It answers three
// questions from work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md:
//
//  1. Which lines executed (and, by omission, which did not — the honest
//     answer to "why didn't my if() fire").
//  2. What did a variable expand to at a given point (the trace's own
//     EXPANDED argument strings are the evidence).
//  3. Which include()/add_subdirectory() calls fired, and in what order.
//
// # Scope: read-only over an EXISTING trace
//
// Running a fresh cmake configure is a MUTATION and is explicitly OUT OF
// SCOPE for this tool. It never shells out to cmake, and never offers to
// generate a missing trace — a missing trace_path is reported as
// unknown(trace_not_found) with the path that was looked for, full stop.
// This is the dynamic counterpart to the STATIC cmakewrap.RunGraph resolver
// (tool cmake_include_graph): that tool computes which include() edges are
// reachable without ever running cmake, and explicitly flags that whether an
// edge executes at configure time is "a separate, genuinely unknown question
// requiring a real cmake trace" — this package IS that follow-up question.
//
// # Input format (json-v1)
//
// `--trace-format=json-v1` emits ONE JSON object PER LINE (JSON Lines, not a
// JSON array). The first line is normally a version header of the shape
// {"version":{"major":1,"minor":0}}; every subsequent line is a command
// record carrying at least file, line, cmd, args (the EXPANDED argument
// strings), time, frame, global_frame, and sometimes defer. Parsing is
// defensive by design: a truncated trace (the normal case for a build that
// was killed mid-configure) or a missing header (a concatenated/trimmed
// file) must never abort the whole parse — see parse.go.
//
// # Honesty requirements — the point of this tool
//
// Executed lines are POSITIVE evidence only. "Line N is not in the executed
// set" means "not observed in THIS trace", which is NOT the same claim as
// "unreachable": the trace input may be incomplete (see
// Result.InputIncomplete), the returned records may be capped (see
// Result.Truncated), or the file containing line N may never have been processed at all in this run
// (see Result.FilesInTrace — a caller can check whether a file appears in
// the trace at all before drawing any conclusion about a line inside it).
// This package never itself labels a line "dead"; it only ever returns
// positive observations plus the qualifiers needed to interpret them
// honestly.
package cmaketrace

import (
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Reason is populated when Status == evidence.StatusUnknown and, for
// ReasonInputMalformed, when Result.InputIncomplete is true. Closed enum.
type Reason string

const (
	// ReasonTraceNotFound: trace_path does not exist. Never silently
	// offered to generate one — running cmake is a mutation, out of scope.
	ReasonTraceNotFound Reason = "trace_not_found"
	// ReasonTraceUnreadable: trace_path exists but could not be read (an
	// I/O error other than not-found — permissions, a directory instead of
	// a file, etc).
	ReasonTraceUnreadable Reason = "trace_unreadable"
	// ReasonTraceEmpty: trace_path exists and is readable but contains no
	// bytes (or only whitespace).
	ReasonTraceEmpty Reason = "trace_empty"
	// ReasonNotJSONLines: the file exists and is non-empty, but NOT ONE
	// line parses as a recognized json-v1 record or version header — e.g.
	// the operator passed a plain configure log instead of a
	// --trace-format=json-v1 trace.
	ReasonNotJSONLines Reason = "not_json_lines"
	// ReasonNoRecordsMatched: the trace parsed (the header and/or at least
	// one record was recognized), but after applying the File/Command
	// filters — or because the trace genuinely carries zero command
	// records — nothing remains in Records to report.
	ReasonNoRecordsMatched Reason = "no_records_matched"
	// ReasonInputMalformed: one or more non-blank lines were not valid
	// json-v1 command records. The valid records remain positive evidence,
	// but the input is incomplete so no absence conclusion is supported.
	ReasonInputMalformed Reason = "input_malformed"
)

// Status aliases evidence.Status so callers of this package do not need a
// second import just to read Result.Status.
type Status = evidence.Status

// Kind is the closed set of calls tracked in the include chain.
type Kind string

const (
	KindInclude         Kind = "include"
	KindAddSubdirectory Kind = "add_subdirectory"
)

// DefaultMaxRecords is the sane cap applied when Args.MaxRecords <= 0.
const DefaultMaxRecords = 1000

// Args is the vcpkg_cmake_trace tool's input contract.
type Args struct {
	// TracePath is required: an absolute path to a json-v1 trace file
	// ALREADY produced by `cmake --trace-expand --trace-format=json-v1`.
	// Never auto-discovered, never generated by this tool.
	TracePath string `json:"trace_path"`
	// File optionally narrows Records to only those recorded from this
	// exact CMake file path (matched verbatim — the trace's own file
	// strings are whatever cmake wrote; no path normalization is applied).
	File string `json:"file,omitempty"`
	// Command optionally narrows Records to only this cmd, matched
	// case-insensitively (CMake commands are themselves case-insensitive).
	Command string `json:"command,omitempty"`
	// MaxRecords bounds the returned Records slice. 0 uses DefaultMaxRecords.
	MaxRecords int `json:"max_records,omitempty"`
}

// Record is one executed command from the trace, after the File/Command
// filters (see Result.Records).
type Record struct {
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Cmd         string   `json:"cmd"`
	Args        []string `json:"args"`
	Time        float64  `json:"time,omitempty"`
	Frame       int      `json:"frame,omitempty"`
	GlobalFrame int      `json:"global_frame,omitempty"`
	// Defer reports whether the trace's own record carried a non-null
	// "defer" field (cmake_language(DEFER ...)) — presence only; the
	// field's own shape (an id string in observed traces) is not otherwise
	// interpreted by this tool.
	Defer bool `json:"defer,omitempty"`
}

// IncludeChainEntry is one include()/add_subdirectory() call, in the order
// it executed. Always computed from the WHOLE trace (never narrowed by
// Args.File/Command) — it answers a structural "what order did files get
// processed" question that filtering to a single file's calls would
// misrepresent.
type IncludeChainEntry struct {
	Kind Kind   `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
	// Argument is the first expanded argument — the included path / added
	// subdirectory — verbatim as the trace recorded it.
	Argument string `json:"argument"`
}

// FileLines is the set of line numbers this trace shows executing within
// one CMake file. See package doc "Honesty requirements": presence of a
// line here is POSITIVE evidence only.
type FileLines struct {
	File  string `json:"file"`
	Lines []int  `json:"lines"`
}

// Result is the vcpkg_cmake_trace tool's output contract.
type Result struct {
	Status Status `json:"status"`
	Reason Reason `json:"reason,omitempty"`

	// IncludeChain is every include()/add_subdirectory() call in the WHOLE
	// trace, in execution order — never narrowed by Args.File/Command.
	IncludeChain []IncludeChainEntry `json:"include_chain,omitempty"`
	// Records is the executed-command records matching Args.File /
	// Args.Command, in trace order, capped at MaxRecords (see Truncated).
	Records []Record `json:"records,omitempty"`
	// ExecutedLines is the per-file line-number index over the WHOLE trace
	// — never narrowed by Args.File/Command, so a caller always has the
	// full honest picture regardless of what Records was filtered to.
	ExecutedLines []FileLines `json:"executed_lines,omitempty"`
	// FilesInTrace lists every distinct CMake file that appears in ANY
	// record of the trace (never filtered). Lets a caller distinguish "this
	// file was never processed at all" (absent here) from "this file ran
	// but line N was never observed" (present here, line missing from its
	// ExecutedLines entry) — see package doc "Honesty requirements".
	FilesInTrace []string `json:"files_in_trace,omitempty"`

	// MalformedLineCount counts lines that were neither a recognized
	// version header nor a valid command record. A trace truncated by a
	// killed build is the NORMAL case, not an exception — parsing always
	// continues past a malformed line.
	MalformedLineCount int `json:"malformed_line_count"`
	// InputIncomplete reports that malformed input was observed. When true,
	// records and indexes remain positive evidence only; no absence
	// conclusion from this result is supported. It is independent from
	// Truncated, which describes only the returned Records cap.
	InputIncomplete bool `json:"input_incomplete"`
	// InputIncompleteReason explains why InputIncomplete is true. It is not
	// Result.Reason because valid records can still produce an ok status.
	InputIncompleteReason Reason `json:"input_incomplete_reason,omitempty"`
	// VersionHeaderPresent reports whether a {"version":{...}} header line
	// was found anywhere in the trace. A concatenated or trimmed trace file
	// may lack one; that is reported here, never treated as a parse failure.
	VersionHeaderPresent bool `json:"version_header_present"`
	// Truncated is true when MaxRecords capped Records. When true, no
	// absence claim drawn from this result is sound (see package doc).
	Truncated bool `json:"truncated"`

	Evidence evidence.Evidence `json:"evidence"`
}

// FS abstracts the one filesystem call this package needs, so tests exercise
// t.TempDir() fixtures without ever touching a real vcpkg/cmake install.
type FS interface {
	ReadFile(path string) ([]byte, error)
}

type osFS struct{}

func (osFS) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// DefaultFS wires FS to the real OS.
func DefaultFS() FS { return osFS{} }

// Deps bounds every ambient input Trace reads.
type Deps struct {
	FS FS
}

// DefaultDeps wires Deps to the real OS.
func DefaultDeps() Deps {
	return Deps{FS: DefaultFS()}
}

// Trace implements vcpkg_cmake_trace. See package doc for the read-only
// scope and the honesty invariants every field on Result exists to serve.
func Trace(args Args, deps Deps) Result {
	var ev evidence.Evidence
	ev.AddPath(args.TracePath)

	data, err := deps.FS.ReadFile(args.TracePath)
	if err != nil {
		reason := ReasonTraceUnreadable
		if errors.Is(err, fs.ErrNotExist) {
			reason = ReasonTraceNotFound
		}
		return Result{Status: evidence.StatusUnknown, Reason: reason, Evidence: ev}
	}

	if strings.TrimSpace(string(data)) == "" {
		return Result{Status: evidence.StatusUnknown, Reason: ReasonTraceEmpty, Evidence: ev}
	}

	parsed := parseTraceLines(data)

	if !parsed.versionHeaderPresent && len(parsed.records) == 0 {
		// Every non-blank line failed to parse as EITHER a header or a
		// command record — not a json-v1 trace at all (e.g. a plain
		// configure log passed in by mistake).
		return Result{
			Status:                evidence.StatusUnknown,
			Reason:                ReasonNotJSONLines,
			MalformedLineCount:    parsed.malformedCount,
			InputIncomplete:       parsed.malformedCount > 0,
			InputIncompleteReason: incompleteReason(parsed.malformedCount),
			Evidence:              ev,
		}
	}

	includeChain := buildIncludeChain(parsed.records)
	executedLines, filesInTrace := buildLineIndex(parsed.records)
	filtered := filterRecords(parsed.records, args.File, args.Command)

	maxRecords := args.MaxRecords
	if maxRecords <= 0 {
		maxRecords = DefaultMaxRecords
	}
	records := filtered
	truncated := false
	if len(records) > maxRecords {
		records = records[:maxRecords]
		truncated = true
	}

	base := Result{
		IncludeChain:          includeChain,
		Records:               records,
		ExecutedLines:         executedLines,
		FilesInTrace:          filesInTrace,
		MalformedLineCount:    parsed.malformedCount,
		InputIncomplete:       parsed.malformedCount > 0,
		InputIncompleteReason: incompleteReason(parsed.malformedCount),
		VersionHeaderPresent:  parsed.versionHeaderPresent,
		Truncated:             truncated,
		Evidence:              ev,
	}

	if len(filtered) == 0 {
		base.Status = evidence.StatusUnknown
		base.Reason = ReasonNoRecordsMatched
		return base
	}

	base.Status = evidence.StatusOK
	return base
}

func incompleteReason(malformedCount int) Reason {
	if malformedCount == 0 {
		return ""
	}
	return ReasonInputMalformed
}

// filterRecords narrows records to those matching file (exact match) and
// command (case-insensitive), each applied only when non-empty.
func filterRecords(records []Record, file, command string) []Record {
	if file == "" && command == "" {
		return records
	}
	var out []Record
	for _, r := range records {
		if file != "" && r.File != file {
			continue
		}
		if command != "" && !strings.EqualFold(r.Cmd, command) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// buildIncludeChain extracts every include()/add_subdirectory() record, in
// trace order. Case-insensitive match on Cmd (CMake commands are themselves
// case-insensitive); Kind is always normalized to the closed lower-case form.
func buildIncludeChain(records []Record) []IncludeChainEntry {
	var out []IncludeChainEntry
	for _, r := range records {
		var kind Kind
		switch {
		case strings.EqualFold(r.Cmd, string(KindInclude)):
			kind = KindInclude
		case strings.EqualFold(r.Cmd, string(KindAddSubdirectory)):
			kind = KindAddSubdirectory
		default:
			continue
		}
		arg := ""
		if len(r.Args) > 0 {
			arg = r.Args[0]
		}
		out = append(out, IncludeChainEntry{Kind: kind, File: r.File, Line: r.Line, Argument: arg})
	}
	return out
}

// buildLineIndex groups every record's (file, line) pair into a sorted,
// deduplicated per-file line index, plus the sorted list of distinct files
// seen. Both are computed from the WHOLE (unfiltered) record set — see
// Result.ExecutedLines / Result.FilesInTrace doc comments.
func buildLineIndex(records []Record) ([]FileLines, []string) {
	byFile := map[string]map[int]bool{}
	for _, r := range records {
		if r.File == "" {
			continue
		}
		if byFile[r.File] == nil {
			byFile[r.File] = map[int]bool{}
		}
		byFile[r.File][r.Line] = true
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	out := make([]FileLines, 0, len(files))
	for _, f := range files {
		lineSet := byFile[f]
		lines := make([]int, 0, len(lineSet))
		for l := range lineSet {
			lines = append(lines, l)
		}
		sort.Ints(lines)
		out = append(out, FileLines{File: f, Lines: lines})
	}
	return out, files
}
