//go:build windows

package cli

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
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
	retried := false
	degraded := false
	want := errors.New("synthetic non-access-denied spawn failure")
	rebuild := func() *exec.Cmd { retried = true; return exec.Command("cmd", "/c", "exit", "0") }
	_, err := startSupervisorDetachedBreakawayWithStart(
		exec.Command("cmd", "/c", "exit", "0"),
		rebuild,
		func(error) { degraded = true },
		func(*exec.Cmd) error { return want },
	)
	if !errors.Is(err, want) {
		t.Fatalf("start error = %v, want synthetic non-ACCESS_DENIED failure", err)
	}
	if retried {
		t.Fatal("a non-ACCESS_DENIED spawn error must NOT trigger the flagless rebuild/retry")
	}
	if degraded {
		t.Fatal("onDegrade must NOT fire for a non-ACCESS_DENIED error")
	}
}

func TestStartSupervisorDetachedBreakaway_AccessDeniedRetriesFlagless(t *testing.T) {
	initial := exec.Command("cmd", "/c", "exit", "0")
	retry := exec.Command("cmd", "/c", "exit", "0")
	starts := 0
	degraded := false
	started, err := startSupervisorDetachedBreakawayWithStart(
		initial,
		func() *exec.Cmd { return retry },
		func(err error) {
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Errorf("degrade error = %v, want ERROR_ACCESS_DENIED", err)
			}
			degraded = true
		},
		func(cmd *exec.Cmd) error {
			starts++
			if starts == 1 {
				if cmd != initial {
					t.Fatal("first start used retry command")
				}
				return windows.ERROR_ACCESS_DENIED
			}
			if cmd != retry {
				t.Fatal("retry start did not use rebuilt command")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("retry start: %v", err)
	}
	if started != retry {
		t.Fatal("returned command is not the successfully retried command")
	}
	if starts != 2 {
		t.Fatalf("start calls = %d, want 2", starts)
	}
	if !degraded {
		t.Fatal("onDegrade was not called for ERROR_ACCESS_DENIED")
	}
	if retry.SysProcAttr != nil && retry.SysProcAttr.CreationFlags&winCreateBreakawayFromJob != 0 {
		t.Fatalf("retry command retained CREATE_BREAKAWAY_FROM_JOB, flags=%#x", retry.SysProcAttr.CreationFlags)
	}
}

func TestStartSupervisorDetachedBreakaway_FlaglessRetryFailureDoesNotReportDegradeSuccess(t *testing.T) {
	initial := exec.Command("cmd", "/c", "exit", "0")
	retry := exec.Command("cmd", "/c", "exit", "0")
	retryErr := errors.New("synthetic flagless retry failure")
	starts := 0
	degradeCalls := 0

	started, err := startSupervisorDetachedBreakawayWithStart(
		initial,
		func() *exec.Cmd { return retry },
		func(error) { degradeCalls++ },
		func(cmd *exec.Cmd) error {
			starts++
			switch starts {
			case 1:
				if cmd != initial {
					t.Fatal("first start used retry command")
				}
				return windows.ERROR_ACCESS_DENIED
			case 2:
				if cmd != retry {
					t.Fatal("retry start did not use rebuilt command")
				}
				return retryErr
			default:
				t.Fatalf("unexpected start call %d", starts)
				return nil
			}
		},
	)

	if started != retry {
		t.Fatal("returned command is not the failed retry command")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("error = %v, want initial ERROR_ACCESS_DENIED in causal chain", err)
	}
	if !errors.Is(err, retryErr) {
		t.Fatalf("error = %v, want flagless retry error in causal chain", err)
	}
	if starts != 2 {
		t.Fatalf("start calls = %d, want 2", starts)
	}
	if degradeCalls != 0 {
		t.Fatalf("onDegrade calls = %d, want 0 when flagless retry failed", degradeCalls)
	}
}
