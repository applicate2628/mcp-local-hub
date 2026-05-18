//go:build windows

package api

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func startFakeSupervisorIPCStatusServer(t *testing.T, stateDir string, hello SupervisorLockOwner, handler func(IPCRequest) IPCResponse) func() {
	t.Helper()
	t.Setenv("USERNAME", fmt.Sprintf("mcphub-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	addr := SupervisorIPCAddress(stateDir)
	ln, err := winio.ListenPipe(addr, &winio.PipeConfig{
		MessageMode:      false,
		InputBufferSize:  4096,
		OutputBufferSize: 4096,
	})
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
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("fake supervisor IPC listener did not stop")
		}
	}
}
