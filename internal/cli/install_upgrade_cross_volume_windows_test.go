//go:build windows

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestStageV5UpgradeBinary_StagesBesideCanonicalTarget(t *testing.T) {
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	sourceFixture := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "CROSS-VOLUME-STAGING")
	source := filepath.Join(sourceDir, "mcphub.exe")
	target := filepath.Join(targetDir, "mcphub.exe")
	want, err := os.ReadFile(sourceFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, want, 0o700); err != nil {
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
	if !bytes.Equal(raw, want) {
		t.Fatalf("staged content differs from the admitted source fixture")
	}
}
