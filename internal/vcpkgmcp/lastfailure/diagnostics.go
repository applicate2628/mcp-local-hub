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

// DetectInterrupted reports whether content carries a user-interrupt marker as
// a WHOLE LINE. A "FAILED:" line in the same log must NOT be reported as a real
// build failure when this is true — the build was stopped, not broken.
//
// See interruptMarkers for the producer evidence and for why a whole-file
// substring scan was the wrong predicate.
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
		trimmed := string(bytes.TrimSpace(line))
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
		line := strings.TrimRight(scanner.Text(), "\r")
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
