//go:build windows

package api

import (
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

func startFakeSupervisorIPCStatusServer(t *testing.T, stateDir string, hello SupervisorLockOwner, handler func(IPCRequest) IPCResponse) func() {
	t.Helper()
	// Route the pipe name through the per-test discriminator instead of the
	// kernel SID. SupervisorIPCAddress (supervisor_ipc_address_windows.go)
	// resolves the SID FIRST and only consults USERNAME on a SID-query
	// failure; on a real Windows host the SID always resolves, so the old
	// `t.Setenv("USERNAME", ...)` override was dead-ended by the PR #212 SID
	// migration and every listening test bound the PRODUCTION pipe
	// `\\.\pipe\mcphub-supervisor-<SID>`, colliding with the live running
	// supervisor ("Access is denied"). The discriminator (installed by this
	// package's TestMain via EnableSupervisorIPCTestPipeIsolation) keys off
	// MCPHUB_STATE_DIR_OVERRIDE, so a unique temp dir here yields a unique
	// `\\.\pipe\mcphub-supervisor-test-<hash>` for BOTH this listener and the
	// client under test — they both call SupervisorIPCAddress and converge.
	// The env var only steers the discriminator; the on-disk state path stays
	// controlled by daemonStateRootOverride (set via withDaemonStateRootOverride),
	// so the seeded supervisor.lock.owner.json is unaffected.
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", t.TempDir())
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
