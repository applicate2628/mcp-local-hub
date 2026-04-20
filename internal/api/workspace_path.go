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

// CanonicalWorkspacePath resolves p to an absolute, cleaned path usable as
// the deterministic input to WorkspaceKey.
//
// Rules:
//   - filepath.Abs(filepath.Clean(p)) — same normalization as internal/cli/setup.go
//   - on Windows, the drive-letter is lowercased (rest preserved) so "C:/foo"
//     and "c:/foo" produce the same workspace key — Windows paths are
//     case-insensitive on NTFS by default
//   - the path must exist (os.Lstat succeeds)
//   - the path must NOT be a symlink / reparse point — reject with a clear error;
//     the user should pass the resolved path instead. This avoids the permissions
//     surface of opening arbitrary directories via GetFinalPathNameByHandle.
func CanonicalWorkspacePath(p string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("workspace path: %w", err)
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("workspace %s: not a directory", abs)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("workspace %s: path is a symlink / reparse point; pass the resolved path instead", abs)
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
