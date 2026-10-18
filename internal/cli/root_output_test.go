package cli

import (
	"fmt"
	"os"
	"testing"
)

// Catches the composition root leaving Cobra's normal writer unset: Command
// Print/Printf/Println then fall back to stderr instead of stdout.
func TestNewRootCmdBindsNormalAndDiagnosticWriters(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	originalStdout, originalStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	t.Cleanup(func() {
		os.Stdout, os.Stderr = originalStdout, originalStderr
		_ = stdout.Close()
		_ = stderr.Close()
	})

	root := NewRootCmd()
	root.Print("normal-output")
	_, _ = fmt.Fprint(root.ErrOrStderr(), "diagnostic-output")
	if err := stdout.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := stderr.Sync(); err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = originalStdout, originalStderr

	stdoutBytes, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	stderrBytes, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(stdoutBytes) != "normal-output" || string(stderrBytes) != "diagnostic-output" {
		t.Fatalf("stdout/stderr = %q/%q, want normal-output/diagnostic-output", stdoutBytes, stderrBytes)
	}
}
