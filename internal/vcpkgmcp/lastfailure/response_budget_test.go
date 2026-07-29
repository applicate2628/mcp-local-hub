package lastfailure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// cellFillingLog produces `per` lines in each of the FOUR reachable
// (severity, tier) budget cells, so one log contributes its full per-log
// ceiling and the phase concatenation is what the response budget has to bound.
func cellFillingLog(per int) string {
	var b strings.Builder
	for i := 0; i < per; i++ {
		fmt.Fprintf(&b, "C:\\src\\project\\module\\source_file_%04d.cpp(%d,15): error C2065: 'identifier_%04d': undeclared identifier\n", i, i+100, i)
		fmt.Fprintf(&b, "FAILED: [code=1] CMakeFiles/target.dir/module/source_file_%04d.cpp.obj\n", i)
		fmt.Fprintf(&b, "C:\\src\\project\\module\\source_file_%04d.cpp(%d,9): warning C4267: 'argument': conversion from 'size_t' to 'int'\n", i, i+200)
		fmt.Fprintf(&b, "libtarget_%04d.lib : warning LNK1120: %d unresolved externals\n", i, i)
	}
	return b.String()
}

// writeInstallPhasePort builds a port whose INSTALL phase carries one out/err
// log pair per build configuration — the real vcpkg shape (rel + dbg by
// default), and the axis along which a phase's diagnostics multiply.
func writeInstallPhasePort(t *testing.T, port string, nConfigs int, content string) (buildtreesRoot string) {
	t.Helper()
	root := t.TempDir()
	buildtreesRoot = filepath.Join(root, "buildtrees")
	portDir := filepath.Join(buildtreesRoot, port)
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgs := []string{"rel", "dbg", "relwithdebinfo", "minsizerel", "c5", "c6", "c7", "c8"}
	if nConfigs > len(cfgs) {
		t.Fatalf("fixture supports at most %d configurations", len(cfgs))
	}
	for n := 0; n < nConfigs; n++ {
		for _, stream := range []string{"out", "err"} {
			p := filepath.Join(portDir, fmt.Sprintf("install-cl-%s-%s.log", cfgs[n], stream))
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(portDir, "cl.vcpkg_abi_info.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return buildtreesRoot
}

func runLastFailure(t *testing.T, port, buildtreesRoot string) Result {
	t.Helper()
	return LastFailure(
		Args{Port: port, BuildtreesRoot: buildtreesRoot, Triplet: "cl"},
		Deps{FS: DefaultFS(), Getenv: func(string) string { return "" }},
	)
}

// The tool had NO total response cap. maxDiagnosticsPerLog bounds one log per
// ranking cell; nothing bounded the RESULT, and LastFailure concatenates every
// log in the chosen phase.
//
// Measured before the budget (this fixture, MSVC-shaped lines, marshaled as
// vcpkgserver/helpers.go does):
//
//	1 config ( 2 logs)   400 diagnostics   103 KB   ~26k tokens
//	2 configs( 4 logs)   800 diagnostics   204 KB   ~52k tokens  <- vcpkg's rel+dbg default
//	8 configs(16 logs)  3200 diagnostics   813 KB  ~208k tokens
//
// The count grows linearly with the number of build configurations, so there
// was no ceiling at all — only a coefficient.
func TestResponseBudget_BoundsThePhaseConcatenation(t *testing.T) {
	for _, nConfigs := range []int{1, 2, 4, 8} {
		root := writeInstallPhasePort(t, "bigport", nConfigs, cellFillingLog(maxDiagnosticsPerLog+20))
		res := runLastFailure(t, "bigport", root)

		if len(res.Diagnostics) > MaxResponseDiagnostics {
			t.Fatalf("%d configs: %d diagnostics returned, ceiling is %d — without a total cap the count grows "+
				"linearly with the number of build configurations", nConfigs, len(res.Diagnostics), MaxResponseDiagnostics)
		}
		if res.DiagnosticsDropped == 0 {
			t.Fatalf("%d configs: %d diagnostics returned with diagnostics_dropped=0 — truncation must never be "+
				"silent; the caller cannot tell a complete answer from a cut one", nConfigs, len(res.Diagnostics))
		}
		if !hasNote(res.Notes, NoteDiagnosticsTruncatedToBudget) {
			t.Fatalf("%d configs: notes %v missing %q", nConfigs, res.Notes, NoteDiagnosticsTruncatedToBudget)
		}

		// The verdict must be computed from the COMPLETE evidence: a budget
		// that could turn `failed` into unknown(no_failure_diagnostic) would
		// be manufacturing a denial out of its own output limit.
		if res.Status != Status("failed") {
			t.Fatalf("%d configs: status=%v reason=%v — the response budget must never change the verdict",
				nConfigs, res.Status, res.Reason)
		}
		if res.FirstError == nil {
			t.Fatalf("%d configs: first_error is nil on a failed verdict", nConfigs)
		}

		body, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d config(s) (%2d logs): %d diagnostics + %d dropped, %d bytes JSON",
			nConfigs, nConfigs*2, len(res.Diagnostics), res.DiagnosticsDropped, len(body))
	}
}

// The drop is by RANK from the TAIL, which is the wire-contract change: the
// old text promised warnings and aggregates were "never dropped". They can be
// now — but only ever in favour of something that OUTRANKS them, never the
// reverse, and the headline is never dropped at all.
//
// This is what makes the truncation safe: the entries lost are by construction
// the ones the tool's own ranking already judged least actionable.
func TestResponseBudget_DropsTheLowestRankedTailOnly(t *testing.T) {
	root := writeInstallPhasePort(t, "bigport", 4, cellFillingLog(maxDiagnosticsPerLog+20))
	res := runLastFailure(t, "bigport", root)

	if len(res.Diagnostics) == 0 {
		t.Fatal("no diagnostics returned")
	}
	// The kept set must be sorted, and no kept entry may be outranked by a
	// dropped one — which, since the input was ranked and the tail was cut, is
	// equivalent to: the kept prefix is itself in rank order.
	for i := 1; i < len(res.Diagnostics); i++ {
		if diagnosticOutranks(res.Diagnostics[i], res.Diagnostics[i-1]) {
			t.Fatalf("diagnostics[%d] %+v outranks diagnostics[%d] %+v — the budget must cut the TAIL of the "+
				"ranked list, never reorder or drop from the middle", i, res.Diagnostics[i], i-1, res.Diagnostics[i-1])
		}
	}
	// With this fixture there are 800 ranked diagnostics of which 400 are
	// error-severity, so a 200-entry budget must be spent entirely on errors —
	// warnings are dropped only because every kept entry outranks them.
	for i, d := range res.Diagnostics {
		if d.Severity != SeverityError {
			t.Fatalf("diagnostics[%d] has severity %q while %d error-severity diagnostics existed — a lower-ranked "+
				"entry was kept in preference to a higher-ranked one", i, d.Severity, 400)
		}
	}
	// The headline survives and still leads the list.
	if res.FirstError == nil {
		t.Fatal("first_error is nil")
	}
	if res.Diagnostics[0].Text != res.FirstError.Text {
		t.Fatalf("diagnostics[0] = %q but first_error = %q — the headline must never be dropped or displaced",
			res.Diagnostics[0].Text, res.FirstError.Text)
	}
	if res.FirstError.Tier != TierSpecific {
		t.Fatalf("first_error tier = %q, want %q — a cause-naming error outranks an aggregate, and the budget "+
			"must not disturb that", res.FirstError.Tier, TierSpecific)
	}
}

// The SECOND unbounded axis: one diagnostic's own size. scanDiagnostics accepts
// a 4 MiB line (its bufio buffer) and Text was uncapped.
//
// Measured before the budget: a single 3 MiB diagnostic line produced a 6.00 MB
// response (~1.57M tokens) — double the line, because the headline's text is
// emitted TWICE, as diagnostics[0] AND as first_error. Both had to be bounded;
// capping only the array would have left the whole line on the wire.
func TestResponseBudget_BoundsOneEnormousDiagnosticLine(t *testing.T) {
	huge := "C:\\src\\a.cpp(1,1): error C2065: " + strings.Repeat("X", 3<<20) + "\n"
	root := writeInstallPhasePort(t, "hugeline", 1, huge)
	res := runLastFailure(t, "hugeline", root)

	// Two, not one: writeInstallPhasePort writes an out AND an err log, and
	// the phase concatenates both.
	if len(res.Diagnostics) == 0 {
		t.Fatalf("no diagnostics returned; status=%v reason=%v", res.Status, res.Reason)
	}
	for i, d := range res.Diagnostics {
		if len(d.Text) > MaxDiagnosticTextBytes+len(truncationMarker)+32 {
			t.Fatalf("diagnostics[%d].Text is %d bytes, ceiling is %d", i, len(d.Text), MaxDiagnosticTextBytes)
		}
		if !strings.Contains(d.Text, "truncated") {
			t.Fatalf("diagnostics[%d].Text was cut without saying so IN BAND: %q...", i, d.Text[:80])
		}
	}
	if res.FirstError == nil {
		t.Fatal("first_error is nil")
	}
	if len(res.FirstError.Text) > MaxDiagnosticTextBytes+len(truncationMarker)+32 {
		t.Fatalf("first_error.Text is %d bytes — the headline is emitted twice, so the per-line cap must bind on "+
			"BOTH copies or one enormous line still reaches the wire", len(res.FirstError.Text))
	}
	if !hasNote(res.Notes, NoteDiagnosticTextTruncated) {
		t.Fatalf("notes %v missing %q", res.Notes, NoteDiagnosticTextTruncated)
	}

	body, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const softCeiling = 256 << 10
	if len(body) > softCeiling {
		t.Fatalf("whole response is %d bytes for ONE diagnostic; the point of the per-line cap is that this "+
			"cannot happen", len(body))
	}
	t.Logf("one 3 MiB diagnostic line -> %d-byte response", len(body))
}

// Truncation must not split a multi-byte rune, or the JSON body carries invalid
// UTF-8 and a strict consumer rejects the whole response rather than one field.
func TestResponseBudget_TextTruncationIsRuneSafe(t *testing.T) {
	// A 3-byte rune repeated so the cut lands mid-rune for at least one offset.
	for _, pad := range []int{0, 1, 2} {
		text := "a.cpp:1:1: error: " + strings.Repeat("x", pad) + strings.Repeat("\u4e2d", MaxDiagnosticTextBytes)
		// BOTH variable-cost fields go through the same primitive and both are
		// exercised here: File is the field the first budget left uncapped.
		got, textCut, fileCut := truncateDiagnostic(Diagnostic{
			Severity: "error",
			Text:     text,
			File:     text,
		})
		if !textCut || !fileCut {
			t.Fatalf("pad=%d: textCut=%v fileCut=%v, want both true for a %d-byte value",
				pad, textCut, fileCut, len(text))
		}
		if !utf8.ValidString(got.Text) {
			t.Fatalf("pad=%d: truncated Text is not valid UTF-8 — a byte-boundary cut splits a rune and makes the "+
				"whole JSON body invalid, not just this field", pad)
		}
		if !utf8.ValidString(got.File) {
			t.Fatalf("pad=%d: truncated File is not valid UTF-8 — the same defect, one field over", pad)
		}
	}
}

// The budget must be a bound on PATHOLOGY, not a change to real answers. Every
// fixture in testdata is measured here so a future tightening that would start
// truncating ordinary results fails loudly.
//
// Measured 2026-07-27 across all six fixture ports: 0-3 diagnostics, longest
// Text 148 bytes, whole response 0.7-4.8 KB. The ceilings are ~28x the longest
// real line and ~13x the largest real response.
func TestResponseBudget_NeverBindsOnRealFixtures(t *testing.T) {
	for _, tc := range []struct{ tree, triplet string }{{"failing_port", "cl"}, {"wingpl_like", "wingpl"}} {
		bt, err := filepath.Abs(filepath.Join("testdata", tc.tree, "buildtrees"))
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(bt)
		if err != nil {
			t.Fatalf("read %s: %v", bt, err)
		}
		seen := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			seen++
			res := LastFailure(Args{Port: e.Name(), BuildtreesRoot: bt, Triplet: tc.triplet},
				Deps{FS: DefaultFS(), Getenv: func(string) string { return "" }})
			if res.DiagnosticsDropped != 0 {
				t.Errorf("%s/%s: %d diagnostics dropped on a REAL fixture — the budget is meant to bound "+
					"pathology, not to change ordinary answers", tc.tree, e.Name(), res.DiagnosticsDropped)
			}
			for i, d := range res.Diagnostics {
				if strings.Contains(d.Text, "truncated,") {
					t.Errorf("%s/%s: diagnostics[%d].Text was truncated on a REAL fixture: %q",
						tc.tree, e.Name(), i, d.Text)
				}
			}
		}
		// Guard the instrument: an empty walk would make every assertion above
		// pass without checking anything.
		if seen == 0 {
			t.Fatalf("%s: no fixture ports found under %s", tc.tree, bt)
		}
	}
}

// applyResponseBudget's own edges, reached directly because a full LastFailure
// fixture cannot construct them.
//
// It no longer truncates: boundWireValues is the single truncator and runs
// first, so this function only ever spends a budget over values that are
// already at or under their per-field caps. The tests below therefore feed it
// pre-capped input, which is exactly the contract boundResponse guarantees.
func TestApplyResponseBudget_Edges(t *testing.T) {
	// Empty in, empty out, nothing reported.
	if out, dropped := applyResponseBudget(nil); len(out) != 0 || dropped != 0 {
		t.Fatalf("empty input: out=%v dropped=%d", out, dropped)
	}

	// The per-line cap is applied BEFORE the byte budget is charged, so no
	// single entry can consume more than its share of it. That makes the
	// constants' ordering an invariant worth stating: while it holds, an entry
	// can never exhaust the whole budget by itself.
	if MaxDiagnosticTextBytes+MaxWirePathBytes > MaxResponseDiagnosticBytes {
		t.Fatalf("one entry's per-field ceilings (%d text + %d file) exceed the aggregate budget (%d) — a single "+
			"diagnostic could then consume the entire response budget",
			MaxDiagnosticTextBytes, MaxWirePathBytes, MaxResponseDiagnosticBytes)
	}

	// An oversize headline is admitted and truncated, and does NOT starve the
	// next entry, precisely because of the ordering above. Asserted through
	// boundResponse because that is the composition the ordering lives in.
	oversize := Diagnostic{Severity: "error", Tier: TierSpecific,
		File: strings.Repeat("F", MaxWirePathBytes*4),
		Text: "a.cpp:1:1: error: " + strings.Repeat("y", MaxResponseDiagnosticBytes*2)}
	res := boundResponse(Result{Diagnostics: []Diagnostic{oversize, {Severity: "error", Tier: TierSpecific, Text: "b"}}})
	if len(res.Diagnostics) != 2 || res.DiagnosticsDropped != 0 {
		t.Fatalf("oversize headline: out=%d dropped=%d, want 2/0 — the per-field caps are charged, not the raw size",
			len(res.Diagnostics), res.DiagnosticsDropped)
	}
	if !hasNote(res.Notes, NoteDiagnosticTextTruncated) || !hasNote(res.Notes, NoteResponseValueTruncated) {
		t.Fatalf("notes %v: an oversize headline must report BOTH the cut text and the cut file — a truncated "+
			"locator is a different fact from a truncated message", res.Notes)
	}
	if len(res.Diagnostics[0].Text) > MaxDiagnosticTextBytes+len(truncationMarker)+32 {
		t.Fatalf("oversize headline Text was admitted at %d bytes", len(res.Diagnostics[0].Text))
	}
	if len(res.Diagnostics[0].File) > MaxWirePathBytes+len(truncationMarker)+32 {
		t.Fatalf("oversize headline File was admitted at %d bytes — File is charged and capped like Text, or a "+
			"3 MiB path rides in free while the response reports itself bounded", len(res.Diagnostics[0].File))
	}

	// The BYTE budget binds independently of the count budget: enough
	// max-length entries to exceed MaxResponseDiagnosticBytes while staying
	// well under MaxResponseDiagnostics. Without this the byte ceiling would
	// be unreachable and therefore untested.
	perEntry := MaxDiagnosticTextBytes
	n := MaxResponseDiagnosticBytes/perEntry + 5
	if n >= MaxResponseDiagnostics {
		t.Fatalf("fixture cannot separate the byte budget from the count budget: n=%d, count ceiling=%d",
			n, MaxResponseDiagnostics)
	}
	big := make([]Diagnostic, n)
	for i := range big {
		big[i] = Diagnostic{Severity: "error", Tier: TierSpecific,
			Text: "a.cpp:1:1: error: " + strings.Repeat("z", perEntry-18)}
	}
	out, dropped := applyResponseBudget(big)
	if dropped == 0 {
		t.Fatalf("%d entries of %d bytes returned whole — the %d-byte aggregate ceiling never bound, so a count "+
			"cap alone would let count x per-line = %d bytes through",
			n, perEntry, MaxResponseDiagnosticBytes, MaxResponseDiagnostics*MaxDiagnosticTextBytes)
	}
	if len(out) == 0 {
		t.Fatal("the headline must always be admitted, whatever the byte budget")
	}
	total := 0
	for _, d := range out {
		total += len(d.Text) + len(d.File)
	}
	if total > MaxResponseDiagnosticBytes+perEntry {
		t.Fatalf("emitted text totals %d bytes, ceiling is %d", total, MaxResponseDiagnosticBytes)
	}

	// The byte budget charges File as well as Text. Entries whose Text is tiny
	// but whose File fills the per-path cap must still exhaust it, or the
	// aggregate ceiling is once again a ceiling on one field.
	fileHeavy := make([]Diagnostic, MaxResponseDiagnosticBytes/MaxWirePathBytes+5)
	for i := range fileHeavy {
		fileHeavy[i] = Diagnostic{Severity: "error", Tier: TierSpecific, Text: "e", File: strings.Repeat("p", MaxWirePathBytes)}
	}
	if _, dropped := applyResponseBudget(fileHeavy); dropped == 0 {
		t.Fatalf("%d file-heavy entries returned whole — the aggregate budget charges len(Text) only, which is the "+
			"defect that made the whole budget non-binding", len(fileHeavy))
	}

	// A set that fits is returned whole, with nothing reported.
	small := make([]Diagnostic, MaxResponseDiagnostics)
	for i := range small {
		small[i] = Diagnostic{Severity: "error", Tier: TierSpecific, Text: "a.cpp:1:1: error: short"}
	}
	out, dropped = applyResponseBudget(small)
	if len(out) != MaxResponseDiagnostics || dropped != 0 {
		t.Fatalf("exactly-at-ceiling set: out=%d dropped=%d, want %d/0", len(out), dropped, MaxResponseDiagnostics)
	}
}

func hasNote(notes []Note, want Note) bool {
	for _, n := range notes {
		if n == want {
			return true
		}
	}
	return false
}
