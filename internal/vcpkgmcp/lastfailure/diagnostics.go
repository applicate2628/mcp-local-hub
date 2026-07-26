package lastfailure

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// Diagnostic-shape matching. This is the fix for the two traps named in
// work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md:
//
//   - "grep error matches filenames" (e.g. error_estimator.h,
//     error_category.hpp) — every pattern here is ANCHORED at the start of
//     the (trimmed) line to a specific diagnostic POSITION shape
//     (file:line:col: severity: ... or file(line): severity code: ...), so
//     a substring "error" appearing inside an unrelated "-- Installing:
//     .../error_category.hpp" status line never matches. Verified against
//     a real observed false-positive: r:\b\wingpl\boost-system\install-*.log
//     contains multiple "-- Installing: .../error_category*.hpp" lines —
//     none of them match either pattern below (confirmed empirically this
//     session; see the parser tests' filename-trap fixture).
//   - "libtool erases the exit code" — classification here NEVER consults
//     an exit code; a phase is judged failed exactly when a line in its
//     log matches one of these anchored diagnostic shapes, independent of
//     any process exit status.
//
// GCC/Clang shape mirrors the established in-repo convention already used
// by internal/perftools/clangtidy.go's diagLineRE (kept deliberately
// consistent rather than re-invented): `file:line:col: severity: message`.
//
// MSVC/MSBuild shape is grounded in the official documented format
// (Microsoft Learn, "MSBuild and Visual Studio format for diagnostic
// messages": origin(line[,col]): category code: text; and
// "/diagnostics (Compiler diagnostic options)": /diagnostics:classic — the
// default — reports only the line, no column). Two sub-shapes are
// recognized: the compiler shape `file(line[,col]): severity CODE: msg`
// and the linker shape `file : severity LNKnnnn: msg` (link.exe has no
// source line, so no parens).
var (
	gccClangDiagRE = regexp.MustCompile(
		`^(?P<file>.+?):(?P<line>\d+):(?P<col>\d+):\s+(?P<sev>fatal error|error|warning|note):\s*(?P<msg>.+)$`)

	msvcCompileDiagRE = regexp.MustCompile(
		`^(?P<file>[^()\r\n]+)\((?P<line>\d+)(?:,\d+)?\):\s+(?P<sev>fatal error|error|warning)\s+(?P<code>[A-Za-z]+\d+)\s*:\s*(?P<msg>.+)$`)

	msvcLinkDiagRE = regexp.MustCompile(
		`^(?P<file>[^:()\r\n]+?)\s*:\s+(?P<sev>fatal error|error|warning)\s+(?P<code>LNK\d+)\s*:\s*(?P<msg>.+)$`)

	// ninjaFailedRE matches ninja's own build-step failure summary line,
	// e.g. `FAILED: [code=2] CMakeFiles/cmTC_e5bae.dir/src.cxx.obj` —
	// verified real sample (scout pass, boost-atomic\config-wingpl-rel-
	// CMakeConfigureLog.yaml.log). Anchored on the literal "FAILED:" token
	// ninja itself emits; no CMake status line or comment observed across
	// 618 real log files begins with this exact token (the closest look-
	// alike, `-- Performing Test X - Failed`, starts with "--", not
	// "FAILED:"). A ninja FAILED line names a build TARGET, not a source
	// file:line, so it carries no Line — but per the scout pass, a
	// "FAILED:" line can ALSO be the tail of a user-interrupted build
	// ("User interrupt" / "ninja: build stopped: interrupted by user."
	// immediately follows), which callers must check separately via
	// DetectInterrupted before trusting this as a real failure.
	ninjaFailedRE = regexp.MustCompile(`^FAILED:\s*(?:\[code=-?\d+\]\s*)?(?P<target>.+)$`)
)

