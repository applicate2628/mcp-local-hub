//go:build !windows

package boundedio

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestR24OpenRegularRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "special")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenRegular(path); err == nil {
		_ = file.Close()
		t.Fatal("FIFO admitted as a regular file")
	}
}
