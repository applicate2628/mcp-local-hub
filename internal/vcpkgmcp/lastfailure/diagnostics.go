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

// interruptMarkers are the exact WHOLE LINES observed when a build was stopped
// by an operator/process signal rather than a genuine build defect — verified
// real sample (scout pass, boost-thread\config-wingpl-out.log, recorded in
// work-items/decisions/2026-07-25-vcpkg-ground-truth-measured.md §4 trap 2):
// "FAILED: [code=1]" immediately followed by "User interrupt" and "ninja:
// build stopped: interrupted by user.", each on its own line — reproduced in
// testdata/failing_port/buildtrees/interruptedlib/install-cl-rel-out.log.
//
// Producer ground truth, probed in the target environment this session rather
// than assumed from the shape of the strings:
//
//	$ strings <ninja>.exe | grep -i interrupt
//	interrupted by user           # the %s of the "build stopped: %s." format
//	$ strings <ninja>.exe | grep "^ninja: "
//	ninja:                        # the prefix literal
//	$ strings <ninja>.exe | grep "User interrupt"
//	(no match)
//
// checked against BOTH C:\msys64\ucrt64\bin\ninja.exe and the Visual Studio
// 18 Community ninja. So ninja OWNS the second line and emits it whole; it
// does NOT own "User interrupt" at all — that line is the killed SUBPROCESS's
// captured output, which ninja relays verbatim after its "FAILED:" summary.
// (cl.exe, link.exe and nmake.exe do not contain the literal either, so the
// exact producer is whichever tool the step ran; what matters here is that it
// arrives as a relayed standalone line.)
//
// They are matched as COMPLETE LINES, not as substrings of the file. The
// substring form was wrong in the dangerous direction: DetectInterrupted's
// verdict is the HIGHEST-precedence branch in LastFailure (lastfailure.go:511,
// above even an unreadable log), so ONE stray occurrence of the 14 characters
// "User interrupt" anywhere in ANY scanned phase log converted a genuine build
// failure into unknown(build_interrupted) and suppressed the real diagnostic.
// That is not hypothetical: a phase log routinely QUOTES arbitrary text —
// ninja -v echoes every command line in full (see the fixture's
// `[1/3] cl.exe /c ... a.cpp`), so any source path or -D value containing the
// phrase lands in the log; clang and gcc echo the offending SOURCE LINE under
// each diagnostic, so a comment or string literal mentioning it lands there
// too; and a project's own CMake message()/check_symbol_exists output is
// copied straight through.
//
// A mid-line occurrence is therefore text the producer QUOTED, never text the
// producer NARRATED, and only the latter is evidence of an interrupt.
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

// --- Aggregate tier recognizers -----------------------------------------
//
// See DiagnosticTier for the rule, the full per-shape sweep, and why an
// aggregate is a legitimate fallback headline while wrapper noise is not.
// The two patterns below are the only places a tier is not fixed by the
// matched shape alone.

// aggregateLinkCodeRE is the CLOSED set of MSVC linker codes Microsoft
// documents as summarising a preceding series of specific errors. Each entry is
// cited, and the set is deliberately tight: wrongly demoting a specific error
// would bury a real cause, whereas omitting an aggregate merely leaves today's
// behavior in place.
//
//   - LNK1120 "<number> unresolved externals" — "reports the NUMBER of
//     unresolved external symbol errors in the current link. Each unresolved
//     external symbol first gets reported by a LNK2001 or LNK2019 error. The
//     LNK1120 message comes last... You don't need to fix this error."
//     (learn.microsoft.com/cpp/error-messages/tool-errors/linker-tools-error-lnk1120,
//     view=msvc-170)
//   - LNK1169 "one or more multiply defined symbols found" — "This error is
//     PRECEDED BY error LNK2005."
//     (learn.microsoft.com/cpp/error-messages/tool-errors/linker-tools-error-lnk1169,
//     view=msvc-170)
//
// Their specific counterparts (LNK2001/LNK2019 unresolved external symbol
// '<symbol>', LNK2005 '<symbol>' already defined) name the symbol and so stay
// specific, as does every cause-naming fatal (LNK1104 cannot open file,
// LNK1181 cannot open input file, LNK1112 machine type conflict).
var aggregateLinkCodeRE = regexp.MustCompile(`^LNK(?:1120|1169)$`)

