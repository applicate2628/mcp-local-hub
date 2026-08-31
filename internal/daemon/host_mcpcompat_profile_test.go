package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStdioHostLegacyProfileAcceptsLLDBStyleTCPJSONL(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				serverDone <- err
				return
			}
			var method string
			_ = json.Unmarshal(msg["method"], &method)
			result := map[string]any{}
			if method == "initialize" {
				result["protocolVersion"] = "2024-11-05"
				result["capabilities"] = map[string]any{"tools": map[string]any{}}
			} else {
				result["tools"] = []any{}
			}
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg["id"]), "result": result})
			if _, err := fmt.Fprintln(conn, string(out)); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- scanner.Err()
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	bridge := `import socket, sys
s = socket.create_connection(("127.0.0.1", int(sys.argv[1])))
f = s.makefile("rwb", buffering=0)
for line in sys.stdin.buffer:
    f.write(line)
    sys.stdout.buffer.write(f.readline())
    sys.stdout.buffer.flush()
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := NewStdioHost(HostConfig{
		Command:                         "python",
		Args:                            []string{"-u", "-c", bridge, fmt.Sprint(port)},
		MCPProtocolCompatibilityProfile: "stdio-http-legacy-2024-11-05",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	ts := httptest.NewServer(h.HTTPHandler())
	defer ts.Close()

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`
	first := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, init, "", "2024-11-05")
	if first.status != http.StatusOK {
		t.Fatalf("initialize status=%d body=%s", first.status, first.body)
	}
	if !strings.Contains(string(first.body), `"protocolVersion":"2024-11-05"`) {
		t.Fatalf("initialize response was rewritten or lost legacy version: %s", first.body)
	}
	sid := first.header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not create a session")
	}

	initialized := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid, "2024-11-05")
	if initialized.status != http.StatusAccepted {
		t.Fatalf("notifications/initialized status=%d body=%s", initialized.status, initialized.body)
	}
	list := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, sid, "2024-11-05")
	if list.status != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", list.status, list.body)
	}

	unsupported := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`, sid, "1900-01-01")
	if unsupported.status != http.StatusBadRequest {
		t.Fatalf("unsupported session header status=%d want=400 body=%s", unsupported.status, unsupported.body)
	}
	mismatch := sessionContractRequest(t, ts.Client(), http.MethodPost, ts.URL, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`, sid, "2025-03-26")
	if mismatch.status != http.StatusBadRequest {
		t.Fatalf("mismatched session header status=%d want=400 body=%s", mismatch.status, mismatch.body)
	}

	deleted := sessionContractRequest(t, ts.Client(), http.MethodDelete, ts.URL, "", sid, "2024-11-05")
	if deleted.status != http.StatusNoContent {
		t.Fatalf("DELETE status=%d want=204 body=%s", deleted.status, deleted.body)
	}
}

func TestNewStdioHostRejectsUnknownCompatibilityProfileBeforeStart(t *testing.T) {
	if _, err := NewStdioHost(HostConfig{
		Command:                         echoSubprocCommand(),
		Args:                            echoSubprocArgs(),
		MCPProtocolCompatibilityProfile: "unknown",
	}); err == nil {
		t.Fatal("NewStdioHost accepted an unknown compatibility profile")
	}
}
