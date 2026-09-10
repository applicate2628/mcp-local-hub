//go:build windows

package gui

import (
	"errors"
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
// that a spawn failure that is NOT ERROR_ACCESS_DENIED (a nonexistent binary
// fails the CreateProcess path lookup) propagates immediately — neither the
// breakaway-cleared retry nor the minimal-flag retry must mask it.
func TestStartDetachedSupervisorTolerant_NonAccessDeniedError_NotRetried(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing", "mcphub-nope.exe")
	calls := 0
	build := func() *exec.Cmd { calls++; return exec.Command(bad) }
	_, err := startDetachedSupervisorTolerant(build, processowner.BreakawayOptional)
	if err == nil {
		t.Fatal("expected a spawn error for a nonexistent binary")
	}
	// build() should fire exactly once: the initial attempt. A non-
	// ACCESS_DENIED error must short-circuit BEFORE any retry rebuild.
	if calls != 1 {
		t.Fatalf("non-ACCESS_DENIED error must not trigger a retry rebuild; build() called %d times (want 1)", calls)
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
