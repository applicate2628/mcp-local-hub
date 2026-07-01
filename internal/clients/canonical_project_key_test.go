// internal/clients/canonical_project_key_test.go
//
// Per-project-GUI Phase 3a: CanonicalProjectKey (the single A+B+C join-key
// owner) + the proof that canonicalClaudeProjectKey is now a thin alias of it
// (one normalizer — design decision 4 / T2 "no 4th normalizer").
package clients

import (
	"runtime"
	"strings"
	"testing"
)

func TestCanonicalProjectKey_Normalization(t *testing.T) {
	if runtime.GOOS == "windows" {
		cases := []struct{ in, want string }{
			{`C:\dev\proj`, "c:/dev/proj"},
			{`C:/dev/proj`, "c:/dev/proj"},
			{`c:/dev/proj`, "c:/dev/proj"},
			{`C:\dev\proj\`, "c:/dev/proj"},       // trailing sep trimmed
			{`C:\dev\.\proj`, "c:/dev/proj"},      // Clean collapses `.`
			{`C:\dev\sub\..\proj`, "c:/dev/proj"}, // Clean collapses `..`
		}
		for _, c := range cases {
			if got := CanonicalProjectKey(c.in); got != c.want {
				t.Errorf("CanonicalProjectKey(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	} else {
		cases := []struct{ in, want string }{
			{"/dev/proj", "/dev/proj"},
			{"/dev/proj/", "/dev/proj"},
			{"/dev/./proj", "/dev/proj"},
			{"/dev/sub/../proj", "/dev/proj"},
		}
		for _, c := range cases {
			if got := CanonicalProjectKey(c.in); got != c.want {
				t.Errorf("CanonicalProjectKey(%q) = %q, want %q", c.in, got, c.want)
			}
		}
		// MIXED-CASE POSIX inputs: the expected form depends on whether this
		// GOOS case-folds. On Darwin (default-case-insensitive APFS/HFS+) the
		// key folds to lower; on Linux (case-sensitive ext4) it is preserved.
		// This is the only branch that actually exercises the darwin fold on a
		// macOS build — the all-lowercase cases above are fold-invariant and
		// would pass even if the darwin fold regressed (bot PR #474 P2).
		mixedCase := "/Dev/Proj"
		wantMixed := mixedCase
		if caseFoldsProjectKey(runtime.GOOS) {
			wantMixed = "/dev/proj"
		}
		if got := CanonicalProjectKey(mixedCase); got != wantMixed {
			t.Errorf("CanonicalProjectKey(%q) on GOOS=%s = %q, want %q (fold=%v)", mixedCase, runtime.GOOS, got, wantMixed, caseFoldsProjectKey(runtime.GOOS))
		}
	}
	if CanonicalProjectKey("") != "" {
		t.Errorf("empty input must return empty")
	}
}

// TestCanonicalProjectKey_NoCWDDependence proves the owner does NOT call
// filepath.Abs (no ambient working-directory read): a relative input is
// normalized purely lexically, NOT resolved against the process CWD. (All real
// callers pass absolute paths; this guards the deliberate omission of Abs.)
func TestCanonicalProjectKey_NoCWDDependence(t *testing.T) {
	// A relative input stays relative-shaped (Clean only), never prefixed with
	// the test process's working directory.
	got := CanonicalProjectKey("rel/sub")
	want := "rel/sub"
	if runtime.GOOS == "windows" {
		want = "rel/sub" // ToSlash + lower; "rel/sub" already lower
	}
	if got != want {
		t.Errorf("relative input resolved against CWD or otherwise altered: got %q, want %q (Abs must NOT be called)", got, want)
	}
}

// TestCanonicalProjectKey_RootStaysAddressable pins finding 3 (bot PR #433 r3):
// a ROOT project/workspace path must canonicalize to a NON-EMPTY, addressable
// key — trimming ALL trailing slashes collapsed `/` → "" and `C:/` → "c:",
// making root projects unaddressable (the aggregate skips empty keys, the
// claude-local matcher treats "" as no-match). A NON-root trailing slash is
// still trimmed (`foo/` → `foo`).
func TestCanonicalProjectKey_RootStaysAddressable(t *testing.T) {
	if runtime.GOOS == "windows" {
		cases := []struct{ in, want string }{
			{`C:\`, "c:/"},          // drive root keeps its slash, NOT "c:"
			{`C:/`, "c:/"},          // forward-slash drive root
			{`c:/`, "c:/"},          // already-lower drive root
			{`D:\`, "d:/"},          // a different drive root
			{`C:\dev\proj\`, "c:/dev/proj"}, // non-root trailing slash trimmed
			{`C:\dev\proj`, "c:/dev/proj"},  // already-trimmed non-root unchanged
		}
		for _, c := range cases {
			got := CanonicalProjectKey(c.in)
			if got != c.want {
				t.Errorf("CanonicalProjectKey(%q) = %q, want %q", c.in, got, c.want)
			}
			if got == "" {
				t.Errorf("CanonicalProjectKey(%q) collapsed to an empty/unaddressable key", c.in)
			}
		}
	} else {
		cases := []struct{ in, want string }{
			{"/", "/"},                  // POSIX root keeps its slash, NOT ""
			{"/dev/proj/", "/dev/proj"}, // non-root trailing slash trimmed
			{"/dev/proj", "/dev/proj"},  // already-trimmed non-root unchanged
			{"foo/", "foo"},             // relative non-root trailing slash trimmed
		}
		for _, c := range cases {
			got := CanonicalProjectKey(c.in)
			if got != c.want {
				t.Errorf("CanonicalProjectKey(%q) = %q, want %q", c.in, got, c.want)
			}
			if got == "" {
				t.Errorf("CanonicalProjectKey(%q) collapsed to an empty/unaddressable key", c.in)
			}
		}
	}
}

// TestCaseFoldsProjectKey_PlatformPredicate pins the GOOS predicate
// CanonicalProjectKey folds case on, independent of the GOOS the test binary
// itself was built for: Windows (NTFS) and Darwin (APFS/HFS+) both default to
// a case-insensitive filesystem and must fold; Linux (ext4) defaults
// case-sensitive and must NOT fold. See the case-sensitivity tradeoff note on
// CanonicalProjectKey for why this matches the platform default rather than
// the live volume's actual mode.
func TestCaseFoldsProjectKey_PlatformPredicate(t *testing.T) {
	cases := []struct {
		goos string
		want bool
	}{
		{"windows", true},
		{"darwin", true},
		{"linux", false},
		{"freebsd", false},
	}
	for _, c := range cases {
		if got := caseFoldsProjectKey(c.goos); got != c.want {
			t.Errorf("caseFoldsProjectKey(%q) = %v, want %v", c.goos, got, c.want)
		}
	}
}

// TestCanonicalClaudeKeyIsAliasOfCanonical proves the claude-specific helper is
// a thin caller of the single owner — they agree on every form, so there is ONE
// normalizer, not two that could drift (T2).
func TestCanonicalClaudeKeyIsAliasOfCanonical(t *testing.T) {
	inputs := []string{
		`C:\dev\proj`, `C:/dev/proj`, `c:/dev/proj`, `C:\dev\proj\`,
		"/dev/proj", "/dev/proj/", "", "rel/x",
	}
	for _, in := range inputs {
		if a, b := canonicalClaudeProjectKey(in), CanonicalProjectKey(in); a != b {
			t.Errorf("canonicalClaudeProjectKey(%q)=%q diverged from CanonicalProjectKey=%q", in, a, b)
		}
	}
}

// TestCanonicalClaudeKey_FoldsPerPlatform pins that the claude alias case-folds
// EXACTLY on the platforms caseFoldsProjectKey says it should — i.e. it folds on
// Darwin too, not only Windows (bot PR #474 P2). Driven by the GOOS-independent
// predicate so the darwin expectation is asserted on every build (the actual
// fold for darwin only runs on a darwin binary, but the predicate-linked
// expectation cannot silently route darwin through the non-fold branch). A
// mixed-case POSIX-shaped input is used so the fold is observable; on Windows
// the drive-letter form folds for the same reason.
func TestCanonicalClaudeKey_FoldsPerPlatform(t *testing.T) {
	var in, wantFolded, wantPreserved string
	if runtime.GOOS == "windows" {
		in, wantFolded, wantPreserved = `C:\Dev\Proj`, "c:/dev/proj", "C:/Dev/Proj"
	} else {
		in, wantFolded, wantPreserved = "/Dev/Proj", "/dev/proj", "/Dev/Proj"
	}
	want := wantPreserved
	if caseFoldsProjectKey(runtime.GOOS) {
		want = wantFolded
	}
	if got := canonicalClaudeProjectKey(in); got != want {
		t.Errorf("canonicalClaudeProjectKey(%q) on GOOS=%s = %q, want %q (fold=%v)", in, runtime.GOOS, got, want, caseFoldsProjectKey(runtime.GOOS))
	}
}

// TestCanonicalProjectWriteKey_PreservesCase pins the write ≠ compare split
// (bot PR #474 P2): the FRESH-entry write key must NOT case-fold, so a first-
// ever ~/.claude.json projects.<key> entry keeps the operator's actual path
// case and matches Claude Code's own path lookup. CanonicalProjectKey (the
// compare key) folds on windows/darwin; canonicalProjectWriteKey never does —
// on EVERY build. A folded write key would write a projects.<key> entry Claude
// Code's exact-path lookup never reads (a silent toggle no-op).
func TestCanonicalProjectWriteKey_PreservesCase(t *testing.T) {
	var inputs []string
	if runtime.GOOS == "windows" {
		inputs = []string{`C:\Dev\MyProj`, `C:\Dev\MyProj\`, `C:/Dev/MyProj`}
	} else {
		inputs = []string{"/Users/Alice/Proj", "/srv/App/Sub", "/Users/Alice/Proj/"}
	}
	for _, in := range inputs {
		w := canonicalProjectWriteKey(in)
		// (1) case preserved: every input has upper-case letters, so a write key
		// equal to its own lower-case means the fold leaked into the write path.
		if w == strings.ToLower(w) {
			t.Errorf("canonicalProjectWriteKey(%q) = %q lost case (write key must NOT fold)", in, w)
		}
		// (2) write and compare share the SAME pre-fold normalization: folding
		// the write key yields exactly the compare key on a folding platform; on
		// a non-folding platform the two are already identical.
		cmp := CanonicalProjectKey(in)
		foldAdjusted := w
		if caseFoldsProjectKey(runtime.GOOS) {
			foldAdjusted = strings.ToLower(w)
		}
		if foldAdjusted != cmp {
			t.Errorf("write/compare normalization mismatch for %q: fold-adjusted write=%q, CanonicalProjectKey=%q", in, foldAdjusted, cmp)
		}
	}
	// Empty stays empty (shared with the compare key).
	if canonicalProjectWriteKey("") != "" {
		t.Errorf("canonicalProjectWriteKey(\"\") must return empty")
	}
}
