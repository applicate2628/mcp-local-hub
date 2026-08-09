//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageV5UpgradeBinary_StagesBesideCanonicalTarget(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	source := filepath.Join(sourceDir, "mcphub.exe")
	target := filepath.Join(targetDir, "mcphub.exe")
	if err := os.WriteFile(source, []byte("new-binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	staged, err := stageV5UpgradeBinary(source, target)
	if err != nil {
		t.Fatalf("stageV5UpgradeBinary: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(staged) })
	if got, want := filepath.Dir(staged), filepath.Dir(target); got != want {
		t.Fatalf("staged directory = %q, want canonical target directory %q", got, want)
	}
	if got, want := staged, target+".new"; got != want {
		t.Fatalf("staged path = %q, want %q", got, want)
	}
	raw, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged binary: %v", err)
	}
	if got, want := string(raw), "new-binary"; got != want {
		t.Fatalf("staged content = %q, want %q", got, want)
	}
}
