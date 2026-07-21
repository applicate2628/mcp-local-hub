//go:build linux || darwin

package cli

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// stderrIsInteractiveConsole reports whether this process's stderr is a
// terminal the operator is actually watching. It exists only to spare such
// an operator a hijacked terminal; everything else must be captured.
//
// This has to be a TTY test, NOT a character-device test. /dev/null is a
// character device, so the previous os.ModeCharDevice check reported a
// detached launch like `mcphub supervise 2>/dev/null` as interactive: the
// redirect was skipped, the Go runtime's panic traceback went to the void,
// and the audit row claimed "interactive-console" while capture was in fact
// disabled. That is precisely the detached-death case this sink exists to
// record, so the predicate was silently defeating its own mechanism.
//
// term.IsTerminal is an ioctl termios get (TCGETS on Linux, TIOCGETA on
// Darwin — the per-OS request constant is maintained upstream). It succeeds
// only on a real tty and fails with ENOTTY on /dev/null, a pipe, and a
// regular file. Any failure therefore reads as "not a terminal", which is
// the fail-safe direction: redirect, and capture. Losing a panic traceback
// is far worse than hijacking a terminal we were unsure about.
//
// fd 2 is tested rather than the os.Stderr variable because fd 2 is the
// descriptor the Go runtime writes panics to, and the one
// redirectProcessStderr rebinds. This is the same predicate the sibling
// daemon-side sink uses (internal/daemon/stderr_sink.go).
func stderrIsInteractiveConsole() bool {
	return term.IsTerminal(2)
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
	// syscall.Dup clears FD_CLOEXEC. This binding remains open until the
	// supervisor exits, so it must not be inherited by supervised children.
	if _, err := unix.FcntlInt(uintptr(dup), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		_ = syscall.Close(dup)
		return savedStderrBinding{}, fmt.Errorf("set close-on-exec on saved stderr dup: %w", err)
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
