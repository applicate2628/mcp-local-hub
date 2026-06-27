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
// trimmed, and case-folded on Windows (NTFS is case-insensitive; the dev
// host showed mixed-case drive letters in the keys). On POSIX it is
// case-sensitive and a no-op on separators.
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
	cleaned = strings.TrimRight(cleaned, "/") // drop a trailing slash if any survived
	if runtime.GOOS == "windows" {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}
