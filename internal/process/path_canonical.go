package process

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// CanonicalizePathStrict returns one absolute, cleaned, fully-resolved path.
// Unlike the best-effort identity normalizers, it never falls back to an
// unresolved spelling: a missing path, broken link/reparse point, permission
// failure, or resolution loop is returned to the caller as an error.
func CanonicalizePathStrict(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("strict path canonicalization: empty path")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("strict path canonicalization %q: absolute path: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("strict path canonicalization %q: resolve links: %w", path, err)
	}
	resolved, err = filepath.Abs(filepath.Clean(resolved))
	if err != nil {
		return "", fmt.Errorf("strict path canonicalization %q: resolved absolute path: %w", path, err)
	}
	if runtime.GOOS == "windows" {
		resolved = normalizeStrictWindowsPath(resolved)
	}
	return filepath.Clean(resolved), nil
}

// normalizeStrictWindowsPath folds the two extended-length spellings returned
// by Windows file APIs into their ordinary drive/UNC equivalents. The path has
// already been resolved successfully; this is representation normalization,
// not a fallback around failed resolution.
func normalizeStrictWindowsPath(path string) string {
	switch {
	case strings.HasPrefix(path, `\\?\UNC\`):
		return `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	case strings.HasPrefix(path, `\\?\`):
		return strings.TrimPrefix(path, `\\?\`)
	default:
		return path
	}
}
