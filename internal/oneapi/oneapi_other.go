//go:build !windows

package oneapi

import (
	"os"
	"path/filepath"
)

// rootProbePaths returns no candidates on non-Windows platforms: oneAPI
// PATH injection is Windows-focused in this iteration, so DetectRoot
// always returns ("", false) here and the supervisor never injects. A
// Linux extension is possible future work.
func rootProbePaths() []string { return nil }

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
