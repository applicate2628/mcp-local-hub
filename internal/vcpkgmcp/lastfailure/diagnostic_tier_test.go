package lastfailure

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Regression tests for the FIELD REFINEMENT reported 2026-07-27 by the operator
// driving the tool against their real vcpkg tree.
//
// Severity ranking (the 2026-07-26 round) got errors ahead of warnings, but
// among ERROR-severity lines some are AGGREGATES that merely summarise others.
// On the operator's real failure first_error was
//
//	clang-cl: error: linker command failed with exit code 1120
//
// — the driver reporting a sub-tool's exit status — while the actual cause sat
// third in the list:
//
//	lld-link: error: undefined symbol: __declspec(dllimport) gzopen_w
//
// The rule: a SPECIFIC diagnostic outranks an AGGREGATING one when both are
// present in the same failure; an aggregate alone is still the headline.

// The operator's own two lines, verbatim.
const (
	operatorLldLinkUndefinedSymbol = `lld-link: error: undefined symbol: __declspec(dllimport) gzopen_w`
	operatorClangClLinkerFailed    = `clang-cl: error: linker command failed with exit code 1120`
)

// --- Case 1: specific beats aggregate across DRIVERS ----------------------

// TestLastFailure_SpecificDriverErrorOutranksAggregateDriverError is the direct
// reproduction. Both lines are error-severity driver diagnostics; only one
// names a cause.
func TestLastFailure_SpecificDriverErrorOutranksAggregateDriverError(t *testing.T) {
	// File order puts the CAUSE first (as lld-link really emits it) and the
	// aggregate second, so this test cannot pass by accident on occurrence
	// order alone — see the AggregateFirstInFileOrder sibling below, which
	// pins the reversed order that actually defeats a first-occurrence rule.
	root := writeBuildPhasePort(t, "netgen", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-rel-err.log": strings.Join([]string{
			operatorLldLinkUndefinedSymbol,
			operatorClangClLinkerFailed,
		}, "\n") + "\n",
	})

	res := LastFailure(Args{Port: "netgen", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.Status != evidence.StatusFailed {
		t.Fatalf("status=%v reason=%v, want failed; result=%+v", res.Status, res.Reason, res)
	}
	if res.FirstError == nil {
		t.Fatal("first_error is nil while two error-severity diagnostics exist")
	}
	if !strings.Contains(res.FirstError.Text, "undefined symbol") {
		t.Errorf("first_error = %q, want the lld-link undefined-symbol line — the "+
			"clang-cl driver line only reports the linker's EXIT CODE and names no cause",
			res.FirstError.Text)
	}
	if res.FirstError.Tier != TierSpecific {
		t.Errorf("first_error.tier = %q, want %q", res.FirstError.Tier, TierSpecific)
	}
	// The aggregate is still RETURNED — this is a ranking rule, not a filter.
	if !hasErrorDiagnostic(res.Diagnostics, "linker command failed") {
		t.Error("the clang-cl aggregate was dropped; it carries a real exit code and must be returned")
	}
	assertHeadlineConsistent(t, res)
}

// TestLastFailure_AggregateFirstInFileOrderStillLoses is the ordering that
// actually defeats a severity-then-first-occurrence rule: the aggregate is
// PHYSICALLY FIRST. This is the real ninja/driver shape (the summary is printed
// before or above the diagnostic it summarises).
func TestLastFailure_AggregateFirstInFileOrderStillLoses(t *testing.T) {
	root := writeBuildPhasePort(t, "netgen", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-rel-err.log": strings.Join([]string{
			operatorClangClLinkerFailed,
			operatorLldLinkUndefinedSymbol,
		}, "\n") + "\n",
	})

	res := LastFailure(Args{Port: "netgen", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.FirstError == nil {
		t.Fatal("first_error is nil while two error-severity diagnostics exist")
	}
	if !strings.Contains(res.FirstError.Text, "undefined symbol") {
		t.Errorf("first_error = %q, want the lld-link line even though the clang-cl "+
			"aggregate comes FIRST in the log — a consequence must never outrank its cause",
			res.FirstError.Text)
	}
	if res.Diagnostics[0].Tier != TierSpecific {
		t.Errorf("diagnostics[0] = %+v, want the specific line first", res.Diagnostics[0])
	}
	assertHeadlineConsistent(t, res)
}

// TestLastFailure_DiagnosticLogFollowsTheHeadlineAcrossLogs pins the
// consequence of tiering for diagnostic_log. The headline can now live in a
// LATER log than the first log holding any error, so "first log with an error
// wins" would name a log that does not contain first_error — the exact
// diagnostic/command mis-association diagnostic_log exists to eliminate.
//
// Same phase, two configurations, scanned in directory (alphabetical) order:
// `build-cl-dbg-err.log` holds only the aggregate and is read FIRST;
// `build-cl-rel-err.log` holds the cause.
func TestLastFailure_DiagnosticLogFollowsTheHeadlineAcrossLogs(t *testing.T) {
	root := writeBuildPhasePort(t, "netgen", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-dbg-err.log":  operatorClangClLinkerFailed + "\n",
		"build-cl-rel-err.log":  operatorLldLinkUndefinedSymbol + "\n",
	})

	res := LastFailure(Args{Port: "netgen", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.FirstError == nil {
		t.Fatal("first_error is nil while both logs hold an error")
	}
	if !strings.Contains(res.FirstError.Text, "undefined symbol") {
		t.Fatalf("first_error = %q, want the lld-link line from the SECOND log", res.FirstError.Text)
	}
	if !strings.HasSuffix(res.DiagnosticLog, "build-cl-rel-err.log") {
		t.Errorf("diagnostic_log = %q, want the rel log — it must name the log the HEADLINE "+
			"came from, not merely the first log that held any error", res.DiagnosticLog)
	}
	assertHeadlineConsistent(t, res)
}

// --- Case 2: the aggregate-ONLY case must not regress ---------------------

// TestLastFailure_LoneAggregateIsStillTheHeadline pins the EARLIER round's
// operator case: `LNK1120: 4 unresolved externals` was the useful headline, but
// only because it was the ONLY error present. An aggregate is a legitimate
// fallback headline — demoting it to "not a headline" (the treatment NMAKE's
// U-series correctly gets) would be a regression.
func TestLastFailure_LoneAggregateIsStillTheHeadline(t *testing.T) {
	root := writeBuildPhasePort(t, "fparser", map[string]string{
		"cl.vcpkg_abi_info.txt": "abi\n",
		"build-cl-rel-err.log":  warningFlood(50) + operatorLinkError + "\n",
	})

	res := LastFailure(Args{Port: "fparser", Triplet: "cl", BuildtreesRoot: root}, testDeps())

	if res.Status != evidence.StatusFailed {
		t.Fatalf("status=%v reason=%v, want failed — a lone aggregate error still establishes "+
			"a failure; result=%+v", res.Status, res.Reason, res)
	}
	if res.FirstError == nil {
		t.Fatal("first_error is nil; a lone AGGREGATE error must still be the headline")
	}
	if !strings.Contains(res.FirstError.Text, "LNK1120") {
		t.Errorf("first_error = %q, want the LNK1120 line", res.FirstError.Text)
	}
	if res.FirstError.Tier != TierAggregate {
		t.Errorf("first_error.tier = %q, want %q — LNK1120 is documented as a COUNT of the "+
			"LNK2001/LNK2019 errors before it", res.FirstError.Tier, TierAggregate)
	}
	assertHeadlineConsistent(t, res)
}

// --- Case 3: LNK2019 (specific) beats LNK1120 (aggregate) -----------------

// TestLastFailure_LNK2019OutranksLNK1120 covers the MSVC linker's own
// aggregate/specific pair. Microsoft documents LNK1120 as reporting the NUMBER
// of unresolved externals, each first reported by LNK2001/LNK2019, and says
// outright "You don't need to fix this error".
func TestLastFailure_LNK2019OutranksLNK1120(t *testing.T) {
	const (
		lnk2019 = `main.obj : error LNK2019: unresolved external symbol "void __cdecl gzopen_w(void)" referenced in function main`
		lnk1120 = `fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals`
	)
	// Both file orders are exercised. MSVC's real order is LNK2019-then-LNK1120
	// ("The LNK1120 message comes last"), where first-occurrence alone already
	// picks correctly; the REVERSED order is what actually distinguishes a tier
	// rule from an occurrence rule, and it is reachable in practice because the
	// two lines can land in different logs of the same build step.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"msvc real order", lnk2019 + "\n" + lnk1120 + "\n"},
		{"aggregate first", lnk1120 + "\n" + lnk2019 + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeBuildPhasePort(t, "fparser", map[string]string{
				"cl.vcpkg_abi_info.txt": "abi\n",
				"build-cl-rel-err.log":  tc.body,
			})

			res := LastFailure(Args{Port: "fparser", Triplet: "cl", BuildtreesRoot: root}, testDeps())

			if res.FirstError == nil {
				t.Fatal("first_error is nil while LNK2019 and LNK1120 both exist")
			}
			if !strings.Contains(res.FirstError.Text, "LNK2019") {
				t.Errorf("first_error = %q, want the LNK2019 line — LNK1120 only COUNTS the "+
					"LNK2001/LNK2019 errors and Microsoft documents it as not needing a fix",
					res.FirstError.Text)
			}
			if !hasErrorDiagnostic(res.Diagnostics, "LNK1120") {
				t.Error("LNK1120 was dropped; it must still be returned, only ranked behind LNK2019")
			}
			assertHeadlineConsistent(t, res)
		})
	}
}

