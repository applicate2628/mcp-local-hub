//go:build windows

package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the SECURITY properties of the executable-path conjunct of
// the PID-identity gate, not its speed. The gate exists because Windows
// recycles PIDs: a PID the supervisor recorded for one of its daemons can
// later belong to an unrelated process, and this comparison is part of what
// proves the process holding the PID is the binary we spawned.
//
// The 2026-07-21 change added an equality short-circuit to
// executablePathMatches so the common case skips two filepath.EvalSymlinks
// walks. The tests below are the mutation targets for that change: they fail
// if the short-circuit is ever widened into a memo, and they fail if the
// rejection path stops rejecting.

// TestExecutablePathMatchesRejectsMismatchedExecutable is the core
// anti-recycled-PID assertion: a DIFFERENT executable at a different path must
// never satisfy the gate.
func TestExecutablePathMatchesRejectsMismatchedExecutable(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "mcphub.exe")
	theirs := filepath.Join(dir, "attacker.exe")
	writeFile(t, ours, "ours")
	writeFile(t, theirs, "theirs")

	if executablePathMatches(theirs, ours) {
		t.Fatalf("identity gate accepted a foreign executable: got=%q expected=%q", theirs, ours)
	}
	if executablePathMatches(ours, theirs) {
		t.Fatalf("identity gate accepted a foreign executable (reversed): got=%q expected=%q", ours, theirs)
	}
}

// TestExecutablePathMatchesRejectsSiblingInDifferentDirectory covers the
// same-basename case: an attacker process running a file also called
// mcphub.exe, but from a directory we never installed to.
func TestExecutablePathMatchesRejectsSiblingInDifferentDirectory(t *testing.T) {
	root := t.TempDir()
	installed := filepath.Join(root, "install", "mcphub.exe")
	planted := filepath.Join(root, "temp", "mcphub.exe")
	mkdirAll(t, filepath.Dir(installed))
	mkdirAll(t, filepath.Dir(planted))
	writeFile(t, installed, "installed")
	writeFile(t, planted, "planted")

	if executablePathMatches(planted, installed) {
		t.Fatalf("identity gate accepted same-basename executable from a foreign directory: got=%q expected=%q", planted, installed)
	}
}

// TestExecutablePathMatchesRejectsEmptyOperand pins the fail-closed posture for
// a missing path — an absent proof must never read as a match.
func TestExecutablePathMatchesRejectsEmptyOperand(t *testing.T) {
	dir := t.TempDir()
	ours := filepath.Join(dir, "mcphub.exe")
	writeFile(t, ours, "ours")

	if executablePathMatches("", ours) {
		t.Fatal("identity gate accepted an empty observed path")
	}
	if executablePathMatches(ours, "") {
		t.Fatal("identity gate accepted an empty expected path")
	}
	if executablePathMatches("", "") {
		t.Fatal("identity gate accepted two empty paths")
	}
}

// TestExecutablePathMatchesReresolvesAfterBinarySwap is the anti-cache
// assertion for the rename-aside binary replacement `mcphub install --upgrade`
// performs (MoveFileEx target -> target.old-<ts>, then target.new -> target).
//
// It swaps what lives at the SAME path between calls and asserts the gate
// answers against the filesystem as it is NOW, not as it was on an earlier
// call. A memo keyed on the path string would return the first call's answer
// and fail the final assertion.
func TestExecutablePathMatchesReresolvesAfterBinarySwap(t *testing.T) {
	root := t.TempDir()
	realV1 := filepath.Join(root, "v1", "mcphub.exe")
	realV2 := filepath.Join(root, "v2", "mcphub.exe")
	mkdirAll(t, filepath.Dir(realV1))
	mkdirAll(t, filepath.Dir(realV2))
	writeFile(t, realV1, "v1")
	writeFile(t, realV2, "v2")

	// The canonical install path is a symlink, so normalization is genuinely
	// load-bearing: the gate must resolve it, not compare the literal.
	link := filepath.Join(root, "mcphub.exe")
	if err := os.Symlink(realV1, link); err != nil {
		t.Skipf("symlink creation unavailable (needs Developer Mode or elevation): %v", err)
	}

	// Before the swap the link resolves to v1, so a v1 process matches and a
	// v2 process does not.
	if !executablePathMatches(realV1, link) {
		t.Fatalf("pre-swap: expected the v1 binary to match the install path %q", link)
	}
	if executablePathMatches(realV2, link) {
		t.Fatalf("pre-swap: the v2 binary must not match the install path %q", link)
	}

	// Rename-aside: repoint the install path at v2.
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.Symlink(realV2, link); err != nil {
		t.Fatalf("relink: %v", err)
	}

	// AFTER the swap the verdicts must INVERT. A cached resolution of `link`
	// would keep answering v1 here.
	if executablePathMatches(realV1, link) {
		t.Fatalf("post-swap: stale resolution — the v1 binary still matched install path %q after it was repointed to v2", link)
	}
	if !executablePathMatches(realV2, link) {
		t.Fatalf("post-swap: expected the v2 binary to match install path %q after the swap", link)
	}
}

