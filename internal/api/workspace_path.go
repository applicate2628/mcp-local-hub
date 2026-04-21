package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CanonicalWorkspacePath resolves p to an absolute, symlink-resolved,
// cleaned path usable as the deterministic input to WorkspaceKey.
//
// Rules:
//   - filepath.Abs(filepath.Clean(p)) for initial normalization
//   - filepath.EvalSymlinks on the full path so symlinks in ANY component
//     (not just the final one) resolve to their targets. Critical for
//     de-duplication: aliased parents like "/tmp/foo → /var/tmp/foo"
//     must not produce two different WorkspaceKey values for the same
//     underlying directory, which would spawn duplicate scheduler and
//     client state and make unregister/migration inconsistent across
//     aliases.
//   - the final resolved path must exist and be a directory
//   - on Windows, the drive-letter is lowercased (rest preserved) so
//     "C:/foo" and "c:/foo" produce the same workspace key.
func CanonicalWorkspacePath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("workspace path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", abs, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", resolved, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("workspace %s: not a directory", resolved)
	}
	if runtime.GOOS == "windows" && len(resolved) >= 2 && resolved[1] == ':' {
		resolved = strings.ToLower(string(resolved[0])) + resolved[1:]
	}
	return resolved, nil
}

// CanonicalWorkspacePathForCleanup returns a canonical path for
// unregister / cleanup: same normalization as CanonicalWorkspacePath
// (abs + clean + EvalSymlinks + drive-letter lowercase) but does NOT
// require the directory to exist. If the path still resolves, we use
// the resolved form so the workspace_key matches what Register
// persisted. If the path is gone (deleted, moved, unavailable drive),
// we fall back to abs+clean+drive-lowercase so the operator can still
// remove orphan scheduler tasks / client entries / registry rows.
func CanonicalWorkspacePathForCleanup(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("workspace path: %w", err)
	}
	// Try to resolve symlinks so Register/Unregister agree on the key.
	// Failure here (path missing, permission denied) is not fatal — fall
	// back to the unresolved abs form.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if runtime.GOOS == "windows" && len(abs) >= 2 && abs[1] == ':' {
		abs = strings.ToLower(string(abs[0])) + abs[1:]
	}
	return abs, nil
}

// WorkspaceKey returns a short deterministic hex identifier for a canonical
// workspace path. 8 hex chars = 32 bits of entropy. Used in scheduler task
// names and client entry suffixes — the raw path (with backslashes, colons,
// spaces) is unsafe inside Task Scheduler names.
//
// Collision risk within a single user: birthday bound ≈ 77k workspaces for
// 50% collision probability. Real users have <100.
func WorkspaceKey(canonicalPath string) string {
	sum := sha256.Sum256([]byte(canonicalPath))
	return hex.EncodeToString(sum[:])[:8]
}
