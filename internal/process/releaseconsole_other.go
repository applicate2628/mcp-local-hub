//go:build !windows

package process

// ReleaseParentConsole is a no-op on non-Windows platforms.
//
// The Windows variant exists because mcphub.exe is a Windows-subsystem
// binary that attaches to its parent's console at startup, which makes it
// a client of that console and therefore a casualty of CTRL_CLOSE_EVENT
// when the terminal closes. POSIX has no equivalent coupling: closing a
// terminal emulator sends SIGHUP to the foreground process group, and the
// GUI path on POSIX is not the shipped operator entry point. Keeping the
// seam cross-platform lets the caller stay build-tag free.
func ReleaseParentConsole() {}
