//go:build !windows

package tray

import "io"

// runChildImpl is a no-op stub for non-Windows hosts. The tray is
// Windows-only per spec §2.2; the child subcommand returns
// immediately if invoked here. The parent-side Run() in tray.go
// still spawns the child but the child exits straight away and the
// parent's stdout-reader sees EOF, which is the correct "no tray"
// outcome on Linux/macOS.
func runChildImpl(r io.Reader, w io.Writer) error {
	return nil
}
