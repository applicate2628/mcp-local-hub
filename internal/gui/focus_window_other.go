// internal/gui/focus_window_other.go
//go:build !windows

package gui

import "fmt"

// FocusBrowserWindow is a no-op on non-Windows. The GUI is
// Windows-first per spec §2.2; Linux/macOS tray and browser-focus
// integration are explicit non-goals.
//
// Codex PR #22 r3 P2 fix: returns ErrFocusNoWindow (cross-platform
// sentinel) so the activate-window callback in cli/gui.go falls
// through to LaunchBrowser exactly as if no matching window
// existed. Returning a non-sentinel error here would have made
// every Linux/macOS activate request spawn a duplicate window
// after the fallback was added.
func FocusBrowserWindow(titleSubstring string) error {
	return fmt.Errorf("%w: non-Windows build (tray/focus is Windows-only per spec §2.2)",
		ErrFocusNoWindow)
}
