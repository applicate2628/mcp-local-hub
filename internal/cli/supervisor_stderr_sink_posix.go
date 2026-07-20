//go:build linux || darwin

package cli

import (
	"fmt"
	"os"
	"syscall"
)

// stderrIsInteractiveConsole reports whether this process's stderr is a
// terminal the operator is watching. A tty is a character device; a pipe,
// a regular file, and a closed/rebound descriptor are not. Stat failure is
// treated as non-interactive (the detached shape), so the redirect fires
// and capture is preserved.
func stderrIsInteractiveConsole() bool {
	st, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// savedStderrBinding holds the process stderr binding displaced by a
// redirect, so release() can put it back. The saved descriptor is a dup of
// fd 2 taken before the redirect.
type savedStderrBinding struct {
	dupFD int
	file  *os.File
}

// captureStderrBinding duplicates the current fd 2 so it can be restored.
func captureStderrBinding() (savedStderrBinding, error) {
	dup, err := syscall.Dup(2)
	if err != nil {
		return savedStderrBinding{}, fmt.Errorf("dup(2): %w", err)
	}
	return savedStderrBinding{dupFD: dup, file: os.Stderr}, nil
}

// restore puts the previously-captured binding back and releases the dup.
// A zero binding (never captured) is a no-op.
func (s savedStderrBinding) restore() error {
	if s.file == nil {
		return nil
	}
	if err := dupOntoStderr(s.dupFD); err != nil {
		return fmt.Errorf("restore fd 2: %w", err)
	}
	if err := syscall.Close(s.dupFD); err != nil {
		return fmt.Errorf("close saved stderr dup: %w", err)
	}
	os.Stderr = s.file
	return nil
}

// redirectProcessStderr points file descriptor 2 at f.
//
// dup2/dup3 onto fd 2 is what makes a Go RUNTIME panic land in the sink:
// the runtime writes panics to the raw descriptor, not through the
// os.Stderr variable, so rebinding the descriptor itself is required.
// The dup is atomic — fd 2 is never momentarily closed — so there is no
// window in which a panic could be lost.
func redirectProcessStderr(f *os.File) error {
	return dupOntoStderr(int(f.Fd()))
}
