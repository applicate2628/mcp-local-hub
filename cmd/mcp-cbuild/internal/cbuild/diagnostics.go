package cbuild

import (
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic is a single parsed compiler/linker/CMake message. Fields are
// omitted from JSON when empty so a linker error with no line info stays
// compact.
type Diagnostic struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Col      int    `json:"col,omitempty"`
	Severity string `json:"severity"` // error | warning | note
	Code     string `json:"code,omitempty"`
	Message  string `json:"message"`
}

// Diagnostic-format matchers. Ordered by specificity in parseLine: the MSVC
// parenthesized form and the LNK linker form are tried before the more general
// GCC/Clang colon form, because a Windows path's drive-letter colon can make
// the colon form match spuriously.
var (
	// MSVC / icx-cl (MSVC driver): path(line[,col]): error C2065: message
	// Also matches `fatal error C####` and the rare `note`.
	msvcRe = regexp.MustCompile(`^\s*(.+?)\((\d+)(?:,(\d+))?\)\s*:\s+(fatal error|error|warning|note)\s+([A-Za-z]+\d+)\s*:\s*(.*)$`)

	// MSVC linker: main.obj : error LNK2019: unresolved external symbol ...
	// (no line/col, code is LNK####).
	msvcLinkRe = regexp.MustCompile(`^\s*(.+?)\s*:\s+(fatal error|error|warning)\s+(LNK\d+)\s*:\s*(.*)$`)

	// GCC / Clang / icx-cl (clang driver): path:line[:col]: error: message
	gccClangRe = regexp.MustCompile(`^\s*(.+?):(\d+):(?:(\d+):)?\s+(fatal error|error|warning|note):\s+(.*)$`)

	// CMake: `CMake Error at path:line (cmd):` (location optional) or `CMake Error:`
	cmakeRe = regexp.MustCompile(`^CMake (Error|Warning|Deprecation Warning|Internal Error)(?: at (.+?):(\d+)(?: \(([^)]*)\))?)?\s*:\s*(.*)$`)

	// Trailing Clang/GCC warning flag, e.g. `[-Wunused-variable]` or
	// `[-Werror,-Wshadow]` — captured into Code when present.
	clangFlagRe = regexp.MustCompile(`\[(-W[^\]]+)\]\s*$`)
)

// parseDiagnostics extracts structured diagnostics from combined
// stdout+stderr build output. It is multi-format (MSVC, GCC, Clang, icx-cl in
// either driver mode, CMake, and best-effort linker) and order-preserving:
// diagnostics appear in the slice in the order they occur in the output.
//
// It is the single owner of build-output parsing. Unrecognized lines are
// dropped from the returned slice — callers always also surface a bounded
// raw_tail so nothing is silently lost.
func parseDiagnostics(raw string) []Diagnostic {
	// Normalize CRLF so line matching is platform-independent.
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")

	diags := make([]Diagnostic, 0, 8)
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}

		// CMake diagnostics can span multiple lines (an indented body follows
		// the header). Handle them first and advance i past the consumed body.
		if strings.HasPrefix(line, "CMake ") {
			if d, consumed, ok := parseCMake(lines, i); ok {
				diags = append(diags, d)
				i += consumed
				continue
			}
		}

		if d, ok := parseLine(line); ok {
			diags = append(diags, d)
		}
	}
	return diags
}

// parseLine matches a single line against the single-line diagnostic formats.
func parseLine(line string) (Diagnostic, bool) {
	if m := msvcRe.FindStringSubmatch(line); m != nil {
		d := Diagnostic{
			File:     strings.TrimSpace(m[1]),
			Line:     atoi(m[2]),
			Col:      atoi(m[3]),
			Severity: normalizeSeverity(m[4]),
			Code:     m[5],
			Message:  strings.TrimSpace(m[6]),
		}
		return d, true
	}
	if m := msvcLinkRe.FindStringSubmatch(line); m != nil {
		d := Diagnostic{
			File:     strings.TrimSpace(m[1]),
			Severity: normalizeSeverity(m[2]),
			Code:     m[3],
			Message:  strings.TrimSpace(m[4]),
		}
		return d, true
	}
	if m := gccClangRe.FindStringSubmatch(line); m != nil {
		msg := strings.TrimSpace(m[5])
		code := ""
		if fm := clangFlagRe.FindStringSubmatch(msg); fm != nil {
			code = fm[1]
		}
		d := Diagnostic{
			File:     strings.TrimSpace(m[1]),
			Line:     atoi(m[2]),
			Col:      atoi(m[3]),
			Severity: normalizeSeverity(m[4]),
			Code:     code,
			Message:  msg,
		}
		return d, true
	}
	// GNU ld undefined-reference lines carry no file:line the parser can trust;
	// surface them as best-effort error diagnostics so a link failure is visible.
	if strings.Contains(line, "undefined reference to") {
		return Diagnostic{Severity: "error", Message: strings.TrimSpace(line)}, true
	}
	return Diagnostic{}, false
}

// parseCMake parses a CMake diagnostic starting at lines[start], returning the
// diagnostic, the number of ADDITIONAL lines consumed (body lines beyond the
// header), and whether the header matched. The body is the run of indented /
// non-empty follow-on lines up to the first blank line or a "Call Stack" marker.
func parseCMake(lines []string, start int) (Diagnostic, int, bool) {
	m := cmakeRe.FindStringSubmatch(lines[start])
	if m == nil {
		return Diagnostic{}, 0, false
	}
	d := Diagnostic{
		Severity: normalizeSeverity(m[1]),
		File:     strings.TrimSpace(m[2]),
		Line:     atoi(m[3]),
		Code:     strings.TrimSpace(m[4]), // the invoking command, when present
	}
	var body []string
	if rest := strings.TrimSpace(m[5]); rest != "" {
		body = append(body, rest)
	}
	consumed := 0
	for j := start + 1; j < len(lines); j++ {
		l := lines[j]
		if strings.TrimSpace(l) == "" {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(l), "Call Stack") {
			break
		}
		// Body lines are indented; a non-indented line ends the diagnostic.
		if l == strings.TrimLeft(l, " \t") {
			break
		}
		body = append(body, strings.TrimSpace(l))
		consumed++
	}
	d.Message = strings.Join(body, " ")
	return d, consumed, true
}

// normalizeSeverity collapses CMake/compiler severity spellings to the three
// canonical values.
func normalizeSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fatal error", "error", "internal error":
		return "error"
	case "warning", "deprecation warning":
		return "warning"
	case "note":
		return "note"
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

func atoi(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
