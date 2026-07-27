package lastfailure

import (
	"fmt"
	"strings"
	"testing"
)

// The per-log budget must split along EVERY key diagnosticOutranks ranks by,
// or one side of a key starves the other — and the starved side is by
// definition the one ranking would have preferred.
//
// Splitting on severity alone (the 2026-07-26 fix) left AGGREGATE and SPECIFIC
// errors sharing one budget, so the identical field failure reappeared one
// level in. ninja emits one `FAILED: [code=N] <target>` per failing edge, and
// each is an error-severity TierAggregate; a wide parallel build — or any
// keep-going build — produces dozens before the log's later output. Those
// spent the whole error budget, and the trailing `lld-link: error: undefined
// symbol: X` that actually names the cause was dropped, so
// headlineErrorDiagnostic returned an aggregate and the operator was handed
// "FAILED: <target>" instead of the undefined symbol.
func TestScanDiagnostics_AggregateFloodCannotStarveTheSpecificError(t *testing.T) {
	const cause = "lld-link: error: undefined symbol: gzopen_w"

	var b strings.Builder
	for i := 0; i < maxDiagnosticsPerLog+10; i++ {
		fmt.Fprintf(&b, "FAILED: [code=1] CMakeFiles/flood.dir/obj%d.obj\n", i)
	}
	b.WriteString(cause + "\n")

	// Preconditions: the flood really is error-severity AGGREGATE and the
	// cause really is error-severity SPECIFIC, or the fixture proves nothing.
	flood, ok := matchDiagnosticLine("FAILED: [code=1] CMakeFiles/flood.dir/obj0.obj")
	if !ok || flood.Severity != SeverityError || flood.Tier != TierAggregate {
		t.Fatalf("precondition failed: the flood line must be an error-severity AGGREGATE; got ok=%v sev=%q tier=%q",
			ok, flood.Severity, flood.Tier)
	}
	specific, ok := matchDiagnosticLine(cause)
	if !ok || specific.Severity != SeverityError || specific.Tier != TierSpecific {
		t.Fatalf("precondition failed: the cause line must be an error-severity SPECIFIC; got ok=%v sev=%q tier=%q",
			ok, specific.Severity, specific.Tier)
	}

	diags := ScanDiagnostics([]byte(b.String()))

	var kept *Diagnostic
	for i := range diags {
		if diags[i].Text == cause {
			kept = &diags[i]
		}
	}
	if kept == nil {
		t.Fatalf("the only cause-naming error in the log was dropped: %d leading AGGREGATE errors spent the shared "+
			"error budget, so the answer can no longer contain the undefined symbol at all (kept %d diagnostics)",
			maxDiagnosticsPerLog+10, len(diags))
	}

	headline := headlineErrorDiagnostic(diags)
	if headline == nil || headline.Text != cause {
		got := "<none>"
		if headline != nil {
			got = headline.Text
		}
		t.Fatalf("headline = %q, want %q — an aggregate outranking the specific error it merely summarises is the "+
			"exact outcome the tier ranking exists to prevent", got, cause)
	}
}

// Complement 1: the severity split the 2026-07-26 fix established must survive
// the tier split — a trailing error is still unloseable to leading warnings.
func TestScanDiagnostics_WarningFloodStillCannotStarveTheError(t *testing.T) {
	const cause = "fparser_parse-opt.exe : fatal error LNK1120: 4 unresolved externals"

	var b strings.Builder
	for i := 0; i < maxDiagnosticsPerLog+10; i++ {
		b.WriteString("clang-cl: warning: unknown argument ignored in clang-cl: '-fopenmp'\n")
	}
	b.WriteString(cause + "\n")

	diags := ScanDiagnostics([]byte(b.String()))
	if !ContainsFailureDiagnostic(diags) {
		t.Fatalf("a warning flood buried the trailing error again, so the tool would report "+
			"unknown(no_failure_diagnostic) for a failure that plainly happened (kept %d diagnostics)", len(diags))
	}
}

// Complement 2: the budget still BOUNDS the answer. Widening it per cell must
// not turn it into no cap at all — the point of the ceiling is that an
// adversarial or pathologically noisy log cannot inflate the result.
func TestScanDiagnostics_BudgetIsStillBounded(t *testing.T) {
	var b strings.Builder
	// Every cell flooded well past its own budget.
	for i := 0; i < maxDiagnosticsPerLog*4; i++ {
		fmt.Fprintf(&b, "FAILED: [code=1] agg-error-%d.obj\n", i)                      // error   / aggregate
		fmt.Fprintf(&b, "lld-link: error: undefined symbol: sym%d\n", i)               // error   / specific
		fmt.Fprintf(&b, "clang-cl: warning: unknown argument ignored: '-flag%d'\n", i) // warning / specific
		fmt.Fprintf(&b, "x%d.exe : warning LNK1120: %d unresolved externals\n", i, i)  // warning / aggregate
	}

	diags := ScanDiagnostics([]byte(b.String()))
	max := severityBudgetClasses * tierBudgetClasses * maxDiagnosticsPerLog
	if len(diags) > max {
		t.Fatalf("ScanDiagnostics returned %d diagnostics, over the documented per-log ceiling of %d", len(diags), max)
	}

	// ...and each populated cell is individually capped, so no single cell can
	// consume another's reservation.
	counts := map[[2]int]int{}
	for _, d := range diags {
		counts[[2]int{severityBudgetClass(d), tierRank(d.Tier)}]++
	}
	if len(counts) < 4 {
		t.Fatalf("fixture populated only %d cells, want at least 4 — the per-cell assertion below would be vacuous", len(counts))
	}
	for cell, n := range counts {
		if n > maxDiagnosticsPerLog {
			t.Errorf("cell (severity=%d, tier=%d) kept %d, over its per-cell budget of %d", cell[0], cell[1], n, maxDiagnosticsPerLog)
		}
	}
}
