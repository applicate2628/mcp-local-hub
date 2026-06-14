//go:build windows

package cli

import (
	"os/exec"
	"testing"
)

// PART 1: the breakaway-tolerant spawn helper must (a) add
// CREATE_BREAKAWAY_FROM_JOB on the common path, and (b) reserve the
// flagless rebuild/retry STRICTLY for the ERROR_ACCESS_DENIED
// (breakaway-rejected) case — a different spawn failure (e.g. missing
// binary) must propagate, not be masked by a silent retry.

func TestStartSupervisorDetachedBreakaway_AddsFlagOnSuccess(t *testing.T) {
	build := func() *exec.Cmd { return exec.Command("cmd", "/c", "exit", "0") }
	degraded := false
	started, err := startSupervisorDetachedBreakaway(build(), build, func(error) { degraded = true })
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started == nil || started.Process == nil {
		t.Fatal("no process started")
	}
	defer func() { _, _ = started.Process.Wait() }()
	if !degraded {
		// Common dev-host case: the parent job permits breakaway (or the
		// process is in no job), so the started cmd carries the flag.
		if started.SysProcAttr == nil || started.SysProcAttr.CreationFlags&winCreateBreakawayFromJob == 0 {
			t.Fatalf("expected CREATE_BREAKAWAY_FROM_JOB on the started cmd, flags=%#x", started.SysProcAttr.CreationFlags)
		}
	}
	// If degraded (locked-down host without BREAKAWAY_OK), the retry cmd
	// is intentionally flagless — the correct fallback.
}

func TestStartSupervisorDetachedBreakaway_NonAccessDeniedError_NotRetried(t *testing.T) {
	bad := `Z:\definitely\nonexistent\mcphub-nope.exe`
	retried := false
	degraded := false
	rebuild := func() *exec.Cmd { retried = true; return exec.Command(bad) }
	_, err := startSupervisorDetachedBreakaway(exec.Command(bad), rebuild, func(error) { degraded = true })
	if err == nil {
		t.Fatal("expected a spawn error for a nonexistent binary")
	}
	if retried {
		t.Fatal("a non-ACCESS_DENIED spawn error must NOT trigger the flagless rebuild/retry")
	}
	if degraded {
		t.Fatal("onDegrade must NOT fire for a non-ACCESS_DENIED error")
	}
}
