package perftools

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestRunCaptureLimited_EnforcesStdoutLimit(t *testing.T) {
	t.Setenv("BASH_ENV", "")
	t.Setenv("ENV", "")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; test needs a portable stdout-producing helper")
	}
	// Exercise output limits with explicit fixture environment, without oneAPI
	// discovery or capture. Environment policy is separate from this unit oracle.
	_, err = runCaptureLimitedWithEnv(t.Context(), bash, "", []string{"-p", "-c", "printf '1234567890'"}, 5, 1024, os.Environ())
	if err == nil {
		t.Fatal("expected output limit error, got nil")
	}
	if !strings.Contains(err.Error(), errOutputLimitExceeded.Error()) {
		t.Fatalf("expected output limit error, got: %v", err)
	}
}