// interruptMarkers are exact narrative phrases ninja emits when a build was
// stopped by an operator/process signal rather than a genuine build defect
// — verified real sample (scout pass, boost-thread\config-wingpl-out.log):
// "FAILED: [code=1]" immediately followed by "User interrupt" and "ninja:
// build stopped: interrupted by user.". These are deliberately matched as
// plain substrings rather than an anchored shape: unlike "error" (a common
// English word that collides with filenames/comments), these are fixed,
// low-ambiguity ninja-owned narration sentences with negligible risk of
// appearing as an unrelated false positive.
var interruptMarkers = []string{
	"User interrupt",
	"ninja: build stopped: interrupted by user.",
}

// DetectInterrupted reports whether content carries a ninja user-interrupt
// marker. A "FAILED:" line in the same log must NOT be reported as a real
// build failure when this is true — the build was stopped, not broken.
func DetectInterrupted(content []byte) bool {
	s := string(content)
	for _, m := range interruptMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// normalizeSeverity folds "fatal error" into "error" (same category, just
// MSVC's own more urgent spelling) so callers switch on a small set.
func normalizeSeverity(sev string) string {
	if sev == "fatal error" {
		return "error"
	}
	return sev
}

// matchDiagnosticLine tries each anchored shape against one already-trimmed
// line. Returns ok=false for anything that does not match one of the
// closed set of recognized shapes — including a bare substring match of
// "error"/"warning" with no recognized position, which is deliberately
// NOT treated as a diagnostic (that would resurrect the filename trap).
func matchDiagnosticLine(line string) (Diagnostic, bool) {
	if m := gccClangDiagRE.FindStringSubmatch(line); m != nil {
		lineNum, _ := strconv.Atoi(m[gccClangDiagRE.SubexpIndex("line")])
		return Diagnostic{
			File:     m[gccClangDiagRE.SubexpIndex("file")],
			Line:     lineNum,
			Severity: normalizeSeverity(m[gccClangDiagRE.SubexpIndex("sev")]),
			Text:     line,
		}, true
	}
	if m := msvcCompileDiagRE.FindStringSubmatch(line); m != nil {
		lineNum, _ := strconv.Atoi(m[msvcCompileDiagRE.SubexpIndex("line")])
		return Diagnostic{
			File:     strings.TrimSpace(m[msvcCompileDiagRE.SubexpIndex("file")]),
			Line:     lineNum,
			Severity: normalizeSeverity(m[msvcCompileDiagRE.SubexpIndex("sev")]),
			Text:     line,
		}, true
	}
	if m := msvcLinkDiagRE.FindStringSubmatch(line); m != nil {
		return Diagnostic{
			File:     strings.TrimSpace(m[msvcLinkDiagRE.SubexpIndex("file")]),
			Severity: normalizeSeverity(m[msvcLinkDiagRE.SubexpIndex("sev")]),
			Text:     line,
		}, true
	}
	if m := ninjaFailedRE.FindStringSubmatch(line); m != nil {
		return Diagnostic{
			File:     strings.TrimSpace(m[ninjaFailedRE.SubexpIndex("target")]),
			Severity: "error",
			Text:     line,
		}, true
	}
	return Diagnostic{}, false
}

// maxDiagnosticsPerLog bounds how many matches ScanDiagnostics returns from
// one file, so an adversarial or pathologically noisy log cannot inflate
// the result unboundedly.
const maxDiagnosticsPerLog = 50

// ScanDiagnostics scans content line by line and returns every recognized
// diagnostic, in file order, capped at maxDiagnosticsPerLog. The first
// element is "the FIRST real diagnostic" the design doc calls the
// headline finding; the rest are returned too since the tool's schema is
// diagnostics[] (plural) — an agent that wants more than the first one
// does not have to re-read the file itself.
func ScanDiagnostics(content []byte) []Diagnostic {
	var out []Diagnostic
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		d, ok := matchDiagnosticLine(line)
		if !ok {
			continue
		}
		out = append(out, d)
		if len(out) >= maxDiagnosticsPerLog {
			break
		}
	}
	return out
}
