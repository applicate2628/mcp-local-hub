package lldb

import (
	"bytes"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBridgeSignalReturnsWhenStdinIdle(t *testing.T) {
	t.Parallel()

	sockA, sockB := net.Pipe()
	defer sockB.Close()

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()

	var out bytes.Buffer
	sigCh := make(chan os.Signal, 1)
	done := make(chan error, 1)

	go func() {
		done <- bridge(sockA, stdinR, &out, sigCh)
	}()

	sigCh <- syscall.SIGTERM

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "interrupted by") {
			t.Fatalf("expected interrupted error, got: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("bridge did not return promptly on signal while stdin was idle")
	}
}