// TestNormalizeWindowsExecutablePathFollowsSymlinkRepoint is the same anti-cache
// property one layer down, on the normalizer itself, so a memo added there
// (rather than in executablePathMatches) is also caught.
func TestNormalizeWindowsExecutablePathFollowsSymlinkRepoint(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "mcphub.exe")
	b := filepath.Join(root, "b", "mcphub.exe")
	mkdirAll(t, filepath.Dir(a))
	mkdirAll(t, filepath.Dir(b))
	writeFile(t, a, "a")
	writeFile(t, b, "b")

	link := filepath.Join(root, "mcphub.exe")
	if err := os.Symlink(a, link); err != nil {
		t.Skipf("symlink creation unavailable (needs Developer Mode or elevation): %v", err)
	}

	first := normalizeWindowsExecutablePath(link)
	if !strings.EqualFold(first, mustEval(t, a)) {
		t.Fatalf("initial resolution = %q, want the a-target %q", first, a)
	}

	if err := os.Remove(link); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.Symlink(b, link); err != nil {
		t.Fatalf("relink: %v", err)
	}

	second := normalizeWindowsExecutablePath(link)
	if strings.EqualFold(second, first) {
		t.Fatalf("stale resolution: normalizeWindowsExecutablePath returned %q for %q both before and after the target was repointed from %q to %q", second, link, a, b)
	}
	if !strings.EqualFold(second, mustEval(t, b)) {
		t.Fatalf("post-repoint resolution = %q, want the b-target %q", second, b)
	}
}

// TestExecutablePathMatchesShortCircuitAgreesWithFullPath pins the monotonicity
// property the short-circuit's safety argument rests on: whenever the
// short-circuit answers "match", the full normalize-and-compare must agree.
// The short-circuit must never be the ONLY reason a pair matches.
func TestExecutablePathMatchesShortCircuitAgreesWithFullPath(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real", "mcphub.exe")
	mkdirAll(t, filepath.Dir(real))
	writeFile(t, real, "real")

	cases := []struct{ name, got, expected string }{
		{"identical", real, real},
		{"case-differing", strings.ToUpper(real), real},
		{"unclean-expected", real, filepath.Join(root, "real", ".", "mcphub.exe")},
		{"missing-file-identical", filepath.Join(root, "gone.exe"), filepath.Join(root, "gone.exe")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			short := executablePathMatches(tc.got, tc.expected)
			full := fullNormalizeCompare(tc.got, tc.expected)
			if short && !full {
				t.Fatalf("short-circuit accepted a pair the full normalize-and-compare rejects: got=%q expected=%q", tc.got, tc.expected)
			}
			if short != full {
				t.Fatalf("short-circuit disagreed with full compare: short=%v full=%v got=%q expected=%q", short, full, tc.got, tc.expected)
			}
		})
	}
}

// fullNormalizeCompare is the pre-short-circuit body of executablePathMatches,
// kept here as the oracle the short-circuit is checked against.
func fullNormalizeCompare(got, expected string) bool {
	got = normalizeWindowsExecutablePath(got)
	expected = normalizeWindowsExecutablePath(expected)
	return got != "" && expected != "" && strings.EqualFold(got, expected)
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return filepath.Clean(resolved)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
}
