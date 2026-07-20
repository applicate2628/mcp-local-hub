//go:build windows

package process

import (
	"syscall"
	"testing"
	"unsafe"
)

var (
	kernel32TestDLL           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32TestDLL.NewProc("GetConsoleProcessList")
	procAllocConsole          = kernel32TestDLL.NewProc("AllocConsole")
)

// hasConsole reports whether this process is currently a CLIENT of a
// console — the property that decides whether a closing terminal can
// deliver CTRL_CLOSE_EVENT to it.
//
// GetConsoleProcessList is the correct probe. The obvious-looking
// alternative, GetConsoleWindow() != 0, is WRONG here and was verified so
// on Windows 11: under a pseudoconsole (ConPTY — what Windows Terminal and
// modern shells use) an attached process has NO console WINDOW, so
// GetConsoleWindow returns NULL while GetConsoleProcessList correctly
// reports the process as attached. Using the window probe made this test
// mis-detect its own precondition and skip instead of assert.
//
// GetConsoleProcessList returns 0 (with ERROR_INVALID_HANDLE) when the
// caller has no console, and a nonzero client count otherwise.
func hasConsole() bool {
	var buf [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return n != 0
}

// TestReleaseParentConsole_DetachesAnAttachedConsole is the real assertion
// behind the "GUI survives its terminal" fix.
//
// The test must run with a console ACTUALLY ATTACHED, otherwise it passes
// vacuously — it would still pass against a ReleaseParentConsole that did
// nothing at all. Depending on how the suite is invoked the test binary
// may already have inherited a console (terminal run) or have none at all
// (stdout is a pipe under `go test` in CI), so the precondition is
// established WITHOUT calling the function under test:
//
//   - console already attached → use it as-is.
//   - none → allocate one.
//
// Establishing the precondition via ReleaseParentConsole itself would
// couple setup to the thing being measured: a broken (no-op)
// implementation would then leave the inherited console in place,
// AllocConsole would fail with ERROR_ACCESS_DENIED (a process may own only
// one console), and the test would SKIP instead of FAIL — silently
// downgrading a real regression to a green run.
func TestReleaseParentConsole_DetachesAnAttachedConsole(t *testing.T) {
	if !hasConsole() {
		if ret, _, err := procAllocConsole.Call(); ret == 0 {
			t.Skipf("no console attached and AllocConsole unavailable in this "+
				"environment (%v); cannot construct the precondition", err)
		}
	}
	// Leave a console-free state for sibling tests regardless of outcome.
	defer ReleaseParentConsole()

	if !hasConsole() {
		t.Fatal("precondition failed: no console attached, so this test would assert nothing")
	}

	ReleaseParentConsole()

	if hasConsole() {
		t.Fatal("ReleaseParentConsole left a console attached; the process remains a " +
			"CTRL_CLOSE_EVENT target and `mcphub gui` will still die with its terminal")
	}
}

// TestReleaseParentConsole_NoConsoleIsSafeNoOp pins the Explorer
// double-click / detached-spawn path: calling with nothing attached must
// neither panic nor leave a console attached, and must stay idempotent.
func TestReleaseParentConsole_NoConsoleIsSafeNoOp(t *testing.T) {
	ReleaseParentConsole()
	if hasConsole() {
		t.Fatal("console attached after release")
	}

	ReleaseParentConsole() // repeat: must be a harmless no-op
	if hasConsole() {
		t.Fatal("console attached after repeated release")
	}
}
