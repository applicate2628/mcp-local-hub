//go:build windows

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
)

func TestBootstrapCopyToTargetIdentityAndFailurePaths(t *testing.T) {
	t.Run("same path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcphub.exe")
		if err := os.WriteFile(path, []byte("same-path"), 0o755); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := bootstrapCopyToTarget(&out, path, path); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "no copy needed") {
			t.Fatalf("output=%q", out.String())
		}
	})

	t.Run("different path identical bytes", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "package", "mcphub.exe")
		target := filepath.Join(dir, "canonical", "mcphub.exe")
		body := []byte("byte-identical-in-use-fixture")
		for _, path := range []string{source, target} {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		before, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := bootstrapCopyToTarget(&out, source, target); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) || !strings.Contains(out.String(), "already at") {
			t.Fatalf("identical target was replaced or output missing: same=%v output=%q", os.SameFile(before, after), out.String())
		}
	})

	t.Run("different bytes use normal admitted copy", func(t *testing.T) {
		dir := t.TempDir()
		source := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "NEW")
		target := filepath.Join(dir, "mcphub.exe")
		copyAdmissionPEFixture(t, writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "OLD"), target)
		if err := bootstrapCopyToTarget(&bytes.Buffer{}, source, target); err != nil {
			t.Fatal(err)
		}
		if got, want := mustReadPairFile(t, target), mustReadPairFile(t, source); !bytes.Equal(got, want) {
			t.Fatal("different target did not take the normal admitted copy path")
		}
	})

	t.Run("hash read error does not become false identity", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "missing.exe")
		target := filepath.Join(dir, "mcphub.exe")
		prior := []byte("prior-exact")
		if err := os.WriteFile(target, prior, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := bootstrapCopyToTarget(&bytes.Buffer{}, source, target); err == nil {
			t.Fatal("unreadable source was accepted as identical")
		}
		if got := mustReadPairFile(t, target); !bytes.Equal(got, prior) {
			t.Fatalf("unreadable source mutated target: %q", got)
		}
	})
}