// aggregateDriverMsgRE matches a compiler/linker DRIVER message that relays
// only the exit status of a sub-tool the driver launched. clang's driver
// diagnostic is "%0 command failed with exit code %1", where %0 is the sub-tool
// role (linker / assembler / clang frontend / ...), so the tier is decided by
// the literal phrase rather than by enumerating roles.
//
// Verified in the target environment (clang 21, msys2 ucrt64, this session):
//
//	$ clang++ undef.cpp -o undef.exe
//	C:/msys64/ucrt64/bin/ld: undef.cpp:(.text+0x17): undefined reference to `missing_fn()'
//	clang++: error: linker command failed with exit code 1 (use -v to see invocation)
//
// The operator's own field line is the clang-cl spelling of the same
// diagnostic: `clang-cl: error: linker command failed with exit code 1120`.
//
// Everything else a driver says stays specific, because it names its own cause:
// `lld-link: error: undefined symbol: X`, `clang-cl: warning: unknown argument
// ignored in clang-cl: '-fopenmp'`, `clang: error: no such file or directory`.
var aggregateDriverMsgRE = regexp.MustCompile(`^[A-Za-z][A-Za-z ]* command failed with exit code \d+`)

// msvcLinkTier classifies an MSVC-linker-shaped diagnostic by its LNK code.
// The code is matched uppercase by msvcLinkDiagRE, so no folding is needed.
func msvcLinkTier(code string) DiagnosticTier {
	if aggregateLinkCodeRE.MatchString(code) {
		return TierAggregate
	}
	return TierSpecific
}

// driverTier classifies a compiler/linker-driver diagnostic by its message.
func driverTier(msg string) DiagnosticTier {
	if aggregateDriverMsgRE.MatchString(msg) {
		return TierAggregate
	}
	return TierSpecific
}

// --- Log-line normalization (single owner) ------------------------------
//
// # Why this exists, and why it is shared rather than local
//
// A buildtrees phase log is a CAPTURED TERMINAL STREAM. Whole-line matching —
// which both the interrupt markers and every anchored diagnostic shape rely on
// — is only sound if "the line" means the line's TEXT, not its display bytes.
// Anchoring on raw bytes made three inputs false-negative that the older
// substring form had caught, because bytes.TrimSpace strips none of them
// (measured 2026-07-27):
//
//	"\ufeffUser interrupt"                                    BOM prefix
//	"\x1b[31mninja: build stopped: interrupted by user.\x1b[0m"  ANSI wrap
//	"User interrupt\x00"                                      NUL suffix
//
// BOM and NUL are unrealistic. ANSI is not, and the same measurement showed the
// damage is NOT confined to the interrupt predicate — it is strictly worse in
// scanDiagnostics, where a colourized clang diagnostic matched NO shape at all:
//
//	ScanDiagnostics("\x1b[1ma.cpp:3:5: \x1b[0m\x1b[0;1;31merror: \x1b[0m...")
//	  -> 0 diagnostics
//
// i.e. a build that plainly failed answers unknown(no_diagnostic_found) — a
// confident denial, the exact class this package exists to eliminate.
//
// Reachability was probed in the target environment this session rather than
// assumed from the shape of the strings:
//
//	$ g++ -fsyntax-only -fdiagnostics-color=always g.cpp 2>&1 | od -c
//	033 [ 0 1 m 033 [ K g . c p p : 033 [ m 033 [ K ...
//	$ clang++ -fsyntax-only -fdiagnostics-color=always -fansi-escape-codes ...
//	033 [ 1 m a n s i . c p p : 1 : 1 3 :  033 [ 0 m 033 [ 0 ; 1 ; 3 1 m ...
//	$ strings <ninja>.exe | grep CLICOLOR
//	CLICOLOR_FORCE
//
// (msys2 ucrt64 GCC 15 / clang 21 / C:\msys64\ucrt64\bin\ninja.exe.) GCC emits
// ANSI to a REDIRECTED pipe with nothing but -fdiagnostics-color=always, which
// an ordinary CXXFLAGS or triplet setting can carry — no exotic configuration
// and no CLICOLOR_FORCE needed. Note GCC's `\x1b[K` (erase-in-line): the
// stripper must handle general CSI, not only SGR.
//
// Because ANSI reaches every anchored matcher in this package, the fix is ONE
// normalizer that every phase-log line scanner calls, not a strip inside
// DetectInterrupted. Two notions of "a line" inside one package is how the two
// sides drift apart.
//
// # Wire consequence (deliberate, stated)
//
// Diagnostic.Text and Diagnostic.File are now the NORMALIZED line, not the raw
// bytes. That is a second fix rather than a cost: an MCP result is rendered in
// a terminal and copied into transcripts, so relaying a build log's escape
// sequences verbatim is the same terminal-injection hazard the marketplace
// catalog path already strips C0/C1/ESC to avoid — and a build log (arbitrary
// compiler output, source lines echoed back, third-party build scripts) is at
// least as untrusted as a catalog.
//
// # Scope: which scanners, and which not
//
// Applied by the three PHASE-LOG scanners — DetectInterrupted, scanDiagnostics,
// findRunBuildCommandLine — because they read one input class: a captured
// build-tool stream.
//
// NOT applied by ParseWrapperContent (wrapper.go): a build_failed.log is a
// structured key/value record the operator's OWN wrapper script writes, not a
// relayed terminal capture, and its `command:` line is emitted as
// Result.ExactCommand — a string whose entire purpose is verbatim
// reproducibility, so editing bytes out of it would be a defect, not a fix. If
// a wrapper is ever observed emitting escapes, this same owner is the fix.

