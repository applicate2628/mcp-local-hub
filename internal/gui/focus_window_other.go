// internal/gui/focus_window_other.go
//go:build !windows

package gui

import "fmt"

// FocusBrowserWindow is a no-op on non-Windows. The GUI is
// Windows-first per spec §2.2; Linux/macOS tray and browser-focus
// integration are explicit non-goals. Returning an error rather than
// silently succeeding lets the caller log honestly that the action
// did nothing on this platform.
func FocusBrowserWindow(titleSubstring string) error {
	return fmt.Errorf("FocusBrowserWindow: not implemented on non-Windows (tray/focus is Windows-only per spec §2.2)")
}
