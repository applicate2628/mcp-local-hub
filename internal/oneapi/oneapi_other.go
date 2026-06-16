//go:build !windows

package oneapi

import (
	"os"
	"path/filepath"
)

// rootProbePaths returns the candidate Intel oneAPI install roots on POSIX
// hosts (Linux + macOS), in priority order:
//
//  1. $ONEAPI_ROOT, if set.
//  2. /opt/intel/oneapi          (the default system-wide install).
//  3. <home>/intel/oneapi        (the default per-user install).
//
// DetectRoot picks the FIRST that exists as a directory. On a host without
// oneAPI all candidates are absent, so DetectRoot returns ("", false) and the
// SetvarsEnv / DLLDirs consumers degrade to os.Environ() — the clean no-op.
func rootProbePaths() []string {
	var paths []string
	if r := os.Getenv("ONEAPI_ROOT"); r != "" {
		paths = append(paths, r)
	}
	paths = append(paths, filepath.Join("/opt", "intel", "oneapi"))
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, "intel", "oneapi"))
	}
	return paths
}

// realDirExists reports whether path is an existing directory. Retained on
// POSIX so the test seam (which overrides dirExists) has a real default,
// even though rootProbePaths returns nothing in production.
func realDirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// realDirHasDLL reports whether dir contains at least one *.dll file.
// Retained on POSIX for the test-seam default; production never reaches it
// (DetectRoot returns no root).
func realDirHasDLL(dir string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, "*.dll"))
	if err != nil {
		return false
	}
	return len(matches) > 0
}

// realListComponentDirs returns the immediate sub-directory names under
// root. Retained on POSIX for the test-seam default.
func realListComponentDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}
