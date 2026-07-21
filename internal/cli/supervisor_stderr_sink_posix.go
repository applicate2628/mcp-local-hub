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

// sinkOwnsProcessStderrFD reports whether the sink file IS the process
// stderr descriptor (fd 2) rather than merely being bound to it.
//
// This happens when the parent handed us a process with fd 2 closed: open(2)
// returns the lowest free descriptor, which is then 2 itself. It is the same
// degenerate case redirectProcessStderr already accepts as "already bound".
//
// It matters on RELEASE because closing such a file closes fd 2 itself. See
// (*supervisorStderrSink).release for the consequence.
func sinkOwnsProcessStderrFD(f *os.File) bool {
	return f != nil && int(f.Fd()) == 2
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
	fd := int(f.Fd())
	// If the sink already landed ON fd 2 (possible when the parent handed us
	// a process with fd 2 closed, so open(2) hands the lowest free descriptor
	// straight back), it is already bound and no dup is needed. Guarding is
	// not cosmetic: Linux dup3 returns EINVAL when oldfd == newfd (unlike
	// dup2, which succeeds as a no-op), and the subsequent f.Close() on the
	// error path would then close fd 2 itself — destroying stderr entirely
	// instead of redirecting it. Cheaper to hold than to reason about
	// reachability.
	if fd == 2 {
		return nil
	}
	return dupOntoStderr(fd)
}
