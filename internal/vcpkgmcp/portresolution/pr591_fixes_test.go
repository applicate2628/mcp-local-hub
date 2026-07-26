package portresolution

import (
	"path/filepath"
	"strings"
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
			res := ResolvePort(Args{Port: bad, OverlayPorts: []string{overlay}}, DefaultDeps())

			if res.Status != evidence.StatusFailed {
				t.Fatalf("status = %v, want failed for port %q — an illegal port name is bad caller input", res.Status, bad)
			}
			if res.Reason != ReasonInvalidPortName {
				t.Fatalf("reason = %v, want %v for port %q", res.Reason, ReasonInvalidPortName, bad)
			}
			if res.InvalidPort != bad {
				t.Fatalf("invalid_port = %q, want %q echoed back", res.InvalidPort, bad)
			}
			// The security assertion: nothing outside the granted root was even
			// considered, let alone probed.
			cleanOverlay := filepath.Clean(overlay)
			for _, c := range res.AllCandidates {
				rel, err := filepath.Rel(cleanOverlay, filepath.Clean(c.Directory))
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Fatalf("candidate %q lies outside the granted overlay root %q — the join laundered a traversal name into a probed path",
						c.Directory, cleanOverlay)
				}
			}
			if len(res.Evidence.Paths) > 0 {
				for _, p := range res.Evidence.Paths {
					rel, err := filepath.Rel(cleanOverlay, filepath.Clean(p))
					if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						t.Fatalf("evidence path %q lies outside the granted overlay root %q", p, cleanOverlay)
					}
				}
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
