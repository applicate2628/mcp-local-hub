package lastfailure

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
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
		`^(?P<file>[^()\r\n]+?)[ \t]*:[ \t]+(?P<sev>fatal error|error|warning)[ \t]+(?P<code>LNK[0-9]+)[ \t]*:[ \t]*(?P<msg>[^\r\n]+)$`)

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
		`^(?P<file>clang-cl|lld-link|clang\+\+|clang|link|cl|ld|gcc|g\+\+|collect2)\s*:\s+(?P<sev>fatal error|error|warning)\s*:\s*(?P<msg>.+)$`)

	gnuLDLocationRE = regexp.MustCompile(`^(?P<file>(?:[A-Za-z]:)?[/\\][^:\r\n]*[/\\]ld(?:\.exe)?|ld(?:\.exe)?)\s*:\s*(?P<msg>[^\r\n]+)$`)
	gnuLDCauseRE    = regexp.MustCompile(`(?i)(?:undefined reference|multiple definition|cannot find|cannot open output file|relocation truncated|file format not recognized)`)

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
	ninjaErrorRE  = regexp.MustCompile(`^ninja\s*:\s*error\s*:\s*(?P<msg>.+)$`)
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
var (
	aggregateDriverMsgRE   = regexp.MustCompile(`^[A-Za-z][A-Za-z ]* command failed with exit code \d+`)
	aggregateCollect2MsgRE = regexp.MustCompile(`^ld returned \d+ exit status$`)
)

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
	if aggregateDriverMsgRE.MatchString(msg) || aggregateCollect2MsgRE.MatchString(msg) {
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
	var normalizer logLineNormalizer
	normalizer.reset()
	write := func(value byte) { b.WriteByte(value) }
	for i := 0; i < len(line); i++ {
		normalizer.feedByte(line[i], write)
	}
	normalizer.finish(write)
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

// logLineNormalizer is the single stateful owner of normalizeLogLine's byte
// rules. The whole-line compatibility path and the bounded streaming interrupt
// recognizer feed the same owner, so chunk boundaries cannot create a second
// ANSI/BOM interpretation.
type logLineNormalizer struct {
	escape logLineEscapeState
	bom    uint8
}

type logLineEscapeState uint8

const (
	logLineEscapeNone logLineEscapeState = iota
	logLineEscapeAfter
	logLineEscapeCSIParams
	logLineEscapeCSIIntermediates
	logLineEscapeString
	logLineEscapeStringAfterESC
	logLineEscapeGeneral
)

func (n *logLineNormalizer) reset() {
	n.escape = logLineEscapeNone
	n.bom = 0
}

func (n *logLineNormalizer) feed(data []byte, emit func(byte)) {
	for _, value := range data {
		n.feedByte(value, emit)
	}
}

func (n *logLineNormalizer) feedByte(value byte, emit func(byte)) {
	if n.escape == logLineEscapeNone {
		switch n.bom {
		case 1:
			if value == 0xbb {
				n.bom = 2
				return
			}
			n.bom = 0
			n.emitVisible(0xef, emit)
			n.feedByte(value, emit)
			return
		case 2:
			n.bom = 0
			if value == 0xbf {
				return
			}
			n.emitVisible(0xef, emit)
			n.emitVisible(0xbb, emit)
			n.feedByte(value, emit)
			return
		}
		if value == 0xef {
			n.bom = 1
			return
		}
	}

	switch n.escape {
	case logLineEscapeAfter:
		switch value {
		case '[':
			n.escape = logLineEscapeCSIParams
		case ']', 'P', 'X', '^', '_':
			n.escape = logLineEscapeString
		default:
			if value >= 0x20 && value <= 0x2f {
				n.escape = logLineEscapeGeneral
				return
			}
			if value >= 0x30 && value <= 0x7e {
				n.escape = logLineEscapeNone
				return
			}
			n.escape = logLineEscapeNone
			n.emitVisible(value, emit)
		}
		return
	case logLineEscapeCSIParams:
		if value >= 0x30 && value <= 0x3f {
			return
		}
		if value >= 0x20 && value <= 0x2f {
			n.escape = logLineEscapeCSIIntermediates
			return
		}
		if value >= 0x40 && value <= 0x7e {
			n.escape = logLineEscapeNone
			return
		}
		n.escape = logLineEscapeNone
		n.emitVisible(value, emit)
		return
	case logLineEscapeCSIIntermediates:
		if value >= 0x20 && value <= 0x2f {
			return
		}
		if value >= 0x40 && value <= 0x7e {
			n.escape = logLineEscapeNone
			return
		}
		n.escape = logLineEscapeNone
		n.emitVisible(value, emit)
		return
	case logLineEscapeString:
		if value == 0x07 {
			n.escape = logLineEscapeNone
		} else if value == 0x1b {
			n.escape = logLineEscapeStringAfterESC
		}
		return
	case logLineEscapeStringAfterESC:
		if value == '\\' {
			n.escape = logLineEscapeNone
		} else {
			n.escape = logLineEscapeString
		}
		return
	case logLineEscapeGeneral:
		if value >= 0x20 && value <= 0x2f {
			return
		}
		if value >= 0x30 && value <= 0x7e {
			n.escape = logLineEscapeNone
			return
		}
		n.escape = logLineEscapeNone
		n.emitVisible(value, emit)
		return
	}

	if value == 0x1b {
		n.escape = logLineEscapeAfter
		return
	}
	n.emitVisible(value, emit)
}

func (n *logLineNormalizer) emitVisible(value byte, emit func(byte)) {
	if (value < 0x20 && value != '\t') || value == 0x7f {
		return
	}
	emit(value)
}

func (n *logLineNormalizer) finish(emit func(byte)) {
	// A partial BOM is ordinary UTF-8 data, whereas an unterminated escape
	// sequence is display control through end of line and remains discarded.
	switch n.bom {
	case 1:
		n.emitVisible(0xef, emit)
	case 2:
		n.emitVisible(0xef, emit)
		n.emitVisible(0xbb, emit)
	}
	n.bom = 0
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
		if isInterruptLogLine(line) {
			return true
		}
	}
	return false
}