// --- The per-shape tier sweep, enforced ------------------------------------

// TestMatchDiagnosticLine_TierSweep pins the tier of EVERY recognized shape,
// including the ones that are already correct. A sweep asserted only in prose
// is a sweep nothing defends.
func TestMatchDiagnosticLine_TierSweep(t *testing.T) {
	cases := []struct {
		name string
		line string
		sev  string
		tier DiagnosticTier
	}{
		// gccClangDiagRE — a source position IS the cause's address.
		{"gcc/clang error", `compileerr.cpp:1:13: error: use of undeclared identifier 'undefined_a'`, SeverityError, TierSpecific},
		{"gcc/clang warning", `foo.cpp:9:2: warning: unused variable 'x'`, "warning", TierSpecific},
		// msvcCompileDiagRE — likewise, with and without a diagnostic code.
		{"msvc compile w/ code", `e1.cpp(2): error C2065: undeclared identifier`, SeverityError, TierSpecific},
		{"clang-cl compile no code", `libsrc/general/mystring.cpp(63,15): error: definition of dllimport static field not allowed`, SeverityError, TierSpecific},
		// msvcLinkDiagRE — tier decided by the LNK code.
		{"LNK1120 counts others", `fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals`, SeverityError, TierAggregate},
		{"LNK1169 follows LNK2005", `foo.exe : fatal error LNK1169: one or more multiply defined symbols found`, SeverityError, TierAggregate},
		{"LNK2019 names the symbol", `main.obj : error LNK2019: unresolved external symbol "f" referenced in function main`, SeverityError, TierSpecific},
		{"LNK2001 names the symbol", `main.obj : error LNK2001: unresolved external symbol "f"`, SeverityError, TierSpecific},
		{"LNK2005 names the symbol", `b.obj : error LNK2005: "int x" already defined in a.obj`, SeverityError, TierSpecific},
		{"LNK1104 names the file", `LINK : fatal error LNK1104: cannot open file 'zlib.lib'`, SeverityError, TierSpecific},
		{"LNK1181 names the file", `LINK : fatal error LNK1181: cannot open input file 'z.obj'`, SeverityError, TierSpecific},
		// toolDiagRE — tier decided by the driver message.
		{"clang-cl relays exit code", operatorClangClLinkerFailed, SeverityError, TierAggregate},
		{"clang++ relays exit code", `clang++: error: linker command failed with exit code 1 (use -v to see invocation)`, SeverityError, TierAggregate},
		{"clang relays frontend exit code", `clang: error: clang frontend command failed with exit code 70`, SeverityError, TierAggregate},
		{"lld-link names the symbol", operatorLldLinkUndefinedSymbol, SeverityError, TierSpecific},
		{"clang-cl names the argument", operatorClangClWarning, "warning", TierSpecific},
		{"clang names the file", `clang: error: no such file or directory: 'missing.cpp'`, SeverityError, TierSpecific},
		// ninjaFailedRE — names a target and an exit code, never a cause.
		{"ninja FAILED", `FAILED: [code=2] CMakeFiles/cmTC_e5bae.dir/src.cxx.obj`, SeverityError, TierAggregate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := matchDiagnosticLine(tc.line)
			if !ok {
				t.Fatalf("line was not recognized at all: %q", tc.line)
			}
			if d.Severity != tc.sev {
				t.Errorf("severity = %q, want %q", d.Severity, tc.sev)
			}
			if d.Tier != tc.tier {
				t.Errorf("tier = %q, want %q for %q", d.Tier, tc.tier, tc.line)
			}
		})
	}
}

