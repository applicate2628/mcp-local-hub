//go:build windows

package api

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainBinaryTargetAsideVacatesExactTargetWithoutReplacingCollision(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mcphub-windowless.exe")
	want := []byte("admitted-legacy-windowless")
	if err := os.WriteFile(target, want, 0o700); err != nil {
		t.Fatal(err)
	}

	retained, err := RetainBinaryTargetAside(target)
	if err != nil {
		t.Fatalf("retain target: %v", err)
	}
	if retained == "" || !IsGeneratedRenameAsideForTarget(target, retained) {
		t.Fatalf("retained path %q is not generated for %q", retained, target)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target still exists or is unreadable: %v", err)
	}
	got, err := os.ReadFile(retained)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("retained bytes=%q err=%v", got, err)
	}
}
