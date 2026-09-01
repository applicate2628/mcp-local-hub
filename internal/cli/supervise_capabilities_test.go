package cli

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// The probe is deliberately accepted before reconcile readiness, so a freshly
// started current supervisor can prove its control contract before a caller
// decides whether to mutate stop/restart intent.
func TestDispatchIPCCapabilitiesIsReadOnlyAndPreReadiness(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- dispatchIPCRequest(server, api.IPCRequest{Version: 1, ID: 91, Cmd: "capabilities"}, ipcDispatchDeps{})
	}()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read capabilities response: %v", err)
	}
	var response api.IPCResponse
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatalf("decode capabilities response: %v", err)
	}
	if !response.OK || response.Error != nil || !response.Final {
		t.Fatalf("response=%+v, want final successful read-only capability response", response)
	}
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	var caps api.SupervisorControlCapabilities
	if err := json.Unmarshal(encoded, &caps); err != nil || !caps.StopBatch || !caps.Respawn {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("dispatch capabilities: %v", err)
	}
}
