//go:build !windows

package cmakegraph

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestR26OpenRootRegularDoesNotBlockOnFIFO(t *testing.T) {
	rootPath := t.TempDir()
	if err := syscall.Mkfifo(rootPath+string(os.PathSeparator)+"pipe", 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan error, 1)
	go func() {
		file, openErr := openRootRegular(root, "pipe")
		if file != nil {
			_ = file.Close()
		}
		done <- openErr
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO was admitted as a regular file")
		}
	case <-time.After(time.Second):
		t.Fatal("openRootRegular blocked on a FIFO")
	}
}
