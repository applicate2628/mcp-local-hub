package drmemory

import (
	"bufio"
	"strconv"
	"strings"
)

// MemError is one parsed Dr. Memory error block. It corresponds to a
// single "Error #N: <TYPE>" stanza in results.txt plus the call stack
// that follows it.
type MemError struct {
	// Type is the Dr. Memory error class, e.g. "UNADDRESSABLE ACCESS",
	// "UNINITIALIZED READ", "LEAK", "POSSIBLE LEAK", "INVALID HEAP ARGUMENT".
	Type string `json:"type"`
	// Count is the duplicate count Dr. Memory reports for this error
	// ("N unique, M total" lines roll into per-error counts). When the
	// results.txt does not carry an explicit per-error count it defaults
	// to 1 (one observed occurrence).
	Count int `json:"count"`
	// Location is the top-of-stack frame — the most actionable single
	// line (typically the user's own function + file:line). Empty when
	// the block had no parseable frames.
	Location string `json:"location"`
	// FullStack is every stack frame line for the error, joined by "\n",
	// preserving Dr. Memory's "#N module!func [file:line]" formatting.
	FullStack string `json:"full_stack"`
}

// ParsedResults is the structured form of a Dr. Memory results.txt file.
type ParsedResults struct {
	Errors     []MemError `json:"errors"`
	ErrorCount int        `json:"error_count"`
	LeakCount  int        `json:"leak_count"`
	// Summary is the verbatim "ERRORS FOUND:" / "NO ERRORS FOUND:" block
	// Dr. Memory prints near the end of results.txt.
	Summary string `json:"summary"`
}

// leakTypes are the Dr. Memory error classes that count toward LeakCount
// rather than the general error tally. They still appear in Errors[].
var leakTypes = map[string]bool{
	"LEAK":           true,
	"POSSIBLE LEAK":  true,
	"REACHABLE LEAK": true,
}

// parseResults turns a raw Dr. Memory results.txt body into structured
// findings. It is a PURE function (no I/O) so it can be unit-tested
// against a canned blob without ever running drmemory.exe.
//
// results.txt shape (real Dr. Memory output):
//
//	Dr. Memory version 2.6.0 ...
//	~~Dr.M~~ Running "target.exe"
//	Error #1: UNADDRESSABLE ACCESS: reading 0x... 4 byte(s)
//	# 0 target.exe!do_thing       [c:\src\target.c:42]
//	# 1 target.exe!main           [c:\src\target.c:88]
//	Note: ...
//
//	Error #2: LEAK 16 direct bytes ...
//	# 0 replace_malloc
//	# 1 target.exe!alloc_it       [c:\src\target.c:10]
//
//	ERRORS FOUND:
//	      1 unique,     1 total unaddressable access(es)
//	      1 unique,     1 total uninitialized access(es)
//	      1 unique,     1 total,     16 byte(s) of leak(s)
//
// Dr. Memory prefixes most lines with "~~Dr.M~~ "; the parser strips
// that prefix before classifying a line so both prefixed and bare
// results.txt variants parse identically.
func parseResults(raw string) ParsedResults {
	var res ParsedResults
	var cur *MemError
	var stack []string

	flush := func() {
		if cur == nil {
			return
		}
		if len(stack) > 0 {
			cur.Location = stack[0]
			cur.FullStack = strings.Join(stack, "\n")
		}
		res.Errors = append(res.Errors, *cur)
		cur = nil
		stack = nil
	}

	var summaryLines []string
	inSummary := false

	scanner := bufio.NewScanner(strings.NewReader(raw))
	// results.txt stack frames stay short, but the ERRORS FOUND summary or
	// a verbose note can be long; raise the token cap well above the
	// default 64 KiB so no line is silently dropped.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := stripDrMPrefix(scanner.Text())
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "Error #"):
			// New error block — flush any in-progress one first.
			flush()
			inSummary = false
			cur = parseErrorHeader(trimmed)

		case isStackFrame(trimmed):
			if cur != nil {
				stack = append(stack, trimmed)
			}

		case strings.HasPrefix(trimmed, "ERRORS FOUND:") ||
			strings.HasPrefix(trimmed, "NO ERRORS FOUND:"):
			// Summary block begins; the current error block is complete.
			flush()
			inSummary = true
			summaryLines = append(summaryLines, trimmed)

		default:
			if inSummary && trimmed != "" {
				summaryLines = append(summaryLines, trimmed)
			}
		}
	}
	flush()

	res.Summary = strings.Join(summaryLines, "\n")

	// Tally counts. LeakCount sums leak-class error counts; ErrorCount sums
	// the rest. Using the per-error Count (which may be >1 for duplicates)
	// keeps the tallies consistent with what an operator sees in the file.
	for _, e := range res.Errors {
		if leakTypes[e.Type] {
			res.LeakCount += e.Count
		} else {
			res.ErrorCount += e.Count
		}
	}

	return res
}

