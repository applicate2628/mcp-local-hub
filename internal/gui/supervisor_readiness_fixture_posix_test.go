//go:build !windows

package gui

import (
	"net"
	"os"
	"testing"

	"mcp-local-hub/internal/api"
)

func startReadySupervisorStatusServer(t *testing.T, stateDir string, _ api.SupervisorLockOwner, serve func(net.Conn)) func() {
	t.Helper()
	addr := api.SupervisorIPCAddress(stateDir)
	_ = os.Remove(addr)
	listener, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatalf("listen ready supervisor IPC %s: %v", addr, err)
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
	return func() { _ = listener.Close(); <-done; _ = os.Remove(addr) }
}