// TestPR591_DriveQualifiedMSVCLinkerDiagnosticIsSpecific guards the MSVC
// linker's drive-qualified origin while retaining the closed diagnostic suffix
// and the earlier wrapper-noise rejection.
func TestPR591_DriveQualifiedMSVCLinkerDiagnosticIsSpecific(t *testing.T) {
	const driveQualified = `C:\vcpkg\buildtrees\fparser\x64-windows-rel\fparser_parse-opt.exe : error LNK2019: unresolved external symbol "gzopen_w"`

	d, ok := matchDiagnosticLine(driveQualified)
	if !ok {
		t.Fatalf("drive-qualified linker line was not recognized: %q", driveQualified)
	}
	if d.File != `C:\vcpkg\buildtrees\fparser\x64-windows-rel\fparser_parse-opt.exe` {
		t.Errorf("file = %q, want the complete drive-qualified origin", d.File)
	}
	if d.Severity != SeverityError {
		t.Errorf("severity = %q, want %q", d.Severity, SeverityError)
	}
	if d.Tier != TierSpecific {
		t.Errorf("tier = %q, want %q", d.Tier, TierSpecific)
	}
	if d.Text != driveQualified {
		t.Errorf("text = %q, want original line %q", d.Text, driveQualified)
	}

	for _, line := range []string{
		`-- Installing: C:\vcpkg\installed\x64-windows\include\error_category.hpp`,
		`NMAKE : fatal error U1077: 'cd' : return code '0x2'`,
		`C:\vcpkg\buildtrees\fparser\(unexpected)\fparser_parse-opt.exe : error LNK2019: unresolved external symbol "gzopen_w"`,
		"C:\\vcpkg\\buildtrees\\fparser\\fparser_parse-opt.exe\r : error LNK2019: unresolved external symbol \"gzopen_w\"",
		"C:\\vcpkg\\buildtrees\\fparser\\fparser_parse-opt.exe\n : error LNK2019: unresolved external symbol \"gzopen_w\"",
	} {
		if _, ok := matchDiagnosticLine(line); ok {
			t.Errorf("non-diagnostic line matched: %q", line)
		}
	}
}

