package perftools

import (
	"strings"
	"testing"
)

func TestRunCaptureLimited_EnforcesStdoutLimit(t *testing.T) {
	_, err := runCaptureLimited(t.Context(), "bash", "", []string{"-lc", "printf '1234567890'"}, 5, 1024)
	if err == nil {
		t.Fatal("expected output limit error, got nil")
	}
	if !strings.Contains(err.Error(), errOutputLimitExceeded.Error()) {
		t.Fatalf("expected output limit error, got: %v", err)
	}
}
