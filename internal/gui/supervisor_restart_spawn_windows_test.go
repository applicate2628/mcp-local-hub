//go:build windows

package gui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	processowner "mcp-local-hub/internal/process"
)

// These integration guards pin GUI command/result mapping around the shared
// internal/process policy owner. The owner-level tests deterministically cover
// required, flagless, minimal, attempt-count, and error-classification paths.

// TestStartDetachedSupervisorTolerant_SuccessReturnsStarted asserts the
// common dev-host path: build()+Start() succeeds, the started cmd is returned,
// and no error surfaces.
func TestStartDetachedSupervisorTolerant_SuccessReturnsStarted(t *testing.T) {
	build := func() *exec.Cmd { return exec.Command("cmd", "/c", "exit", "0") }
	started, err := startDetachedSupervisorTolerant(build, processowner.BreakawayOptional)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started == nil || started.Process == nil {
		t.Fatal("no process started")
	}
	defer func() { _, _ = started.Process.Wait() }()
}

// TestStartDetachedSupervisorTolerant_NonAccessDeniedError_NotRetried asserts
// that a non-ACCESS_DENIED spawn failure propagates without either fallback.
// A containing native Job can reject breakaway before checking the image path,
// so an absent executable alone cannot select this error-classification branch.
func TestStartDetachedSupervisorTolerant_NonAccessDeniedError_NotRetried(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing", "mcphub-nope.exe")
	calls, starts := 0, 0
	build := func() *exec.Cmd { calls++; return exec.Command(bad) }
	startErr := os.NewSyscallError("CreateProcess", windows.ERROR_FILE_NOT_FOUND)
	_, err := startDetachedSupervisorTolerantWithStart(build, processowner.BreakawayOptional, func(*exec.Cmd) error {
		starts++
		return startErr
	})
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("original non-ACCESS_DENIED spawn error lost: %v", err)
	}
	if calls != 1 || starts != 1 {
		t.Fatalf("non-ACCESS_DENIED error must not retry; build calls=%d start calls=%d, want one each", calls, starts)
	}
}

func TestStartDetachedSupervisorTolerant_RequiredAccessDeniedNoFallback(t *testing.T) {
	calls := 0
	build := func() *exec.Cmd { calls++; return exec.Command("cmd", "/c", "exit", "0") }
	starts := 0
	started, err := startDetachedSupervisorTolerantWithStart(build, processowner.BreakawayRequired, func(*exec.Cmd) error {
		starts++
		return windows.ERROR_ACCESS_DENIED
	})
	if started == nil || !errors.Is(err, processowner.ErrWindowsBreakawayRequired) {
		t.Fatalf("started=%v error=%v", started, err)
	}
	if calls != 1 || starts != 1 {
		t.Fatalf("build calls=%d start calls=%d, want exactly one required attempt", calls, starts)
	}
}
