package lastfailure

import (
	"strings"
	"testing"
)

// The fixtures below are MEASURED byte sequences, not invented ones. Probed in
// the target environment 2026-07-27 (msys2 ucrt64):
//
//	$ g++ -fsyntax-only -fdiagnostics-color=always g.cpp 2>&1 | od -c
//	033 [ 0 1 m 033 [ K g . c p p : 033 [ m 033 [ K   I n   f u n c t i o n ...
//	$ clang++ -fsyntax-only -fdiagnostics-color=always -fansi-escape-codes ansi.cpp 2>&1 | od -c
//	033 [ 1 m a n s i . c p p : 1 : 1 3 :   033 [ 0 m 033 [ 0 ; 1 ; 3 1 m
//	e r r o r :   033 [ 0 m 033 [ 1 m u s e   o f   u n d e c l a r e d ...
//	$ strings C:\msys64\ucrt64\bin\ninja.exe | grep CLICOLOR
//	CLICOLOR_FORCE
//
// Two facts from that probe drive the design. GCC emits ANSI to a REDIRECTED
// pipe with nothing but -fdiagnostics-color=always — no CLICOLOR_FORCE, no TTY,
// just an ordinary CXXFLAGS or triplet setting — so this is reachable
// configuration, not an exotic one. And GCC's sequences include `\x1b[K`
// (erase-in-line), so a stripper that only understood SGR (`...m`) would leave
// residue behind.
const (
	// ansiClangError is clang's coloured spelling of
	// "ansi.cpp:1:13: error: use of undeclared identifier 'undefined_fn'".
	ansiClangError = "\x1b[1mansi.cpp:1:13: \x1b[0m\x1b[0;1;31merror: \x1b[0m\x1b[1muse of undeclared identifier 'undefined_fn'\x1b[0m"
	// ansiGCCErasesLine carries GCC's CSI-K form around an MSVC-shaped line.
	ansiGCCErasesLine = "\x1b[01m\x1b[Ksrc.cpp(63,15)\x1b[m\x1b[K: \x1b[01;31m\x1b[Kerror\x1b[m\x1b[K: definition not allowed"
	utf8BOM           = "\ufeff"
)

