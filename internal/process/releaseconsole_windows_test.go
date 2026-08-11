//go:build windows

package process

import (
	"os/exec"
	"syscall"
	"testing"
	"unsafe"
)

var (
	kernel32TestDLL              = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessListTst = kernel32TestDLL.NewProc("GetConsoleProcessList")
	procAllocConsole             = kernel32TestDLL.NewProc("AllocConsole")
	procGetStdHandleTst          = kernel32TestDLL.NewProc("GetStdHandle")
	procGetFileTypeTst           = kernel32TestDLL.NewProc("GetFileType")
)

// hasConsole reports whether this process is currently a CLIENT of a
// console — the property that decides whether a closing terminal can
// deliver CTRL_CLOSE_EVENT to it.
//
// This deliberately duplicates the production HasConsole rather than
// calling it. An oracle that IS the code under test cannot fail when that
// code is wrong: a HasConsole stubbed to always-false would make the
// assertions below pass while measuring nothing. Keep the duplicate.
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
	n, _, _ := procGetConsoleProcessListTst.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return n != 0
}

// stdErrHandle returns the raw STD_ERROR_HANDLE slot value.
const stdErrorHandleTst = ^uintptr(11) // (DWORD)-12

func stdErrHandle() uintptr {
	h, _, _ := procGetStdHandleTst.Call(stdErrorHandleTst)
	return h
}

// stdHandleProbesValid mirrors internal/tray's stderrIsValid: the exact
// question CreateProcess will ask when the tray child is spawned with
// this handle.
func stdHandleProbesValid(h uintptr) bool {
	if h == 0 || h == ^uintptr(0) {
		return false
	}
	t, _, err := procGetFileTypeTst.Call(h)
	if errno, ok := err.(syscall.Errno); ok && errno != 0 {
		return false
	}
	return t != 0
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
func TestReleaseParentConsole(t *testing.T) {
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

// TestReleaseParentConsole_StdErrHandleIsNotRecycledByALaterSpawn settles
// the hazard behind internal/tray's stderrIsValid guard.
//
// After the release, `mcphub gui` calls gui.LaunchBrowser and then spawns
// the tray child with `c.Stderr = os.Stderr` — but only if stderrIsValid()
// says the handle is usable. FreeConsole does NOT null the STD_ERROR_HANDLE
// slot; it leaves the same numeric value behind and only GetFileType
// reveals that the kernel object is gone. That raises the real question:
// can a process spawned in between (the browser) cause that stale value to
// be handed back out, so the guard sees a DIFFERENT, live object and
// forwards the tray's diagnostics into someone else's handle?
//
// Measured answer, pinned here: no. The handle's validity verdict is
// stable across intervening spawns. If a future Windows build changes
// that, this fails and the guard needs rethinking rather than silently
// mis-forwarding.
//
// It also asserts the release postcondition, so a ReleaseParentConsole
// reverted to a no-op fails this test and not only its siblings.
func TestReleaseParentConsole_StdErrHandleIsNotRecycledByALaterSpawn(t *testing.T) {
	if !hasConsole() {
		if ret, _, err := procAllocConsole.Call(); ret == 0 {
			t.Skipf("no console attached and AllocConsole unavailable in this "+
				"environment (%v); cannot construct the precondition", err)
		}
	}
	defer ReleaseParentConsole()
	if !hasConsole() {
		t.Fatal("precondition failed: no console attached")
	}

	before := stdErrHandle()
	ReleaseParentConsole()

	if hasConsole() {
		t.Fatal("ReleaseParentConsole left a console attached")
	}
	after := stdErrHandle()
	if after != before {
		t.Logf("NOTE: STD_ERROR_HANDLE changed across FreeConsole (0x%x -> 0x%x); "+
			"the observed platform behavior is that it does not", before, after)
	}
	validImmediately := stdHandleProbesValid(after)

	// Stand in for gui.LaunchBrowser: real process creations between the
	// release and the tray spawn, each an opportunity for the kernel to
	// reissue the freed handle value.
	for i := 0; i < 3; i++ {
		helper := exec.Command("cmd", "/c", "exit")
		NoConsole(helper)
		if err := helper.Run(); err != nil {
			t.Skipf("cannot spawn a helper process to exercise handle reuse: %v", err)
		}
	}

	if got := stdHandleProbesValid(after); got != validImmediately {
		t.Fatalf("STD_ERROR_HANDLE 0x%x changed validity across intervening spawns "+
			"(%v -> %v): the value was recycled, so internal/tray's stderrIsValid guard "+
			"can hand the tray child a handle that is no longer the console it thinks it is",
			after, validImmediately, got)
	}
}
