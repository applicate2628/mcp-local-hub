package perftools

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRunCaptureLimited_EnforcesStdoutLimit(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; test needs a portable stdout-producing helper")
	}
	_, err = runCaptureLimited(t.Context(), bash, "", []string{"-lc", "printf '1234567890'"}, 5, 1024)
	if err == nil {
		t.Fatal("expected output limit error, got nil")
	}
	if !strings.Contains(err.Error(), errOutputLimitExceeded.Error()) {
		t.Fatalf("expected output limit error, got: %v", err)
	}
}
