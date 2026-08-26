//go:build windows

package gui

import (
	"net"
	"testing"

	"mcp-local-hub/internal/api"

	"github.com/Microsoft/go-winio"
)

func startReadySupervisorStatusServer(t *testing.T, stateDir string, _ api.SupervisorLockOwner, serve func(net.Conn)) func() {
	t.Helper()
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
	listener, err := winio.ListenPipe(api.SupervisorIPCAddress(stateDir), &winio.PipeConfig{InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		t.Fatalf("listen ready supervisor IPC: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return func() { _ = listener.Close(); <-done }
}
