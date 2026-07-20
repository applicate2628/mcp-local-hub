//go:build linux

package cli

import "golang.org/x/sys/unix"

// dupOntoStderr rebinds fd 2 to fd. Linux uses Dup3 rather than Dup2
// because Dup2 is not available on every Linux architecture the release
// pipeline cross-builds (notably linux/arm64, where the kernel provides
// dup3 only); dup3 with flags=0 is exactly dup2.
func dupOntoStderr(fd int) error {
	return unix.Dup3(fd, 2, 0)
}
