// internal/clients/canonical_project_key_test.go
//
// Per-project-GUI Phase 3a: CanonicalProjectKey (the single A+B+C join-key
// owner) + the proof that canonicalClaudeProjectKey is now a thin alias of it
// (one normalizer — design decision 4 / T2 "no 4th normalizer").
package clients

import (
	"runtime"
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
