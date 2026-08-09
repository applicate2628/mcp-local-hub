// fake_daemon is a probe fixture — NOT part of the shipped mcp-local-hub
// binary (own go.mod, standalone module; also under an underscore-prefixed
// parent so `go build ./...` etc. from the main module never see it).
//
// It reproduces, as a real OS process, the exact MCP Streamable-HTTP
// handshake + tool-call contract mcp-local-hub/internal/gui/
// serena_router_test.go's fakeSerenaDaemon Go-test fixture implements
// (initialize mints a session id; notifications/initialized acks 202; any
// other POST requires the daemon session header and answers a recognizable
// JSON-RPC result). Used as the upstream a probe-registered workspace points
// at, so the F3 end-to-end probe can prove a REAL forwarded tool-call
// round-trips through the front daemon, not just the router's own
// synthesized `initialize` response.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
)

func main() {
	port := flag.Int("port", 19301, "port to bind on 127.0.0.1")
	flag.Parse()

	var mu sync.Mutex
	issued := map[string]bool{}
	mintCount := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &probe)

		idOrNull := func(id json.RawMessage) string {
			if len(id) == 0 {
				return "null"
			}
			return string(id)
		}

		switch probe.Method {
		case "initialize":
			mu.Lock()
			mintCount++
			sid := fmt.Sprintf("fake-daemon-session-%d", mintCount)
			issued[sid] = true
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"serverInfo":{"name":"serena","version":"fake-probe"},"capabilities":{"tools":{}}}}`,
				idOrNull(probe.ID), probe.Params.ProtocolVersion)
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "notifications/cancelled":
			w.WriteHeader(http.StatusAccepted)
			return
		}

		sid := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		known := sid != "" && issued[sid]
		mu.Unlock()
		if !known {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"missing or unknown Mcp-Session-Id"}}`, idOrNull(probe.ID))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"fake_daemon_alive":true,"tool":"probe-marker"}}`, idOrNull(probe.ID))
	})

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("fake-daemon: listening on %s/mcp", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
