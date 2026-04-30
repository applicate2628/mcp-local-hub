// internal/gui/headless_test.go
package gui

import (
	"runtime"
	"testing"
)

// TestHeadlessSession_LinuxNoDisplay confirms an empty $DISPLAY +
// $WAYLAND_DISPLAY (the SSH-without-X11-forwarding shape) reports
// headless on Linux. Skipped on non-Linux because the function returns
// false there regardless of env.
func TestHeadlessSession_LinuxNoDisplay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only env-driven detection")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if !HeadlessSession() {
		t.Error("expected HeadlessSession()=true with both display env vars empty")
	}
}

// TestHeadlessSession_LinuxWithDisplay confirms either env var set
// flips the result. This is the desktop-Linux shape.
func TestHeadlessSession_LinuxWithDisplay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only env-driven detection")
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":0")
	if HeadlessSession() {
		t.Error("expected HeadlessSession()=false with $DISPLAY set")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	if HeadlessSession() {
		t.Error("expected HeadlessSession()=false with $WAYLAND_DISPLAY set")
	}
}

// TestHeadlessSession_NonLinuxDefaultsFalse confirms the function
// returns false on macOS/Windows regardless of env, since we don't
// have a reliable cross-platform headless heuristic for those yet.
func TestHeadlessSession_NonLinuxDefaultsFalse(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("only meaningful on non-Linux")
	}
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if HeadlessSession() {
		t.Errorf("expected HeadlessSession()=false on %s; only Linux honors $DISPLAY heuristic", runtime.GOOS)
	}
}

// TestSetHeadlessSessionForTest_OverridesAndRestores confirms the test
// seam covers both directions and the restore function actually restores.
func TestSetHeadlessSessionForTest_OverridesAndRestores(t *testing.T) {
	// Capture pre-call state by reading once (no override active).
	before := HeadlessSession()

	restoreTrue := SetHeadlessSessionForTest(true)
	if !HeadlessSession() {
		t.Error("override(true) did not take effect")
	}
	restoreTrue()
	if got := HeadlessSession(); got != before {
		t.Errorf("after restore, HeadlessSession()=%v want %v", got, before)
	}

	restoreFalse := SetHeadlessSessionForTest(false)
	if HeadlessSession() {
		t.Error("override(false) did not take effect")
	}
	restoreFalse()
	if got := HeadlessSession(); got != before {
		t.Errorf("after restore, HeadlessSession()=%v want %v", got, before)
	}
}
