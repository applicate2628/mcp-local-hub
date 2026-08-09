//go:build !windows

package cmakegraph

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
)

func TestR15NonRegularGraphRootsAreRejectedBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "CMakeLists.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	fd, err := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open FIFO without blocking: %v", err)
	}
	defer syscall.Close(fd)

	if _, err := Walk(context.Background(), fifo, dir, Options{}); err == nil {
		t.Fatal("Walk accepted a FIFO root, want a regular-file admission error")
	}
	if _, err := WalkTree(context.Background(), dir, dir, []string{"CMakeLists.txt"}, Options{}); err == nil {
		t.Fatal("WalkTree accepted a matching FIFO root, want a regular-file admission error")
	}
}
