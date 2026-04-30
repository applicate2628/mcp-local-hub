//go:build unix

package cli

import (
	"fmt"
	"os"
	"syscall"
)

// extractSignal returns a "signal=NAME" suffix when the process was
// terminated by a signal, otherwise "". On Unix, ProcessState.ExitCode()
// returns -1 for signal-terminated processes, hiding exactly the
// SIGKILL/OOM/crash distinction the diagnostic is meant to surface.
// Codex bot review on PR #33 P2.
//
// WaitStatus.Signaled() is true when the child was terminated by a
// signal that was not caught; Signal() returns the syscall.Signal
// (SIGKILL, SIGTERM, SIGSEGV, ...). For core-dump cases (Signaled+CoreDump)
// we still surface the signal — the dump itself is on disk if the OS
// is configured for it.
func extractSignal(state *os.ProcessState) string {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return ""
	}
	if !ws.Signaled() {
		return ""
	}
	return fmt.Sprintf(" signal=%s", ws.Signal())
}
