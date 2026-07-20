//go:build windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// stderrIsInteractiveConsole reports whether this process's stderr is a
// Windows console the operator is watching.
//
// GetConsoleMode on the handle is the authoritative test and is applied to
// the STDERR HANDLE ITSELF, not to process console attachment: a process
// can own a console (CONOUT$ opens fine) while its stderr is redirected to
// a pipe, and in that case the operator is NOT watching stderr, so the
// redirect must still fire.
//
// Verified 2026-07-20 on this host: GetConsoleMode succeeds on a real
// console handle (CONOUT$) and fails with "The handle is invalid" on both
// a pipe handle and a regular-file handle — the two detached shapes.
func stderrIsInteractiveConsole() bool {
	h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		// No usable stderr at all (the classic detached Task Scheduler
		// shape). Not a console — redirect so panics become durable.
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(h, &mode) == nil
}

// savedStderrBinding holds the process stderr binding displaced by a
// redirect, so release() can put it back.
type savedStderrBinding struct {
	handle windows.Handle
	file   *os.File
}

// captureStderrBinding records the current process-wide stderr binding
// before it is displaced.
func captureStderrBinding() (savedStderrBinding, error) {
	h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return savedStderrBinding{}, fmt.Errorf("GetStdHandle(STD_ERROR_HANDLE): %w", err)
	}
	return savedStderrBinding{handle: h, file: os.Stderr}, nil
}

// restore puts the previously-captured binding back. A zero binding (never
// captured) is a no-op.
func (s savedStderrBinding) restore() error {
	if s.file == nil {
		return nil
	}
	if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, s.handle); err != nil {
		return fmt.Errorf("restore STD_ERROR_HANDLE: %w", err)
	}
	os.Stderr = s.file
	return nil
}

// redirectProcessStderr points the OS-level STD_ERROR_HANDLE at f.
//
// This is what makes a Go RUNTIME panic land in the sink: the runtime
// resolves the stderr handle through GetStdHandle at write time, so a
// SetStdHandle installed after startup is honored (probed on
// go1.26.5/windows-amd64, 2026-07-20).
//
// The previously-installed handle is deliberately NOT closed here: it is
// retained in the sink's savedStderrBinding so release() can restore it,
// and closing it could disturb a parent still reading that pipe.
func redirectProcessStderr(f *os.File) error {
	return windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
}
