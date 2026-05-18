//go:build !windows

package api

import (
	"net"
	"os"
	"testing"
	"time"
)

func startFakeSupervisorIPCStatusServer(t *testing.T, stateDir string, hello SupervisorLockOwner, handler func(IPCRequest) IPCResponse) func() {
	t.Helper()
	addr := SupervisorIPCAddress(stateDir)
	_ = os.Remove(addr)
	ln, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatalf("listen fake supervisor IPC %s: %v", addr, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serveFakeSupervisorIPCStatusConn(t, conn, hello, handler)
	}()
	return func() {
		_ = ln.Close()
		_ = os.Remove(addr)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("fake supervisor IPC listener did not stop")
		}
	}
}
