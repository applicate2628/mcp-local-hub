//go:build !windows

package main

// attachParentConsoleIfAvailable is a no-op on non-Windows platforms;
// the OS already hands us stdin/stdout/stderr correctly.
//
// It reports false because no POSIX process is a client of a Windows
// console object, so nothing here is a CTRL_CLOSE_EVENT target and the
// console-release path must stay switched off. See process.HasConsole.
func attachParentConsoleIfAvailable() bool { return false }
