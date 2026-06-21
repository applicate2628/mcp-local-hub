// internal/gui/resolve_symlink_open_windows.go
//go:build windows

package gui

import "syscall"

// resolveTargetOpenFlags returns the plain read-only flags on Windows. Windows
// named pipes are not opened via a filesystem path the way POSIX FIFOs are, and
// the resolved target here is a real on-disk file (api.ResolveClientConfigSymlink
// uses GetFinalPathNameByHandle), so os.Open does not block on a non-regular
// target. O_NONBLOCK is not a meaningful Windows open flag, so it is omitted.
func resolveTargetOpenFlags() int {
	return syscall.O_RDONLY
}
