//go:build !windows

package serena_routing

import (
	"os"
	"testing"
)

// Only used on fresh files under the test-owned hardened state directory.
func writeReadRelaxedMarker(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write marker fixture: %v", err)
	}
	// WriteFile applies umask. Chmod establishes the test condition while
	// keeping writes owner-only, regardless of the invoking shell's umask.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("set marker read-only broadening: %v", err)
	}
}
