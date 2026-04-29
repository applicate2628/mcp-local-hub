// internal/gui/focus_window_windows.go
//go:build windows

package gui

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// FocusBrowserWindow brings the GUI's Chrome app-mode window to the
// foreground when the second-instance handshake calls
// /api/activate-window. Returns nil on success, an error if no
// matching window was found or the SetForegroundWindow call failed
// (the caller logs but does not fail user navigation).
//
// Match strategy: enumerate top-level windows via EnumWindows, read
// each window's title via GetWindowTextW, and select the first one
// whose title contains `titleSubstring` case-insensitively. The GUI's
// <title> is "mcp-local-hub" so any Chrome app-mode window opened via
// LaunchBrowser will match. App-mode (Chrome --app=...) opens a
// chromeless window; its title comes from the page <title> rather
// than appended URL/branding, so the match is stable across Chrome
// versions.
//
// Restoration: SW_RESTORE is sent before SetForegroundWindow so a
// minimized window pops up rather than staying in the taskbar with
// only its z-order updated. SetForegroundWindow itself can return
// false even on success (e.g. when the calling thread isn't the
// foreground thread on Windows 10+); we don't treat that as an
// error because the call still requests the OS to flash the
// taskbar entry, which is the recovery path Windows offers.
func FocusBrowserWindow(titleSubstring string) error {
	user32 := syscall.NewLazyDLL("user32.dll")
	enumWindows := user32.NewProc("EnumWindows")
	getWindowText := user32.NewProc("GetWindowTextW")
	getWindowTextLen := user32.NewProc("GetWindowTextLengthW")
	isWindowVisible := user32.NewProc("IsWindowVisible")
	setForegroundWindow := user32.NewProc("SetForegroundWindow")
	showWindow := user32.NewProc("ShowWindow")

	const (
		swRestore   = uintptr(9)
		titleBufLen = 256
	)

	wantLower := strings.ToLower(titleSubstring)
	var foundHwnd uintptr

	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		// Skip invisible windows so we don't pick up a minimized-to-tray
		// chrome.exe sibling or a hidden helper window.
		visible, _, _ := isWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1 // continue enumeration
		}
		titleLen, _, _ := getWindowTextLen.Call(hwnd)
		if titleLen == 0 {
			return 1
		}
		buf := make([]uint16, titleBufLen)
		ret, _, _ := getWindowText.Call(
			hwnd,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(titleBufLen),
		)
		if ret == 0 {
			return 1
		}
		title := syscall.UTF16ToString(buf[:ret])
		if strings.Contains(strings.ToLower(title), wantLower) {
			foundHwnd = hwnd
			return 0 // stop enumeration; we have our match
		}
		return 1
	})

	enumWindows.Call(cb, 0)

	if foundHwnd == 0 {
		// Wrap the cross-platform sentinel so callers can branch on
		// errors.Is(err, ErrFocusNoWindow) without coupling to Win32.
		return fmt.Errorf("%w: no top-level window with title containing %q",
			ErrFocusNoWindow, titleSubstring)
	}
	// SW_RESTORE un-minimizes if the window was minimized; for an
	// already-visible window it is effectively a no-op.
	showWindow.Call(foundHwnd, swRestore)
	setForegroundWindow.Call(foundHwnd)
	return nil
}
