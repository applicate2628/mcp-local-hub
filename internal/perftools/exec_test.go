package perftools

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRunCaptureLimited_EnforcesStdoutLimit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(iwyuHelperModeEnv, "helper-iwyu-stderr")
	args := []string{"-test.run=^TestIWYUHelperProcess$"}
	env := []string{iwyuHelperModeEnv + "=helper-iwyu-stdout"}
	want := strings.Repeat("A", 3*1024*1024)

	got, err := runCaptureLimitedWithEnv(t.Context(), exe, "", args, env, len(want)+1024, 1024)
	if err != nil {
		t.Fatalf("run stdout fixture with uncapped payload: %v", err)
	}
	if got.ExitCode != 0 || len(got.Stderr) != 0 || string(got.Stdout) != want {
		t.Fatalf("wrong stdout fixture: exit=%d, stdout=%d bytes, stderr=%d bytes", got.ExitCode, len(got.Stdout), len(got.Stderr))
	}

	got, err = runCaptureLimitedWithEnv(t.Context(), exe, "", args, env, 5, 1024)
	if !errors.Is(err, errOutputLimitExceeded) || got != nil {
		t.Fatalf("expected stdout limit error without capture, got result=%t, error=%v", got != nil, err)
	}
}
