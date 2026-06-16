//go:build !windows

package oneapi

import (
	"context"
	"fmt"
	"os"
)

// setvarsProbePaths returns no candidates on non-Windows platforms:
// oneAPI env injection is Windows-focused in this iteration, so
// DetectSetvars always returns ("", false) here and the supervisor never
// injects. A Linux `setvars.sh` extension is possible future work.
func setvarsProbePaths() []string { return nil }

// realFileExists reports whether path is an existing non-directory file.
// Retained on POSIX so the test seam (which overrides fileExists) has a
// real default, even though setvarsProbePaths returns nothing in
// production.
func realFileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// realBaselineEnv returns the current process environment.
func realBaselineEnv() []string { return os.Environ() }

// realSetvarsRunner is never reached in production on POSIX (DetectSetvars
// returns no path), so it fails loud if somehow invoked. Tests inject a
// fake runner instead of relying on this.
func realSetvarsRunner(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("oneapi: setvars capture is not supported on this platform")
}
