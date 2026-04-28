// internal/gui/focus_window_windows_test.go
//go:build windows

package gui

import (
	"strings"
	"testing"
)

// TestFocusBrowserWindow_NoMatchReturnsError invokes FocusBrowserWindow
// with a title substring no real window has — guarantees enumeration
// runs to completion without finding a match. Asserts the returned
// error is the typed errFocusNoWindow so callers can distinguish the
// "no window" condition from a Win32 call failure.
//
// Real-match coverage is in the D2.1 manual smoke checklist
// (docs/phase-3b-ii-verification.md): the operator launches mcphub
// gui with --no-tray, opens a second mcphub gui, and confirms the
// existing Chrome window comes to foreground.
func TestFocusBrowserWindow_NoMatchReturnsError(t *testing.T) {
	bogus := "z9q-no-window-with-this-substring-exists-z9q"
	err := FocusBrowserWindow(bogus)
	if err == nil {
		t.Fatal("expected errFocusNoWindow, got nil")
	}
	if !strings.Contains(err.Error(), bogus) {
		t.Errorf("error message %q should mention the substring %q", err.Error(), bogus)
	}
	if _, ok := err.(errFocusNoWindow); !ok {
		t.Errorf("expected errFocusNoWindow, got %T", err)
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
// fine — the test passes in either branch (success OR errFocusNoWindow).
// What we MUST NOT see is a panic or a non-typed unexpected error,
// which would indicate a regression in syscall plumbing.
func TestFocusBrowserWindow_EmptySubstringPathReachesEnumerator(t *testing.T) {
	err := FocusBrowserWindow("")
	if err == nil {
		return // happy path: a window matched
	}
	if _, ok := err.(errFocusNoWindow); !ok {
		t.Errorf("unexpected error type %T: %v (only errFocusNoWindow is acceptable from a clean enumeration)", err, err)
	}
}
