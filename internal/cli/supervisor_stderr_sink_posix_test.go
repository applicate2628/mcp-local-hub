//go:build linux || darwin

package cli

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCaptureStderrBinding_SetsCloseOnExec(t *testing.T) {
	saved, err := captureStderrBinding()
	if err != nil {
		t.Fatalf("capture stderr binding: %v", err)
	}
	defer func() { _ = syscall.Close(saved.dupFD) }()

	flags, err := unix.FcntlInt(uintptr(saved.dupFD), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("get saved stderr dup flags: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("saved stderr dup fd %d lacks FD_CLOEXEC (flags %#x)", saved.dupFD, flags)
	}
}
