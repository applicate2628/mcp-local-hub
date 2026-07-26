package lastfailure

import (
	"bufio"
	"bytes"
	"regexp"
	"sort"
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

	// The diagnostic CODE is OPTIONAL. cl.exe always emits one (error C2065),
	// but clang-cl in MSVC-compatible mode emits the same POSITION shape with
	// no code at all — verified against three real operator failures in a
	// nested vcpkg -> make -> NMAKE -> cmake --build -> clang-cl build:
	//
	//	libsrc/general/mystring.cpp(63,15): error: definition of dllimport static field not allowed
	//	libsrc/core/bitarray.cpp(164,13): error: cannot use 'throw' with exceptions disabled
	//
	// Requiring the code made both invisible. Making it optional cannot
	// resurrect the filename trap: the shape still demands a parenthesised
	// LINE NUMBER at the start of the line, which no "-- Installing: .../
	// error_category.hpp" status line has.
	msvcCompileDiagRE = regexp.MustCompile(
		`^(?P<file>[^()\r\n]+)\((?P<line>\d+)(?:,\d+)?\):\s+(?P<sev>fatal error|error|warning)(?:\s+(?P<code>[A-Za-z]+\d+))?\s*:\s*(?P<msg>.+)$`)

	msvcLinkDiagRE = regexp.MustCompile(
		`^(?P<file>[^:()\r\n]+?)\s*:\s+(?P<sev>fatal error|error|warning)\s+(?P<code>LNK\d+)\s*:\s*(?P<msg>.+)$`)

	// toolDiagRE matches a diagnostic emitted by a compiler/linker DRIVER,
	// which names itself instead of a source position — verified real sample
	// from the same operator failure:
	//
	//	lld-link: error: undefined symbol: __declspec(dllimport) void __cdecl nglib::Ng_Init(void)
	//
	// The driver list is a CLOSED allowlist rather than a generic
	// `^word: error:` pattern on purpose. A generic form would match build
	// wrappers that carry no cause — above all NMAKE's U-series
	// ("NMAKE : fatal error U1077: 'cd' : return code '0x2'"), which is
	// exactly the noise this class of nested build buries the real error
	// under. See wrapperNoiseRE.
	toolDiagRE = regexp.MustCompile(
		`^(?P<file>clang-cl|lld-link|clang\+\+|clang|link|cl|ld|gcc|g\+\+)\s*:\s+(?P<sev>fatal error|error|warning)\s*:\s*(?P<msg>.+)$`)

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

// wrapperNoiseRE matches a BUILD-WRAPPER failure line: a tool reporting that
// a command it launched exited non-zero, without saying why.
//
// NMAKE's U-series is the observed case. In a nested vcpkg -> autotools make
// -> NMAKE -> cmake --build -> clang-cl failure, the real compiler error sits
// thousands of lines up while the TAIL of the log is a cascade of
//
//	NMAKE : fatal error U1077: 'cd' : return code '0x2'
//
// one per wrapper layer. These carry no cause, so they must never be reported
// as the diagnostic: doing so would hand the operator "return code 0x2"
// instead of "cannot use 'throw' with exceptions disabled". A log whose ONLY
// matches are wrapper noise yields unknown(no_diagnostic_found) with
// log_paths — honest, and the same principle as ContainsFailureDiagnostic
// one layer up.
var wrapperNoiseRE = regexp.MustCompile(`^NMAKE\s*:\s*(?:fatal\s+)?error\s+U\d+`)

// isWrapperNoise reports whether an already-trimmed line is a causeless
// build-wrapper failure. Checked BEFORE any diagnostic shape, so no present
// or future pattern can promote wrapper noise to a headline diagnostic.
func isWrapperNoise(line string) bool {
	return wrapperNoiseRE.MatchString(line)
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
	if isWrapperNoise(line) {
		return Diagnostic{}, false
	}
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
	if m := toolDiagRE.FindStringSubmatch(line); m != nil {
		return Diagnostic{
			// A driver diagnostic names no source position, so the driver
			// itself is the most specific locator available — same choice as
			// the ninja branch below, which reports the failing target.
			File:     strings.TrimSpace(m[toolDiagRE.SubexpIndex("file")]),
			Severity: normalizeSeverity(m[toolDiagRE.SubexpIndex("sev")]),
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

// SeverityError is the normalized severity that — and only that — can
// establish a build FAILURE. normalizeSeverity folds MSVC's "fatal error"
// into it; "warning" and "note" are deliberately NOT in this set.
const SeverityError = "error"

// ContainsFailureDiagnostic reports whether diags contains at least one
// diagnostic that actually establishes a failure.
//
// The matcher recognizes warnings and notes on purpose (they are useful
// evidence, and a `-Werror` build's warning is genuinely interesting), but
// recognizing a line is not the same as concluding the build broke. Before
// this predicate existed, ANY non-empty diagnostic set was converted to
// status=failed, so a log whose only match was
// `file.cpp:1:1: warning: deprecated` — the normal state of most successful
// C++ builds — was reported as a build failure with a confident phase and a
// headline "diagnostic". A warning-only log establishes nothing about
// success or failure, and must stay unknown(no_failure_diagnostic).
func ContainsFailureDiagnostic(diags []Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// maxDiagnosticsPerLog bounds how many matches of EACH severity class
// ScanDiagnostics returns from one file, so an adversarial or pathologically
// noisy log cannot inflate the result unboundedly.
//
// The cap is per severity CLASS, not per file, and that distinction is the
// whole point. A single flat cap applied in file order is spent by whatever
// comes first — which in a real clang-cl build is a flood of repeated
// warnings. Verified field failure (2026-07-26): a build log holding 50
// `clang-cl: warning: unknown argument ignored ... '-fopenmp'` lines followed
// by `fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals`
// returned the 50 warnings and DROPPED the error, after which the tool
// reported unknown(no_failure_diagnostic) — a confident denial of a failure
// that plainly happened. Reserving a separate error budget makes a trailing
// error unloseable to leading noise.
const maxDiagnosticsPerLog = 50

// ScanDiagnostics scans content line by line and returns every recognized
// diagnostic IN FILE ORDER, keeping at most maxDiagnosticsPerLog of each
// severity class (see the const doc for why the cap is per class).
//
// File order is deliberately preserved here: this function reports what the
// log says, in the order the log says it. RANKING for presentation is a
// separate concern owned by rankDiagnostics at the result boundary, so the
// two never have to be reasoned about together.
func ScanDiagnostics(content []byte) []Diagnostic {
	diags, _ := scanDiagnostics(content)
	return diags
}

// scanDiagnostics is the internal form that also reports a SCANNER failure.
//
// bufio.Scanner stops at the first line exceeding its buffer and reports it
// only via Err(). Ignoring that (as the exported wrapper's predecessor did)
// silently converts "the rest of this log was never examined" into "nothing
// else matched" — the same silent-truncation defect as an unbounded read, one
// layer down. Callers that issue a verdict must treat a non-nil error as
// incomplete evidence.
func scanDiagnostics(content []byte) ([]Diagnostic, error) {
	var out []Diagnostic
	errBudget, otherBudget := maxDiagnosticsPerLog, maxDiagnosticsPerLog
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		d, ok := matchDiagnosticLine(line)
		if !ok {
			continue
		}
		if d.Severity == SeverityError {
			if errBudget == 0 {
				continue
			}
			errBudget--
		} else {
			if otherBudget == 0 {
				continue
			}
			otherBudget--
		}
		out = append(out, d)
		if errBudget == 0 && otherBudget == 0 {
			break
		}
	}
	return out, scanner.Err()
}

// severityRank orders severities for presentation: the actionable line first.
// Anything unrecognized sorts last rather than silently ahead of a warning.
func severityRank(sev string) int {
	switch sev {
	case SeverityError:
		return 0
	case "warning":
		return 1
	case "note":
		return 2
	default:
		return 3
	}
}

// rankDiagnostics returns diags ordered by the tool's documented presentation
// rule: SEVERITY first (error, warning, note), then FIRST-OCCURRENCE order
// preserved within a severity.
//
// This exists because the tool's whole purpose is to spare the caller a
// filtering pass. Verified field failure (2026-07-26): a real answer carried
// 51 diagnostics — 50 repeated clang-cl warnings and ONE `LNK1120: 4
// unresolved externals`, the only actionable line, returned LAST. The
// package's own prose called the first recognized diagnostic "the headline",
// but nothing anywhere expressed that in the returned document: the list was
// raw scan order, and severity was consulted only by the boolean
// ContainsFailureDiagnostic, which decides the VERDICT and never the order.
//
// The sort is STABLE on purpose: within one severity, the log's own order is
// meaningful (in a nested build the first error is the cause and the rest are
// usually its cascade), so it must survive ranking untouched.
func rankDiagnostics(diags []Diagnostic) []Diagnostic {
	if len(diags) < 2 {
		return diags
	}
	out := make([]Diagnostic, len(diags))
	copy(out, diags)
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

// firstErrorDiagnostic returns the first error-severity diagnostic in
// FIRST-OCCURRENCE order, or nil when the set holds no error at all.
func firstErrorDiagnostic(diags []Diagnostic) *Diagnostic {
	for i := range diags {
		if diags[i].Severity == SeverityError {
			d := diags[i]
			return &d
		}
	}
	return nil
}
