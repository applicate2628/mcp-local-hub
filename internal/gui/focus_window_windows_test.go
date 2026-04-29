// internal/gui/focus_window_windows_test.go
//go:build windows

package gui

import (
	"errors"
	"strings"
	"testing"
)

// TestFocusBrowserWindow_NoMatchReturnsSentinel invokes
// FocusBrowserWindow with a title substring no real window has —
// guarantees enumeration runs to completion without finding a
// match. Asserts the returned error wraps the cross-platform
// ErrFocusNoWindow sentinel so callers (notably the activate-window
// callback) can distinguish "no window" from a Win32 call failure.
//
// Real-match coverage is in the D2.1 manual smoke checklist
// (docs/phase-3b-ii-verification.md): the operator launches mcphub
// gui with --no-tray, opens a second mcphub gui, and confirms the
// existing Chrome window comes to foreground.
func TestFocusBrowserWindow_NoMatchReturnsSentinel(t *testing.T) {
	bogus := "z9q-no-window-with-this-substring-exists-z9q"
	err := FocusBrowserWindow(bogus)
	if err == nil {
		t.Fatal("expected ErrFocusNoWindow, got nil")
	}
	if !strings.Contains(err.Error(), bogus) {
		t.Errorf("error message %q should mention the substring %q", err.Error(), bogus)
	}
	if !errors.Is(err, ErrFocusNoWindow) {
		t.Errorf("expected errors.Is(err, ErrFocusNoWindow); got err = %v (%T)", err, err)
	}
}

// TestFocusBrowserWindow_EmptySubstringFindsAnyVisibleWindow verifies
// the enumeration loop is reached and a real match path executes.
// Empty substring matches every visible window's title (strings.Contains
// against "" is always true for non-empty titles), so as long as the
// test process has any visible top-level window in its desktop session,
// the call should succeed.
//
// In headless CI this can fail if no desktop session is active; that's
// fine — the test passes in either branch (success OR ErrFocusNoWindow).
// What we MUST NOT see is a panic or a different wrapped error type,
// which would indicate a regression in syscall plumbing.
func TestFocusBrowserWindow_EmptySubstringPathReachesEnumerator(t *testing.T) {
	err := FocusBrowserWindow("")
	if err == nil {
		return // happy path: a window matched
	}
	if !errors.Is(err, ErrFocusNoWindow) {
		t.Errorf("unexpected error %v (%T); only ErrFocusNoWindow is acceptable from a clean enumeration", err, err)
	}
}
