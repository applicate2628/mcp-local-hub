//go:build unix

package cli

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// On POSIX, ProcessState.ExitCode() returns -1 when the child was
// terminated by a signal — that's exactly the SIGKILL/OOM/crash mode
// formatChildExit must surface. Verify the signal=NAME suffix lands
// in the diagnostic. Spawns `sleep` then SIGKILLs it, mirroring the
// real silent-exit pattern (no stderr, no exit code, just signal).
//
// Skipped on environments without `sleep` (extremely rare on Unix CI
// runners) or when SIGKILL is unavailable.
func TestFormatChildExit_SignalKilledShowsSignal(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skipf("sleep not on PATH: %v", err)
	}
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	// Give cmd.Wait() a bounded window — sleep returns immediately on signal.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return within 2s after SIGKILL")
	}
	suffix := formatChildExit(cmd.ProcessState)
	if !strings.Contains(suffix, "signal=") {
		t.Errorf("suffix=%q must contain signal=... after SIGKILL — exit_code=-1 alone is insufficient diagnostic", suffix)
	}
	if !strings.Contains(strings.ToLower(suffix), "killed") {
		t.Errorf("suffix=%q expected to contain 'killed' (SIGKILL signal name)", suffix)
	}
	// Verify the test setup was meaningful: ExitCode() must be -1
	// (signal-terminated). If it's not, the build-tag/platform check
	// is broken or a different exit path fired.
	if got := cmd.ProcessState.ExitCode(); got != -1 {
		t.Errorf("ProcessState.ExitCode() = %d, want -1 (signal-terminated). Test cannot validate signal extraction without it.", got)
	}
	// Defense-in-depth: ensure os import is exercised on this build tag.
	_ = os.Getpid
}
