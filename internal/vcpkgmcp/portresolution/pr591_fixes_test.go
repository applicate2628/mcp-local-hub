package portresolution

import (
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// PR #591 P2: only an EMPTY port was rejected, so a traversal name was
// normalised BY filepath.Join into a path outside the overlay root, and that
// path was then stat'ed, listed and reported as a candidate location. That
// turns a port LOOKUP into an arbitrary-directory probe against anything the
// hub daemon can read.
//
// The assertion that matters is not just the reason: it is that NO candidate
// path outside the granted root is ever recorded or probed.
//
// VACUOUS-TEST FIX (2026-07-27): that assertion used to be two loops over
// res.AllCandidates and res.Evidence.Paths. Both are EMPTY on the refusal path
// — measured: every one of the nine names below returns
// failed(invalid_port_name) with AllCandidates=0 and Evidence.Paths=0 — so the
// loop bodies never executed and the "security assertion" could not fail. It
// would have stayed green if the gate were deleted and the join relaxed, as
// long as the reason string was still produced.
//
// It now binds through a SPY Deps: the refusal must happen before the join, so
// the filesystem must never be consulted at all. That is the property the test
// is named for, and it is falsifiable.
//
// It binds on the PROPERTY, not on one particular gate, and that is deliberate.
// Two gates enforce it — the pre-join check in ResolvePort and portDirWithin's
// own name+containment check — so removing either ALONE leaves the test green,
// which is correct: the invariant still holds. Removing BOTH name gates makes
// it fail with the laundered path in the message, e.g.
//
//	port "sub/nested" caused 3 filesystem probe(s) despite being refused:
//	[stat <overlay> readdir <overlay> stat <overlay>\sub\nested]
//
// (the four `..`-shaped names stay refused even then, by the containment check
// that is the real security boundary — see
// TestPortDirWithinRejectsAnEscapeIndependentlyOfTheCharsetRule below).
func TestTraversalPortNameIsRefusedBeforeTheJoin(t *testing.T) {
	overlay := t.TempDir()

	for _, bad := range []string{
		"../../outside",
		"..",
		"../sibling",
		`..\windows-sibling`,
		"sub/nested",
		"UPPERCASE",
		"-leading-hyphen",
		"trailing-hyphen-",
		"under_score",
	} {
		t.Run(bad, func(t *testing.T) {
			var probed []string
			deps := DefaultDeps()
			realStat, realReadDir := deps.Stat, deps.ReadDir
			deps.Stat = func(p string) (os.FileInfo, error) {
				probed = append(probed, "stat "+p)
				return realStat(p)
			}
			deps.ReadDir = func(p string) ([]os.DirEntry, error) {
				probed = append(probed, "readdir "+p)
				return realReadDir(p)
			}

			res := ResolvePort(Args{Port: bad, OverlayPorts: []string{overlay}}, deps)

			// THE security assertion, and the one that binds: an illegal port
			// name is refused BEFORE the join, so nothing is probed at all.
			// filepath.Join would have normalised "../../outside" into a real
			// directory outside the granted root, and that directory would then
			// have been stat'ed and listed.
			if len(probed) != 0 {
				t.Fatalf("port %q caused %d filesystem probe(s) despite being refused: %v — the name reached "+
					"filepath.Join, which is exactly the arbitrary-directory probe this gate exists to prevent",
					bad, len(probed), probed)
			}

			if res.Status != evidence.StatusFailed {
				t.Fatalf("status = %v, want failed for port %q — an illegal port name is bad caller input", res.Status, bad)
			}
			if res.Reason != ReasonInvalidPortName {
				t.Fatalf("reason = %v, want %v for port %q", res.Reason, ReasonInvalidPortName, bad)
			}
			if res.InvalidPort != bad {
				t.Fatalf("invalid_port = %q, want %q echoed back", res.InvalidPort, bad)
			}
			// Nothing was recorded either. Asserted as a COUNT, not as a loop
			// over the (empty) slices: a loop body that never runs cannot
			// report a regression, which is how the original version of this
			// check passed vacuously.
			if len(res.AllCandidates) != 0 {
				t.Fatalf("refused port %q still recorded %d candidate location(s): %+v", bad, len(res.AllCandidates), res.AllCandidates)
			}
			if len(res.Evidence.Paths) != 0 {
				t.Fatalf("refused port %q still recorded %d evidence path(s): %v", bad, len(res.Evidence.Paths), res.Evidence.Paths)
			}
		})
	}
}

// The equal-and-opposite guard: every LEGAL vcpkg port name must still
// resolve. A gate that rejects real port names would be worse than the hole.
func TestLegalPortNamesAreStillAccepted(t *testing.T) {
	for _, good := range []string{"zlib", "mcp-language-server", "abseil", "libpng16", "a-b-c", "x264"} {
		t.Run(good, func(t *testing.T) {
			res := ResolvePort(Args{Port: good, OverlayPorts: []string{t.TempDir()}}, DefaultDeps())
			if res.Reason == ReasonInvalidPortName {
				t.Fatalf("port %q was rejected as an invalid name, but it is a legal vcpkg port name", good)
			}
		})
	}
}

// portDirWithin's containment check is the real boundary, independent of the
// charset rule: it must reject an escape even for a name the regex allows.
func TestPortDirWithinRejectsAnEscapeIndependentlyOfTheCharsetRule(t *testing.T) {
	root := filepath.Join(t.TempDir(), "overlay")

	if _, err := portDirWithin(root, "zlib"); err != nil {
		t.Fatalf("portDirWithin(root, %q) = %v, want a path beneath the root", "zlib", err)
	}
	if _, err := portDirWithin(root, "../escape"); err == nil {
		t.Fatalf("portDirWithin(root, %q) succeeded, want refusal — containment is the security boundary", "../escape")
	}
}