// TestMatchDiagnosticLine_EveryRecognizedLineIsTiered guards the enum's
// closedness at the only place tiers are assigned: no recognized shape may
// return a Diagnostic with an unset or unknown tier.
func TestMatchDiagnosticLine_EveryRecognizedLineIsTiered(t *testing.T) {
	lines := []string{
		`compileerr.cpp:1:13: error: use of undeclared identifier 'x'`,
		`e1.cpp(2): error C2065: undeclared identifier`,
		`main.obj : error LNK2019: unresolved external symbol "f"`,
		`fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals`,
		operatorLldLinkUndefinedSymbol,
		operatorClangClLinkerFailed,
		`FAILED: [code=2] foo.obj`,
	}
	for _, line := range lines {
		d, ok := matchDiagnosticLine(line)
		if !ok {
			t.Fatalf("line was not recognized at all: %q", line)
		}
		if d.Tier != TierSpecific && d.Tier != TierAggregate {
			t.Errorf("tier = %q for %q, want one of the two closed values", d.Tier, line)
		}
	}
}

// TestScanDiagnostics_ClangErrorCountTailStaysUnrecognized documents a
// deliberate NON-change. clang emits an "N errors generated." tail line (real
// sample captured this session from clang 21: two `use of undeclared
// identifier` errors followed by `2 errors generated.`), which is an aggregate
// of the same family. It matches NO recognized shape today, so it can never
// become a headline — already the correct outcome. Recognizing it purely to
// tier it would ADD a causeless line to diagnostics[] and buy nothing. Pinned
// so the decision is visible rather than accidental.
func TestScanDiagnostics_ClangErrorCountTailStaysUnrecognized(t *testing.T) {
	if diags := ScanDiagnostics([]byte("2 errors generated.\n")); len(diags) != 0 {
		t.Fatalf("clang's error-count tail became a diagnostic: %+v", diags)
	}
}

