// canonical_project_key.go — per-project-GUI Phase 3a.
//
// CanonicalProjectKey is the SINGLE general join-key normalizer for the
// per-project-GUI A+B+C composition (design decision 4,
// work-items/decisions/2026-06-25-per-project-gui-p3-design.md). The three
// per-project mechanisms each key off a project path written in a possibly
// divergent form:
//
//   - A (workspace LSP):  workspaces.yaml workspace_path
//   - B (cursor/vscode/claude): the project root resolved by
//     ProjectScanConfigPaths (realRoot)
//   - B-claude Local:     the ~/.claude.json projects.<key> as Claude Code
//     wrote it (verified on the dev host to vary across forward-slash+upper-
//     drive, forward-slash+lower-drive, and backslash+upper-drive forms —
//     see claude_local_scope.go)
//   - C (groups, P3c):    groups.yaml project_path
//
// To compose those four into ONE per-project lens, every path must reduce to
// ONE comparison string for a given project. CanonicalProjectKey is that one
// owner, so no consumer ever re-derives the join form divergently (the T2
// "no 4th normalizer" constraint).
//
// The normalization is: separators made native (FromSlash), Clean'd
// (collapsing `.`/`..`/redundant separators), forced to FORWARD slashes
// (ToSlash) for a separator-agnostic comparison string, the trailing slash
// trimmed, and case-folded on Windows AND macOS (NTFS and the default
// APFS/HFS+ are both case-insensitive; the dev host showed mixed-case drive
// letters in the keys). On Linux it stays case-sensitive (default ext4) and
// the GOOS check is a no-op on separators.
//
// CASE-SENSITIVITY TRADEOFF (deliberate parity call, not an oversight): both
// APFS and NTFS technically SUPPORT a case-sensitive mode (an opt-in APFS
// volume format, or NTFS's per-directory case-sensitive flag), and this
// function folds case unconditionally on both GOOS=windows and GOOS=darwin
// regardless of the actual volume's mode. The existing Windows branch already
// made this call for NTFS (ignoring the rare case-sensitive-NTFS host); this
// extension applies the SAME reasoning to macOS so the two case-insensitive-
// by-default platforms behave consistently. Matching the platform DEFAULT
// rather than probing the live filesystem keeps this a pure, fast,
// dependency-free string function — probing would need a stat/getattrlist
// syscall per call and still race a volume reformat. A host that deliberately
// runs a case-sensitive APFS or NTFS volume accepts the same false-collision
// risk the Windows case already accepted: two distinctly-cased paths that are
// different directories on disk will fold to the same project key. This is
// documented, not silently assumed.
//
// ABSOLUTE-PATH CONTRACT (deliberately NOT filepath.Abs): every real input is
// already an absolute path — realRoot is the EvalSymlinks output of an
// absolute-validated root (ProjectScanConfigPaths), and a raw projects.<key>
// is always written by Claude Code at the project's absolute path. This owner
// therefore does NOT call filepath.Abs: doing so would read the process
// working directory for a (never-occurring) relative input, introducing an
// ambient-input / determinism dependency into a lower module for no real
// benefit. Absoluteness is the caller's contract; this function is a pure,
// CWD-free string normalization. It does NOT validate, stat, or resolve
// symlinks (that is ProjectScanConfigPaths' job for the config-read surface).
// An empty input returns "".
package clients

import (
	"path/filepath"
	"runtime"
	"strings"
)

// CanonicalProjectKey normalizes an absolute project path to ONE comparison
// form so divergent path shapes (forward-slash+upper-drive, forward-slash+
// lower-drive, backslash+upper-drive, trailing-slash variants) for the SAME
// project all reduce to the same string. See the package-level doc for the
// absolute-path contract and the deliberate omission of filepath.Abs. An empty
// input returns "".
func CanonicalProjectKey(p string) string {
	if p == "" {
		return ""
	}
	// FromSlash makes the separators native (/ → \ on Windows; no-op POSIX) so
	// filepath.Clean collapses segments uniformly regardless of the input's
	// separator style. ToSlash then forces forward slashes for a stable,
	// separator-agnostic comparison string.
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	// Trim only a NON-ROOT trailing separator (finding 3, bot PR #433 r3):
	// `foo/` → `foo`, but a ROOT path must keep its slash or the key collapses
	// to an UNADDRESSABLE empty/bare-drive string. POSIX `/` and a Windows drive
	// root `C:/` (Clean already normalizes `C:\` → `C:\` → ToSlash `C:/`) ARE
	// valid project roots the path validator (ProjectScanConfigPaths) ACCEPTS, so
	// trimming ALL trailing slashes here made `/` → "" and `C:/` → "c:": the
	// aggregate skips empty keys and the claude-local matcher treats "" as
	// no-match, so a root project became unreachable. trimNonRootTrailingSlash
	// keeps the root form intact while still trimming a trailing slash from a
	// non-root path.
	cleaned = trimNonRootTrailingSlash(cleaned)
	if caseFoldsProjectKey(runtime.GOOS) {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

// caseFoldsProjectKey reports whether goos's DEFAULT filesystem is
// case-insensitive, so CanonicalProjectKey must fold case for join keys to
// match across differently-cased paths to the SAME project. Windows (NTFS)
// and Darwin (APFS/HFS+) default to case-insensitive; Linux (ext4) defaults
// to case-sensitive. See the case-sensitivity tradeoff note on
// CanonicalProjectKey for why this matches the platform default rather than
// probing the live volume's actual case-sensitivity mode.
func caseFoldsProjectKey(goos string) bool {
	return goos == "windows" || goos == "darwin"
}

// trimNonRootTrailingSlash drops a single trailing forward slash from a
// ToSlash'd, Clean'd path UNLESS doing so would erase a filesystem root,
// keeping a root project addressable (finding 3). filepath.Clean already
// removes redundant separators, so at most one trailing slash can survive and
// only on a root: POSIX `/`, or a Windows drive/UNC root like `C:/` or `//host/`
// — these are kept verbatim; every non-root path (`foo/`, `/dev/proj/`) has its
// trailing slash removed. A path that is already root-shaped (no trailing slash,
// e.g. `/dev/proj`) is returned unchanged.
func trimNonRootTrailingSlash(cleaned string) string {
	if !strings.HasSuffix(cleaned, "/") {
		return cleaned
	}
	trimmed := strings.TrimRight(cleaned, "/")
	// If trimming removed EVERYTHING (POSIX `/`) or left only a volume name with
	// no path component (Windows `C:` from `C:/`, or `//host` from a UNC root),
	// the original was a root: keep the root-with-slash form so the key stays a
	// non-empty, addressable comparison string.
	if trimmed == "" || filepath.VolumeName(filepath.FromSlash(cleaned)) == trimmed {
		return cleaned
	}
	return trimmed
}