// TestNormalizeLogLine is the unit-level contract of the single normalizer both
// diagnostic scanners and the build-command finder call.
//
// The last group is the one that keeps this from being a licence to rewrite log
// text: an ordinary line must come back BYTE-IDENTICAL, including one carrying
// tabs (a diagnostic message legitimately contains them) and one carrying
// multi-byte UTF-8 (whose continuation bytes sit in 0x80-0x9F, the C1 range a
// naive "strip control characters" pass would corrupt).
func TestNormalizeLogLine(t *testing.T) {
	stripped := []struct{ name, in, want string }{
		{"SGR colour wrap", "\x1b[31mUser interrupt\x1b[0m", "User interrupt"},
		{"clang coloured error", ansiClangError, "ansi.cpp:1:13: error: use of undeclared identifier 'undefined_fn'"},
		{"gcc CSI-K erase-in-line", ansiGCCErasesLine, "src.cpp(63,15): error: definition not allowed"},
		{"OSC title sequence terminated by BEL", "\x1b]0;building\x07FAILED: [code=1] a.obj", "FAILED: [code=1] a.obj"},
		{"OSC terminated by ST", "\x1b]0;building\x1b\\FAILED: [code=1] a.obj", "FAILED: [code=1] a.obj"},
		// The nF form: ESC + intermediates + one final byte. ESC ( B is the
		// three-byte "select ASCII charset" a terminal capture really carries;
		// ESC 7 is the zero-intermediate (two-byte) case. Treating every
		// non-CSI escape as exactly two bytes leaves the final byte behind as
		// text, which is what this pair pins.
		{"nF escape with an intermediate (ESC ( B)", "\x1b(BUser interrupt", "User interrupt"},
		{"nF escape with no intermediate (ESC 7)", "\x1b7User interrupt", "User interrupt"},
		{"unterminated CSI consumes to end", "text\x1b[31", "text"},
		{"UTF-8 BOM prefix", utf8BOM + "User interrupt", "User interrupt"},
		{"NUL suffix", "User interrupt\x00", "User interrupt"},
		{"BEL and DEL", "User\x07 interrupt\x7f", "User interrupt"},
	}
	for _, tc := range stripped {
		if got := normalizeLogLine(tc.in); got != tc.want {
			t.Errorf("%s: normalizeLogLine(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	// Complement: nothing else is touched, and the return is the SAME string
	// (no copy) so the common case costs no allocation.
	for _, clean := range []string{
		"src.cpp(63,15): error C2065: 'x': undeclared identifier",
		"FAILED: [code=1] CMakeFiles/foo.dir/src.cxx.obj",
		"a.cpp:3:5: error: message\twith\ttabs",
		"src.cpp:1:1: error: \u00e9\u4e2d\u6587 \u2014 multi-byte UTF-8 must survive",
		"",
	} {
		if got := normalizeLogLine(clean); got != clean {
			t.Errorf("normalizeLogLine(%q) = %q — an ordinary line must round-trip byte-for-byte", clean, got)
		}
	}
}

// F4 (adversarial-gate finding): anchoring DetectInterrupted to whole lines
// fixed a real false POSITIVE class but introduced three false NEGATIVES,
// because bytes.TrimSpace strips none of a BOM, an ANSI wrap, or a NUL.
//
// The ANSI case is the one that matters: an interrupted build whose markers
// carry colour reports the accompanying `FAILED:` line as a genuine build
// defect, sending the operator to fix a bug that does not exist — which is
// precisely the outcome ReasonBuildInterrupted was introduced to prevent.
func TestDetectInterrupted_SurvivesTerminalDisplayBytes(t *testing.T) {
	interrupts := []struct{ name, content string }{
		{"ANSI-wrapped ninja narration", "FAILED: [code=1]\n\x1b[31mninja: build stopped: interrupted by user.\x1b[0m\n"},
		{"ANSI-wrapped relayed subprocess line", "FAILED: [code=1]\n\x1b[31mUser interrupt\x1b[0m\n"},
		{"OSC title sequence before the marker", "FAILED: [code=1]\n\x1b]0;build\x07User interrupt\n"},
		{"UTF-8 BOM prefix", "FAILED: [code=1]\n" + utf8BOM + "User interrupt\n"},
		{"NUL suffix", "FAILED: [code=1]\nUser interrupt\x00\n"},
		{"colour reset leaving trailing space", "FAILED: [code=1]\n\x1b[31mUser interrupt \x1b[0m\n"},
	}
	for _, tc := range interrupts {
		if !DetectInterrupted([]byte(tc.content)) {
			t.Errorf("%s: DetectInterrupted = false — the marker IS the whole line's text; matching raw display "+
				"bytes reports an interrupted build as a genuine defect, content=%q", tc.name, tc.content)
		}
	}

	// Complement — the property e49fafe9 established must NOT regress. A
	// QUOTED mid-line occurrence stays a non-interrupt after normalization,
	// because stripping display bytes never moves surrounding TEXT away.
	notInterrupts := []struct{ name, content string }{
		{"coloured ninja echo quoting the phrase", "\x1b[32m[2/3]\x1b[0m cl.exe /c /DF=\"User interrupt\" src.cpp\n"},
		{"coloured path containing the phrase", "\x1b[1mC:\\dev\\User interrupt\\src.cpp(12): \x1b[0merror C2065: 'x'\n"},
		{"marker with trailing text after the reset", "\x1b[31mUser interrupt\x1b[0m and then some\n"},
	}
	for _, tc := range notInterrupts {
		if DetectInterrupted([]byte(tc.content)) {
			t.Errorf("%s: DetectInterrupted = true — a QUOTED mid-line occurrence must stay a non-interrupt; "+
				"normalization must not weaken the whole-line anchor, content=%q", tc.name, tc.content)
		}
	}
}

// The same bytes silence scanDiagnostics, and there the damage is strictly
// worse: every recognized shape is ANCHORED, so a colourized log matches
// NOTHING and LastFailure answers unknown(no_diagnostic_found) — a confident
// denial of a failure that plainly happened.
//
// This is why the normalizer is shared rather than local to DetectInterrupted.
func TestScanDiagnostics_MatchesColourizedOutput(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantFile string
		wantLine int
		wantSev  string
		wantTier DiagnosticTier
	}{
		{"clang coloured gcc-shape error", ansiClangError, "ansi.cpp", 1, "error", TierSpecific},
		{"gcc CSI-K around an MSVC-shape error", ansiGCCErasesLine, "src.cpp", 63, "error", TierSpecific},
		{"coloured ninja FAILED summary", "\x1b[31mFAILED: \x1b[0m[code=1] foo.obj", "foo.obj", 0, "error", TierAggregate},
		{"coloured lld-link driver diagnostic", "\x1b[1mlld-link\x1b[0m: \x1b[0;1;31merror: \x1b[0mundefined symbol: gzopen_w", "lld-link", 0, "error", TierSpecific},
		{"BOM before an MSVC diagnostic", utf8BOM + "src.cpp(9): error C2065: 'x': undeclared identifier", "src.cpp", 9, "error", TierSpecific},
	}
	for _, tc := range cases {
		got := ScanDiagnostics([]byte(tc.in + "\n"))
		if len(got) != 1 {
			t.Errorf("%s: got %d diagnostics, want 1 — an anchored shape cannot match through terminal display "+
				"bytes, so a colourized log yields unknown(no_diagnostic_found) for a build that plainly failed; in=%q",
				tc.name, len(got), tc.in)
			continue
		}
		d := got[0]
		if d.File != tc.wantFile || d.Line != tc.wantLine || d.Severity != tc.wantSev || d.Tier != tc.wantTier {
			t.Errorf("%s: got %+v, want file=%q line=%d severity=%q tier=%q", tc.name, d, tc.wantFile, tc.wantLine, tc.wantSev, tc.wantTier)
		}
		// The wire contract: Text is the NORMALIZED line. An MCP result is
		// rendered in a terminal and copied into transcripts, so relaying a
		// build log's escape sequences verbatim is a terminal-injection
		// channel — the same reason the marketplace catalog path strips them.
		if strings.ContainsAny(d.Text, "\x1b\x00\x07") || strings.Contains(d.Text, utf8BOM) {
			t.Errorf("%s: Diagnostic.Text = %q still carries terminal display bytes", tc.name, d.Text)
		}
	}
}

// findRunBuildCommandLine is the THIRD reader of the same input class, so it
// takes the same normalizer: a colourized marker is not found by the substring
// search, and a colourized tail would put escape bytes into Result.BuildCommand
// on the wire.
func TestFindRunBuildCommandLine_SurvivesTerminalDisplayBytes(t *testing.T) {
	log := "-- Configuring done\n\x1b[32mRun Build Command(s): \x1b[0mninja -v \x1b[1mall\x1b[0m\n-- Build files written\n"
	got, ok := findRunBuildCommandLine([]byte(log))
	if !ok {
		t.Fatalf("findRunBuildCommandLine found nothing in a colourized log; the marker is present as text")
	}
	const want = "ninja -v all"
	if got != want {
		t.Fatalf("build command = %q, want %q — the recovered command is emitted on the wire and must not carry "+
			"escape sequences", got, want)
	}
}