func isInterruptLogLine(line []byte) bool {
	// Normalize BEFORE trimming: stripping a trailing reset sequence or a
	// leading BOM can expose whitespace the trim then has to remove.
	trimmed := strings.TrimSpace(normalizeLogLine(string(line)))
	if trimmed == "" {
		return false
	}
	for _, marker := range interruptMarkers {
		if trimmed == marker {
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
	if m := gnuLDLocationRE.FindStringSubmatch(line); m != nil && gnuLDCauseRE.MatchString(m[gnuLDLocationRE.SubexpIndex("msg")]) {
		severity := SeverityError
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(m[gnuLDLocationRE.SubexpIndex("msg")])), "warning:") {
			severity = "warning"
		}
		return Diagnostic{
			File:     strings.TrimSpace(m[gnuLDLocationRE.SubexpIndex("file")]),
			Severity: severity,
			Tier:     TierSpecific,
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
	if m := ninjaErrorRE.FindStringSubmatch(line); m != nil {
		return Diagnostic{
			File:     "ninja",
			Severity: SeverityError,
			Tier:     TierSpecific,
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
// Per-log ceiling consequence: maxDiagnosticsPerLog is spent per cell, so the
// worst-case returned count per log is cells*maxDiagnosticsPerLog rather than
// 2*maxDiagnosticsPerLog.
//
// RESOLVED 2026-07-27 (this was an ASSUMPTION (UNVERIFIED) naming two resolving
// steps — "the tool owner stating a response size budget, or measuring a real
// worst-case result". Both were done):
//
//   - MEASURED. The reachable ceiling is 200 per log, not the 300 the constants
//     imply: tierBudgetClasses is 3, but matchDiagnosticLine assigns a tier on
//     every branch, so the unset row is dead and only 4 of the 6 cells can fill.
//     The figure that actually mattered was never the per-log one anyway — a
//     phase CONCATENATES its logs, so an install phase with N build
//     configurations returns 200*2N (measured: 800 diagnostics / 204 KB /
//     ~52k tokens at vcpkg's rel+dbg default; 3200 / 813 KB / ~208k tokens at 8).
//   - BUDGETED. The response now has a stated total ceiling — see
//     MaxResponseDiagnostics and its siblings, which bound the RESULT rather
//     than one log.
//
// So 50 per cell stays, and the reason is now positive rather than an
// assumption: with a total budget in place the per-log figure no longer sets the
// response size, and subdividing it would weaken the guarantee the 2026-07-26
// fix deliberately established — a trailing error must survive 50 leading noise
// lines.
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
	diags, _, _ := scanDiagnostics(content, maxDiagnosticsPerLog)
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
func scanDiagnostics(content []byte, perCellLimit int) ([]Diagnostic, int, error) {
	var out []Diagnostic
	dropped := 0
	// One budget per (severity class, tier) cell — see maxDiagnosticsPerLog.
	var budget [severityBudgetClasses][tierBudgetClasses]int
	for s := range budget {
		for t := range budget[s] {
			budget[s][t] = perCellLimit
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, phaseLogReadChunkBytes), defaultResponseLimits.logLineBytes)
	for scanner.Scan() {
		// normalizeLogLine is the SAME owner DetectInterrupted uses: every
		// shape below is anchored, so a colourized log would otherwise match
		// nothing and answer unknown(no_diagnostic_found) for a build that
		// plainly failed. It also decides what Diagnostic.Text carries on the
		// wire — see the normalizer's doc for that contract change.
		line := normalizeLFLogLine(scanner.Text())
		d, ok := matchDiagnosticLine(line)
		if !ok {
			continue
		}
		sev, tier := severityBudgetClass(d), tierRank(d.Tier)
		if budget[sev][tier] == 0 {
			dropped++
			continue
		}
		budget[sev][tier]--
		out = append(out, d)
	}
	return out, dropped, scanner.Err()
}

// --- Total response budget (single owner) --------------------------------
//
// # The gap this closes
//
// maxDiagnosticsPerLog bounds ONE log's contribution per ranking cell. Nothing
// bounded the RESULT. Two axes were unbounded, and both were measured this
// session rather than argued about:
//
//	AXIS 1 — count. LastFailure concatenates every log in the chosen PHASE
//	(lastfailure.go thisPhaseDiags). The install and patch phases carry one
//	out/err PAIR per build configuration / patch ordinal, so the phase total is
//	logs x 200 (four reachable cells x 50). Measured, MSVC-shaped lines:
//	   1 config (2 logs)   400 diagnostics   103 KB JSON   ~26k tokens
//	   2 configs (4 logs)  800 diagnostics   204 KB JSON   ~52k tokens   <- rel+dbg, the vcpkg default
//	   8 configs (16 logs) 3200 diagnostics  813 KB JSON  ~208k tokens
//	Note 200 per log, not the 300 the constants imply: tierBudgetClasses is 3
//	but matchDiagnosticLine always assigns a tier, so the unset row is dead.
//
//	AXIS 2 — one diagnostic's size. scanDiagnostics accepts a 4 MiB line
//	(its bufio buffer) and Diagnostic.Text was uncapped. Measured: a SINGLE
//	3 MiB diagnostic line produced a 6.00 MB response, ~1.57M tokens — double
//	the line, because the headline's text is emitted TWICE (diagnostics[0] and
//	first_error).
//
// For scale, the same measurement over every real fixture in testdata: 0-3
// diagnostics, longest Text 148 bytes, whole response 0.7-4.8 KB. The budget
// below is ~13x the largest realistic WHOLE response and ~28x the longest
// realistic line, so it bounds pathology without touching a real answer.
//
// # The three constants, one per distinct failure mode
//
// A single cap cannot do this: many lines, one enormous line, and many
// medium lines fail differently and need different bounds.
const (
	// MaxResponseDiagnostics bounds how many diagnostics one result carries.
	//
	// 200 is not arbitrary: it is exactly severityBudgetClasses(2) x the two
	// REACHABLE tiers x maxDiagnosticsPerLog(50), i.e. one log's own ceiling.
	// So a single-log phase can never be truncated at all, and the budget
	// binds only on the multi-log concatenation — the axis that was actually
	// unbounded. It also caps the per-entry JSON structural overhead (~120
	// bytes of field names), which no byte budget over TEXT can see.
	MaxResponseDiagnostics = 200
	// MaxDiagnosticTextBytes bounds ONE diagnostic's Text. Longest real
	// diagnostic across the fixture corpus: 148 bytes. 4 KiB keeps even a
	// pathological MSVC template-instantiation error intact while making the
	// scanner's 4 MiB line ceiling unreachable on the wire.
	MaxDiagnosticTextBytes = 4 << 10
	// MaxResponseDiagnosticBytes bounds the SUM of emitted diagnostic VARIABLE
	// cost — Text plus File, the two fields a diagnostic lifts out of the log.
	//
	// CORRECTED 2026-07-27: this charged len(Text) alone, which is the defect
	// that made the whole budget non-binding. Four of the five matched shapes
	// capture rest-of-line or head-of-line into File (only toolDiagRE's closed
	// driver allowlist does not), so a 3 MiB File was admitted free of charge
	// while the response reported itself as bounded. Charging the ENTRY's wire
	// cost rather than one of its fields is the fix: how the bytes divide
	// between the two fields is not something a budget should have an opinion
	// about. Largest realistic sum measured across the fixture corpus:
	// 203 bytes (148 Text + 55 File).
	MaxResponseDiagnosticBytes = 64 << 10

	// MaxWirePathBytes bounds ONE locator string: Diagnostic.File,
	// DiagnosticLog, a LogPaths or OverlayChain entry, FailedTarget, and the
	// paths inside Evidence.
	//
	// Measured across the whole fixture corpus: longest File 55 bytes, longest
	// path 146, longest overlay entry 40. Windows MAX_PATH is 260. 1 KiB is
	// ~4x MAX_PATH and ~19x the longest value any real fixture produced, so it
	// bounds pathology without touching a real locator — and it still leaves
	// room for the one legitimately multi-valued case, a ninja "FAILED:" line
	// naming several outputs.
	MaxWirePathBytes = 1 << 10
	// MaxWireCommandBytes bounds ONE command string: ExactCommand,
	// BuildCommand, and the commands inside Evidence.
	//
	// Grounded in the producer rather than in taste. These fields exist to be
	// PASTED INTO A SHELL, and Microsoft documents the command prompt's own
	// ceiling as 8191 characters (KB 830473, "Command prompt (Cmd. exe)
	// command-line string limitation"). A longer string could not have been
	// typed or run, so truncating above this cap can only ever damage a value
	// that was already not a runnable command — which is why bounding these
	// fields does not contradict their verbatim-reproducibility contract.
	// Longest real command measured: 522 bytes (the operator's own wrapper
	// sample), 134 bytes across the in-tree fixtures.
	MaxWireCommandBytes = 8 << 10
	// MaxWireListEntries bounds every repeated output list at its producer.
	// Omission is never silent: ResourceReport carries completeness and dropped
	// counts, while the diagnostic list retains its dedicated
	// DiagnosticsDropped contract.
	//
	// Measured: 11 log paths, 5 notes, 1 context source at the corpus maximum.
	MaxWireListEntries = 64
)

// truncationMarker is appended to any wire value that was cut, so the
// truncation is visible IN BAND. A caller reading one field must not have to
// correlate it with a note elsewhere in the document to learn the value is
// incomplete.
//
// It carries a second, load-bearing job on LOCATOR fields. A truncated path is
// not a shortened path, it is a DIFFERENT path — one an operator or an agent
// could open, or feed to another tool, without ever noticing. The marker makes
// that impossible to do silently: a value ending in "… [truncated, N more
// bytes]" contains a space, a non-ASCII ellipsis and a bracket, so it cannot
// be mistaken for, and cannot resolve as, a real filesystem path.
const truncationMarker = "… [truncated, %d more bytes]"

// --- Package-owned evidence shaping ---------------------------------------
//
// boundResponse is the final package-domain shaping pass after producer-side
// limits have already bounded work and retention. LastFailure routes every
// return through it, including early returns.
//
// It receives a finished Result and owns only last-failure evidence semantics:
// valid causal tuples, per-kind value caps, and ranked diagnostic count/byte
// budgets. It does not marshal or reduce the whole result. The shared
// publicresult boundary owns the exact encoded-size decision and invokes this
// package's PublicResultProjection when projection is required.
//
// Two layers, in order:
//
//  1. boundWireValues caps each value by KIND (prose / locator / command).
//     Its job is package-domain degradation quality: a capped field keeps its
//     useful prefix and says so in band.
//  2. applyResponseBudget spends the count and aggregate-byte budget over the
//     already-ranked, already-capped diagnostics.
func boundResponse(r Result) Result {
	r = validateCausality(r)
	textCut, valueCut := boundWireValues(&r)

	var dropped int
	r.Diagnostics, dropped = applyResponseBudget(r.Diagnostics)
	r.DiagnosticsDropped += dropped
	if dropped > 0 {
		r.Notes = append(r.Notes, NoteDiagnosticsTruncatedToBudget)
	}
	if textCut {
		r.Notes = append(r.Notes, NoteDiagnosticTextTruncated)
	}
	if valueCut {
		r.Notes = append(r.Notes, NoteResponseValueTruncated)
	}
	return r
}

func causalLogPaths(diagnosticLog string, paths []string, max int) []string {
	if diagnosticLog == "" {
		return capStrings(paths, max)
	}
	out := make([]string, 0, min(max, len(paths)+1))
	out = append(out, diagnosticLog)
	for _, path := range paths {
		if path == diagnosticLog {
			continue
		}
		if len(out) == max {
			break
		}
		out = append(out, path)
	}
	return out
}

func validateCausality(r Result) Result {
	if r.Status != Status("failed") {
		return r
	}
	valid := r.FirstError != nil && len(r.Diagnostics) > 0 &&
		r.Diagnostics[0] == *r.FirstError && r.DiagnosticLog != ""
	if valid {
		found := false
		for _, path := range r.LogPaths {
			if path == r.DiagnosticLog {
				found = true
				break
			}
		}
		valid = found
		if valid {
			r.LogPaths = causalLogPaths(r.DiagnosticLog, r.LogPaths, MaxWireListEntries)
		}
	}
	if valid {
		return r
	}
	r.Status = Status("unknown")
	r.Reason = ReasonCausalityInvariantViolation
	r.Phase = ""
	r.Notes = append(r.Notes, NoteCausalityInvariantViolated)
	return r
}

func capStrings(in []string, max int) []string {
	if len(in) <= max {
		return in
	}
	return in[:max]
}

// boundWireValues caps every string a Result lifts out of an unbounded log
// line, wrapper line or directory walk, by the KIND of value it is.
//
// The kind distinction is load-bearing and is why this is not one flat cap:
//
//   - PROSE (Diagnostic.Text) is useful truncated — the prefix is still the
//     diagnostic message — and is capped tightly, because two hundred of them
//     share one aggregate budget.
//   - a LOCATOR (a file, a target, a path, an overlay entry) is capped at a
//     path-shaped ceiling, and the in-band marker is what stops the truncated
//     value from being mistaken for a usable one.
//   - a COMMAND is capped at the ceiling above which it could not have been
//     run at all (see MaxWireCommandBytes), so the cap can only ever fire on a
//     string that was never a reproducible command.
//
// This function is the enumeration, and the enumeration is the part that has
// twice been incomplete. It is written out here, in one place, so that the
// list is reviewable and so that the reflective enforcement probe in
// wire_size_test.go has exactly one thing to disagree with. The shared
// publicresult boundary remains the field-agnostic encoded-size guarantee.
//
// Returns whether any diagnostic TEXT was cut and whether any other value was
// cut, kept apart because they are different facts for an operator — an
// incomplete message versus a locator that must not be used verbatim.
func boundWireValues(r *Result) (textCut, valueCut bool) {
	cutPath := func(s *string) {
		v, cut := truncateWireValue(*s, MaxWirePathBytes)
		*s, valueCut = v, valueCut || cut
	}
	cutCommand := func(s *string) {
		v, cut := truncateWireValue(*s, MaxWireCommandBytes)
		*s, valueCut = v, valueCut || cut
	}

	for i := range r.Diagnostics {
		d, tc, vc := truncateDiagnostic(r.Diagnostics[i])
		r.Diagnostics[i], textCut, valueCut = d, textCut || tc, valueCut || vc
	}
	if r.FirstError != nil {
		// The headline is emitted TWICE — here and as Diagnostics[0] — so the
		// caps have to bind on both, or one pathological line still reaches the
		// wire through this field.
		d, tc, vc := truncateDiagnostic(*r.FirstError)
		r.FirstError, textCut, valueCut = &d, textCut || tc, valueCut || vc
	}

	cutPath(&r.FailedTarget)
	cutPath(&r.DiagnosticLog)
	for i := range r.LogPaths {
		cutPath(&r.LogPaths[i])
	}
	for i := range r.OverlayChain {
		cutPath(&r.OverlayChain[i])
	}
	cutCommand(&r.ExactCommand)
	cutCommand(&r.BuildCommand)

	// Evidence re-emits the same commands and paths a second time. That second
	// emission site is precisely the kind of participant a field enumeration
	// misses: nothing in the Result's own field list names it.
	for i := range r.Evidence.Paths {
		cutPath(&r.Evidence.Paths[i])
	}
	for i := range r.Evidence.Commands {
		cutCommand(&r.Evidence.Commands[i])
	}
	for i := range r.Evidence.Locations {
		cutPath(&r.Evidence.Locations[i].File)
	}
	return textCut, valueCut
}

// truncateDiagnostic bounds one diagnostic's two variable-cost fields.
func truncateDiagnostic(d Diagnostic) (_ Diagnostic, textCut, fileCut bool) {
	d.Text, textCut = truncateWireValue(d.Text, MaxDiagnosticTextBytes)
	d.File, fileCut = truncateWireValue(d.File, MaxWirePathBytes)
	return d, textCut, fileCut
}

// truncateWireValue bounds one string, cutting on a RUNE boundary so a
// multi-byte character is never split into invalid UTF-8 (which would make the
// whole JSON body invalid, not just this field), and appending a marker naming
// how many bytes were removed.
//
// The cap applies to the ORIGINAL string; the emitted value is that prefix plus
// the short marker, which keeps the rule statable in one sentence.
func truncateWireValue(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	if max <= 0 {
		return "", true
	}
	// There is no truthful truncation marker that fits. Returning an empty
	// value keeps the hard byte cap and, unlike the old decrement loop, always
	// terminates for tiny residual budgets.
	if len(fmt.Sprintf(truncationMarker, len(s))) > max {
		return "", true
	}
	cut := max
	for {
		marker := fmt.Sprintf(truncationMarker, len(s)-cut)
		allowed := max - len(marker)
		if allowed < 0 {
			allowed = 0
		}
		if cut > allowed {
			cut = allowed
		}
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		finalMarker := fmt.Sprintf(truncationMarker, len(s)-cut)
		if cut+len(finalMarker) <= max {
			return s[:cut] + finalMarker, true
		}
		if cut == 0 {
			return "", true
		}
		cut--
	}
}

// applyResponseBudget spends the budget over an ALREADY-RANKED slice and
// returns what may be emitted, plus what that cost.
//
// # Drop policy, and the wire-contract change it forces
//
// The budget is spent in RANK order and the TAIL is dropped. That is the whole
// policy, and it is deliberately not a second one: rankDiagnostics
// (severity, then tier, then first occurrence) is already this package's single
// owner of "what matters most", so a budget that dropped by any other key would
// be a competing opinion about importance, and the class it dropped would by
// definition be the class ranking preferred.
//
// Diagnostic's wire contract used to read "Warnings are never dropped ...
// Aggregates are never dropped either". A total cap cannot keep that as
// written, so it is CHANGED, explicitly: what those sentences protected — no
// FILTERING policy may hide a class of evidence — still holds exactly, because
// nothing here drops a higher-ranked diagnostic in order to keep a lower-ranked
// one. What changes is that the list is TRUNCATED at a stated ceiling, and the
// truncation is REPORTED (Result.DiagnosticsDropped plus a Note), never silent.
// Affected surface: vcpkg_last_failure's diagnostics[] and notes[].
//
// The headline is never dropped: ranking puts it at index 0, and the first
// entry is admitted even when its own text alone exceeds the byte budget
// (truncated to MaxDiagnosticTextBytes first). A result that reported
// status=failed while omitting the error that established it would be a
// contradiction, not a budget.
//
// This CANNOT change a verdict: it runs inside boundResponse, which receives a
// finished Result. FirstError and the verdict switch are computed in
// lastFailure from the complete chosenDiags, in a different function, before
// this one is ever called.
//
// It expects an input boundWireValues has ALREADY capped. That ordering is the
// invariant that lets the byte budget be spent honestly — a diagnostic is
// charged the cost it will actually have on the wire, not the cost of the raw
// log line it came from.
func applyResponseBudget(ranked []Diagnostic) (out []Diagnostic, dropped int) {
	if len(ranked) == 0 {
		return ranked, 0
	}
	out = make([]Diagnostic, 0, min(len(ranked), MaxResponseDiagnostics))
	spent := 0
	for i, d := range ranked {
		if len(out) >= MaxResponseDiagnostics {
			dropped = len(ranked) - i
			break
		}
		// The ENTRY's variable cost, not one of its fields. Charging len(Text)
		// alone was the defect that made this budget non-binding: File is
		// rest-of-line or head-of-line in four of the five matched shapes, so
		// megabytes rode in free while the response reported itself bounded.
		cost := len(d.Text) + len(d.File)
		// i > 0 so the headline is always admitted; every later entry must fit
		// what is left of the byte budget.
		if i > 0 && spent+cost > MaxResponseDiagnosticBytes {
			dropped = len(ranked) - i
			break
		}
		spent += cost
		out = append(out, d)
	}
	return out, dropped
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