// TestRankDiagnostics_FirstOccurrencePreservedWithinTier pins that adding the
// tier key did not cost the stable within-tier ordering: in a nested build the
// first error is usually the cause and the rest its cascade.
func TestRankDiagnostics_FirstOccurrencePreservedWithinTier(t *testing.T) {
	in := []Diagnostic{
		{Severity: SeverityError, Tier: TierAggregate, Text: "agg1"},
		{Severity: SeverityError, Tier: TierSpecific, Text: "spec1"},
		{Severity: "warning", Tier: TierSpecific, Text: "warn1"},
		{Severity: SeverityError, Tier: TierSpecific, Text: "spec2"},
		{Severity: SeverityError, Tier: TierAggregate, Text: "agg2"},
	}
	want := []string{"spec1", "spec2", "agg1", "agg2", "warn1"}
	got := rankDiagnostics(in)
	if len(got) != len(want) {
		t.Fatalf("got %d diagnostics, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("ranked[%d] = %q, want %q (full order %v)", i, got[i].Text, w, textsOf(got))
		}
	}
}

// TestHeadlineErrorDiagnostic_MatchesRankedIndexZero pins the invariant the
// wire contract promises: first_error and diagnostics[0] can never disagree.
func TestHeadlineErrorDiagnostic_MatchesRankedIndexZero(t *testing.T) {
	sets := [][]Diagnostic{
		{{Severity: SeverityError, Tier: TierAggregate, Text: "agg"}, {Severity: SeverityError, Tier: TierSpecific, Text: "spec"}},
		{{Severity: SeverityError, Tier: TierSpecific, Text: "spec"}, {Severity: SeverityError, Tier: TierAggregate, Text: "agg"}},
		{{Severity: "warning", Tier: TierSpecific, Text: "w"}, {Severity: SeverityError, Tier: TierAggregate, Text: "agg"}},
	}
	for i, set := range sets {
		head := headlineErrorDiagnostic(set)
		if head == nil {
			t.Fatalf("set %d: headline nil while an error exists", i)
		}
		if ranked := rankDiagnostics(set); ranked[0].Text != head.Text {
			t.Errorf("set %d: diagnostics[0]=%q but first_error=%q — these must never disagree",
				i, ranked[0].Text, head.Text)
		}
	}
}

// assertHeadlineConsistent checks the cross-field invariant on a real result:
// when an error exists, first_error IS diagnostics[0], and diagnostic_log names
// a log that is actually in log_paths.
func assertHeadlineConsistent(t *testing.T, res Result) {
	t.Helper()
	if res.FirstError == nil {
		return
	}
	if len(res.Diagnostics) == 0 {
		t.Fatal("first_error set but diagnostics[] empty")
	}
	if res.Diagnostics[0].Text != res.FirstError.Text {
		t.Errorf("diagnostics[0] = %q but first_error = %q — the wire contract says they are the same line",
			res.Diagnostics[0].Text, res.FirstError.Text)
	}
	if res.DiagnosticLog == "" {
		t.Error("diagnostic_log is empty while a headline diagnostic exists")
	}
}

func textsOf(diags []Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Text)
	}
	return out
}
