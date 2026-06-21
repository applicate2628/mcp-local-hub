// internal/gui/resolve_symlink_open_posix.go
//go:build !windows

package gui

import "syscall"

// resolveTargetOpenFlags adds O_NONBLOCK on POSIX so that os.OpenFile on a
// resolved symlink target that has been swapped to a FIFO returns IMMEDIATELY
// instead of blocking forever waiting for a writer. Without it, a local
// process replacing the resolved target with a FIFO in the os.Stat -> os.Open
// window would hang the handler goroutine before the post-open f.Stat()
// IsRegular re-check ever runs. For a regular file O_NONBLOCK is harmless to
// the subsequent bounded io.LimitReader read; the post-open fstat IsRegular
// gate refuses the FIFO once the nonblocking open returns.
func resolveTargetOpenFlags() int {
	return syscall.O_RDONLY | syscall.O_NONBLOCK
}