// normalizeLogLine returns line with terminal display bytes removed, leaving
// the text a whole-line comparison can be made against.
//
// The rules, and why each is safe on a byte-oriented walk:
//
//   - ANSI/VT escape sequences are dropped: CSI (ESC '[' params intermediates
//     final), OSC and the other string sequences (ESC ']' / 'P' / 'X' / '^' /
//     '_' up to BEL or ST), and any other two-byte ESC form. An UNTERMINATED
//     sequence consumes to end of line — a truncated escape is still not text.
//   - A UTF-8 BOM (U+FEFF, EF BB BF) is dropped wherever it appears. It is a
//     zero-width no-break space and is never part of a diagnostic token.
//   - C0 controls below 0x20 other than TAB, plus DEL (0x7F), are dropped. CR
//     and LF never reach here: they are the line separators.
//
// C1 controls (0x80-0x9F) are deliberately NOT dropped. As raw bytes those are
// UTF-8 continuation bytes, so removing them would corrupt every multi-byte
// rune in the line. Every byte value this function does drop is one that cannot
// occur inside a UTF-8 multi-byte sequence, which is what makes the byte-level
// walk correct without decoding.
//
// Known edge, accepted: a control byte EMBEDDED in text is removed rather than
// replaced, so "User\x01 interrupt" normalizes to "User interrupt" and would
// match. That input is display garbage for which no answer is more defensible
// than another, and the alternative (replacing with a sentinel) would break the
// diagnostic shapes this same function has to keep matchable.
//
// Allocation-free when there is nothing to strip, which is the overwhelmingly
// common case across a size-bounded log's every line.
func normalizeLogLine(line string) string {
	if !needsLogLineNormalization(line) {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == 0x1b:
			i = skipEscapeSequence(line, i)
		case c == 0xEF && strings.HasPrefix(line[i:], "\ufeff"):
			i += len("\ufeff")
		case c < 0x20 && c != '\t':
			i++
		case c == 0x7f:
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// needsLogLineNormalization is the fast path: report whether any byte would be
// stripped, so an ordinary line is returned as-is with no copy.
func needsLogLineNormalization(line string) bool {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == 0x1b || c == 0x7f || (c < 0x20 && c != '\t') {
			return true
		}
		if c == 0xEF && strings.HasPrefix(line[i:], "\ufeff") {
			return true
		}
	}
	return false
}

// skipEscapeSequence returns the index just past the escape sequence starting
// at line[i] (which the caller has established is ESC). An unterminated
// sequence consumes the rest of the line.
func skipEscapeSequence(line string, i int) int {
	i++ // the ESC itself
	if i >= len(line) {
		return i
	}
	switch line[i] {
	case '[': // CSI: params 0x30-0x3F, intermediates 0x20-0x2F, one final 0x40-0x7E
		i++
		for i < len(line) && line[i] >= 0x30 && line[i] <= 0x3f {
			i++
		}
		for i < len(line) && line[i] >= 0x20 && line[i] <= 0x2f {
			i++
		}
		if i < len(line) && line[i] >= 0x40 && line[i] <= 0x7e {
			i++
		}
		return i
	case ']', 'P', 'X', '^', '_': // OSC / DCS / SOS / PM / APC: run to BEL or ST
		i++
		for i < len(line) {
			if line[i] == 0x07 { // BEL
				return i + 1
			}
			if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '\\' { // ST
				return i + 2
			}
			i++
		}
		return i
	default:
		// The general "nF" escape form: zero or more intermediate bytes
		// (0x20-0x2F) then one final byte (0x30-0x7E). This covers the
		// three-byte charset designators a terminal capture really carries —
		// ESC ( B (select ASCII), ESC ) 0, ESC # 8 — as well as the plain
		// two-byte forms (ESC 7, ESC =, ESC M), which are just the zero-
		// intermediate case. Treating every non-CSI escape as two bytes left
		// the final byte behind as text: this branch was written that way and
		// TestNormalizeLogLine's "two-byte escape" case caught it, returning
		// "BUser interrupt" for "\x1b(BUser interrupt".
		for i < len(line) && line[i] >= 0x20 && line[i] <= 0x2f {
			i++
		}
		if i < len(line) && line[i] >= 0x30 && line[i] <= 0x7e {
			i++
		}
		return i
	}
}

// DetectInterrupted reports whether content carries a user-interrupt marker as
// a WHOLE LINE. A "FAILED:" line in the same log must NOT be reported as a real
// build failure when this is true — the build was stopped, not broken.
//
// See interruptMarkers for the producer evidence and for why a whole-file
// substring scan was the wrong predicate, and normalizeLogLine for why the
// whole-line comparison is made against the line's TEXT rather than its raw
// display bytes.
//
// The walk is deliberately NOT a bufio.Scanner: a scanner stops at the first
// line over its buffer and reports that only through Err(), so a marker in the
// tail would be silently missed. Here every byte of the (already size-bounded)
// content is examined and there is no error channel to drop. Lines are split
// on '\n' AND '\r' so a CRLF log, and a capture that retained a terminal's
// carriage-return overwrites, both decompose correctly.
func DetectInterrupted(content []byte) bool {
	for len(content) > 0 {
		line := content
		if i := bytes.IndexAny(content, "\r\n"); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		// Normalize BEFORE trimming: stripping a trailing reset sequence or a
		// leading BOM can expose whitespace the trim then has to remove.
		trimmed := strings.TrimSpace(normalizeLogLine(string(line)))
		if trimmed == "" {
			continue
		}
		for _, m := range interruptMarkers {
			if trimmed == m {
				return true
			}
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
			// A source position IS the cause's address; never an aggregate.
			Tier: TierSpecific,
			Text: line,
		}, true
	}
	if m := msvcCompileDiagRE.FindStringSubmatch(line); m != nil {
		lineNum, _ := strconv.Atoi(m[msvcCompileDiagRE.SubexpIndex("line")])
		return Diagnostic{
			File:     strings.TrimSpace(m[msvcCompileDiagRE.SubexpIndex("file")]),
			Line:     lineNum,
			Severity: normalizeSeverity(m[msvcCompileDiagRE.SubexpIndex("sev")]),
			Tier:     TierSpecific,
			Text:     line,
		}, true
	}
	if m := msvcLinkDiagRE.FindStringSubmatch(line); m != nil {
		return Diagnostic{
			File:     strings.TrimSpace(m[msvcLinkDiagRE.SubexpIndex("file")]),
			Severity: normalizeSeverity(m[msvcLinkDiagRE.SubexpIndex("sev")]),
			Tier:     msvcLinkTier(m[msvcLinkDiagRE.SubexpIndex("code")]),
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
			Tier:     driverTier(m[toolDiagRE.SubexpIndex("msg")]),
			Text:     line,
		}, true
	}
	if m := ninjaFailedRE.FindStringSubmatch(line); m != nil {
		return Diagnostic{
			File:     strings.TrimSpace(m[ninjaFailedRE.SubexpIndex("target")]),
			Severity: "error",
			// ninja names the failed TARGET and an exit code, never the cause
			// — which is the compiler output ninja prints immediately after
			// this line. In file order the summary therefore always precedes
			// the diagnostic it summarises.
			Tier: TierAggregate,
			Text: line,
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

// maxDiagnosticsPerLog bounds how many matches of EACH RANKING CELL
// ScanDiagnostics returns from one file, so an adversarial or pathologically
// noisy log cannot inflate the result unboundedly.
//
// The cap is per cell, not per file, and that distinction is the whole point.
// A single flat cap applied in file order is spent by whatever comes first —
// which in a real clang-cl build is a flood of repeated warnings. Verified
// field failure (2026-07-26): a build log holding 50 `clang-cl: warning:
// unknown argument ignored ... '-fopenmp'` lines followed by
// `fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals`
// returned the 50 warnings and DROPPED the error, after which the tool
// reported unknown(no_failure_diagnostic) — a confident denial of a failure
// that plainly happened. Reserving a separate error budget made a trailing
// error unloseable to leading noise.
//
// A "cell" is (severity class, tier) — the SAME two keys, in the same order,
// that diagnosticOutranks uses to choose the headline. That correspondence is
// the rule, not a coincidence: a budget that does not split along a ranking key
// lets one side of that key starve the other, and the starved side is by
// definition the one ranking would have preferred.
//
// The tier key was the open half. Splitting on severity alone left AGGREGATE
// and SPECIFIC errors sharing one budget, so the identical failure reappeared
// one level in: a log whose first 50 error-severity lines are ninja's own
// `FAILED: [code=N] <target>` summaries (one per failing edge — routine with a
// wide parallel build, and unavoidable under keep-going) spent the whole error
// budget on aggregates and DROPPED the trailing `lld-link: error: undefined
// symbol: X`. headlineErrorDiagnostic then returned an aggregate, so the
// operator was handed "FAILED: <target>" instead of the cause — precisely the
// outcome the 2026-07-27 tier work was introduced to prevent, reintroduced
// through the budget. The operator's own nested clang-cl build has the same
// shape, each wrapper layer emitting `clang-cl: error: linker command failed
// with exit code N`.
//
// Per-log ceiling consequence: maxDiagnosticsPerLog is now spent per cell, so
// the worst-case returned count per log is
// severityBudgetClasses*tierBudgetClasses*maxDiagnosticsPerLog rather than
// 2*maxDiagnosticsPerLog. ASSUMPTION (UNVERIFIED): holding the per-cell figure
// at 50 (and accepting the larger ceiling) is preferred over subdividing 50
// into smaller cells. The code and docs establish the SPLIT (above) but say
// nothing about the intended total; 50 is kept because subdividing would
// weaken the guarantee the 2026-07-26 fix deliberately established — a trailing
// error must survive 50 leading noise lines — which is a regression, whereas a
// larger bounded ceiling is not. Resolved by the tool owner stating a response
// size budget, or by measuring a real worst-case result.
const maxDiagnosticsPerLog = 50

// Budget cell dimensions. They mirror diagnosticOutranks' two keys:
// severityBudgetClasses is the error/non-error split scanDiagnostics has always
// used, and tierBudgetClasses is the range of tierRank (specific, aggregate,
// and the unset fallback tierRank sorts last).
const (
	severityBudgetClasses = 2
	tierBudgetClasses     = 3
)

// severityBudgetClass maps a diagnostic to its budget row. It is deliberately
// the SAME error/non-error question ContainsFailureDiagnostic asks, so a
// diagnostic that can establish a failure is budgeted apart from one that
// cannot.
func severityBudgetClass(d Diagnostic) int {
	if d.Severity == SeverityError {
		return 0
	}
	return 1
}

// ScanDiagnostics scans content line by line and returns every recognized
// diagnostic IN FILE ORDER, keeping at most maxDiagnosticsPerLog of each
// (severity class, tier) cell (see the const doc for why the cap is per cell).
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
	// One budget per (severity class, tier) cell — see maxDiagnosticsPerLog.
	var budget [severityBudgetClasses][tierBudgetClasses]int
	for s := range budget {
		for t := range budget[s] {
			budget[s][t] = maxDiagnosticsPerLog
		}
	}
	remaining := severityBudgetClasses * tierBudgetClasses * maxDiagnosticsPerLog

	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		// normalizeLogLine is the SAME owner DetectInterrupted uses: every
		// shape below is anchored, so a colourized log would otherwise match
		// nothing and answer unknown(no_diagnostic_found) for a build that
		// plainly failed. It also decides what Diagnostic.Text carries on the
		// wire — see the normalizer's doc for that contract change.
		line := normalizeLogLine(strings.TrimRight(scanner.Text(), "\r"))
		d, ok := matchDiagnosticLine(line)
		if !ok {
			continue
		}
		sev, tier := severityBudgetClass(d), tierRank(d.Tier)
		if budget[sev][tier] == 0 {
			continue
		}
		budget[sev][tier]--
		remaining--
		out = append(out, d)
		if remaining == 0 {
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

// tierRank orders tiers within one severity: the line that names a cause
// first. An unset tier sorts last rather than silently ahead of a classified
// one — the same fail-safe posture as severityRank's default.
func tierRank(t DiagnosticTier) int {
	switch t {
	case TierSpecific:
		return 0
	case TierAggregate:
		return 1
	default:
		return 2
	}
}

// diagnosticOutranks reports whether a ranks STRICTLY ahead of b under the
// tool's documented presentation order: severity first, then tier.
//
// This is the single owner of that order. rankDiagnostics, the headline error,
// and the headline SOURCE log all consult it, so the returned diagnostics[0],
// first_error and diagnostic_log can never disagree about which line is the
// headline — which is exactly what diagnostic_log exists to guarantee.
// First-occurrence order is deliberately NOT part of this predicate: it is the
// caller's tiebreak (a stable sort, or a strict-improvement-only loop), which
// keeps "equal rank" and "earlier in the log" separable.
func diagnosticOutranks(a, b Diagnostic) bool {
	if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
		return ra < rb
	}
	return tierRank(a.Tier) < tierRank(b.Tier)
}

// rankDiagnostics returns diags ordered by the tool's documented presentation
// rule: SEVERITY first (error, warning, note), then TIER within a severity
// (specific before aggregate), then FIRST-OCCURRENCE order.
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
// The tier key is the 2026-07-27 refinement of the same principle one level
// in: severity got the errors to the front, but among ERRORS a driver's
// `linker command failed with exit code 1120` was still outranking the
// `lld-link: error: undefined symbol: gzopen_w` it was merely reporting. See
// DiagnosticTier.
//
// The sort is STABLE on purpose: within one severity AND tier, the log's own
// order is meaningful (in a nested build the first error is the cause and the
// rest are usually its cascade), so it must survive ranking untouched.
func rankDiagnostics(diags []Diagnostic) []Diagnostic {
	if len(diags) < 2 {
		return diags
	}
	out := make([]Diagnostic, len(diags))
	copy(out, diags)
	sort.SliceStable(out, func(i, j int) bool {
		return diagnosticOutranks(out[i], out[j])
	})
	return out
}

// headlineErrorDiagnostic returns the HEADLINE error — the highest-ranked
// error-severity diagnostic under diagnosticOutranks, which is the first
// CAUSE-NAMING error when one exists and otherwise the first aggregate. Nil
// when the set holds no error at all.
//
// The strict-improvement comparison is what preserves first-occurrence order
// within a tier: an equally-ranked later error never displaces an earlier one.
// The result is by construction the same diagnostic rankDiagnostics puts at
// index 0 whenever any error is present.
func headlineErrorDiagnostic(diags []Diagnostic) *Diagnostic {
	var best *Diagnostic
	for i := range diags {
		if diags[i].Severity != SeverityError {
			continue
		}
		if best == nil || diagnosticOutranks(diags[i], *best) {
			d := diags[i]
			best = &d
		}
	}
	return best
}
