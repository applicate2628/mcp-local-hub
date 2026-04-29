// internal/gui/focus_window.go — cross-platform sentinel error for
// FocusBrowserWindow. Defined here (no build tag) so callers can do
// `errors.Is(err, gui.ErrFocusNoWindow)` from any GOOS.
package gui

import "errors"

// ErrFocusNoWindow signals "enumeration completed without finding a
// matching window." Callers (notably the activate-window callback in
// cli/gui.go) use this sentinel to decide whether to fall back to
// opening a fresh browser via LaunchBrowser. Other Win32 failures
// (e.g., transient SetForegroundWindow rejection on Windows 10+ when
// the calling thread isn't the foreground thread) wrap a different
// error so the fallback does NOT fire — those are best logged and
// retried, not duplicated into spurious extra windows.
//
// Codex PR #22 r3 P2 fix: prior implementation logged via plain
// fmt.Errorf and the callback fell back on ANY error, which on
// non-Windows (focus_window_other.go always errors) and on
// transient Win32 failures spawned a new dashboard every activate
// request.
var ErrFocusNoWindow = errors.New("FocusBrowserWindow: no matching window")
