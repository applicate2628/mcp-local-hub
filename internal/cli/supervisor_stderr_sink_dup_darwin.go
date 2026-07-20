//go:build darwin

package cli

import "golang.org/x/sys/unix"

// dupOntoStderr rebinds fd 2 to fd. Darwin has no dup3; Dup2 is the
// portable call there.
func dupOntoStderr(fd int) error {
	return unix.Dup2(fd, 2)
}