// stripDrMPrefix removes the "~~Dr.M~~ " (or "~~DrM~~ ") sentinel that
// Dr. Memory prepends to every results.txt line. The sentinel may be
// preceded by leading whitespace. Lines without the prefix pass through
// unchanged.
func stripDrMPrefix(line string) string {
	t := strings.TrimLeft(line, " \t")
	for _, p := range []string{"~~Dr.M~~ ", "~~Dr.M~~", "~~DrM~~ ", "~~DrM~~"} {
		if strings.HasPrefix(t, p) {
			return strings.TrimPrefix(t, p)
		}
	}
	return line
}

// parseErrorHeader extracts the error TYPE (and, when present, a count)
// from an "Error #N: <TYPE>: <detail>" header line. The type is the token
// run between the first ": " after the error number and the next ":".
//
// Examples:
//
//	"Error #1: UNADDRESSABLE ACCESS: reading 0x.. 4 byte(s)" → "UNADDRESSABLE ACCESS"
//	"Error #2: UNINITIALIZED READ: reading register eax"     → "UNINITIALIZED READ"
//	"Error #3: LEAK 16 direct bytes + 0 indirect bytes"      → "LEAK"
//	"Error #4: POSSIBLE LEAK 8 direct bytes ..."             → "POSSIBLE LEAK"
func parseErrorHeader(line string) *MemError {
	// Drop the "Error #N:" prefix.
	idx := strings.Index(line, ":")
	if idx < 0 {
		return &MemError{Type: "UNKNOWN", Count: 1}
	}
	rest := strings.TrimSpace(line[idx+1:])

	typ := classifyErrorType(rest)
	return &MemError{Type: typ, Count: 1}
}

// knownErrorTypes is the ordered list of Dr. Memory error-class prefixes
// recognized in an error header's detail text. Longer / more specific
// names come before shorter ones so "POSSIBLE LEAK" wins over "LEAK" and
// "INVALID HEAP ARGUMENT" is matched whole.
var knownErrorTypes = []string{
	"UNADDRESSABLE ACCESS",
	"UNINITIALIZED READ",
	"INVALID HEAP ARGUMENT",
	"GDI USAGE ERROR",
	"HANDLE LEAK",
	"WARNING",
	"REACHABLE LEAK",
	"POSSIBLE LEAK",
	"LEAK",
}

// classifyErrorType returns the recognized error class whose name prefixes
// the detail text, or the leading uppercase run as a best-effort fallback
// for an unknown class so the structured output never silently loses the
// type.
func classifyErrorType(detail string) string {
	upper := strings.ToUpper(detail)
	for _, t := range knownErrorTypes {
		if strings.HasPrefix(upper, t) {
			return t
		}
	}
	// Fallback: the type is usually the run of words before the first ":"
	// or the first numeric token. Take the leading alpha/space run.
	if i := strings.IndexByte(detail, ':'); i >= 0 {
		return strings.TrimSpace(detail[:i])
	}
	// Otherwise take leading words up to the first digit.
	var b strings.Builder
	for _, r := range detail {
		if r >= '0' && r <= '9' {
			break
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "UNKNOWN"
	}
	return out
}

// isStackFrame reports whether a trimmed line is a Dr. Memory call-stack
// frame. Frames begin with "#" followed by an optional space and a digit,
// e.g. "#0 target.exe!main [c:\src\target.c:88]" or "# 1 replace_malloc".
func isStackFrame(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "#") {
		return false
	}
	rest := strings.TrimSpace(trimmed[1:])
	if rest == "" {
		return false
	}
	// First token after "#" must be a frame number.
	tok := rest
	if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
		tok = rest[:sp]
	}
	_, err := strconv.Atoi(tok)
	return err == nil
}
