package gui

import (
	"path/filepath"
	"testing"
)

// TestOpenFolderAt_RecordsSeam verifies the helper invokes its
// injectable spawn function with the resolved file path. We don't
// actually open Explorer/Finder — that's a manual smoke (D2.1) per
// the verification matrix.
func TestOpenFolderAt_RecordsSeam(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "gui.pidport")
	var spawnedName string
	var spawnedArgs []string
	prev := openFolderSpawn
	defer func() { openFolderSpawn = prev }()
	openFolderSpawn = func(name string, args ...string) error {
		spawnedName = name
		spawnedArgs = args
		return nil
	}
	if err := OpenFolderAt(target); err != nil {
		t.Fatalf("OpenFolderAt: %v", err)
	}
	if spawnedName == "" {
		t.Fatalf("spawn was not invoked")
	}
	// Some args list must mention the target path (as-is or as the
	// containing directory). On Windows it's `explorer.exe /select,<path>`;
	// on macOS `open -R <path>`; on Linux `xdg-open <dir>`.
	joined := spawnedName
	for _, a := range spawnedArgs {
		joined += " " + a
	}
	if !contains(joined, target) && !contains(joined, filepath.Dir(target)) {
		t.Errorf("spawn args %q do not reference target path or its dir", joined)
	}
}

// TestOpenFolderAt_FireAndForget verifies that a spawn error does
// not propagate (best-effort by design — Codex r5 #3 PASS). The
// caller should still receive nil so the diagnostic flow continues.
func TestOpenFolderAt_FireAndForget(t *testing.T) {
	prev := openFolderSpawn
	defer func() { openFolderSpawn = prev }()
	openFolderSpawn = func(name string, args ...string) error {
		return errSpawnTest
	}
	if err := OpenFolderAt("/nonexistent/path"); err != nil {
		t.Errorf("OpenFolderAt should swallow spawn errors; got %v", err)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// errSpawnTest is a sentinel for the fire-and-forget test.
var errSpawnTest = &spawnTestError{}

type spawnTestError struct{}

func (*spawnTestError) Error() string { return "spawn-test-error" }
