//go:build windows

package gui

import (
	"os/exec"
	"testing"
)

// These tests pin the breakaway-tolerant manual-restart spawn helper
// (POST /api/supervisor/restart). They cover the two STATE-SAFE branches:
//
//   (a) common path — a successful spawn returns the started cmd and never
//       strips flags;
//   (b) a non-ERROR_ACCESS_DENIED start failure (e.g. missing binary)
//       propagates unchanged — it must NOT be masked by the flagless /
//       minimal-flag retry chain.
//
// The §5-residual minimal-flag fallback (clear ALL CreationFlags on a second
// ERROR_ACCESS_DENIED) is NOT unit-tested here: triggering a real
// ERROR_ACCESS_DENIED requires a breakaway-rejecting parent Job Object (same
// reason the sibling internal/cli helper documents it as field-path-only),
// and that branch also emits a hub-mcp event (state write) which must not run
// in a fleet-live test. The live locked-down-host field path covers it.

// TestStartDetachedSupervisorTolerant_SuccessReturnsStarted asserts the
// common dev-host path: build()+Start() succeeds, the started cmd is returned,
// and no error surfaces.
func TestStartDetachedSupervisorTolerant_SuccessReturnsStarted(t *testing.T) {
	build := func() *exec.Cmd { return exec.Command("cmd", "/c", "exit", "0") }
	started, err := startDetachedSupervisorTolerant(build)
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
	bad := `Z:\definitely\nonexistent\mcphub-nope.exe`
	calls := 0
	build := func() *exec.Cmd { calls++; return exec.Command(bad) }
	_, err := startDetachedSupervisorTolerant(build)
	if err == nil {
		t.Fatal("expected a spawn error for a nonexistent binary")
	}
	// build() should fire exactly once: the initial attempt. A non-
	// ACCESS_DENIED error must short-circuit BEFORE any retry rebuild.
	if calls != 1 {
		t.Fatalf("non-ACCESS_DENIED error must not trigger a retry rebuild; build() called %d times (want 1)", calls)
	}
}
