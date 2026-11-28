package lldb

import (
	"errors"
	"net"
	"testing"
)

func TestWaitForPortUsesOwnedExit(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	t.Run("running child can connect", func(t *testing.T) {
		s := &spawnedLldb{done: make(chan struct{})}
		conn, err := waitForPort(listener.Addr().String(), s)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	})

	t.Run("reaped child returns its error before dialing", func(t *testing.T) {
		want := errors.New("fixture exit")
		s := &spawnedLldb{done: make(chan struct{}), waitErr: want}
		close(s.done)
		conn, err := waitForPort(listener.Addr().String(), s)
		if conn != nil {
			conn.Close()
			t.Fatal("dialed after the original child exited")
		}
		if !errors.Is(err, want) {
			t.Fatalf("waitForPort: %v, want original exit %v", err, want)
		}
	})
}
