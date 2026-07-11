package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"mcp-local-hub/internal/api"
)

func supervisorIPCOwnerForHello(ownerOpt ...api.SupervisorLockOwner) api.SupervisorLockOwner {
	if len(ownerOpt) > 0 && ownerOpt[0].PID > 0 && ownerOpt[0].StartedAt != "" {
		return ownerOpt[0]
	}
	return api.SupervisorLockOwner{
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// WriteHello writes the spec-required hello handshake frame to conn:
//
//	{"hello":{"version":1,"pid":<pid>,"started_at":"<RFC3339Nano>"}}\n
//
// It is the FIRST server frame the client reads, before any command
// response (Spec §"Wire format" / §"Handshake"). Both platform
// SupervisorIPCListener structs (windows / posix) carry the identity
// fields this reads, so the method lives here in the shared file and
// compiles on every target.
//
// This I/O USED to run synchronously inside Accept(). It was moved OFF
// the accept hot path into the per-connection serveIPCConn goroutine so
// a client that dials, then vanishes before the server finishes the
// hello write (common under host saturation — "write hello: The pipe is
// being closed.") can no longer stall Accept() of the NEXT client. On
// any write error the caller (serveIPCConn) closes the connection and
// emits a distinct ipc-hello-write-error event; the accept loop's
// consecutive-error budget is untouched.
func (l *SupervisorIPCListener) WriteHello(conn net.Conn) error {
	hello := api.IPCHello{
		Version:   1,
		PID:       l.pid,
		StartedAt: l.startedAt,
	}
	frame := map[string]any{"hello": hello}
	body, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal hello: %w", err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("write hello: %w", err)
	}
	return nil
}
