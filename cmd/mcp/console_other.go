//go:build !windows

package main

// attachParentConsoleIfAvailable is a no-op on non-Windows platforms;
// the OS already hands us stdin/stdout/stderr correctly.
func attachParentConsoleIfAvailable() {}
